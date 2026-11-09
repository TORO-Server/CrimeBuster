package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warp は EssentialsX のワープ地点 (plugins/Essentials/warps/*.yml)。報告に「どこの近く」かを書くために使う。
type Warp struct {
	Name    string
	World   string
	X, Y, Z float64
}

// LoadWarps は dir の *.yml を全部読む。dir が空・読めないときは何も返さない。
func LoadWarps(dir string) []Warp {
	if dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.yml"))
	var out []Warp
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var y struct {
			Name      string  `yaml:"name"`
			WorldName string  `yaml:"world-name"`
			X         float64 `yaml:"x"`
			Y         float64 `yaml:"y"`
			Z         float64 `yaml:"z"`
		}
		if yaml.Unmarshal(b, &y) != nil {
			continue
		}
		name := y.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(f), ".yml")
		}
		out = append(out, Warp{Name: name, World: y.WorldName, X: y.X, Y: y.Y, Z: y.Z})
	}
	return out
}

// Nearest は (x, z) にいちばん近い同じワールドのワープと、水平の距離。
func Nearest(ws []Warp, world string, x, z float64) (Warp, float64, bool) {
	best, bd, ok := Warp{}, math.MaxFloat64, false
	for _, w := range ws {
		if w.World != "" && world != "" && w.World != world {
			continue
		}
		if d := math.Hypot(w.X-x, w.Z-z); d < bd {
			best, bd, ok = w, d, true
		}
	}
	return best, bd, ok
}
