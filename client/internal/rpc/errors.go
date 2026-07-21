package rpc

import "fmt"

// TransportError はトランスポート層の失敗（接続失敗・タイムアウト・5xx・
// 応答の JSON 破損など）を表す。呼び出し側は errors.As で RPCError と区別し、
// リトライ可否の判断に使う。
type TransportError struct {
	Op         string // 発生箇所（"post", "decode" 等）
	StatusCode int    // HTTP ステータス（無い場合は 0）
	Err        error  // 原因
}

func (e *TransportError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("transport error (%s, status=%d): %v", e.Op, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("transport error (%s): %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// retryable は TransportError が再送候補かを返す。
// 接続失敗・タイムアウト（StatusCode==0）と 5xx を対象とする。
func (e *TransportError) retryable() bool {
	if e.StatusCode == 0 {
		return true // 接続失敗・タイムアウト
	}
	return e.StatusCode >= 500
}
