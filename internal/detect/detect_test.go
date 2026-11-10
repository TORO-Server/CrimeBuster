package detect

import (
	"crime-buster/internal/config"
	"testing"
	"time"
)

func testCfg() *config.Config {
	return &config.Config{
		DetectConfig: config.DetectConfig{
			BurstWindow: 5 * time.Second, BreakBurst: 100, PlaceBurst: 150,
			SustainedWindow: 10 * time.Minute, SustainedBreaks: 1500,
		},
	}
}

func feedN(d *Detector, user int, sec int64, action, n int) *Trigger {
	var last *Trigger
	for i := 0; i < n; i++ {
		if t := d.Feed(Event{User: user, Time: sec, Action: action}); t != nil {
			last = t
		}
	}
	return last
}

func TestDetectorHumanSpeedNotFlagged(t *testing.T) {
	d := NewDetector(testCfg())
	// 1 秒に 8 個を 2 分続けても (960 個)、バーストにも長い窓にも当たらない
	for s := int64(1000); s < 1120; s++ {
		if tr := feedN(d, 1, s, actBreak, 8); tr != nil {
			t.Fatalf("flagged at %d: %v", s, tr)
		}
	}
}

func TestDetectorBurstBreak(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actBreak, 60); tr != nil {
		t.Fatalf("early: %v", tr)
	}
	tr := feedN(d, 1, 1002, actBreak, 60) // 5 秒の窓に 120 個
	if tr == nil || tr.Kind != "burst-break" || tr.Breaks < 100 {
		t.Fatalf("want burst-break, got %v", tr)
	}
}

func TestDetectorWindowExpires(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 90)
	// 5 秒以上あいた次の 90 個は、足されない
	if tr := feedN(d, 1, 1010, actBreak, 90); tr != nil {
		t.Fatalf("flagged across the gap: %v", tr)
	}
}

func TestDetectorBurstPlace(t *testing.T) {
	d := NewDetector(testCfg())
	tr := feedN(d, 2, 1000, actPlace, 150)
	if tr == nil || tr.Kind != "burst-place" {
		t.Fatalf("want burst-place, got %v", tr)
	}
}

func TestDetectorSustained(t *testing.T) {
	d := NewDetector(testCfg())
	var tr *Trigger
	// 1 秒に 5 個 (人でも出せる速さ) を 5 分以上続ける → 1500 個で長い窓に当たる
	for s := int64(1000); s < 1400 && tr == nil; s++ {
		tr = feedN(d, 3, s, actBreak, 5)
	}
	if tr == nil || tr.Kind != "sustained-break" {
		t.Fatalf("want sustained-break, got %v", tr)
	}
}

func TestDetectorIgnoresClicksAndSeparatesUsers(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actClick, 500); tr != nil {
		t.Fatalf("click flagged: %v", tr)
	}
	feedN(d, 1, 1000, actBreak, 60)
	if tr := feedN(d, 2, 1000, actBreak, 60); tr != nil {
		t.Fatalf("users mixed: %v", tr)
	}
}

func TestDetectorOutOfOrder(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1005, actBreak, 60)
	// 少し前の秒が後から来ても、窓の中なら足される
	tr := feedN(d, 1, 1003, actBreak, 60)
	if tr == nil {
		t.Fatal("out-of-order rows inside the window must count")
	}
}

func TestDetectorReset(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 99)
	d.Reset(1)
	if tr := feedN(d, 1, 1000, actBreak, 5); tr != nil {
		t.Fatalf("not reset: %v", tr)
	}
}
