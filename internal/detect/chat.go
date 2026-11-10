package detect

import (
	"bufio"
	"context"
	"crime-buster/internal/config"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// ChatMsg は CoreProtect の chat の 1 行 (プレイヤーの発言)。
type ChatMsg struct {
	RowID int64
	Time  int64
	User  int
	Msg   string
}

// PollChat は after より後ろの発言を、rowid 順に最大 limit 件返す。
func (c *CP) PollChat(ctx context.Context, after int64, limit int) ([]ChatMsg, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT rowid, time, user, message FROM "+c.p+"chat WHERE rowid > ? ORDER BY rowid LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChat(rows)
}

func (c *CP) MaxChatRowID(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := c.db.QueryRowContext(ctx, "SELECT MAX(rowid) FROM "+c.p+"chat").Scan(&n)
	return n.Int64, err
}

// maxChatFor は、ChatFor が返す発言の数の上限 (新しいほうを残す)。
const maxChatFor = 5000

// ChatFor は 1 人の発言を、時間の順に返す。多いときは、新しいほうから maxChatFor 件まで。
func (c *CP) ChatFor(ctx context.Context, user int, from, to int64) ([]ChatMsg, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT rowid, time, user, message FROM "+c.p+"chat WHERE user = ? AND time BETWEEN ? AND ? ORDER BY time DESC, rowid DESC LIMIT ?", user, from, to, maxChatFor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs, err := scanChat(rows)
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}

// ChatSince は since 以降の発言を、新しいほうから最大 limit 件取って、時間の順に返す。user が 0 でなければ、その人だけ。
func (c *CP) ChatSince(ctx context.Context, since int64, user, limit int) ([]ChatMsg, error) {
	q := "SELECT rowid, time, user, message FROM " + c.p + "chat WHERE time >= ?"
	args := []any{since}
	if user != 0 {
		q += " AND user = ?"
		args = append(args, user)
	}
	rows, err := c.db.QueryContext(ctx, q+" ORDER BY rowid DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs, err := scanChat(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func scanChat(rows *sql.Rows) ([]ChatMsg, error) {
	var out []ChatMsg
	for rows.Next() {
		var m ChatMsg
		var t, u sql.NullInt64
		var msg sql.NullString
		if err := rows.Scan(&m.RowID, &t, &u, &msg); err != nil {
			return nil, err
		}
		m.Time, m.User, m.Msg = t.Int64, int(u.Int64), msg.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChatTrigger は、チャットのスパムを見つけたとき。
type ChatTrigger struct {
	User   int
	Time   int64
	Kind   string // flood / repeat / run / link / badword
	Detail string
}

func (t ChatTrigger) String() string { return t.Kind + ": " + t.Detail }

type chatRec struct {
	t    int64
	norm string
	link bool
	inv  bool
}

// ChatDetector は、プレイヤーごとの直近 5 分の発言を覚えて、スパムを見つける。
type ChatDetector struct {
	cfg *config.Config
	bad []string
	per map[int][]chatRec
}

const chatKeep = 5 * 60 // 秒

func NewChatDetector(cfg *config.Config) *ChatDetector {
	return &ChatDetector{cfg: cfg, bad: loadWords(cfg.ChatConfig.ChatBadWordsFile), per: map[int][]chatRec{}}
}

// loadWords は 1 行に 1 語のファイルを読む (空の行と # の行は飛ばす)。無ければ空。
func loadWords(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if w := strings.ToLower(strings.TrimSpace(sc.Text())); w != "" && !strings.HasPrefix(w, "#") {
			out = append(out, w)
		}
	}
	return out
}

var (
	linkRE   = regexp.MustCompile(`(?i)https?://\S+|www\.\S+|\b[a-z0-9-]+\.(?:com|net|org|jp|gg|xyz|ru|tk|ml|io|me)\b`)
	inviteRE = regexp.MustCompile(`(?i)discord(?:app)?\.(?:gg|com/invite)/\S+|discord\.gg\b`)
)

// normChat は、数字と記号と空白を除いて小文字にする (「aaa 1」「aaa 2」を同じ発言と見るため)。
func normChat(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// longestRun は、同じ文字がつづく最大の長さ。
func longestRun(s string) int {
	best, cur := 0, 0
	var prev rune = -1
	for _, r := range s {
		if r == prev {
			cur++
		} else {
			cur, prev = 1, r
		}
		if cur > best {
			best = cur
		}
	}
	return best
}

// Reset は 1 人の覚えを消す (通報したあと、続きを新しく数え直す)。
func (d *ChatDetector) Reset(user int) { delete(d.per, user) }

// Feed は発言を 1 つ受けて、スパムなら ChatTrigger を返す。時間の順に渡す。
func (d *ChatDetector) Feed(m ChatMsg) *ChatTrigger {
	cfg := d.cfg
	rec := chatRec{t: m.Time, norm: normChat(m.Msg), link: linkRE.MatchString(m.Msg), inv: inviteRE.MatchString(m.Msg)}
	recs := d.per[m.User]
	k := 0
	for _, r := range recs {
		if m.Time-r.t <= chatKeep {
			recs[k] = r
			k++
		}
	}
	recs = append(recs[:k], rec)
	d.per[m.User] = recs

	trig := func(kind, detail string) *ChatTrigger {
		return &ChatTrigger{User: m.User, Time: m.Time, Kind: kind, Detail: detail}
	}

	lower := strings.ToLower(m.Msg)
	for _, w := range d.bad {
		if strings.Contains(lower, w) {
			return trig("badword", "禁止語を含む発言")
		}
	}
	if n := countSince(recs, m.Time-int64(cfg.ChatConfig.ChatRateWindow.Seconds()), nil); cfg.ChatConfig.ChatRate > 0 && n >= cfg.ChatConfig.ChatRate {
		return trig("flood", fmt.Sprintf("%d 件 / %s", n, cfg.ChatConfig.ChatRateWindow))
	}
	if rec.norm != "" && len([]rune(rec.norm)) >= 2 && cfg.ChatConfig.ChatRepeat > 0 {
		n := countSince(recs, m.Time-int64(cfg.ChatConfig.ChatRepeatWindow.Seconds()), func(r chatRec) bool { return r.norm == rec.norm })
		if n >= cfg.ChatConfig.ChatRepeat {
			return trig("repeat", fmt.Sprintf("同じ発言 %d 回 / %s", n, cfg.ChatConfig.ChatRepeatWindow))
		}
	}
	if cfg.ChatConfig.ChatRun > 0 && longestRun(m.Msg) >= cfg.ChatConfig.ChatRun {
		return trig("run", fmt.Sprintf("同じ文字が %d 個つづく", longestRun(m.Msg)))
	}
	if rec.inv {
		if n := countSince(recs, m.Time-chatKeep, func(r chatRec) bool { return r.inv }); n >= 2 {
			return trig("invite", fmt.Sprintf("Discord の招待リンクを %d 回 / 5 分", n))
		}
	}
	if rec.link && cfg.ChatConfig.ChatLinkRepeat > 0 {
		if n := countSince(recs, m.Time-chatKeep, func(r chatRec) bool { return r.link }); n >= cfg.ChatConfig.ChatLinkRepeat {
			return trig("link", fmt.Sprintf("リンクつきの発言 %d 回 / 5 分", n))
		}
	}
	return nil
}

func countSince(recs []chatRec, since int64, pred func(chatRec) bool) int {
	n := 0
	for _, r := range recs {
		if r.t >= since && (pred == nil || pred(r)) {
			n++
		}
	}
	return n
}

// ChatRowsSince は、since 以降の発言を rowid の順に全部返す (scan-chat 用)。
func (c *CP) ChatRowsSince(ctx context.Context, since int64) ([]ChatMsg, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT rowid, time, user, message FROM "+c.p+"chat WHERE time >= ? ORDER BY rowid", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChat(rows)
}
