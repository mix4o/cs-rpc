package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mockRPC は固定の JSON-RPC 応答を返すテストサーバを立てる。
func mockRPC(t *testing.T, handler func(req Request) (any, *RPCError)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		result, rpcErr := handler(req)
		resp := Response{JSONRPC: Version, ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			b, _ := json.Marshal(result)
			resp.Result = b
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	opts := DefaultOptions()
	opts.Endpoint = srv.URL + "/rpc"
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallSuccess(t *testing.T) {
	c := mockRPC(t, func(req Request) (any, *RPCError) {
		return map[string]string{"echo": req.Method}, nil
	})
	var out map[string]string
	if err := c.Call(context.Background(), "echo", map[string]string{"m": "hi"}, &out); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if out["echo"] != "echo" {
		t.Fatalf("got %v", out)
	}
}

func TestCallRPCError(t *testing.T) {
	c := mockRPC(t, func(req Request) (any, *RPCError) {
		return nil, &RPCError{Code: -32601, Message: "Method not found"}
	})
	err := c.Call(context.Background(), "nope", nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("expected RPCError -32601, got %v", err)
	}
}

func TestCallTransportErrorOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	opts := DefaultOptions()
	opts.Endpoint = srv.URL + "/rpc"
	opts.Retries = 0
	c, _ := New(opts)
	err := c.Call(context.Background(), "x", nil, nil)
	var te *TransportError
	if !errors.As(err, &te) || te.StatusCode != 500 {
		t.Fatalf("expected TransportError 500, got %v", err)
	}
}

func TestRetryOnTransientThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{JSONRPC: Version, ID: "x", Result: json.RawMessage(`{"ok":true}`)})
	}))
	defer srv.Close()
	opts := DefaultOptions()
	opts.Endpoint = srv.URL + "/rpc"
	opts.Retries = 3
	c, _ := New(opts)
	var out map[string]bool
	if err := c.Call(context.Background(), "x", nil, &out, Idempotent()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !out["ok"] || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls=%d out=%v", calls, out)
	}
}

func TestNoRetryWhenNotIdempotent(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	opts := DefaultOptions()
	opts.Endpoint = srv.URL + "/rpc"
	opts.Retries = 3
	c, _ := New(opts)
	_ = c.Call(context.Background(), "x", nil, nil) // idempotent 指定なし
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	opts := DefaultOptions()
	opts.Endpoint = srv.URL + "/rpc"
	opts.Timeout = 50 * time.Millisecond
	c, _ := New(opts)
	err := c.Call(context.Background(), "x", nil, nil)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("expected TransportError, got %v", err)
	}
}
