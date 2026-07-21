package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"
)

const (
	retryBaseDelay = 200 * time.Millisecond
	maxBodyRead    = 16 << 20 // 応答読み込み上限 16MB
)

// postRPC は 1 リクエストを送り、JSON-RPC 応答をデコードして返す。
// idempotent が真の場合、トランスポートエラー時に指数バックオフで再送する。
func (c *Client) postRPC(ctx context.Context, req *Request, idempotent bool) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &TransportError{Op: "encode", Err: err}
	}

	attempts := 1
	if idempotent {
		attempts += c.opts.Retries
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			delay := time.Duration(float64(retryBaseDelay) * math.Pow(2, float64(i-1)))
			c.logf("retry %d/%d after %s (id=%s method=%s)", i, attempts-1, delay, req.ID, req.Method)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, &TransportError{Op: "post", Err: ctx.Err()}
			}
		}

		resp, terr := c.doOnce(ctx, body)
		if terr == nil {
			return resp, nil
		}
		lastErr = terr
		if te, ok := terr.(*TransportError); ok && te.retryable() {
			continue
		}
		return nil, terr // 再送対象外
	}
	return nil, lastErr
}

// doOnce は 1 回だけ HTTP POST し、応答をデコードする。
func (c *Client) doOnce(ctx context.Context, body []byte) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &TransportError{Op: "build", Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range c.opts.Headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &TransportError{Op: "post", Err: err}
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyRead))
	if err != nil {
		return nil, &TransportError{Op: "read", StatusCode: httpResp.StatusCode, Err: err}
	}

	// 5xx などはトランスポートエラー扱い（本文に JSON-RPC error があっても再送対象）。
	if httpResp.StatusCode >= 500 {
		return nil, &TransportError{Op: "post", StatusCode: httpResp.StatusCode, Err: errStatus(httpResp.StatusCode)}
	}

	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &TransportError{Op: "decode", StatusCode: httpResp.StatusCode, Err: err}
	}
	return &resp, nil
}

type statusError int

func (s statusError) Error() string { return http.StatusText(int(s)) }
func errStatus(code int) error      { return statusError(code) }
