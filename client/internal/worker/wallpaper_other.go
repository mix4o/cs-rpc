//go:build !windows

package worker

import "errors"

// Windows 以外では壁紙設定は非対応（hWallpaper が事前に 1008 を返すため実際には呼ばれない）。
const wallpaperSupported = false

func osSetWallpaper(string) error { return errors.New("wallpaper not supported on this OS") }
func osGetWallpaper() string      { return "" }
