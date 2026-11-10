package domain

import (
	"regexp"
	"strings"
)

func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ctlRE は、制御文字と、行を分ける文字。コンソールの 1 行にも、Discord の 1 行にも入れない。
var CtlRE = regexp.MustCompile(`[\p{C}\p{Zl}\p{Zp}]`)

// cleanText は、コンソールや Discord に渡す理由の文字から、制御文字 (改行など) を除いて短くする。
func CleanText(s string, max int) string {
	s = strings.TrimSpace(CtlRE.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// safeChat は、Discord に載せるプレイヤーの文字を無害にする。制御文字を除いて 1 行にし、長さを切り、
// メンション・インラインコードの区切り・マスクリンクを無効にする。
func SafeChat(s string, max int) string {
	s = strings.Join(strings.Fields(CtlRE.ReplaceAllString(s, " ")), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	s = strings.ReplaceAll(s, "@", "@​")
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "](", "] (")
	return s
}

// コンソールに渡す値の形。中継の許可 (relay.go) も、同じ形を使う。
const (
	NamePattern = `[A-Za-z0-9_.]{1,32}`
	IDPattern   = `[A-Za-z0-9_-]{1,64}` // ワールドと地図の名前
)

var (
	nameRE  = regexp.MustCompile(`^` + NamePattern + `$`)
	worldRE = regexp.MustCompile(`^` + IDPattern + `$`)
)

func ValidName(n string) bool { return nameRE.MatchString(n) }

// validWorld は、ワールドと地図の名前として安全か。
func ValidWorld(w string) bool { return worldRE.MatchString(w) }
