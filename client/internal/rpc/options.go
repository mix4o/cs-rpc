package rpc

import (
	"io"
	"log"
	"net/http"
	"time"
)

// Options はクライアントの設定（設計書 01 の3章）。
type Options struct {
	Endpoint   string            // RPC エンドポイント（例: http://127.0.0.1:8080/rpc）
	Timeout    time.Duration     // 1 回の呼び出しのタイムアウト
	Retries    int               // トランスポートエラー時の最大リトライ回数
	Headers    map[string]string // 追加ヘッダ（将来の認証トークン等）
	HTTPClient *http.Client      // 差し替え用（テスト・TLS 設定）
	Logger     *log.Logger       // ログ出力先（nil で無効）
}

// DefaultOptions は既定値を返す。
func DefaultOptions() Options {
	return Options{
		Endpoint: "http://127.0.0.1:8080/rpc",
		Timeout:  30 * time.Second,
		Retries:  2,
		Headers:  map[string]string{},
		Logger:   log.New(io.Discard, "", 0),
	}
}

// CallOption は 1 回の呼び出し単位の設定。
type CallOption func(*callConfig)

type callConfig struct {
	idempotent bool
}

// Idempotent は冪等な呼び出しであることを示し、トランスポートエラー時の
// 再送を許可する（既定は再送しない安全側）。
func Idempotent() CallOption {
	return func(c *callConfig) { c.idempotent = true }
}
