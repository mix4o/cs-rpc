//go:build windows

package worker

import (
	"syscall"
	"unsafe"
)

const wallpaperSupported = true

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSystemParameters = user32.NewProc("SystemParametersInfoW")
)

const (
	spiSetDeskWallpaper = 0x0014
	spiGetDeskWallpaper = 0x0073
	spifUpdateAndSend   = 0x0001 | 0x0002 // SPIF_UPDATEINIFILE | SPIF_SENDWININICHANGE
)

// osSetWallpaper は user32!SystemParametersInfoW で壁紙を設定する。
func osSetWallpaper(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, callErr := procSystemParameters.Call(
		spiSetDeskWallpaper, 0, uintptr(unsafe.Pointer(p)), spifUpdateAndSend)
	if r == 0 {
		return callErr
	}
	return nil
}

// osGetWallpaper は現在の壁紙パスを取得する。
func osGetWallpaper() string {
	buf := make([]uint16, 520) // 余裕を持って MAX_PATH 超
	procSystemParameters.Call(
		spiGetDeskWallpaper, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])), 0)
	return syscall.UTF16ToString(buf)
}
