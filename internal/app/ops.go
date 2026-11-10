package app

import (
	"encoding/json"
	"os"
	"strings"
)

// LoadOps は ops.json (サーバーの op の一覧) から、名前の集まりを読む。op は荒らしの検知の対象にしない
// (運営が、重機のように地形を直すことがあるため)。読めないときは空 (対象外が増えないだけ)。
func LoadOps(path string) map[string]bool {
	out := map[string]bool{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var ops []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &ops) != nil {
		return out
	}
	for _, o := range ops {
		if n := strings.ToLower(strings.TrimSpace(o.Name)); n != "" {
			out[n] = true
		}
	}
	return out
}
