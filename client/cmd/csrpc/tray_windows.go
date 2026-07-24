//go:build windows

// Windows タスクトレイ（通知領域）実装。CGO 不使用・追加依存なしで、
// user32/shell32/kernel32 を syscall 直呼びして実現する（壁紙 wallpaper_windows.go と同じ流儀）。
//
// worker を --tray（Windows 既定 ON）で起動すると、ウィンドウやブラウザを出さずに
// トレイアイコンとして常駐する。アイコンを右クリック（または左ダブルクリック）すると
// メニューが出て、GUI を開く / 終了 ができる。メッセージループはメイン goroutine を
// 占有するため、presentGUI の代わりにこの関数がフォアグラウンドのライフサイクルを持つ。
package main

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx  = modUser32.NewProc("RegisterClassExW")
	procCreateWindowEx   = modUser32.NewProc("CreateWindowExW")
	procDefWindowProc    = modUser32.NewProc("DefWindowProcW")
	procDestroyWindow    = modUser32.NewProc("DestroyWindow")
	procGetMessage       = modUser32.NewProc("GetMessageW")
	procTranslateMessage = modUser32.NewProc("TranslateMessage")
	procDispatchMessage  = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage  = modUser32.NewProc("PostQuitMessage")
	procPostMessage      = modUser32.NewProc("PostMessageW")
	procLoadIcon         = modUser32.NewProc("LoadIconW")
	procCreatePopupMenu  = modUser32.NewProc("CreatePopupMenu")
	procDestroyMenu      = modUser32.NewProc("DestroyMenu")
	procAppendMenu       = modUser32.NewProc("AppendMenuW")
	procTrackPopupMenu   = modUser32.NewProc("TrackPopupMenu")
	procGetCursorPos     = modUser32.NewProc("GetCursorPos")
	procSetForegroundWin = modUser32.NewProc("SetForegroundWindow")

	procShellNotifyIcon  = modShell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle  = modKernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmCommand      = 0x0111
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1
	wmLButtonDblClk = 0x0203
	wmRButtonUp    = 0x0205

	idiApplication = 32512 // 既定のアプリアイコン（.ico リソース不要）

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	menuOpen = 1
	menuQuit = 2
)

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type point struct{ X, Y int32 }

type msgStruct struct {
	Hwnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type notifyIconData struct {
	CbSize            uint32
	HWnd              syscall.Handle
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             syscall.Handle
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UVersionOrTimeout uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
	GuidItem          [16]byte
	HBalloonIcon      syscall.Handle
}

// トレイは単一インスタンスなので、WndProc コールバックから触る状態は
// パッケージ変数に置く（ウィンドウ生成前に確定させる）。
var (
	trayHWnd   syscall.Handle
	trayIcon   syscall.Handle
	trayURL    string
	trayCancel context.CancelFunc
	trayData   notifyIconData
)

// runTray はトレイアイコンを表示し、ユーザーが「終了」を選ぶ（または ctx が
// キャンセルされる）までブロックする。true を返す＝トレイを表示した。
func runTray(ctx context.Context, cancel context.CancelFunc, cfg guiConfig) bool {
	// Win32 のウィンドウ/メッセージループは生成スレッドに紐づくため固定する。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	trayURL = cfg.URL
	trayCancel = cancel

	hInstance, _, _ := procGetModuleHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString("csrpcTrayWindow")

	icon, _, _ := procLoadIcon.Call(0, uintptr(idiApplication))
	trayIcon = syscall.Handle(icon)

	wc := wndClassEx{
		Style:         0,
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     syscall.Handle(hInstance),
		HIcon:         trayIcon,
		LpszClassName: className,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if ret, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return false // クラス登録に失敗したらトレイ非対応として呼び出し側にフォールバックさせる
	}

	winName, _ := syscall.UTF16PtrFromString("cs-rpc worker")
	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(winName)),
		0, 0, 0, 0, 0, // 表示しない隠しウィンドウ（トレイのメッセージ受信専用）
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return false
	}
	trayHWnd = syscall.Handle(hwnd)

	// トレイにアイコンを追加（ツールチップ付き）。
	trayData = notifyIconData{
		HWnd:             trayHWnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayCallback,
		HIcon:            trayIcon,
	}
	trayData.CbSize = uint32(unsafe.Sizeof(trayData))
	copyUTF16(trayData.SzTip[:], cfg.Title)
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayData)))

	// 起動をバルーン通知で知らせる（「トレイに常駐した」ことを可視化）。
	showBalloon("cs-rpc worker", "タスクトレイで実行中です。アイコンから GUI を開けます。")

	// Ctrl-C 等で ctx がキャンセルされたらウィンドウを閉じてループを抜ける。
	go func() {
		<-ctx.Done()
		procPostMessage.Call(uintptr(trayHWnd), wmClose, 0, 0)
	}()

	// メッセージループ（終了まで）。
	var m msgStruct
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0=WM_QUIT / -1=エラー
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	return true
}

// trayWndProc はトレイウィンドウのメッセージ処理。
// syscall.NewCallback の制約により全引数を uintptr で受ける（64bit で uint32 引数は不可）。
func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmTrayCallback:
		// lparam の下位ワードがマウスイベント種別。
		switch uint32(lparam) & 0xffff {
		case wmLButtonDblClk:
			openTrayURL()
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch uint32(wparam) & 0xffff {
		case menuOpen:
			openTrayURL()
		case menuQuit:
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayData)))
		if trayCancel != nil {
			trayCancel() // ワーカ停止
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, msg, wparam, lparam)
	return ret
}

// showTrayMenu は右クリックのポップアップメニューを出す。
func showTrayMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	open, _ := syscall.UTF16PtrFromString("GUI を開く")
	quit, _ := syscall.UTF16PtrFromString("終了")
	procAppendMenu.Call(hMenu, mfString, menuOpen, uintptr(unsafe.Pointer(open)))
	procAppendMenu.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenu.Call(hMenu, mfString, menuQuit, uintptr(unsafe.Pointer(quit)))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// メニューを正しく閉じるには前面化が必要（Win32 の定石）。
	procSetForegroundWin.Call(hwnd)
	// TPM_RETURNCMD で選択結果を戻り値で受け取り、WM_COMMAND として自分へ投げ直す。
	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu, tpmReturnCmd|tpmRightButton,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	if cmd != 0 {
		procPostMessage.Call(hwnd, wmCommand, cmd, 0)
	}
}

// showBalloon はトレイのバルーン通知を表示する。
func showBalloon(title, text string) {
	trayData.UFlags = nifInfo
	copyUTF16(trayData.SzInfoTitle[:], title)
	copyUTF16(trayData.SzInfo[:], text)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&trayData)))
	// 常駐フラグに戻す（次回 Modify で INFO を再送しないように）。
	trayData.UFlags = nifMessage | nifIcon | nifTip
}

func openTrayURL() {
	if trayURL == "" {
		return
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", trayURL).Start()
}

// copyUTF16 は Go 文字列を固定長 UTF-16 バッファへ（NUL 終端込みで）書き込む。
func copyUTF16(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	n := len(u)
	if n > len(dst) {
		n = len(dst)
		u[n-1] = 0 // 末尾 NUL を保証
	}
	copy(dst[:n], u[:n])
}
