// Package gui は worker のデモ用ローカル Web GUI。
//
// Hub はコマンド受領イベントとデバッグログを保持し（リングバッファ）、
// SSE で接続中のブラウザへ配信する。左ペイン=コマンドと結果、
// 右ペイン=クライアント処理ログ、に対応するデータ源。
package gui

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// CommandEvent は「サーバから受領したコマンド」1件（左ペイン）。
type CommandEvent struct {
	Seq        int             `json:"seq"`
	ID         string          `json:"id"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params,omitempty"`
	State      string          `json:"state"` // running | done | error | canceled
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Progress   map[string]any  `json:"progress,omitempty"` // 実行中の途中経過
	ReceivedAt string          `json:"receivedAt"`
	DoneAt     string          `json:"doneAt,omitempty"`
	ElapsedMs  int64           `json:"elapsedMs,omitempty"`
}

// LogEvent はクライアント処理ログ1行（右ペイン）。
type LogEvent struct {
	TS    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type envelope struct {
	Type string `json:"type"` // snapshot | command | log
	Data any    `json:"data"`
}

type snapshotData struct {
	Commands []*CommandEvent `json:"commands"`
	Logs     []LogEvent      `json:"logs"`
}

// Hub はイベントの保持と SSE 配信を担う。全メソッドは並行安全。
type Hub struct {
	mu       sync.Mutex
	cmds     []*CommandEvent
	cmdIndex map[string]*CommandEvent
	startAt  map[string]time.Time
	logs     []LogEvent
	subs     map[chan []byte]struct{}
	maxCmds  int
	maxLogs  int
	mirror   io.Writer // ログを端末にも出す場合の出力先（nil 可）
}

func NewHub(mirror io.Writer) *Hub {
	return &Hub{
		cmdIndex: map[string]*CommandEvent{},
		startAt:  map[string]time.Time{},
		subs:     map[chan []byte]struct{}{},
		maxCmds:  500,
		maxLogs:  2000,
		mirror:   mirror,
	}
}

func nowStr() string { return time.Now().Format("15:04:05.000") }

// Subscribe は新規 SSE 購読チャネルと、その時点のスナップショット（初期表示用）を
// ロック下で同時に返す。これにより購読開始直後のイベント取りこぼしを防ぐ。
func (h *Hub) Subscribe() (chan []byte, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan []byte, 128)
	h.subs[ch] = struct{}{}
	snap, _ := json.Marshal(envelope{Type: "snapshot", Data: snapshotData{Commands: h.cmds, Logs: h.logs}})
	return ch, snap
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// CommandReceived はコマンド受領（running）を記録・配信する。
func (h *Hub) CommandReceived(seq int, id, method string, params json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := &CommandEvent{
		Seq: seq, ID: id, Method: method, Params: params,
		State: "running", ReceivedAt: nowStr(),
	}
	h.cmds = append(h.cmds, ev)
	h.cmdIndex[id] = ev
	h.startAt[id] = time.Now()
	h.trimCmds()
	h.broadcast(envelope{Type: "command", Data: ev})
}

// CommandProgress は実行中ジョブの途中経過を更新・配信する。
func (h *Hub) CommandProgress(id string, progress map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := h.cmdIndex[id]
	if ev == nil {
		return
	}
	ev.Progress = progress
	h.broadcast(envelope{Type: "command", Data: ev})
}

// CommandFinished は完了（done/error/canceled）を記録・配信する。
func (h *Hub) CommandFinished(id, state string, result json.RawMessage, errMsg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := h.cmdIndex[id]
	if ev == nil {
		return
	}
	ev.State = state
	ev.Result = result
	ev.Error = errMsg
	ev.DoneAt = nowStr()
	if t, ok := h.startAt[id]; ok {
		ev.ElapsedMs = time.Since(t).Milliseconds()
		delete(h.startAt, id)
	}
	h.broadcast(envelope{Type: "command", Data: ev})
}

// Logf はクライアント処理ログを記録・配信する（右ペイン）。
func (h *Hub) Logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	h.mu.Lock()
	le := LogEvent{TS: nowStr(), Level: level, Msg: msg}
	h.logs = append(h.logs, le)
	h.trimLogs()
	h.broadcast(envelope{Type: "log", Data: le})
	h.mu.Unlock()
	if h.mirror != nil {
		fmt.Fprintf(h.mirror, "%s [%s] %s\n", le.TS, level, msg)
	}
}

// broadcast はロック保持前提。バッファ満杯の購読者へは非ブロッキングで送る
// （遅い/切断間際のクライアントで全体が詰まらないようにする）。
func (h *Hub) broadcast(e envelope) {
	msg, err := json.Marshal(e)
	if err != nil {
		return
	}
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *Hub) trimCmds() {
	if len(h.cmds) <= h.maxCmds {
		return
	}
	drop := len(h.cmds) - h.maxCmds
	for _, ev := range h.cmds[:drop] {
		delete(h.cmdIndex, ev.ID)
		delete(h.startAt, ev.ID)
	}
	h.cmds = append([]*CommandEvent(nil), h.cmds[drop:]...)
}

func (h *Hub) trimLogs() {
	if len(h.logs) <= h.maxLogs {
		return
	}
	drop := len(h.logs) - h.maxLogs
	h.logs = append([]LogEvent(nil), h.logs[drop:]...)
}
