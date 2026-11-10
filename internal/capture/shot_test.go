package capture

import (
	"bytes"
	"context"
	"crime-buster/internal/detect"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCoreBoxIgnoresFarOutliers(t *testing.T) {
	s := &detect.Stats{X0: -5712, X1: -5602, Z0: -10210, Z1: -10048, Y0: 55, Y1: 94}
	for x := -5650; x < -5630; x++ {
		for z := -10140; z < -10120; z++ {
			s.Columns = append(s.Columns, detect.Column{X: x, Z: z, N: 10, Y0: 60, Y1: 70})
		}
	}
	// 遠くに 1 列だけ (全体の 0.03% 以下)
	s.Columns = append(s.Columns, detect.Column{X: -5712, Z: -10210, N: 1, Y0: 60, Y1: 60})
	b := CoreBox(s)
	if b[0] < -5655 || b[2] > -5627 || b[1] < -10145 || b[3] > -10117 {
		t.Fatalf("box は中心の塊に収まるはず: %v", b)
	}
	if b[0] >= b[2] || b[1] >= b[3] {
		t.Fatalf("box が逆: %v", b)
	}
}

func TestCoreBoxWithoutColumnsUsesRange(t *testing.T) {
	s := &detect.Stats{X0: 1, X1: 9, Z0: 2, Z1: 8}
	if b := CoreBox(s); b != [4]int{1, 2, 10, 9} {
		t.Fatalf("box: %v", b)
	}
}

func TestAnnotForLimitsPillars(t *testing.T) {
	s := &detect.Stats{X0: 0, X1: 100, Z0: 0, Z1: 100, Y0: 60, Y1: 70}
	for i := 0; i < maxPillars+500; i++ {
		s.Columns = append(s.Columns, detect.Column{X: i % 100, Z: i / 100, N: 1 + i%7, Y0: 60, Y1: 62})
	}
	a := AnnotFor(s, "x", "y", true)
	if len(a.Pillars) != maxPillars {
		t.Fatalf("pillars=%d", len(a.Pillars))
	}
	if b := AnnotFor(s, "x", "y", false); len(b.Pillars) != 0 {
		t.Fatalf("復旧後の画面に赤い線は要らない: %d", len(b.Pillars))
	}
}

// 射影: view と projection の積で、画面の点になる。
func TestProjector(t *testing.T) {
	ident := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	p := projector{view: ident, proj: ident, w: 100, h: 100}
	x, y, ok := p.project(0, 0, 0)
	if !ok || x != 50 || y != 50 {
		t.Fatalf("中心: %v %v %v", x, y, ok)
	}
	x, y, ok = p.project(0.5, -0.5, 0)
	if !ok || x != 75 || y != 75 {
		t.Fatalf("右下: %v %v %v", x, y, ok)
	}
	// 後ろの点 (clip の w が 0 以下) は描かない
	flip := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, -1, 0, 0, 0, 0} // w = -z
	if _, _, ok := p.project(0, 0, 1); !ok {
		t.Fatal("恒等では w=1 で描くはず")
	}
	p2 := projector{view: ident, proj: flip, w: 100, h: 100}
	if _, _, ok := p2.project(0, 0, 1); ok {
		t.Fatal("後ろの点を描いた")
	}
	if _, _, ok := p2.project(0, 0, -1); !ok {
		t.Fatal("前の点を落とした")
	}
}

// Renderer の API の試験用サーバーで、要求の形と、注釈の合成を確かめる。
func TestShooterAgainstFakeRenderer(t *testing.T) {
	base := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			base.Set(x, y, color.RGBA{30, 60, 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, base); err != nil {
		t.Fatal(err)
	}
	ident := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/render" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":      base64.StdEncoding.EncodeToString(buf.Bytes()),
			"view":       ident,
			"projection": ident,
		})
	}))
	defer srv.Close()

	s := &Shooter{URL: srv.URL, W: 64, H: 48, hc: &http.Client{}}
	out := filepath.Join(t.TempDir(), "shot.png")
	ctx := context.Background()
	cam := Cam{X: 1, Y: 70, Z: 2, Dist: 100, Rot: -0.6, Angle: 0.75}

	// 注釈が無いときは、受けた PNG をそのまま書く
	if err := s.Shot(ctx, "world", cam, out, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, buf.Bytes()) {
		t.Fatal("注釈の無い画面は、そのままのはず")
	}
	if gotReq["map"] != "world" || gotReq["priority"] != "high" {
		t.Fatalf("要求: %v", gotReq)
	}
	camReq, _ := gotReq["camera"].(map[string]any)
	if camReq["distance"] != 100.0 || camReq["rotation"] != -0.6 || camReq["angle"] != 0.75 {
		t.Fatalf("カメラ: %v", camReq)
	}
	tgt, _ := camReq["target"].([]any)
	if len(tgt) != 3 || tgt[0] != 1.0 || tgt[1] != 70.0 || tgt[2] != 2.0 {
		t.Fatalf("中心: %v", tgt)
	}
	if bg, _ := gotReq["background"].([]any); len(bg) != 3 {
		t.Fatalf("背景: %v", gotReq["background"])
	}

	// 注釈があるときは、印を重ねた PNG になり、結果の文字列が残る
	ann := &Annot{
		Label: "Steve grief 42 blocks in 1h", Sub: "world: main",
		Pillars: []Pillar{{X: 0, Z: 0, Y0: 0, Y1: 0}},
		Box:     [4]int{-1, -1, 1, 1}, BoxY: 0, BoxColor: "#ffd400",
	}
	if err := s.Shot(ctx, "world", cam, out, ann); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != 64 || got.Y != 48 {
		t.Fatalf("大きさ %v", got)
	}
	if s.LastOverlay != "ok:1 pillars, box=true" {
		t.Fatalf("overlay: %q", s.LastOverlay)
	}
	// ラベルのパネルで、左上の画素が変わっている
	if r, _, _, _ := img.At(13, 13).RGBA(); r>>8 == 30 {
		t.Fatal("ラベルが描かれていない")
	}
}

func TestShooterErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	s := &Shooter{URL: srv.URL, W: 64, H: 48, hc: &http.Client{}}
	if err := s.Shot(context.Background(), "world", Cam{}, filepath.Join(t.TempDir(), "x.png"), nil); err == nil {
		t.Fatal("HTTP の失敗を成功とした")
	}
}
