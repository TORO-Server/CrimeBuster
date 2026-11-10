package console

import (
	"crime-buster/internal/domain"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 許可するコマンドの形。crime-buster が送るコマンドは、送る前にここで確かめる。
// 文字列全体が一致するものだけを通す (コマンドの注入を避けるため)。
// 以前は別のプロセス (中継) が確かめていたが、RCON では送る側 (このパッケージ) が確かめる。
// 確かめる場所が変わるだけで、通す形は変えない。BlueMap の形は、地図の更新をコンソールで
// 頼まなくなったので除いた。

// MaxKickReason は kick の理由の長さ。理由に制御文字と改行の仲間は入れない。
const MaxKickReason = 100

var (
	rollbackCmdRE = regexp.MustCompile(`^co rollback u:` + domain.NamePattern + ` t:([0-9]{1,9})s r:#` + domain.IDPattern + `$`)
	kickCmdRE     = regexp.MustCompile(fmt.Sprintf(`^kick %s [^\p{C}\p{Zl}\p{Zp}]{1,%d}$`, domain.NamePattern, MaxKickReason))
)

// Allow は、送ってよいコマンドの形。
type Allow struct {
	muteTmpl string
	mute     *regexp.Regexp
}

// NewAllow は、CHAT_MUTE_CMD の形も許可に足す。空なら、ミュートは通さない。
func NewAllow(muteTmpl string) (*Allow, error) {
	a := &Allow{muteTmpl: strings.TrimSpace(muteTmpl)}
	if a.muteTmpl == "" {
		return a, nil
	}
	if domain.CtlRE.MatchString(a.muteTmpl) || !strings.Contains(a.muteTmpl, "{name}") {
		return nil, fmt.Errorf("CHAT_MUTE_CMD: {name} を含む 1 行にしてください")
	}
	parts := strings.Split(a.muteTmpl, "{name}")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("^" + parts[0] + "(" + domain.NamePattern + ")" + strings.Join(parts[1:], domain.NamePattern) + "$")
	if err != nil {
		return nil, fmt.Errorf("CHAT_MUTE_CMD: %w", err)
	}
	a.mute = re
	return a, nil
}

// Allowed は、cmd が crime-buster の送る形かどうか。
// "list" は、サーバーの状態を変えない読み取りだけのコマンドで、疎通の確認に使う。
func (a *Allow) Allowed(cmd string) bool {
	switch cmd {
	case "save-all flush", "list":
		return true
	}
	if m := rollbackCmdRE.FindStringSubmatch(cmd); m != nil {
		return inRange(m[1], 1, domain.MaxRollbackSeconds)
	}
	if kickCmdRE.MatchString(cmd) {
		return true
	}
	if a.mute != nil {
		// 全部の {name} が同じ名前のときだけ (作り直して比べる)
		if m := a.mute.FindStringSubmatch(cmd); m != nil {
			return MuteCommand(a.muteTmpl, m[1]) == cmd
		}
	}
	return false
}

func inRange(s string, lo, hi int) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= lo && n <= hi
}
