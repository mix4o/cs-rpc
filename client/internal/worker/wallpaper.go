package worker

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// wallpaper のドメインエラーコード
const (
	errWallpaperUnsupported = 1008 // 当該OSでは非対応
	errWallpaperFailed      = 1009 // 生成/設定/復元の失敗
)

// hWallpaper はデスクトップ壁紙を変更する（デモ用の「無害だが目立つ」効果）。
//
// params: {text?, color?, path?, width?, height?, restore?}
//   - text  : 指定するとクライアント側で大きな文字入り画像を生成して壁紙にする（外部ファイル不要）
//   - color : 背景色（"#rrggbb" または "r g b"）。既定は暗赤
//   - path  : 既存画像ファイルを壁紙にする
//   - restore: true で変更前の壁紙に戻す
//
// 実行側(OS)が非対応なら 1008。壁紙変更は無害・可逆なので既定で有効（gate なし）。
func hWallpaper(_ context.Context, params json.RawMessage, _ Emit) (any, *HandlerError) {
	p, herr := decode[struct {
		Text    string `json:"text"`
		Color   string `json:"color"`
		Path    string `json:"path"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Restore bool   `json:"restore"`
	}](params)
	if herr != nil {
		return nil, herr
	}
	if !wallpaperSupported {
		return nil, &HandlerError{Code: errWallpaperUnsupported,
			Message: "wallpaper not supported on " + runtime.GOOS}
	}

	if p.Restore {
		prev := readPrevWallpaper()
		if prev == "" {
			return nil, &HandlerError{Code: errWallpaperFailed, Message: "no saved wallpaper to restore"}
		}
		if err := osSetWallpaper(prev); err != nil {
			return nil, &HandlerError{Code: errWallpaperFailed, Message: "restore: " + err.Error()}
		}
		return map[string]any{"restored": prev}, nil
	}

	// 最初の変更時に元の壁紙を保存しておく（restore 用）。
	savePrevWallpaperIfAbsent(osGetWallpaper())

	path := p.Path
	if path == "" {
		generated, err := generateWallpaperImage(p.Text, p.Color, p.Width, p.Height)
		if err != nil {
			return nil, &HandlerError{Code: errWallpaperFailed, Message: "generate: " + err.Error()}
		}
		path = generated
	}
	if err := osSetWallpaper(path); err != nil {
		return nil, &HandlerError{Code: errWallpaperFailed, Message: "set: " + err.Error()}
	}
	return map[string]any{"set": path, "previous": readPrevWallpaper()}, nil
}

func prevWallpaperFile() string { return filepath.Join(os.TempDir(), "csrpc-wall-prev.txt") }

func savePrevWallpaperIfAbsent(cur string) {
	if cur == "" {
		return
	}
	if _, err := os.Stat(prevWallpaperFile()); err == nil {
		return // 既に元の壁紙を保存済み
	}
	_ = os.WriteFile(prevWallpaperFile(), []byte(cur), 0o644)
}

func readPrevWallpaper() string {
	b, err := os.ReadFile(prevWallpaperFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// generateWallpaperImage は背景色＋（任意で）大きな文字の BMP を生成し、そのパスを返す。
func generateWallpaperImage(text, colorStr string, w, h int) (string, error) {
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}
	bg := parseColor(colorStr, color.RGBA{139, 0, 0, 255}) // 既定: 暗赤
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	if strings.TrimSpace(text) != "" {
		drawBigText(img, text, color.RGBA{255, 255, 255, 255})
	}
	f, err := os.CreateTemp("", "csrpc-wall-*.bmp")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := bmp.Encode(f, img); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// drawBigText は basicfont で描いた文字を最近傍拡大して中央に大きく載せる。
func drawBigText(dst *image.RGBA, text string, col color.RGBA) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	const adv, lh, gap = 7, 13, 4 // basicfont.Face7x13 の1文字幅/行高/行間
	maxLen := 0
	for _, ln := range lines {
		if len(ln) > maxLen {
			maxLen = len(ln)
		}
	}
	if maxLen == 0 {
		return
	}
	sw, sh := maxLen*adv, len(lines)*(lh+gap)
	small := image.NewRGBA(image.Rect(0, 0, sw, sh))
	d := &font.Drawer{Dst: small, Src: image.NewUniform(col), Face: basicfont.Face7x13}
	for i, ln := range lines {
		d.Dot = fixed.P(0, i*(lh+gap)+lh)
		d.DrawString(ln)
	}
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	scale := math.Min(float64(dw)*0.85/float64(sw), float64(dh)*0.55/float64(sh))
	if scale < 1 {
		scale = 1
	}
	tw, th := int(float64(sw)*scale), int(float64(sh)*scale)
	tx, ty := (dw-tw)/2, (dh-th)/2
	xdraw.NearestNeighbor.Scale(dst, image.Rect(tx, ty, tx+tw, ty+th), small, small.Bounds(), xdraw.Over, nil)
}

func parseColor(s string, def color.RGBA) color.RGBA {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if strings.HasPrefix(s, "#") && len(s) == 7 {
		r, e1 := strconv.ParseUint(s[1:3], 16, 8)
		g, e2 := strconv.ParseUint(s[3:5], 16, 8)
		b, e3 := strconv.ParseUint(s[5:7], 16, 8)
		if e1 == nil && e2 == nil && e3 == nil {
			return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
		}
	}
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
	if len(f) == 3 {
		r, _ := strconv.Atoi(f[0])
		g, _ := strconv.Atoi(f[1])
		b, _ := strconv.Atoi(f[2])
		return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
	}
	return def
}
