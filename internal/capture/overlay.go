package capture

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// 画面への印の描画。以前は BlueMap の画面へ JavaScript で描いていたものを、
// Renderer が返したカメラの変換 (three.js の要素の順の view と projection) を使って
// Go で描く。描く内容と色と太さは、以前のものと同じ。

var (
	pillarColor = color.RGBA{255, 30, 30, 140}  // rgba(255,30,30,0.55)
	dotColor    = color.RGBA{255, 60, 60, 191}  // rgba(255,60,60,0.75)
	boxUnder    = color.RGBA{0, 0, 0, 179}      // rgba(0,0,0,0.7)
	labelPanel  = color.RGBA{10, 10, 14, 209}   // rgba(10,10,14,0.82)
	labelLine1  = color.RGBA{0xff, 0x5a, 0x5a, 0xff}
	labelLine2  = color.RGBA{0xe8, 0xe8, 0xee, 0xff}
)

// projector は world の点を画面の点へ移す。
type projector struct {
	view, proj []float64
	w, h       int
}

// project は、点がカメラの前にあるときだけ ok。
// 行列は列が先の要素の順 (three.js の elements) で、clip は P・V・点。
func (p projector) project(x, y, z float64) (float64, float64, bool) {
	v, m := p.view, p.proj
	if len(v) < 16 || len(m) < 16 {
		return 0, 0, false
	}
	vx := v[0]*x + v[4]*y + v[8]*z + v[12]
	vy := v[1]*x + v[5]*y + v[9]*z + v[13]
	vz := v[2]*x + v[6]*y + v[10]*z + v[14]
	vw := v[3]*x + v[7]*y + v[11]*z + v[15]
	cx := m[0]*vx + m[4]*vy + m[8]*vz + m[12]*vw
	cy := m[1]*vx + m[5]*vy + m[9]*vz + m[13]*vw
	cw := m[3]*vx + m[7]*vy + m[11]*vz + m[15]*vw
	if !(cw > 0) {
		return 0, 0, false
	}
	return (cx/cw*0.5 + 0.5) * float64(p.w), (1 - (cy/cw*0.5 + 0.5)) * float64(p.h), true
}

// drawOverlay は印を描き、確認用の文字列を返す (書式は以前のものと同じ: "ok:<n> pillars, box=<bool|none>")。
func drawOverlay(img *image.RGBA, view, proj []float64, ann *Annot) string {
	if ann == nil {
		return "no annotation"
	}
	pr := projector{view: view, proj: proj, w: img.Bounds().Dx(), h: img.Bounds().Dy()}
	drawn := 0
	for _, pl := range ann.Pillars {
		x0, y0, ok0 := pr.project(float64(pl.X)+0.5, float64(pl.Y0), float64(pl.Z)+0.5)
		x1, y1, ok1 := pr.project(float64(pl.X)+0.5, float64(pl.Y1)+1, float64(pl.Z)+0.5)
		if !ok0 || !ok1 {
			continue
		}
		strokeLine(img, x0, y0, x1, y1, 4, pillarColor)
		fillCircle(img, x1, y1, 3, dotColor)
		drawn++
	}
	box := "none"
	if !ann.NoBox {
		box = "false"
		corners := [][2]float64{
			{float64(ann.Box[0]), float64(ann.Box[1])}, {float64(ann.Box[2]), float64(ann.Box[1])},
			{float64(ann.Box[2]), float64(ann.Box[3])}, {float64(ann.Box[0]), float64(ann.Box[3])},
		}
		pts := make([][2]float64, 4)
		ok := true
		for i, c := range corners {
			x, y, vis := pr.project(c[0], float64(ann.BoxY), c[1])
			if !vis {
				ok = false
				break
			}
			pts[i] = [2]float64{x, y}
		}
		if ok {
			col := parseHexColor(ann.BoxColor)
			dashedPath(img, pts, 7, boxUnder)
			dashedPath(img, pts, 4, col)
			box = "true"
		}
	}
	lines := []string{}
	if ann.Label != "" {
		lines = append(lines, ann.Label)
	}
	if ann.Sub != "" {
		lines = append(lines, ann.Sub)
	}
	if len(lines) > 0 {
		drawLabel(img, lines)
	}
	return fmt.Sprintf("ok:%d pillars, box=%s", drawn, box)
}

// strokeLine は、丸い端の太い線を 1 回の塗りで描く (半透明の色が重なって濃くならないように)。
func strokeLine(img *image.RGBA, x0, y0, x1, y1, width float64, col color.Color) {
	r := width / 2
	dx, dy := x1-x0, y1-y0
	l := math.Hypot(dx, dy)
	if l == 0 {
		fillCircle(img, x0, y0, r, col)
		return
	}
	nx, ny := -dy/l*r, dx/l*r
	th := math.Atan2(ny, nx) // 法線の向き
	fillPath(img, col, func(z *vector.Rasterizer) {
		z.MoveTo(float32(x0+nx), float32(y0+ny))
		z.LineTo(float32(x1+nx), float32(y1+ny))
		arc(z, x1, y1, r, th, th-math.Pi) // 終点の丸み (線の先の側を通る)
		z.LineTo(float32(x0-nx), float32(y0-ny))
		arc(z, x0, y0, r, th-math.Pi, th-2*math.Pi) // 始点の丸み
		z.ClosePath()
	})
}

// arc は、中心 (cx, cy)・半径 r の円周を、角 from から to まで多角形で足す。
func arc(z *vector.Rasterizer, cx, cy, r, from, to float64) {
	const steps = 8
	for i := 1; i <= steps; i++ {
		a := from + (to-from)*float64(i)/steps
		z.LineTo(float32(cx+r*math.Cos(a)), float32(cy+r*math.Sin(a)))
	}
}

func fillCircle(img *image.RGBA, cx, cy, r float64, col color.Color) {
	fillPath(img, col, func(z *vector.Rasterizer) {
		const steps = 16
		z.MoveTo(float32(cx+r), float32(cy))
		for i := 1; i <= steps; i++ {
			a := 2 * math.Pi * float64(i) / steps
			z.LineTo(float32(cx+r*math.Cos(a)), float32(cy+r*math.Sin(a)))
		}
		z.ClosePath()
	})
}

// fillPath は、build で作った道を 1 回だけ塗る。
func fillPath(img *image.RGBA, col color.Color, build func(z *vector.Rasterizer)) {
	b := img.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	z.DrawOp = draw.Over
	build(z)
	z.Draw(img, b, image.NewUniform(col), b.Min)
}

// dashedPath は、閉じた道を破線 [16, 10] で描く (破線は道全体で続く)。
func dashedPath(img *image.RGBA, pts [][2]float64, width float64, col color.Color) {
	const on, off = 16.0, 10.0
	remain := on     // いまの区間の残り
	drawing := true  // 描く区間かどうか
	n := len(pts)
	for i := 0; i < n; i++ {
		a := pts[i]
		b := pts[(i+1)%n]
		dx, dy := b[0]-a[0], b[1]-a[1]
		seg := math.Hypot(dx, dy)
		if seg == 0 {
			continue
		}
		ux, uy := dx/seg, dy/seg
		pos := 0.0
		for pos < seg {
			step := math.Min(remain, seg-pos)
			if drawing && step > 0 {
				strokeLine(img, a[0]+ux*pos, a[1]+uy*pos, a[0]+ux*(pos+step), a[1]+uy*(pos+step), width, col)
			}
			pos += step
			remain -= step
			if remain <= 0 {
				drawing = !drawing
				if drawing {
					remain = on
				} else {
					remain = off
				}
			}
		}
	}
}

// drawLabel は、左上に文字のパネルを描く (太字の等幅、22px)。
func drawLabel(img *image.RGBA, lines []string) {
	face, err := labelFace()
	if err != nil {
		return
	}
	defer face.Close()
	d := &font.Drawer{Face: face}
	maxw := fixed.I(0)
	for _, s := range lines {
		if w := d.MeasureString(s); w > maxw {
			maxw = w
		}
	}
	b := img.Bounds()
	panel := image.Rect(12, 12, 12+maxw.Ceil()+24, 12+16+len(lines)*30)
	draw.Draw(img, panel.Intersect(b), image.NewUniform(labelPanel), image.Point{}, draw.Over)
	d.Dst = img
	for i, s := range lines {
		col := labelLine2
		if i == 0 {
			col = labelLine1
		}
		d.Src = image.NewUniform(col)
		d.Dot = fixed.P(24, 42+i*30)
		d.DrawString(s)
	}
}

var labelFaceCache font.Face

func labelFace() (font.Face, error) {
	if labelFaceCache != nil {
		return labelFaceCache, nil
	}
	f, err := opentype.Parse(gomonobold.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 22, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	labelFaceCache = face
	return face, nil
}

// parseHexColor は "#rrggbb" を読む。読めなければ黄色 (以前の枠の既定)。
func parseHexColor(s string) color.RGBA {
	s = strings.TrimPrefix(s, "#")
	if len(s) == 6 {
		if v, err := strconv.ParseUint(s, 16, 32); err == nil {
			return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
		}
	}
	return color.RGBA{0xff, 0xd4, 0x00, 0xff}
}
