package main

import "testing"

// TestCapabilitiesFor は、4つのモード全てについて、能力の全フィールドが
// 現行の挙動 (モードの直接比較で決まっていた可否) と一致することを確かめる。
func TestCapabilitiesFor(t *testing.T) {
	cases := []struct {
		mode Mode
		want Capabilities
	}{
		{ModeObserve, Capabilities{RecordOnly: true}},
		{ModeDryRun, Capabilities{Capture: true, DryRunNotice: true}},
		{ModeReview, Capabilities{Capture: true, AcceptOps: true, KeepEditCommandUsers: true}},
		{ModeLive, Capabilities{
			Capture: true, AcceptOps: true,
			AutoReport: true, AutoRollback: true, AutoMute: true, AutoKick: true,
		}},
		// 知らないモードには、能力を何も与えない。
		{Mode("unknown"), Capabilities{}},
	}
	for _, c := range cases {
		if got := CapabilitiesFor(c.mode); got != c.want {
			t.Errorf("CapabilitiesFor(%q) = %+v, want %+v", c.mode, got, c.want)
		}
	}
}
