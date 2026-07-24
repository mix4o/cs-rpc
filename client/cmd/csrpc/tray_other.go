//go:build !windows

// Windows 以外ではタスクトレイ（通知領域）は非対応。runTray は false を返し、
// 呼び出し側（cmdWorker）は通常の GUI 表示（presentGUI）にフォールバックする。
package main

import "context"

func runTray(ctx context.Context, cancel context.CancelFunc, cfg guiConfig) bool {
	return false
}
