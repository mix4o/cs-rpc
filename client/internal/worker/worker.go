package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Sink はワーカが発するイベントの出力先（GUI の Hub が実装する）。
type Sink interface {
	CommandReceived(seq int, id, method string, params json.RawMessage)
	CommandProgress(id string, progress map[string]any)
	CommandFinished(id, state string, result json.RawMessage, errMsg string)
	Logf(level, format string, args ...any)
}

// Worker はサーバ制御キューを lease→実行→complete で回す。実行はジョブごとに
// goroutine で行い（非同期）、長時間コマンド中もポーリングとキャンセルを止めない。
type Worker struct {
	Name string
	Ctrl *Control
	Out  Sink
	Poll time.Duration
	seq  int64
}

func (w *Worker) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = 500 * time.Millisecond
	}
	// 自ワーカが実行できるメソッドを申告（コントロールページの選択肢に反映される）。
	if err := w.Ctrl.Announce(ctx, w.Name, Methods()); err != nil {
		w.Out.Logf("warn", "announce failed: %v", err)
	}
	w.Out.Logf("info", "worker %q started (poll=%s)", w.Name, w.Poll)
	w.Out.Logf("info", "local methods: %v", Methods())

	var wg sync.WaitGroup
	idleLogged := false
	for {
		if ctx.Err() != nil {
			break
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
		wg.Add(1)
		go func(j *Job) {
			defer wg.Done()
			w.handle(ctx, j)
		}(job)
	}
	wg.Wait()
	w.Out.Logf("info", "worker stopped")
}

func (w *Worker) handle(ctx context.Context, job *Job) {
	seq := int(atomic.AddInt64(&w.seq, 1))
	w.Out.CommandReceived(seq, job.ID, job.Method, job.Params)
	w.Out.Logf("info", "received #%d id=%s method=%s params=%s", seq, job.ID, job.Method, string(job.Params))

	// ジョブ単位のキャンセル可能 ctx。emit がサーバの中断要求を検知したらここを止める。
	jobCtx, jobCancel := context.WithCancel(ctx)
	defer jobCancel()

	emit := func(progress map[string]any) bool {
		w.Out.CommandProgress(job.ID, progress)
		cancel, err := w.Ctrl.Progress(jobCtx, job.ID, progress)
		if err != nil {
			w.Out.Logf("warn", "progress report failed id=%s: %v", job.ID, err)
			return false
		}
		if cancel {
			w.Out.Logf("info", "cancel requested id=%s", job.ID)
			jobCancel()
		}
		return cancel
	}

	start := time.Now()
	result, herr := w.execute(jobCtx, job, emit)
	elapsed := time.Since(start).Round(time.Millisecond)

	if herr != nil && herr.Canceled {
		partial, _ := json.Marshal(herr.Data)
		w.Out.Logf("info", "id=%s canceled after %s", job.ID, elapsed)
		if err := w.Ctrl.Complete(context.Background(), job.ID, partial, nil, true); err != nil {
			w.Out.Logf("error", "report(complete) failed id=%s: %v", job.ID, err)
		}
		w.Out.CommandFinished(job.ID, "canceled", partial, "canceled")
		return
	}
	if herr != nil {
		e := &ErrObj{Code: herr.Code, Message: herr.Message, Data: herr.Data}
		w.Out.Logf("error", "id=%s failed in %s: %d %s", job.ID, elapsed, herr.Code, herr.Message)
		if err := w.Ctrl.Complete(context.Background(), job.ID, nil, e, false); err != nil {
			w.Out.Logf("error", "report(complete) failed id=%s: %v", job.ID, err)
		}
		w.Out.CommandFinished(job.ID, "error", nil, herr.Message)
		return
	}

	w.Out.Logf("info", "id=%s done in %s → %s", job.ID, elapsed, string(result))
	if err := w.Ctrl.Complete(context.Background(), job.ID, result, nil, false); err != nil {
		w.Out.Logf("error", "report(complete) failed id=%s: %v", job.ID, err)
	}
	w.Out.CommandFinished(job.ID, "done", result, "")
}

func (w *Worker) execute(ctx context.Context, job *Job, emit Emit) (json.RawMessage, *HandlerError) {
	h, ok := localHandlers[job.Method]
	if !ok {
		return nil, &HandlerError{Code: -32601, Message: "no local handler: " + job.Method}
	}
	w.Out.Logf("debug", "executing %s locally…", job.Method)
	res, herr := h(ctx, job.Params, emit)
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
