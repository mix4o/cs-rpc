package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"time"

	"csrpc/internal/gui"
	"csrpc/internal/worker"
)

// guiConfig は presentGUI（ビルドタグで実装が切り替わる）へ渡す表示設定。
type guiConfig struct {
	Title string // ウィンドウ/タブのタイトル
	URL   string // ローカル GUI の URL
	Open  bool   // ブラウザ版のみ: 既定ブラウザを開くか
}

// cmdWorker はサーバ制御キューからコマンドを受け取り、クライアント側で実行する
// デモ用ワーカ。GUI はネイティブウィンドウ（-tags webview）またはブラウザで表示する。
func cmdWorker(args []string) int {
	fs := newFlagSet("worker")
	common := registerCommon(fs)
	var guiAddr, name string
	var poll time.Duration
	var takeOver, open, tray bool
	defaultName, _ := os.Hostname()
	if defaultName == "" {
		defaultName = "worker"
	}
	// タスクトレイ常駐は Windows のみ対応。既定は Windows で ON（起動時にトレイへ格納）、
	// 他 OS では OFF（runTray が非対応で false を返し GUI 表示にフォールバックする）。
	defaultTray := envBoolOr("CSRPC_TRAY", runtime.GOOS == "windows")
	fs.StringVar(&guiAddr, "gui", "127.0.0.1:8787", "GUI listen address (empty to disable GUI)")
	fs.StringVar(&name, "name", defaultName, "worker name reported to the server")
	fs.DurationVar(&poll, "poll", 500*time.Millisecond, "lease poll interval")
	fs.BoolVar(&takeOver, "take-over", true, "disable server autorun so this worker executes commands")
	fs.BoolVar(&open, "open", true, "(browser build) open the GUI in the default browser")
	fs.BoolVar(&tray, "tray", defaultTray, "(Windows) run in the system tray on startup instead of opening a window")
	if err := fs.Parse(args); err != nil {
		return exitTransport
	}

	ctrl, err := worker.NewControl(common.endpoint, &http.Client{Timeout: common.timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTransport
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	hub := gui.NewHub(os.Stderr) // ログは端末にもミラー

	var srv *gui.Server
	var url string
	if guiAddr != "" {
		srv = gui.NewServer(hub, guiAddr, name, common.endpoint)
		bound, err := srv.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start GUI on %s: %v\n", guiAddr, err)
			return exitTransport
		}
		url = "http://" + bound
		fmt.Fprintf(os.Stderr, "GUI: %s\n", url)
		hub.Logf("info", "GUI serving at %s", url)
	}

	if takeOver {
		if err := ctrl.SetAutorun(ctx, false); err != nil {
			hub.Logf("warn", "could not disable server autorun: %v", err)
		} else {
			hub.Logf("info", "took over execution (server autorun disabled)")
		}
	}

	w := &worker.Worker{Name: name, Ctrl: ctrl, Out: hub, Poll: poll}

	if srv == nil {
		// GUI 無効: ワーカをフォアグラウンドで回す（Ctrl-C で終了）。
		w.Run(ctx)
		return exitOK
	}

	// GUI 有効: ワーカは別 goroutine、GUI/トレイをメイン goroutine で保持。
	runCtx, cancel := context.WithCancel(ctx)
	go w.Run(runCtx)

	cfg := guiConfig{Title: fmt.Sprintf("cs-rpc CLIENT — worker: %s", name), URL: url, Open: open}
	// --tray（Windows 既定 ON）: ウィンドウ/ブラウザを出さずトレイへ常駐する。
	// トレイ非対応の OS では runTray が false を返すので通常の GUI 表示に戻す。
	if tray && runTray(runCtx, cancel, cfg) {
		// runTray がトレイ常駐でフォアグラウンドを保持した（終了までブロック済み）。
	} else {
		if tray {
			hub.Logf("warn", "system tray not supported on this OS; showing GUI instead")
		}
		presentGUI(runCtx, cancel, cfg) // ウィンドウが閉じる / Ctrl-C までブロック
	}
	cancel()

	shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	_ = srv.Shutdown(shutCtx)
	return exitOK
}
