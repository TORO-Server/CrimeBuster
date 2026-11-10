package notify

import (
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/store"
	"fmt"
	"strings"
	"time"
)

var JST = time.FixedZone("JST", 9*3600)

// Outcome は、ロールバックなどの対応の結果。
type Outcome struct {
	Planned     bool // dry-run: やる予定のコマンドだけ
	RolledBack  bool
	Cmd         string
	RestoreCmd  string
	RolledDone  int
	RolledTotal int
	Error       string
	Kicked      bool
	AI          *domain.Verdict // AI の判定 (使わないときは nil)
	AIError     string
	Skipped     string // ロールバックをしなかった理由
	CmdUntil    int64  // Cmd の t: で足りる期限 (人が後で実行するときだけ。0 なら書かない)
}

func VerdictJP(v string) string {
	switch v {
	case "grief":
		return "荒らし"
	case "legit":
		return "正当な作業"
	}
	return "判断できない"
}

// ReportInput は報告文の材料。
type ReportInput struct {
	Stats   *detect.Stats
	Trigger string
	Warp    string // 「ワープ kanso_town の近く (約 12 ブロック)」。無ければ空
	Out     Outcome
	Images  []string     // 画像の説明 (添付の順)
	Center  *capture.Cam // いちばん壊した場所の中心 (「座標 x z 付近」と書く)
	Mode    config.Mode
}

// ThreadTitle はフォーラムの投稿の名前。この通報チャンネルでは、荒らしの名前にしている。
func ThreadTitle(s *detect.Stats) string { return s.User.Name }

// noCmdText は、名前かワールドの形が安全でなく、コマンドを書けないときの文。
const noCmdText = "名前かワールドの形が正しくないので、コマンドを作れません。運営が記録を確かめてください"

// BuildReport は通報チャンネルの規則 (ワールド名・荒らしプレイヤーの名前・証拠画像) に合わせた本文。
// このチャンネルでは、ボットに「埋め込みのリンク」の権限が無いので、埋め込み (embed) は使わず、
// 本文は普通の文字だけにする。
func BuildReport(in ReportInput) string {
	s := in.Stats
	var b strings.Builder
	if config.CapabilitiesFor(in.Mode).DryRunNotice {
		b.WriteString("**[試運転: 投稿もロールバックもしていません]**\n")
	}
	fmt.Fprintf(&b, "**ワールド**: %s\n", domain.SafeChat(orDash(s.World), 64))
	fmt.Fprintf(&b, "**荒らしプレイヤー**: %s (`%s`)\n", domain.SafeChat(s.User.Name, 40), domain.SafeChat(s.User.UUID, 40))
	if in.Center != nil {
		fmt.Fprintf(&b, "**座標**: %.0f %.0f 付近 (x z)\n", in.Center.X, in.Center.Z)
	}
	loc := fmt.Sprintf("x %d 〜 %d, y %d 〜 %d, z %d 〜 %d", s.X0, s.X1, s.Y0, s.Y1, s.Z0, s.Z1)
	if in.Warp != "" {
		loc += " (" + in.Warp + ")"
	}
	fmt.Fprintf(&b, "**範囲**: %s\n", loc)
	fmt.Fprintf(&b, "**時間**: %s 〜 %s (%s)\n", fmtT(s.From), fmtTEnd(s.From, s.To), fmtDur(s.To-s.From))
	fmt.Fprintf(&b, "**内容**: ブロック破壊 %s 個 / 設置 %s 個", Comma(s.Breaks), Comma(s.Places))
	if s.PeakPerS > 0 {
		fmt.Fprintf(&b, "。最大 %s 個/秒", Comma(s.PeakPerS))
	}
	b.WriteString("\n")
	if len(s.TopBreaks) > 0 {
		parts := make([]string, 0, 5)
		for i, m := range s.TopBreaks {
			if i == 5 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s", domain.SafeChat(m.Material, 40), Comma(m.Count)))
		}
		fmt.Fprintf(&b, "**主な破壊**: %s\n", strings.Join(parts, ", "))
	}
	if s.Containers > 0 {
		fmt.Fprintf(&b, "**コンテナの出し入れ**: %d 件\n", s.Containers)
	}
	if len(s.Commands) > 0 {
		fmt.Fprintf(&b, "**使ったコマンド**: %s\n", strings.Join(summarizeCommands(s.Commands), ", "))
	} else {
		b.WriteString("**使ったコマンド**: なし (道具を使わずにこの速さ)\n")
	}
	fmt.Fprintf(&b, "**検知**: %s\n", in.Trigger)

	if in.Out.AI != nil {
		fmt.Fprintf(&b, "**AI の判定**: %s (確からしさ %d%%) %s\n", VerdictJP(in.Out.AI.Verdict), in.Out.AI.Confidence, in.Out.AI.Summary)
	}
	switch {
	case in.Out.Skipped != "" && in.Out.Cmd == "":
		fmt.Fprintf(&b, "**対応**: 自動ロールバックはしていません (%s)。%s\n", in.Out.Skipped, noCmdText)
	case in.Out.Skipped != "":
		fmt.Fprintf(&b, "**対応**: 自動ロールバックはしていません (%s)。運営が確認してください。ロールバックするときは `%s`\n", in.Out.Skipped, in.Out.Cmd)
	case in.Out.RolledBack:
		fmt.Fprintf(&b, "**対応**: CoreProtect で自動ロールバック済み (%s / %s 件)\n", Comma(in.Out.RolledDone), Comma(in.Out.RolledTotal))
	case in.Out.Planned && in.Out.Cmd == "":
		fmt.Fprintf(&b, "**ロールバック (未実行)**: %s\n", noCmdText)
	case in.Out.Planned && in.Out.CmdUntil > 0:
		fmt.Fprintf(&b, "**ロールバック (未実行)**: `%s`\n", in.Out.Cmd)
		fmt.Fprintf(&b, "%s までに実行してください。それより後は、t: を「実行する時刻 − %s」より長くしてください。\n", fmtT(in.Out.CmdUntil), fmtT(s.From))
	case in.Out.Planned:
		fmt.Fprintf(&b, "**対応 (予定)**: `%s`\n", in.Out.Cmd)
	case in.Out.Error != "":
		fmt.Fprintf(&b, "**対応**: 自動ロールバックに失敗 (%s)。手動で確認してください\n", domain.SafeChat(in.Out.Error, 200))
	}
	if in.Out.Kicked {
		b.WriteString("**対応**: キック済み\n")
	}
	if in.Out.RestoreCmd != "" {
		fmt.Fprintf(&b, "誤検知のときは `%s` で戻せます。\n", in.Out.RestoreCmd)
	}
	if len(in.Images) > 0 {
		fmt.Fprintf(&b, "画像: %s\n", strings.Join(in.Images, " / "))
	}
	out := b.String()
	if len([]rune(out)) > 1900 { // Discord の上限は 2000 文字
		out = string([]rune(out)[:1900])
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fmtT(unix int64) string { return time.Unix(unix, 0).In(JST).Format("01/02 15:04:05") }

// fmtTEnd は、開始と同じ日なら、時刻だけ。
func fmtTEnd(from, to int64) string {
	if time.Unix(from, 0).In(JST).YearDay() == time.Unix(to, 0).In(JST).YearDay() {
		return time.Unix(to, 0).In(JST).Format("15:04:05")
	}
	return fmtT(to)
}

func fmtDur(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("約 %d 分", (sec+30)/60)
	default:
		return fmt.Sprintf("約 %d 時間 %d 分", sec/3600, (sec%3600+30)/60)
	}
}

func Comma(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// summarizeCommands は同じコマンドをまとめる ("/co i" が 2 回 → "/co i ×2")。
func summarizeCommands(cmds []string) []string {
	var order []string
	n := map[string]int{}
	for _, c := range cmds {
		if n[c] == 0 {
			order = append(order, c)
		}
		n[c]++
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		disp := domain.SafeChat(key, 60) // プレイヤーが書いた文字。インラインコードを閉じられないようにする
		if n[key] > 1 {
			out = append(out, fmt.Sprintf("`%s` ×%d", disp, n[key]))
		} else {
			out = append(out, "`"+disp+"`")
		}
	}
	return out
}

// WarpText は「ワープ kanso_town の近く (約 12 ブロック)」。遠い (600 ブロック超) ときは書かない。
func WarpText(w store.Warp, dist float64) string {
	if dist > 600 {
		return ""
	}
	return fmt.Sprintf("ワープ %s の近く (約 %d ブロック)", domain.SafeChat(w.Name, 40), int(dist+0.5))
}
