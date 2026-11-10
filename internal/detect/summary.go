package detect

import (
	"crime-buster/internal/domain"
)

func SummaryOf(s *Stats) map[string]any {
	commands := make([]string, 0, len(s.Commands))
	for _, c := range s.Commands {
		commands = append(commands, domain.SafeChat(c, 120)) // プレイヤーが書いた文字。短くして、1 行にする
	}
	return map[string]any{
		"player": s.User.Name, "uuid": s.User.UUID, "world": s.World,
		"from": s.From, "to": s.To, "breaks": s.Breaks, "places": s.Places, "clicks": s.Clicks,
		"peak_per_s": s.PeakPerS, "peak_at": s.PeakAt,
		"x": []int{s.X0, s.X1}, "y": []int{s.Y0, s.Y1}, "z": []int{s.Z0, s.Z1},
		"top_breaks": s.TopBreaks, "commands": commands, "containers": s.Containers,
	}
}
