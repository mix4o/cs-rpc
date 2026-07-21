package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var noEmit Emit = func(map[string]any) bool { return false }

func TestHandlers(t *testing.T) {
	ctx := context.Background()
	if out, e := hEcho(ctx, json.RawMessage(`{"message":"hi"}`), noEmit); e != nil || out.(map[string]string)["message"] != "hi" {
		t.Fatalf("echo: %v %v", out, e)
	}
	if out, e := hAdd(ctx, json.RawMessage(`{"a":2,"b":3}`), noEmit); e != nil || out.(map[string]float64)["result"] != 5 {
		t.Fatalf("add: %v %v", out, e)
	}
	if _, e := hDiv(ctx, json.RawMessage(`{"a":1,"b":0}`), noEmit); e == nil || e.Code != 1001 {
		t.Fatalf("div-by-zero should error 1001, got %v", e)
	}
	if out, e := hSysInfo(ctx, nil, noEmit); e != nil || out.(map[string]any)["executedOn"] != "client" {
		t.Fatalf("sysinfo: %v %v", out, e)
	}
}

func TestFindMatches(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.conf", "b.conf", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	params, _ := json.Marshal(map[string]any{"path": dir, "name": "*.conf"})
	out, herr := hFind(context.Background(), params, noEmit)
	if herr != nil {
		t.Fatalf("find error: %v", herr)
	}
	m := out.(map[string]any)
	if m["matched"].(int) != 2 {
		t.Fatalf("expected 2 matches, got %v", m["matched"])
	}
}

func TestExecDisabledWithoutAllowlist(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "")
	params, _ := json.Marshal(map[string]any{"program": "echo", "wait": true})
	_, herr := hExec(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errExecDisabled {
		t.Fatalf("expected disabled(%d), got %v", errExecDisabled, herr)
	}
}

func TestExecNotAllowed(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "calc,notepad")
	params, _ := json.Marshal(map[string]any{"program": "echo", "wait": true})
	_, herr := hExec(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errExecNotAllowed {
		t.Fatalf("expected not-allowed(%d), got %v", errExecNotAllowed, herr)
	}
}

func TestExecWaitCapturesOutput(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "echo")
	params, _ := json.Marshal(map[string]any{"program": "echo", "args": []string{"hello"}, "wait": true})
	out, herr := hExec(context.Background(), params, noEmit)
	if herr != nil {
		t.Fatalf("exec error: %v", herr)
	}
	m := out.(map[string]any)
	if m["exitCode"].(int) != 0 || !strings.Contains(m["stdout"].(string), "hello") {
		t.Fatalf("unexpected result: %v", m)
	}
}

func TestFindCancel(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.log", i)), []byte("x"), 0o644)
	}
	// 最初の emit で即キャンセルを要求する。ただし emit は約300ms間隔でしか
	// 呼ばれないので、ここでは ctx キャンセルで確実に止める経路を検証する。
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即キャンセル
	params, _ := json.Marshal(map[string]any{"path": dir, "name": "*.log"})
	_, herr := hFind(ctx, params, noEmit)
	if herr == nil || !herr.Canceled {
		t.Fatalf("expected canceled, got %v", herr)
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
func (f *fakeSink) CommandProgress(id string, progress map[string]any)                 {}
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
		case "/control/autorun", "/control/announce":
			w.Write([]byte(`{}`))
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
