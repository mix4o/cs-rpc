package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHandlers(t *testing.T) {
	ctx := context.Background()
	if out, e := hEcho(ctx, json.RawMessage(`{"message":"hi"}`)); e != nil || out.(map[string]string)["message"] != "hi" {
		t.Fatalf("echo: %v %v", out, e)
	}
	if out, e := hAdd(ctx, json.RawMessage(`{"a":2,"b":3}`)); e != nil || out.(map[string]float64)["result"] != 5 {
		t.Fatalf("add: %v %v", out, e)
	}
	if _, e := hDiv(ctx, json.RawMessage(`{"a":1,"b":0}`)); e == nil || e.Code != 1001 {
		t.Fatalf("div-by-zero should error 1001, got %v", e)
	}
	if out, e := hSysInfo(ctx, nil); e != nil || out.(map[string]any)["executedOn"] != "client" {
		t.Fatalf("sysinfo: %v %v", out, e)
	}
}

// fakeSink はワーカのイベントを記録し、完了を通知する。
type fakeSink struct {
	mu       sync.Mutex
	finished map[string]string
	done     chan string
	logs     int
}

func newFakeSink() *fakeSink {
	return &fakeSink{finished: map[string]string{}, done: make(chan string, 8)}
}

func (f *fakeSink) CommandReceived(seq int, id, method string, params json.RawMessage) {}
func (f *fakeSink) CommandFinished(id, state string, result json.RawMessage, errMsg string) {
	f.mu.Lock()
	f.finished[id] = state
	f.mu.Unlock()
	f.done <- id
}
func (f *fakeSink) Logf(level, format string, args ...any) {
	f.mu.Lock()
	f.logs++
	f.mu.Unlock()
}

// oneJobServer は最初の lease で1件返し、その後は 204。complete を記録する。
func oneJobServer(t *testing.T, job Job) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	leased := false
	completed := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/autorun":
			w.Write([]byte(`{"autorun":false}`))
		case "/control/lease":
			mu.Lock()
			defer mu.Unlock()
			if leased {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			leased = true
			_ = json.NewEncoder(w).Encode(job)
		case "/control/complete":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			completed = append(completed, body["id"].(string))
			mu.Unlock()
			w.Write([]byte(`{"state":"done"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &completed
}

func TestWorkerProcessesJob(t *testing.T) {
	job := Job{ID: "abc", Seq: 1, Method: "math.add", Params: json.RawMessage(`{"a":40,"b":2}`)}
	srv, completed := oneJobServer(t, job)

	ctrl, err := NewControl(srv.URL+"/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := newFakeSink()
	w := &Worker{Name: "test", Ctrl: ctrl, Out: sink, Poll: 20 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	select {
	case id := <-sink.done:
		if id != "abc" {
			t.Fatalf("unexpected id %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish job in time")
	}
	cancel()

	sink.mu.Lock()
	state := sink.finished["abc"]
	sink.mu.Unlock()
	if state != "done" {
		t.Fatalf("expected done, got %q", state)
	}
	if len(*completed) != 1 || (*completed)[0] != "abc" {
		t.Fatalf("expected complete reported for abc, got %v", *completed)
	}
}

func TestLeaseEmptyReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ctrl, _ := NewControl(srv.URL, nil)
	job, err := ctrl.Lease(context.Background(), "w")
	if err != nil || job != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", job, err)
	}
}
