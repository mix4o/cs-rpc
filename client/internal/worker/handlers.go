package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	"exec":       hExec,
	"script":     hScript,
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

// exec のドメインエラーコード
const (
	errExecNotAllowed = 1002 // allowlist 外
	errExecDisabled   = 1003 // allowlist 未設定（無効）
	errExecFailed     = 1004 // 起動/実行失敗
)

const execOutputCap = 64 * 1024 // 回収する出力の上限（バイト）

// execAllowlist は環境変数 CSRPC_EXEC_ALLOW（カンマ区切り）から許可プログラム集合を
// 作る。空なら空集合＝exec 無効。比較はベース名・小文字・.exe 除去で正規化する。
func execAllowlist() map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(os.Getenv("CSRPC_EXEC_ALLOW"), ",") {
		if strings.TrimSpace(p) == "" {
			continue // 空トークンは無視（filepath.Base("") が "." になるのを防ぐ）
		}
		m[normalizeProg(p)] = true
	}
	return m
}

func normalizeProg(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = filepath.Base(p)
	return strings.TrimSuffix(p, ".exe")
}

// scriptSpec はインタプリタ種別ごとの一時ファイル拡張子と起動 argv を決める。
// name=起動プログラム, rest=その引数（末尾に呼び出し側 args を足す）。
func scriptArgv(interp, file string) (name string, rest []string, ext string) {
	switch normalizeProg(interp) {
	case "powershell", "pwsh":
		return interp, []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", file}, ".ps1"
	case "cmd":
		return interp, []string{"/c", file}, ".cmd"
	case "bash", "sh":
		return interp, []string{file}, ".sh"
	default:
		return interp, []string{file}, ".txt"
	}
}

// hScript はスクリプト本文を一時ファイルに書いてインタプリタで実行する。
//
// セキュリティ: exec 以上に強力な RCE。allowlist（CSRPC_EXEC_ALLOW）に interpreter が
// 入っているときだけ実行可能。`powershell` を許可する＝そのマシンで任意 PowerShell が
// 実行できることに等しい。信頼できるネットワーク限定で。
//
// params: {interpreter?: string, script: string, args?: []string, wait?: bool}
//   - interpreter 既定: Windows="powershell" / それ以外="bash"
//   - wait 既定=true（完了まで待ち stdout/stderr/exitCode を返す。ctx で中断可）
func hScript(ctx context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct {
		Interpreter string   `json:"interpreter"`
		Script      string   `json:"script"`
		Args        []string `json:"args"`
		Wait        *bool    `json:"wait"` // 既定 true にするためポインタで受ける
	}](params)
	if herr != nil {
		return nil, herr
	}
	if strings.TrimSpace(p.Script) == "" {
		return nil, &HandlerError{Code: -32602, Message: "script is required"}
	}
	interp := p.Interpreter
	if interp == "" {
		if runtime.GOOS == "windows" {
			interp = "powershell"
		} else {
			interp = "bash"
		}
	}
	allow := execAllowlist()
	if len(allow) == 0 {
		return nil, &HandlerError{Code: errExecDisabled,
			Message: "script/exec is disabled: set CSRPC_EXEC_ALLOW to enable"}
	}
	if !allow[normalizeProg(interp)] {
		return nil, &HandlerError{Code: errExecNotAllowed,
			Message: "interpreter not allowed: " + interp}
	}

	name, rest, ext := scriptArgv(interp, "")
	f, err := os.CreateTemp("", "csrpc-script-*"+ext)
	if err != nil {
		return nil, &HandlerError{Code: errExecFailed, Message: "temp file: " + err.Error()}
	}
	tmp := f.Name()
	if _, err := f.WriteString(p.Script); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, &HandlerError{Code: errExecFailed, Message: "write script: " + err.Error()}
	}
	f.Close()
	// ファイル名が確定したので argv を作り直す（scriptArgv は file を埋め込む）。
	name, rest, _ = scriptArgv(interp, tmp)
	rest = append(rest, p.Args...)

	wait := p.Wait == nil || *p.Wait // 既定 true
	if !wait {
		cmd := exec.Command(name, rest...)
		if err := cmd.Start(); err != nil {
			os.Remove(tmp)
			return nil, &HandlerError{Code: errExecFailed, Message: "start failed: " + err.Error()}
		}
		pid := cmd.Process.Pid
		go func() { _ = cmd.Wait(); os.Remove(tmp) }() // 終了後に後始末
		return map[string]any{"started": true, "pid": pid, "interpreter": interp}, nil
	}

	defer os.Remove(tmp)
	cmd := exec.CommandContext(ctx, name, rest...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, &HandlerError{Canceled: true, Message: "canceled",
				Data: map[string]any{"stdout": truncate(stdout.String()), "stderr": truncate(stderr.String())}}
		}
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return nil, &HandlerError{Code: errExecFailed, Message: "run failed: " + runErr.Error()}
		}
	}
	return map[string]any{
		"interpreter": interp,
		"exitCode":    exitCode,
		"stdout":      truncate(stdout.String()),
		"stderr":      truncate(stderr.String()),
	}, nil
}

// splitCommand は単一コマンド文字列を引数トークンに分割する。
// クォート（"…" / '…'）でグループ化し、クォートは除去する。バックスラッシュは
// エスケープ扱いしない（Windows パス `C:\dir` を壊さないため）。
func splitCommand(s string) []string {
	var toks []string
	var cur strings.Builder
	inTok := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inTok = true
		case r == '"' || r == '\'':
			quote = r
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inTok {
				toks = append(toks, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if inTok {
		toks = append(toks, cur.String())
	}
	return toks
}

func truncate(s string) string {
	if len(s) > execOutputCap {
		return s[:execOutputCap] + "…(truncated)"
	}
	return s
}

// hExec は外部プログラムを実行する。
//
// セキュリティ: これは実質リモートコード実行。既定では無効で、実行側（ワーカ）の
// 環境変数 CSRPC_EXEC_ALLOW に許可プログラム名を列挙したときだけ、その中のものだけ
// 実行できる。信頼できるネットワーク限定で使うこと。
//
// params: {program: string, args?: []string, wait?: bool}
//   - wait=false（既定）: 起動して即完了（PID を返す。突き放し）。calc.exe 等の GUI 向け。
//   - wait=true: 実行完了まで待ち、stdout/stderr/終了コードを返す。ctx で中断・タイムアウト可。
func hExec(ctx context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
		Command string   `json:"command"` // 単一文字列。program 未指定時にこれを分割して使う
		Wait    bool     `json:"wait"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	// program 未指定なら command をトークン分割して program+args を得る。
	// 先頭トークンが program として allowlist 判定されるため、シェル丸投げより安全。
	if p.Program == "" {
		toks := splitCommand(p.Command)
		if len(toks) == 0 {
			return nil, &HandlerError{Code: -32602, Message: "program or command is required"}
		}
		p.Program, p.Args = toks[0], toks[1:]
	}

	allow := execAllowlist()
	if len(allow) == 0 {
		return nil, &HandlerError{Code: errExecDisabled,
			Message: "exec is disabled: set CSRPC_EXEC_ALLOW to enable"}
	}
	if !allow[normalizeProg(p.Program)] {
		return nil, &HandlerError{Code: errExecNotAllowed,
			Message: "program not allowed: " + p.Program}
	}

	if !p.Wait {
		// 突き放し: 起動して親から切り離し、完了を待たない。
		cmd := exec.Command(p.Program, p.Args...)
		if err := cmd.Start(); err != nil {
			return nil, &HandlerError{Code: errExecFailed, Message: "start failed: " + err.Error()}
		}
		pid := cmd.Process.Pid
		_ = cmd.Process.Release()
		return map[string]any{"started": true, "pid": pid, "program": p.Program}, nil
	}

	// 出力回収: ctx に紐付けて実行（キャンセル/タイムアウトでプロセスを止められる）。
	cmd := exec.CommandContext(ctx, p.Program, p.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ctx.Err() != nil {
			return nil, &HandlerError{Canceled: true, Message: "canceled",
				Data: map[string]any{"stdout": truncate(stdout.String()), "stderr": truncate(stderr.String())}}
		}
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return nil, &HandlerError{Code: errExecFailed, Message: "run failed: " + err.Error()}
		}
	}
	return map[string]any{
		"exitCode": exitCode,
		"stdout":   truncate(stdout.String()),
		"stderr":   truncate(stderr.String()),
	}, nil
}
