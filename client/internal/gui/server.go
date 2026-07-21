package gui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Server は GUI（HTML + SSE）を提供するローカル HTTP サーバ。
type Server struct {
	hub    *Hub
	addr   string
	worker string // ワーカ名（ヘッダ表示用）
	server string // 接続先サーバ（ヘッダ表示用）
	srv    *http.Server
}

func NewServer(hub *Hub, addr, worker, server string) *Server {
	s := &Server{hub: hub, addr: addr, worker: worker, server: server}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/events", s.events)
	mux.HandleFunc("/meta", s.meta)
	s.srv = &http.Server{Addr: addr, Handler: mux}
	return s
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"worker": s.worker, "server": s.server})
}

// Start はリスナを開いて別 goroutine で配信を始める。実際に bind した
// アドレスを返す（addr のポートが 0 の場合の解決に使える）。
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return "", err
	}
	go func() { _ = s.srv.Serve(ln) }()
	return ln.Addr().String(), nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// events は Server-Sent Events で snapshot → 以降のイベントを配信する。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, snapshot := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	writeSSE(w, snapshot)
	flusher.Flush()

	// keep-alive 用の定期コメント（プロキシのアイドル切断対策）。
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case msg := <-ch:
			writeSSE(w, msg)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, data []byte) {
	fmt.Fprint(w, "data: ")
	_, _ = w.Write(data)
	fmt.Fprint(w, "\n\n")
}
