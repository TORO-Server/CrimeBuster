package domain

import ()

// Verdict は事件の判定。
type Verdict struct {
	Verdict    string `json:"verdict"`    // grief / legit / unclear
	Confidence int    `json:"confidence"` // 0〜100
	Summary    string `json:"summary"`    // 日本語で 1〜2 文
}

// Allows は、ロールバックしてよい判定か。荒らしで、確からしさが minConf 以上のときだけ。
func (v *Verdict) Allows(minConf int) bool {
	return v != nil && v.Verdict == "grief" && v.Confidence >= minConf
}
