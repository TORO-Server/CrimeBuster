package notify

import (
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"fmt"
	"strings"
	"time"
)

// ChatReportInput は、チャットの報告文の材料。
type ChatReportInput struct {
	Name    string
	UUID    string
	Msgs    []detect.ChatMsg
	Trigger string
	Mode    config.Mode
	Muted   bool   // ミュートした (live)
	MuteCmd string // ミュートのコマンド (した、または予定)
	MuteErr string
}

// maxChatLines は、報告文に載せる発言の数 (多いときは、はじめと終わりを残す)。
const maxChatLines = 12

// BuildChatReport は、チャットのスパムの報告文 (1900 字以内)。Discord の @everyone などで人を呼ばないように、@ を無効にする。
func BuildChatReport(in ChatReportInput) string {
	var b strings.Builder
	if config.CapabilitiesFor(in.Mode).DryRunNotice {
		b.WriteString("**【試運転】投稿もミュートもしていません (内容の確認用)**\n")
	}
	b.WriteString("**種類**: チャットのスパム\n")
	b.WriteString(fmt.Sprintf("**荒らしプレイヤー**: %s (`%s`)\n", domain.SafeChat(in.Name, 40), domain.SafeChat(in.UUID, 40)))
	if n := len(in.Msgs); n > 0 {
		t0, t1 := in.Msgs[0].Time, in.Msgs[n-1].Time
		b.WriteString(fmt.Sprintf("**時間**: %s 〜 %s (約 %d 秒)\n", time.Unix(t0, 0).In(JST).Format("01/02 15:04:05"), time.Unix(t1, 0).In(JST).Format("15:04:05"), t1-t0))
		b.WriteString(fmt.Sprintf("**発言**: %d 件\n", n))
	}
	b.WriteString("**検知**: " + in.Trigger + "\n")
	switch {
	case in.Muted:
		b.WriteString("**対応**: ミュートした `" + in.MuteCmd + "`\n")
	case in.MuteErr != "":
		b.WriteString("**対応**: ミュートに失敗 (" + domain.SafeChat(in.MuteErr, 120) + ")\n")
	case in.MuteCmd != "":
		b.WriteString("**対応 (予定)**: `" + in.MuteCmd + "`\n")
	}
	b.WriteString("**発言 (抜粋)**:\n")
	for _, m := range pickChatLines(in.Msgs, maxChatLines) {
		b.WriteString(fmt.Sprintf("> `%s` %s\n", time.Unix(m.Time, 0).In(JST).Format("15:04:05"), domain.SafeChat(m.Msg, 80)))
	}
	out := b.String()
	if r := []rune(out); len(r) > 1900 {
		out = string(r[:1900]) + "…"
	}
	return out
}

// pickChatLines は、n 件を超えるときは、はじめの半分と終わりの半分を残す。
func pickChatLines(m []detect.ChatMsg, n int) []detect.ChatMsg {
	if len(m) <= n {
		return m
	}
	h := n / 2
	return append(append([]detect.ChatMsg(nil), m[:h]...), m[len(m)-(n-h):]...)
}
