//go:build webview

// ネイティブウィンドウ実装。`go build -tags webview`（CGO 必須）で有効。
// OS のシステム webview（Windows=WebView2 / Linux=WebKitGTK / macOS=WKWebView）に
// ローカル GUI を表示する。独立したデスクトップウィンドウとして起動するため、
// ブラウザで開くサーバのコントロールページと一目で区別できる。
package main

import (
	"context"

	"github.com/webview/webview_go"
)

func presentGUI(ctx context.Context, cancel context.CancelFunc, cfg guiConfig) {
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle(cfg.Title)
	w.SetSize(1100, 680, webview.HintNone)

	// Ctrl-C（ctx キャンセル）でウィンドウを閉じる。
	go func() {
		<-ctx.Done()
		w.Terminate()
	}()

	w.Navigate(cfg.URL)
	w.Run()  // ウィンドウが閉じられるまでブロック（メイン goroutine 必須）
	cancel() // ウィンドウが閉じられたらワーカを停止
}
