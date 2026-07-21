//go:build !webview

// フォールバック実装（ネイティブ webview を使わない既定ビルド）。
// GUI は既定ブラウザで開き、Ctrl-C まで待機する。CGO 不要で全 OS 共通ビルド可能。
package main

import (
	"context"
	"os/exec"
	"runtime"
)

func presentGUI(ctx context.Context, cancel context.CancelFunc, cfg guiConfig) {
	if cfg.Open {
		openBrowser(cfg.URL)
	}
	<-ctx.Done() // SIGINT でワーカ側 ctx がキャンセルされるまで待つ
}

// openBrowser は既定ブラウザで URL を開く（失敗は無視）。
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
