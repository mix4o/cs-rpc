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

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`echo hi there`, []string{"echo", "hi", "there"}},
		{`notepad "C:\My Docs\a.txt"`, []string{"notepad", `C:\My Docs\a.txt`}},
		{`cmd /c dir C:\Windows`, []string{"cmd", "/c", "dir", `C:\Windows`}},
		{`  spaced   out  `, []string{"spaced", "out"}},
	}
	for _, c := range cases {
		got := splitCommand(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%q -> %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q -> %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestExecCommandString(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "echo")
	params, _ := json.Marshal(map[string]any{"command": `echo "a b c"`, "wait": true})
	out, herr := hExec(context.Background(), params, noEmit)
	if herr != nil {
		t.Fatalf("exec error: %v", herr)
	}
	if !strings.Contains(out.(map[string]any)["stdout"].(string), "a b c") {
		t.Fatalf("unexpected: %v", out)
	}
}

func TestExecCommandStringAllowlistOnFirstToken(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "echo") // rm は許可外
	params, _ := json.Marshal(map[string]any{"command": `rm -rf /`, "wait": true})
	_, herr := hExec(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errExecNotAllowed {
		t.Fatalf("expected not-allowed on first token, got %v", herr)
	}
}

func TestScriptRunsMultilineAndCaptures(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "bash")
	body := "echo line-one\necho line-two"
	params, _ := json.Marshal(map[string]any{"interpreter": "bash", "script": body})
	out, herr := hScript(context.Background(), params, noEmit)
	if herr != nil {
		t.Fatalf("script error: %v", herr)
	}
	m := out.(map[string]any)
	so := m["stdout"].(string)
	if m["exitCode"].(int) != 0 || !strings.Contains(so, "line-one") || !strings.Contains(so, "line-two") {
		t.Fatalf("unexpected: %v", m)
	}
}

func TestScriptDisabledWithoutAllowlist(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "")
	params, _ := json.Marshal(map[string]any{"interpreter": "bash", "script": "echo x"})
	_, herr := hScript(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errExecDisabled {
		t.Fatalf("expected disabled, got %v", herr)
	}
}

func TestScriptInterpreterNotAllowed(t *testing.T) {
	t.Setenv("CSRPC_EXEC_ALLOW", "bash")
	params, _ := json.Marshal(map[string]any{"interpreter": "powershell", "script": "echo x"})
	_, herr := hScript(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errExecNotAllowed {
		t.Fatalf("expected not-allowed, got %v", herr)
	}
}

func TestPutfileDisabledWithoutBaseDir(t *testing.T) {
	t.Setenv("CSRPC_PUTFILE_DIR", "")
	params, _ := json.Marshal(map[string]any{"path": "x.txt", "content": "hi"})
	_, herr := hPutfile(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errPutDisabled {
		t.Fatalf("expected disabled, got %v", herr)
	}
}

func TestPutfileWritesWithinBase(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CSRPC_PUTFILE_DIR", base)
	params, _ := json.Marshal(map[string]any{"path": "sub/test.sh", "content": "echo hi\n", "mode": "0755"})
	out, herr := hPutfile(context.Background(), params, noEmit)
	if herr != nil {
		t.Fatalf("putfile error: %v", herr)
	}
	got, err := os.ReadFile(filepath.Join(base, "sub", "test.sh"))
	if err != nil || string(got) != "echo hi\n" {
		t.Fatalf("file not written correctly: %q err=%v", got, err)
	}
	if out.(map[string]any)["bytes"].(int) != len("echo hi\n") {
		t.Fatalf("bytes mismatch: %v", out)
	}
}

func TestPutfileRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CSRPC_PUTFILE_DIR", base)
	params, _ := json.Marshal(map[string]any{"path": "../escape.txt", "content": "x"})
	_, herr := hPutfile(context.Background(), params, noEmit)
	if herr == nil || herr.Code != errPutOutside {
		t.Fatalf("expected outside(%d), got %v", errPutOutside, herr)
	}
}

func TestPutfileBase64(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CSRPC_PUTFILE_DIR", base)
	enc := "aGVsbG8=" // "hello"
	params, _ := json.Marshal(map[string]any{"path": "b.bin", "content": enc, "encoding": "base64"})
	if _, herr := hPutfile(context.Background(), params, noEmit); herr != nil {
		t.Fatalf("putfile b64 error: %v", herr)
	}
	got, _ := os.ReadFile(filepath.Join(base, "b.bin"))
	if string(got) != "hello" {
		t.Fatalf("base64 decode wrong: %q", got)
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
