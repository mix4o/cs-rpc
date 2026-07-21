package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Sink はワーカが発するイベントの出力先（GUI の Hub が実装する）。
// worker が gui に直接依存しないよう、この最小インタフェースで疎結合にする。
type Sink interface {
	CommandReceived(seq int, id, method string, params json.RawMessage)
	CommandFinished(id, state string, result json.RawMessage, errMsg string)
	Logf(level, format string, args ...any)
}

// Worker はサーバ制御キューを lease→実行→complete で回す。
type Worker struct {
	Name string
	Ctrl *Control
	Out  Sink
	Poll time.Duration
	seq  int
}

// Run はコンテキストがキャンセルされるまでポーリングループを回す。
func (w *Worker) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = 500 * time.Millisecond
	}
	w.Out.Logf("info", "worker %q started (poll=%s)", w.Name, w.Poll)
	w.Out.Logf("info", "local methods: %v", Methods())

	idleLogged := false
	for {
		if ctx.Err() != nil {
			w.Out.Logf("info", "worker stopped")
			return
		}
		job, err := w.Ctrl.Lease(ctx, w.Name)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				continue
			}
			w.Out.Logf("error", "lease failed: %v", err)
			sleep(ctx, w.Poll)
			continue
		}
		if job == nil {
			if !idleLogged {
				w.Out.Logf("debug", "no pending command; polling every %s…", w.Poll)
				idleLogged = true
			}
			sleep(ctx, w.Poll)
			continue
		}
		idleLogged = false
		w.handle(ctx, job)
	}
}

func (w *Worker) handle(ctx context.Context, job *Job) {
	w.seq++
	w.Out.CommandReceived(w.seq, job.ID, job.Method, job.Params)
	w.Out.Logf("info", "received #%d id=%s method=%s params=%s", w.seq, job.ID, job.Method, string(job.Params))

	start := time.Now()
	result, herr := w.execute(ctx, job)
	elapsed := time.Since(start).Round(time.Millisecond)

	if herr != nil {
		e := &ErrObj{Code: herr.Code, Message: herr.Message, Data: herr.Data}
		w.Out.Logf("error", "id=%s failed in %s: %d %s", job.ID, elapsed, herr.Code, herr.Message)
		if err := w.Ctrl.Complete(ctx, job.ID, nil, e); err != nil {
			w.Out.Logf("error", "report(complete) failed id=%s: %v", job.ID, err)
		}
		w.Out.CommandFinished(job.ID, "error", nil, herr.Message)
		return
	}

	w.Out.Logf("info", "id=%s done in %s → %s", job.ID, elapsed, string(result))
	if err := w.Ctrl.Complete(ctx, job.ID, result, nil); err != nil {
		w.Out.Logf("error", "report(complete) failed id=%s: %v", job.ID, err)
	}
	w.Out.CommandFinished(job.ID, "done", result, "")
}

func (w *Worker) execute(ctx context.Context, job *Job) (json.RawMessage, *HandlerError) {
	h, ok := localHandlers[job.Method]
	if !ok {
		return nil, &HandlerError{Code: -32601, Message: "no local handler: " + job.Method}
	}
	w.Out.Logf("debug", "executing %s locally…", job.Method)
	res, herr := h(ctx, job.Params)
	if herr != nil {
		return nil, herr
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, &HandlerError{Code: -32603, Message: "marshal result: " + err.Error()}
	}
	return b, nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
