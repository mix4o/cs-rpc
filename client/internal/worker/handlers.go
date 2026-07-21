package worker

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// HandlerError はローカルハンドラの業務エラー。Canceled=true は中断を表す
// （Data に途中までの結果を入れてよい）。
type HandlerError struct {
	Code     int
	Message  string
	Data     any
	Canceled bool
}

// Emit は途中経過を報告するコールバック。戻り値 true は「中断要求あり」。
type Emit func(progress map[string]any) bool

// Handler はローカルコマンド実装。長時間処理は emit で進捗を送り、その戻り値または
// ctx.Err() で中断を検知する。
type Handler func(ctx context.Context, params json.RawMessage, emit Emit) (any, *HandlerError)

// localHandlers はクライアント側で実行するコマンド群。
var localHandlers = map[string]Handler{
	"echo":       hEcho,
	"math.add":   hAdd,
	"math.div":   hDiv,
	"sys.info":   hSysInfo,
	"sys.time":   hSysTime,
	"demo.sleep": hSleep,
	"find":       hFind,
}

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

func hEcho(_ context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct {
		Message string `json:"message"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	return map[string]string{"message": p.Message}, nil
}

func hAdd(_ context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct{ A, B float64 }](params)
	if herr != nil {
		return nil, herr
	}
	return map[string]float64{"result": p.A + p.B}, nil
}

func hDiv(_ context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct{ A, B float64 }](params)
	if herr != nil {
		return nil, herr
	}
	if p.B == 0 {
		return nil, &HandlerError{Code: 1001, Message: "division by zero"}
	}
	return map[string]float64{"result": p.A / p.B}, nil
}

func hSysInfo(_ context.Context, _ json.RawMessage, _ Emit) (any, *HandlerError) {
	host, _ := os.Hostname()
	return map[string]any{
		"executedOn": "client",
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"host":       host,
		"goVersion":  runtime.Version(),
	}, nil
}

func hSysTime(_ context.Context, _ json.RawMessage, _ Emit) (any, *HandlerError) {
	now := time.Now()
	return map[string]any{"epoch": now.Unix(), "iso": now.Format(time.RFC3339)}, nil
}

func hSleep(ctx context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
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
		return nil, &HandlerError{Canceled: true, Message: "canceled"}
	}
}

// hFind は path 配下を走査し、name（glob）に一致するファイルを収集する。
// 長時間になり得るため、約300ms間隔で進捗(scanned/matched)を emit し、その戻り値
// または ctx で中断する。結果件数は maxResults で頭打ちにする。
func hFind(ctx context.Context, params json.RawMessage, emit Emit) (any, *HandlerError) {
	p, herr := decode[struct {
		Path       string `json:"path"`
		Name       string `json:"name"`
		MaxResults int    `json:"maxResults"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	if p.Path == "" {
		p.Path = "."
	}
	if p.Name == "" {
		p.Name = "*"
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 1000
	}

	var matches []string
	scanned := 0
	truncated := false
	canceled := false
	lastEmit := time.Now()

	report := func() {
		lastEmit = time.Now()
		if emit(map[string]any{"scanned": scanned, "matched": len(matches)}) {
			canceled = true
		}
	}

	walkErr := filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 読めないエントリはスキップ（権限エラー等）
		}
		if ctx.Err() != nil || canceled {
			return filepath.SkipAll
		}
		scanned++
		if !d.IsDir() {
			if ok, _ := filepath.Match(p.Name, d.Name()); ok {
				matches = append(matches, path)
				if len(matches) >= p.MaxResults {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		if time.Since(lastEmit) >= 300*time.Millisecond {
			report()
		}
		return nil
	})

	result := map[string]any{
		"matches":   matches,
		"scanned":   scanned,
		"matched":   len(matches),
		"truncated": truncated,
	}
	if canceled || ctx.Err() != nil {
		return nil, &HandlerError{Canceled: true, Message: "canceled", Data: result}
	}
	if walkErr != nil {
		return nil, &HandlerError{Code: -32603, Message: "find failed: " + walkErr.Error()}
	}
	return result, nil
}
