package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
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
	cfg *Config
	bad []string
	per map[int][]chatRec
}

const chatKeep = 5 * 60 // 秒

func NewChatDetector(cfg *Config) *ChatDetector {
	return &ChatDetector{cfg: cfg, bad: loadWords(cfg.ChatBadWordsFile), per: map[int][]chatRec{}}
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
	if n := countSince(recs, m.Time-int64(cfg.ChatRateWindow.Seconds()), nil); cfg.ChatRate > 0 && n >= cfg.ChatRate {
		return trig("flood", fmt.Sprintf("%d 件 / %s", n, cfg.ChatRateWindow))
	}
	if rec.norm != "" && len([]rune(rec.norm)) >= 2 && cfg.ChatRepeat > 0 {
		n := countSince(recs, m.Time-int64(cfg.ChatRepeatWindow.Seconds()), func(r chatRec) bool { return r.norm == rec.norm })
		if n >= cfg.ChatRepeat {
			return trig("repeat", fmt.Sprintf("同じ発言 %d 回 / %s", n, cfg.ChatRepeatWindow))
		}
	}
	if cfg.ChatRun > 0 && longestRun(m.Msg) >= cfg.ChatRun {
		return trig("run", fmt.Sprintf("同じ文字が %d 個つづく", longestRun(m.Msg)))
	}
	if rec.inv {
		if n := countSince(recs, m.Time-chatKeep, func(r chatRec) bool { return r.inv }); n >= 2 {
			return trig("invite", fmt.Sprintf("Discord の招待リンクを %d 回 / 5 分", n))
		}
	}
	if rec.link && cfg.ChatLinkRepeat > 0 {
		if n := countSince(recs, m.Time-chatKeep, func(r chatRec) bool { return r.link }); n >= cfg.ChatLinkRepeat {
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

// ChatReportInput は、チャットの報告文の材料。
type ChatReportInput struct {
	Name    string
	UUID    string
	Msgs    []ChatMsg
	Trigger string
	Mode    Mode
	Muted   bool   // ミュートした (live)
	MuteCmd string // ミュートのコマンド (した、または予定)
	MuteErr string
}

// maxChatLines は、報告文に載せる発言の数 (多いときは、はじめと終わりを残す)。
const maxChatLines = 12

// BuildChatReport は、チャットのスパムの報告文 (1900 字以内)。Discord の @everyone などで人を呼ばないように、@ を無効にする。
func BuildChatReport(in ChatReportInput) string {
	var b strings.Builder
	if CapabilitiesFor(in.Mode).DryRunNotice {
		b.WriteString("**【試運転】投稿もミュートもしていません (内容の確認用)**\n")
	}
	b.WriteString("**種類**: チャットのスパム\n")
	b.WriteString(fmt.Sprintf("**荒らしプレイヤー**: %s (`%s`)\n", safeChat(in.Name, 40), safeChat(in.UUID, 40)))
	if n := len(in.Msgs); n > 0 {
		t0, t1 := in.Msgs[0].Time, in.Msgs[n-1].Time
		b.WriteString(fmt.Sprintf("**時間**: %s 〜 %s (約 %d 秒)\n", time.Unix(t0, 0).In(jst).Format("01/02 15:04:05"), time.Unix(t1, 0).In(jst).Format("15:04:05"), t1-t0))
		b.WriteString(fmt.Sprintf("**発言**: %d 件\n", n))
	}
	b.WriteString("**検知**: " + in.Trigger + "\n")
	switch {
	case in.Muted:
		b.WriteString("**対応**: ミュートした `" + in.MuteCmd + "`\n")
	case in.MuteErr != "":
		b.WriteString("**対応**: ミュートに失敗 (" + safeChat(in.MuteErr, 120) + ")\n")
	case in.MuteCmd != "":
		b.WriteString("**対応 (予定)**: `" + in.MuteCmd + "`\n")
	}
	b.WriteString("**発言 (抜粋)**:\n")
	for _, m := range pickChatLines(in.Msgs, maxChatLines) {
		b.WriteString(fmt.Sprintf("> `%s` %s\n", time.Unix(m.Time, 0).In(jst).Format("15:04:05"), safeChat(m.Msg, 80)))
	}
	out := b.String()
	if r := []rune(out); len(r) > 1900 {
		out = string(r[:1900]) + "…"
	}
	return out
}

// pickChatLines は、n 件を超えるときは、はじめの半分と終わりの半分を残す。
func pickChatLines(m []ChatMsg, n int) []ChatMsg {
	if len(m) <= n {
		return m
	}
	h := n / 2
	return append(append([]ChatMsg(nil), m[:h]...), m[len(m)-(n-h):]...)
}

// safeChat は、Discord に載せるプレイヤーの文字を無害にする。制御文字を除いて 1 行にし、長さを切り、
// メンション・インラインコードの区切り・マスクリンクを無効にする。
func safeChat(s string, max int) string {
	s = strings.Join(strings.Fields(ctlRE.ReplaceAllString(s, " ")), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	s = strings.ReplaceAll(s, "@", "@​")
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "](", "] (")
	return s
}

// MuteCommand は ChatMuteCmd の {name} を名前にしたコマンド。名前が Minecraft の名前として正しくないときは空 (コマンドの注入を避ける)。
func MuteCommand(tmpl, name string) string {
	if strings.TrimSpace(tmpl) == "" || !validName(name) {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(tmpl), "{name}", name)
}

// コンソールに渡す値の形。中継の許可 (relay.go) も、同じ形を使う。
const (
	namePattern = `[A-Za-z0-9_.]{1,32}`
	idPattern   = `[A-Za-z0-9_-]{1,64}` // ワールドと地図の名前
)

var (
	nameRE  = regexp.MustCompile(`^` + namePattern + `$`)
	worldRE = regexp.MustCompile(`^` + idPattern + `$`)
)

func validName(n string) bool { return nameRE.MatchString(n) }

// validWorld は、ワールドと地図の名前として安全か。
func validWorld(w string) bool { return worldRE.MatchString(w) }

// ---- scan-chat: 過去の発言に、いまの決まりを当てはめて、誤検知を見積もる (何も書かない・投稿しない) ----

func cmdScanChat(args []string) error {
	days := 30.0
	for i, a := range args {
		if a == "-days" && i+1 < len(args) {
			fmt.Sscan(args[i+1], &days)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cp, err := OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()
	since := time.Now().Add(-time.Duration(days * 24 * float64(time.Hour))).Unix()
	rows, err := cp.db.QueryContext(ctx, "SELECT rowid, time, user, message FROM "+cp.p+"chat WHERE time >= ? ORDER BY rowid", since)
	if err != nil {
		return err
	}
	msgs, err := scanChat(rows)
	rows.Close()
	if err != nil {
		return err
	}
	det := NewChatDetector(cfg)
	ops := LoadOps(cfg.OpsFile)
	type hit struct {
		T    int64
		Name string
		Trig string
		Why  string
	}
	var hits []hit
	cool := map[int]int64{}
	for _, m := range msgs {
		t := det.Feed(m)
		if t == nil || m.Time < cool[m.User] {
			continue
		}
		u, err := cp.User(ctx, m.User)
		if err != nil {
			continue
		}
		h := hit{T: m.Time, Name: u.Name, Trig: t.String()}
		if cfg.TrustedPlayers[strings.ToLower(u.Name)] || ops[strings.ToLower(u.Name)] {
			h.Why = "信頼する人"
		}
		cool[m.User] = m.Time + 600
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].T < hits[j].T })
	fmt.Printf("見た発言: %d 件 (%.0f 日)  閾値: 連投 %d 件/%s、同文 %d 回/%s、同じ文字 %d、リンク %d 回/5 分\n",
		len(msgs), days, cfg.ChatRate, cfg.ChatRateWindow, cfg.ChatRepeat, cfg.ChatRepeatWindow, cfg.ChatRun, cfg.ChatLinkRepeat)
	n := 0
	for _, h := range hits {
		if h.Why != "" {
			continue
		}
		n++
		fmt.Printf("  %s  %-20s %s\n", time.Unix(h.T, 0).In(jst).Format("01/02 15:04:05"), h.Name, h.Trig)
	}
	fmt.Printf("通報の対象になる検知: %d 件 (対象外 %d 件)\n", n, len(hits)-n)
	return nil
}

// ChatResult は、チャットの事件 1 件の処理の結果。
type ChatResult struct {
	Dir      string
	Report   string
	ThreadID string
	Muted    bool
}

// ProcessChat は、チャットのスパム 1 件を、mode に合わせて処理する。
//
//	observe: まとめを記録するだけ
//	dry-run: 報告文をフォルダに置くだけ (投稿もミュートもしない)
//	live:    (設定があれば) ミュート → 通報。AI の判定だけの事件は、ミュートしない
func (g *Guard) ProcessChat(ctx context.Context, inc *Incident, end int64) (*ChatResult, error) {
	if !validID(inc.ID) {
		return nil, fmt.Errorf("事件の ID %q はフォルダに使えません", inc.ID)
	}
	msgs, err := g.cp.ChatFor(ctx, inc.UserID, inc.Start-60, end)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no chat found for %s", inc.Name)
	}
	dir := filepath.Join(g.cfg.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &ChatResult{Dir: dir}
	if CapabilitiesFor(g.cfg.Mode).RecordOnly {
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs)})
	}

	in := ChatReportInput{Name: inc.Name, UUID: inc.UUID, Msgs: msgs, Trigger: inc.Trigger, Mode: g.cfg.Mode}
	if !inc.AIOnly {
		in.MuteCmd = MuteCommand(g.cfg.ChatMuteCmd, inc.Name)
	}
	if CapabilitiesFor(g.cfg.Mode).AutoMute && in.MuteCmd != "" && g.ops != nil {
		if _, err := g.ops.C.Run(ctx, in.MuteCmd, sendOnlyTimeout, nil); err != nil {
			in.MuteErr = err.Error()
			g.log.Error("ミュートに失敗", "player", inc.Name, "err", err)
		} else {
			in.Muted, res.Muted = true, true
		}
	}
	res.Report = BuildChatReport(in)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs), "muted": res.Muted})

	if CapabilitiesFor(g.cfg.Mode).AutoReport && g.reporter != nil {
		now := time.Now().Unix()
		key := threadKey(inc.Name)
		existing := ""
		g.mu.Lock()
		if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		g.mu.Unlock()
		var tags []string
		if res.Muted && g.cfg.ChatMuteTagID != "" {
			tags = append(tags, g.cfg.ChatMuteTagID)
		}
		id, err := g.reporter.PostTags(ctx, existing, safeChat(inc.Name, 90), res.Report, nil, tags)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		g.mu.Lock()
		g.store.S.Threads[key] = ThreadRef{ThreadID: id, At: now}
		_ = g.store.Save()
		g.mu.Unlock()
	}
	rec := &Record{
		ID: inc.ID, Kind: "chat", Name: inc.Name, UUID: inc.UUID, UserID: inc.UserID,
		Created: time.Now().Unix(), Start: inc.Start, End: end, Trigger: inc.Trigger,
		State: StatePending, Messages: len(msgs), Muted: res.Muted,
	}
	if res.ThreadID != "" {
		rec.State, rec.Reported, rec.Thread = StateReported, true, res.ThreadID
		rec.decide(rec.Created, "crime-buster", "report", "")
	}
	if res.Muted {
		rec.decide(rec.Created, "crime-buster", "mute", "")
	}
	if err := saveRecord(dir, rec); err != nil {
		g.log.Warn("事件の記録が書けない", "incident", inc.ID, "err", err)
	}
	return res, nil
}
