package worker

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"time"
)

// HandlerError はローカルハンドラの業務エラー。
type HandlerError struct {
	Code    int
	Message string
	Data    any
}

// Handler はローカルコマンド実装。params は生 JSON で受け取る。
type Handler func(ctx context.Context, params json.RawMessage) (any, *HandlerError)

// localHandlers はクライアント側で実行するコマンド群。サーバの登録メソッドと
// 同名でも、実行はこのクライアント上で行われる（sys.info はクライアントの
// OS/アーキを返すので「クライアントで実行された」ことが分かる）。
var localHandlers = map[string]Handler{
	"echo":       hEcho,
	"math.add":   hAdd,
	"math.div":   hDiv,
	"sys.info":   hSysInfo,
	"sys.time":   hSysTime,
	"demo.sleep": hSleep,
}

// Methods は登録済みローカルメソッド名を返す。
func Methods() []string {
	out := make([]string, 0, len(localHandlers))
	for k := range localHandlers {
		out = append(out, k)
	}
	return out
}

func decode[T any](params json.RawMessage) (T, *HandlerError) {
	var v T
	if len(params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(params, &v); err != nil {
		return v, &HandlerError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	return v, nil
}

func hEcho(_ context.Context, params json.RawMessage) (any, *HandlerError) {
	p, herr := decode[struct {
		Message string `json:"message"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	return map[string]string{"message": p.Message}, nil
}

func hAdd(_ context.Context, params json.RawMessage) (any, *HandlerError) {
	p, herr := decode[struct{ A, B float64 }](params)
	if herr != nil {
		return nil, herr
	}
	return map[string]float64{"result": p.A + p.B}, nil
}

func hDiv(_ context.Context, params json.RawMessage) (any, *HandlerError) {
	p, herr := decode[struct{ A, B float64 }](params)
	if herr != nil {
		return nil, herr
	}
	if p.B == 0 {
		return nil, &HandlerError{Code: 1001, Message: "division by zero"}
	}
	return map[string]float64{"result": p.A / p.B}, nil
}

func hSysInfo(_ context.Context, _ json.RawMessage) (any, *HandlerError) {
	host, _ := os.Hostname()
	return map[string]any{
		"executedOn": "client",
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"host":       host,
		"goVersion":  runtime.Version(),
	}, nil
}

func hSysTime(_ context.Context, _ json.RawMessage) (any, *HandlerError) {
	now := time.Now()
	return map[string]any{"epoch": now.Unix(), "iso": now.Format(time.RFC3339)}, nil
}

// hSleep はデモ用。running 状態を目視しやすくする。ctx キャンセルで中断。
func hSleep(ctx context.Context, params json.RawMessage) (any, *HandlerError) {
	p, herr := decode[struct {
		Seconds float64 `json:"seconds"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	d := time.Duration(p.Seconds * float64(time.Second))
	if d <= 0 {
		d = time.Second
	}
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	select {
	case <-time.After(d):
		return map[string]any{"slept": d.Seconds()}, nil
	case <-ctx.Done():
		return nil, &HandlerError{Code: -32603, Message: "canceled"}
	}
}
