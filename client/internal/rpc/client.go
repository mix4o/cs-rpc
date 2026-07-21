// Package rpc は cs-rpc の JSON-RPC 2.0 over HTTP クライアントライブラリ。
// CLI からも他プログラムからも利用できる（設計書 01 の1章）。
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client は cs-rpc サーバへの RPC クライアント。生成後は複数 goroutine から
// 同時利用してよい。
type Client struct {
	endpoint   string
	healthzURL string
	methodsURL string
	http       *http.Client
	opts       Options
}

// New は Options からクライアントを生成する。
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultOptions().Endpoint
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultOptions().Timeout
	}
	if opts.Headers == nil {
		opts.Headers = map[string]string{}
	}
	if opts.Logger == nil {
		opts.Logger = DefaultOptions().Logger
	}

	u, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	base := *u

	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: opts.Timeout}
	}

	return &Client{
		endpoint:   opts.Endpoint,
		healthzURL: withPath(base, "/healthz"),
		methodsURL: withPath(base, "/rpc/methods"),
		http:       hc,
		opts:       opts,
	}, nil
}

// Call は同期 RPC を実行する。result に非 nil を渡すとサーバの result を
// アンマーシャルする。サーバが error を返した場合は *RPCError を、
// 通信失敗時は *TransportError を返す。
func (c *Client) Call(ctx context.Context, method string, params any, result any, opts ...CallOption) error {
	cfg := callConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	rawParams, err := marshalParams(params)
	if err != nil {
		return err
	}
	req := &Request{JSONRPC: Version, ID: newID(), Method: method, Params: rawParams}

	resp, err := c.postRPC(ctx, req, cfg.idempotent)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return &TransportError{Op: "decode", Err: err}
		}
	}
	return nil
}

// Notify は応答不要の通知（id なし）を送る。サーバは 204 を返す。
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	rawParams, err := marshalParams(params)
	if err != nil {
		return err
	}
	req := &Request{JSONRPC: Version, Method: method, Params: rawParams}
	// 通知は再送しない（重複実行を避ける）。
	_, err = c.postRPC(ctx, req, false)
	return err
}

// Ping は /healthz を叩いて疎通確認する。
func (c *Client) Ping(ctx context.Context) error {
	return c.getExpectOK(ctx, c.healthzURL)
}

// Methods は /rpc/methods の生 JSON を返す。
func (c *Client) Methods(ctx context.Context) (json.RawMessage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.methodsURL, nil)
	if err != nil {
		return nil, &TransportError{Op: "build", Err: err}
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &TransportError{Op: "get", Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return nil, &TransportError{Op: "read", StatusCode: resp.StatusCode, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &TransportError{Op: "get", StatusCode: resp.StatusCode, Err: errStatus(resp.StatusCode)}
	}
	return raw, nil
}

func (c *Client) getExpectOK(ctx context.Context, u string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return &TransportError{Op: "build", Err: err}
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return &TransportError{Op: "get", Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
	if resp.StatusCode != http.StatusOK {
		return &TransportError{Op: "get", StatusCode: resp.StatusCode, Err: errStatus(resp.StatusCode)}
	}
	return nil
}

func (c *Client) logf(format string, args ...any) {
	if c.opts.Logger != nil {
		c.opts.Logger.Printf(format, args...)
	}
}

// marshalParams は params を json.RawMessage へ変換する。nil はそのまま省略。
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, &TransportError{Op: "encode", Err: err}
	}
	return b, nil
}

func withPath(u url.URL, p string) string {
	u.Path = p
	u.RawQuery = ""
	return u.String()
}
