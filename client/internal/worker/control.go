// Package worker はサーバの制御キューから受け取ったコマンドをクライアント側で
// 実行するワーカと、そのローカルコマンドハンドラを提供する。
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Job はサーバの /control/lease が返すジョブ。
type Job struct {
	ID     string          `json:"id"`
	Seq    int             `json:"seq"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ErrObj は JSON-RPC 風のエラー表現（/control/complete へ渡す）。
type ErrObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Control はサーバの /control/* REST API を叩く制御プレーン用クライアント。
type Control struct {
	base string // 例: http://127.0.0.1:8080
	http *http.Client
}

// NewControl は RPC エンドポイント（.../rpc）またはベース URL から Control を作る。
func NewControl(endpoint string, hc *http.Client) (*Control, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	base := u.Scheme + "://" + u.Host
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Control{base: base, http: hc}, nil
}

func (c *Control) SetAutorun(ctx context.Context, enabled bool) error {
	_, err := c.post(ctx, "/control/autorun", map[string]bool{"enabled": enabled})
	return err
}

// Lease は次の queued ジョブを取得する。キューが空なら (nil, nil)。
func (c *Control) Lease(ctx context.Context, worker string) (*Job, error) {
	status, body, err := c.postRaw(ctx, "/control/lease", map[string]string{"worker": worker})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("lease: unexpected status %d: %s", status, body)
	}
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, fmt.Errorf("lease: decode: %w", err)
	}
	return &job, nil
}

// Complete は結果またはエラーを報告する。
func (c *Control) Complete(ctx context.Context, id string, result json.RawMessage, e *ErrObj) error {
	payload := struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *ErrObj         `json:"error,omitempty"`
	}{ID: id, Result: result, Error: e}
	_, err := c.post(ctx, "/control/complete", payload)
	return err
}

func (c *Control) post(ctx context.Context, path string, body any) ([]byte, error) {
	status, raw, err := c.postRaw(ctx, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("%s: status %d: %s", path, status, raw)
	}
	return raw, nil
}

func (c *Control) postRaw(ctx context.Context, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}
