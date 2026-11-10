package connector

import (
	"context"
	"crime-buster/internal/app"
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
	"crime-buster/internal/store"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 操作のためのコネクタ。TORO-MCP が、Admin 以上の人の頼みを、ここに取り次ぐ。
//
//	GET  /v1/status
//	GET  /v1/incidents?state=pending&limit=20          事件の一覧 (新しい順)
//	GET  /v1/incidents/{id}                            事件 1 件 (記録と報告文)
//	GET  /v1/incidents/{id}/images/{name}              証拠の画像 (PNG)
//	POST /v1/incidents/{id}/report    {"note"}          フォーラムに通報する
//	POST /v1/incidents/{id}/rollback                    ロールバックを裏で始める (202)
//	POST /v1/incidents/{id}/dismiss   {"reason"}        荒らしではないとして閉じる
//	POST /v1/players/{name}/mute      {"reason"}        ミュート (CHAT_MUTE_CMD)
//	POST /v1/players/{name}/kick      {"reason"}        退出させる
//	GET  /v1/players/{name}/activity?hours=6            その人の最近のブロックの変更・コマンド・発言
//	GET  /v1/chat?minutes=30&player=NAME                最近のチャット
//	POST /v1/screenshot {"x","y","z","distance",...}   BlueMap の画面の撮影を始める (202 と job)
//	GET  /v1/screenshots/{job}                          撮れた画像 (PNG)。まだなら 202
//	GET  /v1/discord/tools?query=                       使える Discord のツール
//	POST /v1/discord/call {"tool","pathParams","body"}  Discord のツールを呼ぶ (許可したものだけ)
//
// 認証は OAuth 2.0 の Bearer トークン (利用者のもの。共有の秘密は使わない)。auth の introspection (/introspect) に尋ね、
// いまの Discord のロールが ADMIN_ROLE_IDS (Owner と Admin) のどれかの人だけを通す (authz.go)。内部のネットワークだけに出す。
// 操作は、トークンの持ち主 (Discord の ID と名前) つきで、DataDir/audit.jsonl に残す。

const maxBody = 1 << 20

type Connector struct {
	g     *app.Guard
	authz *Authz
	allow *regexp.Regexp
	base  context.Context // 裏で動く操作 (ロールバック) の寿命
	log   *slog.Logger
	audit string

	shotMu sync.Mutex
	shots  map[string]*shotJob // job の ID → 撮影の状態
}

func NewConnector(base context.Context, g *app.Guard) (*Connector, error) {
	re, err := regexp.Compile(g.Config().ConnectorConfig.DiscordAllow)
	if err != nil {
		return nil, fmt.Errorf("CONNECTOR_DISCORD_ALLOW: %w", err)
	}
	return &Connector{g: g, authz: NewAuthz(g.Config()), allow: re, base: base, log: g.Logger(), audit: filepath.Join(g.Config().StorageConfig.DataDir, "audit.jsonl"), shots: map[string]*shotJob{}}, nil
}

// Handler はコネクタの HTTP API の全部の経路。
func (c *Connector) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", c.wrap(c.status))
	mux.HandleFunc("GET /v1/incidents", c.wrap(c.incidents))
	mux.HandleFunc("GET /v1/incidents/{id}", c.wrap(c.incident))
	mux.HandleFunc("GET /v1/incidents/{id}/images/{name}", c.image)
	mux.HandleFunc("POST /v1/incidents/{id}/report", c.wrap(c.report))
	mux.HandleFunc("POST /v1/incidents/{id}/rollback", c.wrap(c.rollback))
	mux.HandleFunc("POST /v1/incidents/{id}/dismiss", c.wrap(c.dismiss))
	mux.HandleFunc("POST /v1/players/{name}/mute", c.wrap(c.mute))
	mux.HandleFunc("POST /v1/players/{name}/kick", c.wrap(c.kick))
	mux.HandleFunc("GET /v1/players/{name}/activity", c.wrap(c.activity))
	mux.HandleFunc("GET /v1/chat", c.wrap(c.chat))
	mux.HandleFunc("POST /v1/screenshot", c.wrap(c.screenshot))
	mux.HandleFunc("GET /v1/screenshots/{job}", c.screenshotResult)
	mux.HandleFunc("GET /v1/discord/tools", c.wrap(c.discordTools))
	mux.HandleFunc("POST /v1/discord/call", c.wrap(c.discordCall))
	return c.authz.Middleware(mux)
}

// Serve は addr でコネクタを開き、ctx が終わるまで受ける。
func (c *Connector) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr: addr, Handler: c.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	c.log.Info("コネクタを開いた", "addr", addr, "mode", c.g.Config().Mode)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// result は handler の返すもの。code が 0 なら 200。
type result struct {
	code int
	body any
}

// who は、トークンの持ち主 (「名前 (id Discord の ID)」)。記録と、Discord の監査ログの理由に使う。
func (c *Connector) who(r *http.Request) string {
	if p := principalOf(r); p != nil {
		return p.String()
	}
	return "unknown"
}

// auditKey は、audit.jsonl の 1 行に足す項目 (map) を、リクエストに持たせる鍵。
type auditKey struct{}

// auditAdd は、audit.jsonl の 1 行に項目を足す。
func auditAdd(r *http.Request, key string, value any) {
	if m, ok := r.Context().Value(auditKey{}).(map[string]any); ok {
		m[key] = value
	}
}

// wrap は handler の返したエラーを、状態コードに直し、書き換える操作を audit.jsonl に残す。
func (c *Connector) wrap(h func(r *http.Request) (*result, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		extra := map[string]any{}
		r = r.WithContext(context.WithValue(r.Context(), auditKey{}, extra))
		res, err := h(r)
		code := http.StatusOK
		switch {
		case err == nil:
			if res.code != 0 {
				code = res.code
			}
		case errors.Is(err, store.ErrNoRecord):
			code = http.StatusNotFound
		case errors.Is(err, domain.ErrForbidden):
			code = http.StatusForbidden
		case errors.Is(err, domain.ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, domain.ErrUnavailable):
			code = http.StatusServiceUnavailable
		case errors.Is(err, errBadRequest):
			code = http.StatusBadRequest
		default:
			code = http.StatusInternalServerError
			c.log.Error("コネクタの処理に失敗", "path", r.URL.Path, "err", err)
		}
		if r.Method != http.MethodGet {
			c.record(r, code, err, extra)
		}
		if err != nil {
			writeJSONResp(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSONResp(w, code, res.body)
	}
}

var errBadRequest = errors.New("bad request")

func badRequest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, a...))
}

func (c *Connector) record(r *http.Request, code int, err error, extra map[string]any) {
	line := map[string]any{"t": time.Now().Unix(), "user": c.who(r), "method": r.Method, "path": r.URL.Path, "status": code}
	maps.Copy(line, extra)
	if err != nil {
		line["error"] = domain.OneLine(err.Error(), 200)
	}
	b, _ := json.Marshal(line)
	f, ferr := os.OpenFile(c.audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if ferr != nil {
		c.log.Warn("audit.jsonl に書けない", "err", ferr)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return badRequest("body is not JSON: %v", err)
	}
	return nil
}

// ---- 読むだけ ----
func (c *Connector) status(r *http.Request) (*result, error) {
	g := c.g
	pending, _ := g.Records(domain.StatePending, 0)
	active := g.ActiveIncidents()
	return &result{body: map[string]any{
		"mode": g.Config().Mode, "version": Version, "now": time.Now().Unix(),
		"pending": len(pending), "open_incidents": active,
		"can_act": config.CapabilitiesFor(g.Config().Mode).AcceptOps, "can_post": g.Reporter() != nil,
		"can_rollback": g.Ops() != nil && config.CapabilitiesFor(g.Config().Mode).AcceptOps, "can_mute": strings.TrimSpace(g.Config().ChatConfig.ChatMuteCmd) != "",
		"can_screenshot": g.Shooter() != nil, "ai": g.AI() != nil,
	}}, nil
}

func summary(r *domain.Record) map[string]any {
	return map[string]any{
		"id": r.ID, "kind": r.Kind, "name": r.Name, "created": r.Created, "trigger": r.Trigger, "state": r.State,
		"busy": r.Busy, "reported": r.Reported, "rolled_back": r.RolledBack, "muted": r.Muted,
		"breaks": r.Breaks, "places": r.Places, "messages": r.Messages, "images": r.Images,
	}
}

func (c *Connector) incidents(r *http.Request) (*result, error) {
	state := domain.IncidentState(r.URL.Query().Get("state"))
	switch state {
	case "", domain.StatePending, domain.StateReported, domain.StateDismissed:
	case "all":
		state = ""
	default:
		return nil, badRequest("state は pending / reported / dismissed / all")
	}
	if r.URL.Query().Get("state") == "" {
		state = domain.StatePending
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, badRequest("limit は 1〜100")
		}
		limit = n
	}
	recs, err := c.g.Records(state, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, summary(rec))
	}
	return &result{body: map[string]any{"incidents": out}}, nil
}

func (c *Connector) incident(r *http.Request) (*result, error) {
	rec, err := c.g.Record(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	report := ""
	if b, err := os.ReadFile(filepath.Join(c.g.ReportDir(rec.ID), "report.md")); err == nil {
		report = string(b)
	}
	return &result{body: map[string]any{"incident": rec, "report": report}}, nil
}

func (c *Connector) image(w http.ResponseWriter, r *http.Request) {
	rec, err := c.g.Record(r.PathValue("id"))
	if err != nil {
		writeJSONResp(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	name := r.PathValue("name")
	for _, n := range rec.Images { // 記録にある画像だけ (パスをそのまま開かない)
		if n == name {
			b, err := os.ReadFile(filepath.Join(c.g.ReportDir(rec.ID), filepath.Base(n)))
			if err != nil {
				break
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(b)
			return
		}
	}
	writeJSONResp(w, http.StatusNotFound, map[string]any{"error": "image not found"})
}

func (c *Connector) activity(r *http.Request) (*result, error) {
	name := r.PathValue("name")
	if !domain.ValidName(name) {
		return nil, badRequest("名前が正しくありません")
	}
	hours := 6.0
	if v := r.URL.Query().Get("hours"); v != "" {
		h, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(h) || h <= 0 || h > 72 {
			return nil, badRequest("hours は 0 より大きく 72 まで")
		}
		hours = h
	}
	g := c.g
	u, err := g.CP().UserByName(r.Context(), name)
	if err != nil {
		return nil, fmt.Errorf("%w: プレイヤー %s の記録がありません", store.ErrNoRecord, name)
	}
	to := time.Now().Unix()
	from := to - int64(hours*3600)
	body := map[string]any{"player": u.Name, "uuid": u.UUID, "hours": hours, "protected": g.Protected(u.Name)}
	stats, err := g.CP().Stats(r.Context(), u, from, to)
	if err != nil {
		return nil, err
	}
	if !stats.Empty() {
		body["blocks"] = detect.SummaryOf(stats)
	}
	msgs, err := g.CP().ChatFor(r.Context(), u.ID, from, to)
	if err != nil {
		return nil, err
	}
	lines := []map[string]any{}
	for _, m := range lastN(msgs, 40) {
		lines = append(lines, map[string]any{"t": m.Time, "msg": domain.SafeChat(m.Msg, 200)})
	}
	body["chat_count"], body["chat"] = len(msgs), lines
	recs, _ := g.Records("", 0)
	ids := []string{}
	for _, rec := range recs {
		if strings.EqualFold(rec.Name, u.Name) {
			ids = append(ids, rec.ID)
		}
	}
	body["incidents"] = ids
	return &result{body: body}, nil
}

func lastN[T any](a []T, n int) []T {
	if len(a) > n {
		return a[len(a)-n:]
	}
	return a
}

func (c *Connector) chat(r *http.Request) (*result, error) {
	minutes := 30
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1440 {
			return nil, badRequest("minutes は 1〜1440")
		}
		minutes = n
	}
	g := c.g
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
	var uid int
	if p := r.URL.Query().Get("player"); p != "" {
		if !domain.ValidName(p) {
			return nil, badRequest("名前が正しくありません")
		}
		u, err := g.CP().UserByName(r.Context(), p)
		if err != nil {
			return nil, fmt.Errorf("%w: プレイヤー %s の記録がありません", store.ErrNoRecord, p)
		}
		uid = u.ID
	}
	msgs, err := g.CP().ChatSince(r.Context(), since, uid, 200)
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		n, ok := names[m.User]
		if !ok {
			if u, err := g.CP().User(r.Context(), m.User); err == nil {
				n = u.Name
			}
			names[m.User] = n
		}
		out = append(out, map[string]any{"t": m.Time, "player": n, "msg": domain.SafeChat(m.Msg, 200)})
	}
	return &result{body: map[string]any{"minutes": minutes, "messages": out}}, nil
}

// ---- 書き換える操作 ----
func (c *Connector) report(r *http.Request) (*result, error) {
	var in struct{ Note string }
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	rec, err := c.g.ReportIncident(r.Context(), r.PathValue("id"), c.who(r), in.Note)
	if err != nil {
		return nil, err
	}
	return &result{body: map[string]any{"incident": summary(rec), "thread": rec.Thread}}, nil
}

func (c *Connector) rollback(r *http.Request) (*result, error) {
	rec, err := c.g.StartRollback(c.base, r.PathValue("id"), c.who(r))
	if err != nil {
		return nil, err
	}
	return &result{code: http.StatusAccepted, body: map[string]any{
		"incident": summary(rec), "message": "ロールバックを始めました。数十秒〜数分かかります。終わったかは、事件の記録 (busy が空で rolled_back が true) で確かめてください",
	}}, nil
}

func (c *Connector) dismiss(r *http.Request) (*result, error) {
	var in struct{ Reason string }
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	if err := c.g.CheckMode(); err != nil {
		return nil, err
	}
	rec, err := c.g.Dismiss(r.PathValue("id"), c.who(r), in.Reason)
	if err != nil {
		return nil, err
	}
	return &result{body: map[string]any{"incident": summary(rec)}}, nil
}

func (c *Connector) mute(r *http.Request) (*result, error) {
	var in struct{ Reason string }
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	if err := c.g.Mute(r.Context(), r.PathValue("name"), c.who(r), in.Reason); err != nil {
		return nil, err
	}
	return &result{body: map[string]any{"ok": true}}, nil
}

func (c *Connector) kick(r *http.Request) (*result, error) {
	var in struct{ Reason string }
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	if err := c.g.Kick(r.Context(), r.PathValue("name"), c.who(r), in.Reason); err != nil {
		return nil, err
	}
	return &result{body: map[string]any{"ok": true}}, nil
}

// ShotJobState は、撮影ジョブの状態。事件の状態 (IncidentState) とは別の概念なので、型も分ける。
// HTTP の応答には、この文字列のまま出す。
type ShotJobState string

const (
	ShotJobPending ShotJobState = "pending" // 撮影中
	ShotJobDone    ShotJobState = "done"    // 撮影できた
	ShotJobFailed  ShotJobState = "failed"  // 撮影できなかった
)

// shotJob は、裏で撮っている BlueMap の画像 1 枚。
type shotJob struct {
	State ShotJobState
	Path  string
	Err   string
	At    time.Time
}

// screenshot は、BlueMap のいまの地図 (更新は最大 5 分遅れる) の撮影を裏で始めて、すぐ job の ID を返す (1 分ほどかかるので)。
// できた画像は GET /v1/screenshots/{job} で取る。
func (c *Connector) screenshot(r *http.Request) (*result, error) {
	var in struct {
		World    string   `json:"world"`
		X        *float64 `json:"x"`
		Y        *float64 `json:"y"`
		Z        *float64 `json:"z"`
		Distance float64  `json:"distance"`
	}
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	g := c.g
	if g.Shooter() == nil {
		return nil, fmt.Errorf("%w: 画面を撮る設定ではありません", domain.ErrUnavailable)
	}
	if in.X == nil || in.Z == nil {
		return nil, badRequest("x と z が要ります")
	}
	world := in.World
	if world == "" {
		world = "main"
	}
	mapID := g.Config().ShotConfig.MapIDs[world]
	if mapID == "" {
		return nil, badRequest("world %q の地図が設定にありません", world)
	}
	y := 70.0
	if in.Y != nil {
		y = *in.Y
	}
	dist := in.Distance
	if dist == 0 {
		dist = 120
	}
	if dist < 20 || dist > 600 {
		return nil, badRequest("distance は 20〜600")
	}

	c.shotMu.Lock()
	for id, j := range c.shots { // 古いものは捨てる
		if time.Since(j.At) > time.Hour {
			if j.Path != "" {
				_ = os.Remove(j.Path)
			}
			delete(c.shots, id)
		}
	}
	pending := 0
	for _, j := range c.shots {
		if j.State == ShotJobPending {
			pending++
		}
	}
	if pending >= 3 {
		c.shotMu.Unlock()
		return nil, fmt.Errorf("%w: 撮影が詰まっています。終わってからやり直してください", domain.ErrConflict)
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	id := hex.EncodeToString(buf)
	job := &shotJob{State: ShotJobPending, At: time.Now()}
	c.shots[id] = job
	c.shotMu.Unlock()

	cam := capture.Cam{X: *in.X, Y: y, Z: *in.Z, Dist: dist, Rot: -0.6, Angle: 0.9}
	go func() {
		g.LockOp()
		defer g.UnlockOp()
		ctx, cancel := context.WithTimeout(c.base, 3*time.Minute)
		defer cancel()
		dir := filepath.Join(g.Config().StorageConfig.DataDir, "shots")
		out := filepath.Join(dir, id+".png")
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			err = g.Shooter().Shot(ctx, mapID, cam, out, nil)
		}
		c.shotMu.Lock()
		defer c.shotMu.Unlock()
		if err != nil {
			job.State, job.Err = ShotJobFailed, err.Error()
			return
		}
		job.State, job.Path = ShotJobDone, out
	}()
	return &result{code: http.StatusAccepted, body: map[string]any{
		"job": id, "status": string(ShotJobPending), "message": "撮影を始めました。1 分ほどで、この job の画像を取れます",
	}}, nil
}

// screenshotResult は、撮影の結果。できていれば PNG、まだなら 202、失敗なら 500。
func (c *Connector) screenshotResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job")
	c.shotMu.Lock()
	job, ok := c.shots[id]
	var snap shotJob
	if ok {
		snap = *job
	}
	c.shotMu.Unlock()
	switch {
	case !ok:
		writeJSONResp(w, http.StatusNotFound, map[string]any{"error": "job not found (もう捨てたか、番号が違います)"})
	case snap.State == ShotJobPending:
		writeJSONResp(w, http.StatusAccepted, map[string]any{"job": id, "status": string(ShotJobPending)})
	case snap.State == ShotJobFailed:
		writeJSONResp(w, http.StatusInternalServerError, map[string]any{"error": snap.Err})
	default:
		b, err := os.ReadFile(snap.Path)
		if err != nil {
			writeJSONResp(w, http.StatusNotFound, map[string]any{"error": "画像がありません"})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(b)
	}
}

// ---- Discord ----
const maxDiscordReply = 60000

func (c *Connector) discordTools(r *http.Request) (*result, error) {
	if c.g.Reporter() == nil {
		return nil, fmt.Errorf("%w: Discord につなぐ設定がありません", domain.ErrUnavailable)
	}
	q := r.URL.Query().Get("query")
	out, err := c.g.Reporter().MCP().Tool(r.Context(), "discord_search_tools", map[string]any{"query": q})
	if err != nil {
		return nil, err
	}
	var found struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		return nil, fmt.Errorf("discord_search_tools の返事を読めません: %s", domain.OneLine(out, 120))
	}
	allowed := []map[string]any{}
	for _, t := range found.Tools {
		if name, _ := t["toolName"].(string); c.allow.MatchString(name) {
			allowed = append(allowed, t)
		}
	}
	return &result{body: map[string]any{"tools": allowed, "note": "許可したツールだけを出しています"}}, nil
}

func (c *Connector) discordCall(r *http.Request) (*result, error) {
	var in struct {
		Tool       string         `json:"tool"`
		PathParams map[string]any `json:"pathParams"`
		Body       map[string]any `json:"body"`
		Query      map[string]any `json:"queryParams"`
		Fields     string         `json:"fields"`
		Reason     string         `json:"reason"`
	}
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	auditAdd(r, "tool", domain.OneLine(in.Tool, 80))
	if !c.allow.MatchString(in.Tool) {
		return nil, fmt.Errorf("%w: ツール %q は許可していません (許可: discord_tools で確かめてください)", domain.ErrForbidden, in.Tool)
	}
	if c.g.Reporter() == nil {
		return nil, fmt.Errorf("%w: Discord につなぐ設定がありません", domain.ErrUnavailable)
	}
	if err := c.g.CheckMode(); err != nil {
		return nil, err
	}
	if id, ok := in.PathParams["channel_id"].(string); ok {
		auditAdd(r, "channel", domain.OneLine(id, 32))
	}
	if err := limitDiscordBody(in.Tool, in.Body); err != nil {
		return nil, err
	}
	if err := c.checkDiscordCall(r.Context(), in.Tool, in.PathParams); err != nil {
		return nil, err
	}
	if b, err := json.Marshal(in.Body); err == nil && in.Body != nil {
		sum := sha256.Sum256(b)
		auditAdd(r, "body_sha256", hex.EncodeToString(sum[:]))
	}
	out, err := c.g.Reporter().MCP().Discord(r.Context(), notify.DiscordCall{
		Tool: in.Tool, PathParams: in.PathParams, Body: in.Body, Query: in.Query, Fields: in.Fields, Auth: c.g.Reporter().Auth(),
		// 理由は 200 文字で切れるので、頼んだ人の名前を先に書く
		Reason: domain.CleanText(c.who(r)+": "+in.Reason, 200),
	})
	if err != nil {
		return nil, err
	}
	if len(out) > maxDiscordReply {
		out = out[:maxDiscordReply] + "\n…(長いので切りました)"
	}
	return &result{body: map[string]any{"result": out}}, nil
}

var (
	snowflakeRE = regexp.MustCompile(`^[0-9]{17,20}$`)
	pathParamRE = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

// discordGuildTools は、チャンネルを指定しないツール。ほかのツールは、channel_id が要る。
var discordGuildTools = map[string]bool{
	"discord_get_my_user": true, "discord_list_guild_channels": true, "discord_list_guild_roles": true,
	"discord_get_guild_member": true, "discord_search_guild_members": true, "discord_list_active_guild_threads": true,
}

// checkDiscordCall は、パスの値が ID の形か、チャンネルが通報フォーラムかそのスレッドかを確かめる。
func (c *Connector) checkDiscordCall(ctx context.Context, tool string, params map[string]any) error {
	for k, v := range params {
		id, ok := v.(string)
		if !pathParamRE.MatchString(k) || !ok || !snowflakeRE.MatchString(id) {
			return badRequest("pathParams の %q は Discord の ID にしてください", domain.OneLine(k, 32))
		}
	}
	if discordGuildTools[tool] {
		return nil
	}
	id, _ := params["channel_id"].(string)
	if id == "" {
		return badRequest("pathParams.channel_id が要ります")
	}
	rep := c.g.Reporter()
	if id == rep.ChannelID() {
		return nil
	}
	notAllowed := fmt.Errorf("%w: 通報フォーラムとそのスレッドのほかは、触れません", domain.ErrForbidden)
	if tool == "discord_create_thread" {
		return notAllowed
	}
	out, err := rep.MCP().Discord(ctx, notify.DiscordCall{Tool: "discord_get_channel", PathParams: map[string]any{"channel_id": id}, Fields: "id,parent_id", Auth: rep.Auth()})
	if err != nil {
		return notAllowed // Discord が見せないチャンネルは、確かめられないので断る
	}
	var ch struct {
		ParentID string `json:"parent_id"`
	}
	if err := json.Unmarshal([]byte(out), &ch); err != nil || ch.ParentID != rep.ChannelID() {
		return notAllowed
	}
	return nil
}

// limitDiscordBody は、本文の決まりを当てはめる。だれにも通知しない設定は、呼ぶ側に選ばせない。
// update_channel は、タグだけを変えられる (名前や権限などは変えさせない)。
func limitDiscordBody(tool string, body map[string]any) error {
	switch tool {
	case "discord_update_channel":
		tags, ok := body["applied_tags"].([]any)
		if !ok || len(body) != 1 {
			return fmt.Errorf("%w: update_channel で変えられるのは applied_tags だけです", domain.ErrForbidden)
		}
		for _, t := range tags {
			if id, ok := t.(string); !ok || !snowflakeRE.MatchString(id) {
				return badRequest("applied_tags は Discord の ID の配列にしてください")
			}
		}
	case "discord_create_message":
		if body == nil {
			return badRequest("body が要ります")
		}
		body["allowed_mentions"] = notify.NoMentions()
	case "discord_create_thread":
		msg, ok := body["message"].(map[string]any)
		if !ok {
			return badRequest("body.message が要ります")
		}
		msg["allowed_mentions"] = notify.NoMentions()
	}
	return nil
}

// Version は、このバイナリの版。cmd が起動時に自分の版を設定する。
var Version = "dev"
