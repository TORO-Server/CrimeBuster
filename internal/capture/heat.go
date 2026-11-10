package capture

import (
	"crime-buster/internal/detect"
	"fmt"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
)

// RenderHeatmap は壊した場所を真上から見た図にする。BlueMap が使えないときも、必ず作れる証拠の絵。
// 文字は basicfont (ASCII だけ) なので、数字と英語だけを書く。説明は報告の本文に書く。
func RenderHeatmap(s *detect.Stats, out string) error {
	const W, H = 1100, 760
	const margin, top = 60, 60
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{18, 20, 26, 255}), image.Point{}, draw.Src)

	spanX := s.X1 - s.X0 + 1
	spanZ := s.Z1 - s.Z0 + 1
	if spanX < 1 {
		spanX = 1
	}
	if spanZ < 1 {
		spanZ = 1
	}
	// 余白を少し足して、1 ブロックを何ピクセルにするか
	padX, padZ := maxInt(spanX/12, 8), maxInt(spanZ/12, 8)
	vx0, vz0 := s.X0-padX, s.Z0-padZ
	vw, vh := spanX+2*padX, spanZ+2*padZ
	scale := math.Min(float64(W-2*margin)/float64(vw), float64(H-margin-top)/float64(vh))
	if scale > 12 {
		scale = 12
	}
	if scale <= 0 {
		scale = 1
	}
	ox, oy := margin, top

	px := func(x int) int { return ox + int(float64(x-vx0)*scale) }
	pz := func(z int) int { return oy + int(float64(z-vz0)*scale) }

	// 格子 (10 / 50 / 100 / 500 ブロックごとのうち、粗すぎず細かすぎないもの)
	step := niceStep(float64(maxInt(vw, vh)) / 8)
	gridCol := color.RGBA{44, 48, 60, 255}
	for x := roundUp(vx0, step); x <= vx0+vw; x += step {
		line(img, px(x), oy, px(x), oy+int(float64(vh)*scale), gridCol)
		label(img, px(x)+2, oy+int(float64(vh)*scale)+14, fmt.Sprint(x), color.RGBA{150, 156, 170, 255})
	}
	for z := roundUp(vz0, step); z <= vz0+vh; z += step {
		line(img, ox, pz(z), ox+int(float64(vw)*scale), pz(z), gridCol)
		label(img, 4, pz(z)+4, fmt.Sprint(z), color.RGBA{150, 156, 170, 255})
	}

	// 壊した場所。数が多い列ほど赤く
	maxN := 1
	for _, c := range s.Columns {
		if c.N > maxN {
			maxN = c.N
		}
	}
	cell := int(math.Ceil(scale))
	if cell < 1 {
		cell = 1
	}
	for _, c := range s.Columns {
		t := math.Log(float64(c.N)+1) / math.Log(float64(maxN)+1)
		col := heatColor(t)
		x, z := px(c.X), pz(c.Z)
		draw.Draw(img, image.Rect(x, z, x+cell, z+cell), image.NewUniform(col), image.Point{}, draw.Src)
	}

	// 範囲の枠
	frame := color.RGBA{255, 255, 255, 200}
	rect(img, px(s.X0), pz(s.Z0), px(s.X1+1), pz(s.Z1+1), frame)

	label(img, margin, 24, fmt.Sprintf("%s  world=%s  top view (north = up)", s.User.Name, s.World), color.RGBA{235, 238, 245, 255})
	label(img, margin, 44, fmt.Sprintf("broke %d / placed %d blocks   x %d..%d  z %d..%d   peak %d/s", s.Breaks, s.Places, s.X0, s.X1, s.Z0, s.Z1, s.PeakPerS), color.RGBA{190, 196, 210, 255})

	// 凡例
	lx, ly := W-margin-180, H-24
	for i := 0; i < 160; i++ {
		col := heatColor(float64(i) / 159)
		line(img, lx+i, ly-8, lx+i, ly, col)
	}
	label(img, lx-14, ly, "1", color.RGBA{190, 196, 210, 255})
	label(img, lx+164, ly, fmt.Sprint(maxN), color.RGBA{190, 196, 210, 255})

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func heatColor(t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	// 暗い黄 → 橙 → 赤
	r := uint8(200 + 55*t)
	g := uint8(190 * (1 - t))
	b := uint8(40 * (1 - t))
	return color.RGBA{r, g, b, 255}
}

func niceStep(raw float64) int {
	if raw < 10 {
		return 10
	}
	for _, s := range []int{10, 20, 50, 100, 200, 500, 1000, 2000, 5000} {
		if float64(s) >= raw {
			return s
		}
	}
	return 10000
}

func roundUp(v, step int) int {
	r := ((v % step) + step) % step
	if r == 0 {
		return v
	}
	return v + step - r
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func line(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	if x0 == x1 {
		for y := minInt(y0, y1); y <= maxInt(y0, y1); y++ {
			setPx(img, x0, y, c)
		}
		return
	}
	for x := minInt(x0, x1); x <= maxInt(x0, x1); x++ {
		setPx(img, x, y0, c)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func rect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	line(img, x0, y0, x1, y0, c)
	line(img, x0, y1, x1, y1, c)
	line(img, x0, y0, x0, y1, c)
	line(img, x1, y0, x1, y1, c)
}

func setPx(img *image.RGBA, x, y int, c color.RGBA) {
	if image.Pt(x, y).In(img.Bounds()) {
		img.SetRGBA(x, y, c)
	}
}

func label(img *image.RGBA, x, y int, s string, c color.RGBA) {
	d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(s)
}
