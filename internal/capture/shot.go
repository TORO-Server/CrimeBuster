package capture

import (
	"bytes"
	"context"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"io"
	"image/png"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Cam は Renderer のカメラ。BlueMap のカメラと同じ意味 (中心・距離・向き・見下ろし角) で、
// Renderer は同じ規則で実効の注視点を決める (docs は TORO-Renderer の render-api)。
type Cam struct {
	X, Y, Z float64
	Dist    float64
	Rot     float64 // 向き (ラジアン)
	Angle   float64 // 見下ろす角 (0 が真上、π/2 が水平)
}

// CamFor は荒らされた場所の中心が画面に収まるカメラ。
// 壊した場所 (x, z ごとの数) の重み付きの中央値を中心にし、遠くに離れた少数の点は無視する (範囲の外れ値で遠くなりすぎないように)。
// 列が無いときは、範囲 (x0..x1, z0..z1) の真ん中を使う。
func CamFor(s *detect.Stats) Cam {
	cx, cz := float64(s.X0+s.X1)/2, float64(s.Z0+s.Z1)/2
	span := math.Max(float64(s.X1-s.X0), float64(s.Z1-s.Z0))
	if len(s.Columns) > 0 {
		cx, cz = weightedMedian(s.Columns, func(c detect.Column) int { return c.X }), weightedMedian(s.Columns, func(c detect.Column) int { return c.Z })
		span = 2 * weightedRadius(s.Columns, cx, cz, 0.8) // 赤い印があるので、広めに撮っても、荒らされた所は見える
	}
	dist := math.Min(math.Max(span*1.1+40, 70), 600)
	y := math.Min(math.Max(float64(s.Y0+s.Y1)/2, 62), 90)
	return Cam{X: cx, Y: y, Z: cz, Dist: dist, Rot: -0.6, Angle: 0.75}
}

// CloseCamFor は、いちばん壊された所 (12 ブロック四方で壊した数が最大の区画) に寄ったカメラ。
// 荒らされた範囲が広いと、全体の画面では 1 か所ずつが小さいので、寄った画面も足す。
// 範囲が狭くて全体の画面と変わらないとき (全体の距離が近いとき) は、false。
func CloseCamFor(s *detect.Stats, overview Cam) (Cam, bool) {
	if len(s.Columns) == 0 || overview.Dist < 130 {
		return Cam{}, false
	}
	const cell = 12
	type key struct{ a, b int }
	type acc struct{ n, sx, sz, sy int }
	cells := map[key]*acc{}
	for _, c := range s.Columns {
		k := key{floorDiv(c.X, cell), floorDiv(c.Z, cell)}
		a := cells[k]
		if a == nil {
			a = &acc{}
			cells[k] = a
		}
		a.n += c.N
		a.sx += c.X * c.N
		a.sz += c.Z * c.N
		a.sy += c.Y1 * c.N
	}
	var best *acc
	for _, a := range cells {
		if best == nil || a.n > best.n {
			best = a
		}
	}
	y := math.Min(math.Max(float64(best.sy)/float64(best.n), 0), 300)
	return Cam{X: float64(best.sx) / float64(best.n), Y: y, Z: float64(best.sz) / float64(best.n), Dist: 45, Rot: -0.6, Angle: 0.9}, true
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// CoreBox は、壊した数の 97% が入る範囲の枠 (x0, z0, x1, z1)。遠くに離れた少数の点で、枠が画面を埋めないように、
// 外側の 1.5% ずつを除く。列が無いときは、範囲そのもの。
func CoreBox(s *detect.Stats) [4]int {
	if len(s.Columns) == 0 {
		return [4]int{s.X0, s.Z0, s.X1 + 1, s.Z1 + 1}
	}
	kx := func(c detect.Column) int { return c.X }
	kz := func(c detect.Column) int { return c.Z }
	return [4]int{
		int(weightedQuantile(s.Columns, kx, 0.015)) - 1, int(weightedQuantile(s.Columns, kz, 0.015)) - 1,
		int(weightedQuantile(s.Columns, kx, 0.985)) + 2, int(weightedQuantile(s.Columns, kz, 0.985)) + 2,
	}
}

// weightedQuantile は、壊した数で重みを付けた q (0〜1) の分位点。
func weightedQuantile(cols []detect.Column, key func(detect.Column) int, q float64) float64 {
	idx := make([]int, len(cols))
	total := 0
	for i, c := range cols {
		idx[i] = i
		total += c.N
	}
	sort.Slice(idx, func(a, b int) bool { return key(cols[idx[a]]) < key(cols[idx[b]]) })
	acc := 0
	for _, i := range idx {
		acc += cols[i].N
		if float64(acc) >= q*float64(total) {
			return float64(key(cols[i]))
		}
	}
	return float64(key(cols[idx[len(idx)-1]]))
}

func weightedMedian(cols []detect.Column, key func(detect.Column) int) float64 {
	idx := make([]int, len(cols))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return key(cols[idx[a]]) < key(cols[idx[b]]) })
	total := 0
	for _, c := range cols {
		total += c.N
	}
	acc := 0
	for _, i := range idx {
		acc += cols[i].N
		if acc*2 >= total {
			return float64(key(cols[i]))
		}
	}
	return 0
}

// weightedRadius は、中心から、壊した数の p (0〜1) の割合が入る水平の距離。
func weightedRadius(cols []detect.Column, cx, cz, p float64) float64 {
	type dw struct {
		d float64
		n int
	}
	ds := make([]dw, len(cols))
	total := 0
	for i, c := range cols {
		ds[i] = dw{math.Hypot(float64(c.X)-cx, float64(c.Z)-cz), c.N}
		total += c.N
	}
	sort.Slice(ds, func(a, b int) bool { return ds[a].d < ds[b].d })
	acc := 0
	for _, x := range ds {
		acc += x.n
		if float64(acc) >= p*float64(total) {
			return x.d
		}
	}
	return 0
}

// Pillar は、壊された 1 列 (x, z) とその高さ。画面では、縦の赤い線になる。
type Pillar struct {
	X, Z   int
	Y0, Y1 int
	N      int
}

// Annot は、撮った画面の上に重ねる印。地形だけでは、穴や欠けだけではどこが荒らされたか分からない。
// 壊された列を赤い線で、荒らされた範囲を黄色の枠で示す。
// 文字は ASCII だけ (実行する環境には日本語のフォントが無い)。
type Annot struct {
	Label    string   // 左上に書く 1 行目
	Sub      string   // 2 行目
	Pillars  []Pillar // 赤い線 (復旧後の画面では空)
	Box      [4]int   // 枠: x0, z0, x1, z1
	BoxY     int      // 枠を置く高さ
	BoxColor string   // 枠の色 (CSS の #rrggbb)
	NoBox    bool     // 枠を描かない (寄った画面では、枠が画面を横切るだけなので)
}

// maxPillars は、1 枚に描く列の数の上限 (多いときは、壊した数が多い列を残す)。
const maxPillars = 6000

// AnnotFor は Stats から、被害の画面に重ねる印を作る。
func AnnotFor(s *detect.Stats, label, sub string, withPillars bool) *Annot {
	a := &Annot{Label: label, Sub: sub, Box: CoreBox(s), BoxY: s.Y0, BoxColor: "#ffd400"}
	if withPillars {
		for _, c := range s.TopBreakColumns(maxPillars) {
			a.Pillars = append(a.Pillars, Pillar{X: c.X, Z: c.Z, Y0: c.Y0, Y1: c.Y1, N: c.N})
		}
	}
	return a
}

// Shooter は、Renderer に描かせて画面を得る。以前は BlueMap の web をブラウザで開いて撮っていた。
// Renderer は region ファイルを直接読むので、地図の更新を待つ必要はない。
// 印は、Renderer が返したカメラの変換を使って、こちらで描く (overlay.go)。
type Shooter struct {
	URL string // Renderer の API の接続先 (例 http://toro-renderer:8080)
	W   int
	H   int

	hc *http.Client

	LastOverlay string // 直前に印を描いた結果 (確認用)
}

func NewShooter(c *config.Config) *Shooter {
	return &Shooter{
		URL: strings.TrimRight(c.ShotConfig.RendererURL, "/"),
		W:   c.ShotConfig.ShotWidth, H: c.ShotConfig.ShotHeight,
		hc: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	}
}

// OverlayResult は、直前の撮影で印を描いた結果を返す (Shot が LastOverlay に残したもの)。
func (s *Shooter) OverlayResult() string { return s.LastOverlay }

type renderRequest struct {
	Map      string         `json:"map"`
	Camera   map[string]any `json:"camera"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Priority string         `json:"priority"`
	// 何も描かれなかった所は不透明の黒にする (以前の撮影の空の色と同じ)
	Background []int `json:"background"`
}

type renderResponse struct {
	Image      string    `json:"image"`
	View       []float64 `json:"view"`
	Projection []float64 `json:"projection"`
}

// Shot は Renderer に 1 枚描かせ、(ann があれば印を重ねて) PNG を out に書く。
func (s *Shooter) Shot(ctx context.Context, mapID string, cam Cam, out string, ann *Annot) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	body, err := json.Marshal(renderRequest{
		Map: mapID,
		Camera: map[string]any{
			"target":   []float64{cam.X, cam.Y, cam.Z},
			"distance": cam.Dist, "rotation": cam.Rot, "angle": cam.Angle,
		},
		Width: s.W, Height: s.H, Priority: "high", Background: []int{0, 0, 0},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/render", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("renderer: HTTP %d", resp.StatusCode)
	}
	var rr renderResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<27)).Decode(&rr); err != nil {
		return fmt.Errorf("renderer: 応答を読めません: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(rr.Image)
	if err != nil {
		return fmt.Errorf("renderer: 画像を読めません: %w", err)
	}
	if ann == nil {
		return os.WriteFile(out, raw, 0o644)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("renderer: PNG を読めません: %w", err)
	}
	base := image.NewRGBA(img.Bounds())
	draw.Draw(base, base.Bounds(), img, img.Bounds().Min, draw.Src)
	res := drawOverlay(base, rr.View, rr.Projection, ann)
	s.LastOverlay = res
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, base)
}
