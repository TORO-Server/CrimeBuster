package capture

import (
	"crime-buster/internal/detect"
	"strings"
	"testing"
)

func TestCloseCamFor(t *testing.T) {
	s := &detect.Stats{}
	for x := 0; x < 200; x++ { // 薄く広い
		s.Columns = append(s.Columns, detect.Column{X: x, Z: 0, N: 1, Y0: 60, Y1: 64})
	}
	for x := -30; x < -20; x++ { // 濃い所
		for z := 50; z < 60; z++ {
			s.Columns = append(s.Columns, detect.Column{X: x, Z: z, N: 20, Y0: 60, Y1: 70})
		}
	}
	if _, ok := CloseCamFor(s, Cam{Dist: 100}); ok {
		t.Fatal("全体が近いときは、寄った画面は要らない")
	}
	c, ok := CloseCamFor(s, Cam{Dist: 300})
	if !ok || c.X > -20 || c.X < -30 || c.Z < 50 || c.Z > 60 || c.Y != 70 {
		t.Fatalf("いちばん濃い所に寄る: %+v ok=%v", c, ok)
	}
	if floorDiv(-1, 12) != -1 || floorDiv(-12, 12) != -1 || floorDiv(-13, 12) != -2 || floorDiv(11, 12) != 0 {
		t.Fatal("floorDiv")
	}
}

func TestCamFor(t *testing.T) {
	// 点が 1 つの塊 (x -5660..-5640) と、遠くの外れ値 1 個 → 中心は塊、距離は外れ値で伸びない
	s := &detect.Stats{X0: -5712, X1: -5602, Y0: 55, Y1: 94, Z0: -10210, Z1: -10048}
	for x := -5660; x <= -5640; x++ {
		for z := -10130; z <= -10120; z++ {
			s.Columns = append(s.Columns, detect.Column{X: x, Z: z, N: 10})
		}
	}
	s.Columns = append(s.Columns, detect.Column{X: -5712, Z: -10048, N: 1})
	c := CamFor(s)
	if c.X < -5660 || c.X > -5640 || c.Z < -10130 || c.Z > -10120 {
		t.Fatalf("center %v", c)
	}
	if c.Dist < 70 || c.Dist > 150 {
		t.Fatalf("outlier stretched the camera: dist %v", c.Dist)
	}
	if h := c.Hash("overworld"); !strings.HasPrefix(h, "#overworld:") || !strings.HasSuffix(h, ":0:0:perspective") {
		t.Fatalf("hash %s", h)
	}
	// 列が無いときは範囲の真ん中
	c2 := CamFor(&detect.Stats{X0: 0, X1: 100, Z0: 0, Z1: 200, Y0: 64, Y1: 64})
	if c2.X != 50 || c2.Z != 100 {
		t.Fatalf("bbox center %v", c2)
	}
}
