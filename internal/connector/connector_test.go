package connector

import (
	"bytes"
	"context"
	"crime-buster/internal/app"
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
	"crime-buster/internal/relay"
	"crime-buster/internal/store"
	"crime-buster/internal/testutil"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 利用者のトークン (OAuth 2.0 の Bearer)。偽の auth が、これを claims に引き換える。
const (
	tokAdmin      = "tok-admin"       // Admin ロール (いまの Discord で確かめた)。scope なし (本人が発行した 90 日トークン)
	tokMod        = "tok-mod"         // Admin ではないロール
	tokStale      = "tok-stale"       // Admin ロールだが、auth が Discord で確かめていない (トークンの値のまま)
	tokExpired    = "tok-expired"     // 無効
	tokOAuthMCP   = "tok-oauth-mcp"   // Admin ロールの OAuth のトークン。scope に mcp がある
	tokOAuthRead  = "tok-oauth-read"  // Admin ロールの OAuth のトークン。読み取りだけに同意した
	tokOAuthLike  = "tok-oauth-like"  // scope に mcp に似た言葉しかない
	tokOAuthEmpty = "tok-oauth-empty" // scope が空
	tokModMCP     = "tok-mod-mcp"     // scope に mcp があるが、Admin ではない
)

const adminRole = "222222222222222222"

// 通報フォーラムと、そのスレッド、外のチャンネル (試験用の ID)。
const (
	forumID   = "3333333333333333333"
	threadID  = "1000000000000000003"
	outsideID = "1000000000000000004"
)

// fakeAuth は auth の /introspect の代わり。
type fakeAuth struct {
	down  bool
	calls int
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	if f.down || r.URL.Path != "/introspect" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var in struct{ Token string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	claims := map[string]map[string]any{
		tokAdmin: {"id": "42", "username": "admin1", "roles": []string{"1", adminRole}, "roles_source": "live"},
		tokMod:   {"id": "43", "username": "mod1", "roles": []string{"100000000000000003"}, "roles_source": "live"},
		tokStale: {"id": "44", "username": "stale1", "roles": []string{adminRole}, "roles_source": "token"},

		tokOAuthMCP:   {"id": "45", "username": "oauth1", "roles": []string{adminRole}, "roles_source": "live", "scope": "read mcp"},
		tokOAuthRead:  {"id": "46", "username": "oauth2", "roles": []string{adminRole}, "roles_source": "live", "scope": "read"},
		tokOAuthLike:  {"id": "47", "username": "oauth3", "roles": []string{adminRole}, "roles_source": "live", "scope": "mcpx notmcp"},
		tokOAuthEmpty: {"id": "48", "username": "oauth4", "roles": []string{adminRole}, "roles_source": "live", "scope": ""},
		tokModMCP:     {"id": "49", "username": "oauth5", "roles": []string{"100000000000000003"}, "roles_source": "live", "scope": "mcp"},
	}[in.Token]
	w.Header().Set("Content-Type", "application/json")
	if claims == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "claims": claims})
}

type connEnv struct {
	auth *fakeAuth
	g    *app.Guard
	srv  *httptest.Server
	con  *testutil.FakeConsole
	mcp  *testutil.FakeMCP
	dir  string
}

func newConnEnv(t *testing.T, mode config.Mode) *connEnv {
	return newConnEnvCP(t, mode, nil)
}

func newConnEnvCP(t *testing.T, mode config.Mode, cp *detect.CP) *connEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Mode:            mode,
		StorageConfig:   config.StorageConfig{DataDir: dir},
		ConnectorConfig: config.ConnectorConfig{DiscordAllow: config.DefaultDiscordAllow, AdminRoles: []string{"111111111111111111", adminRole}, RequireLiveRoles: true},
		ChatConfig:      config.ChatConfig{ChatMuteCmd: "mute {name} 10m spam"},
		DetectConfig:    config.DetectConfig{TrustedPlayers: map[string]bool{"trusty": true}},
		ShotConfig:      config.ShotConfig{MapIDs: map[string]string{"main": "overworld"}},
	}
	st, err := store.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	con := &testutil.FakeConsole{}
	fm := &testutil.FakeMCP{Reply: func(tool string, args map[string]any) (string, bool) {
		if tool == "discord_search_tools" {
			return `{"tools":[{"toolName":"discord_get_channel"},{"toolName":"discord_delete_channel"},{"toolName":"discord_create_message"}]}`, false
		}
		if tool == "discord_get_channel" {
			if pp, _ := args["pathParams"].(map[string]any); pp["channel_id"] == threadID {
				return `{"id":"` + threadID + `","parent_id":"` + forumID + `"}`, false
			}
			return `{"id":"` + outsideID + `","parent_id":"999"}`, false
		}
		return `{"id":"1"}`, false
	}}
	mcpSrv := httptest.NewServer(fm)
	t.Cleanup(mcpSrv.Close)
	g := app.NewGuard(cfg, cp, st, &relay.Ops{C: con}, nil, notify.NewReporter(notify.NewMCP(mcpSrv.URL, "2025-11-25", true), "TOKEN", forumID, "55"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	fa := &fakeAuth{}
	authSrv := httptest.NewServer(fa)
	t.Cleanup(authSrv.Close)
	cfg.AuthInternalURL = authSrv.URL
	conn, err := NewConnector(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(conn.Handler())
	t.Cleanup(srv.Close)
	return &connEnv{auth: fa, g: g, srv: srv, con: con, mcp: fm, dir: dir}
}

func (e *connEnv) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokAdmin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *connEnv) putRecord(t *testing.T, r *domain.Record) {
	t.Helper()
	dir := filepath.Join(e.dir, "reports", r.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "report.md"), []byte("**ワールド**: main"), 0o644)
	for _, n := range r.Images {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("PNGDATA"), 0o644)
	}
}

func TestConnectorAuthenticatesWithOAuthTokens(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	get := func(auth string) (int, string, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/status", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode, res.Header.Get("WWW-Authenticate"), res.Header.Get("Retry-After")
	}
	// トークンが無い・無効 → 401 (再認可を促す)
	for _, auth := range []string{"", "Basic abc", "Bearer ", "Bearer " + tokExpired, "Bearer nonsense", tokAdmin} {
		code, challenge, _ := get(auth)
		if code != 401 || !strings.Contains(challenge, "invalid_token") {
			t.Errorf("auth %q: %d %q, want 401 + challenge", auth, code, challenge)
		}
	}
	// Admin ではない人 → 403
	if code, _, _ := get("Bearer " + tokMod); code != 403 {
		t.Errorf("non-admin: %d, want 403", code)
	}
	// auth が Discord で確かめていないロールは、信じない (古いトークンの値) → 503
	if code, _, retry := get("Bearer " + tokStale); code != 503 || retry == "" {
		t.Errorf("stale roles: %d retry=%q, want 503", code, retry)
	}
	// Admin → 200
	if code, _, _ := get("Bearer " + tokAdmin); code != 200 {
		t.Errorf("admin: %d, want 200", code)
	}
	// auth が落ちている → 503 (401 にして、再認可させない)
	e.auth.down = true
	if code, _, _ := get("Bearer " + tokAdmin); code != 503 {
		t.Errorf("auth down: %d, want 503", code)
	}
}

// OAuth のトークンの scope: 無ければ通し、あれば mcp という言葉が要る。
func TestConnectorScopeRule(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	for tok, want := range map[string]int{
		tokAdmin:      200, // scope なし (本人が発行した 90 日トークン)
		tokOAuthMCP:   200,
		tokOAuthRead:  403,
		tokOAuthLike:  403,
		tokOAuthEmpty: 403,
		tokModMCP:     403, // scope があっても、ロールは要る
	} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/status", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", tok, res.StatusCode, want)
		}
	}
	empty, some := "", "read mcp write"
	if scopeAllows(&empty) || !scopeAllows(&some) || !scopeAllows(nil) {
		t.Fatal("scopeAllows")
	}
}

// auth の答えの scope が文字列でないときは、読めないものとして断る。
func TestConnectorScopeWrongTypeFailsClosed(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active":true,"claims":{"id":"1","roles":["` + adminRole + `"],"roles_source":"live","scope":["mcp"]}}`))
	}))
	defer auth.Close()
	a := NewAuthz(&config.Config{ConnectorConfig: config.ConnectorConfig{AuthInternalURL: auth.URL, AdminRoles: []string{adminRole}, RequireLiveRoles: true}})
	if p, res, _ := a.Check(context.Background(), "tok"); p != nil || res != authUnavailable {
		t.Fatalf("%v %v", p, res)
	}
}

func TestConnectorStaleRolesAllowedWhenNotRequired(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	e.g.Config().RequireLiveRoles = false
	e.srv.Close()
	conn, _ := NewConnector(context.Background(), e.g)
	srv := httptest.NewServer(conn.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+tokStale)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d, want 200 when CONNECTOR_REQUIRE_LIVE_ROLES=false", res.StatusCode)
	}
}

func TestConnectorIncidentsListGetImage(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	now := time.Now().Unix()
	e.putRecord(t, &domain.Record{ID: "a-old", Kind: "block", Name: "old", Created: now - 100, State: domain.StatePending, Images: []string{"damage.png"}})
	e.putRecord(t, &domain.Record{ID: "b-new", Kind: "chat", Name: "new", Created: now, State: domain.StatePending})
	e.putRecord(t, &domain.Record{ID: "c-done", Kind: "block", Name: "done", Created: now - 50, State: domain.StateReported})

	code, b := e.do(t, "GET", "/v1/incidents", "")
	var list struct{ Incidents []map[string]any }
	_ = json.Unmarshal(b, &list)
	if code != 200 || len(list.Incidents) != 2 || list.Incidents[0]["id"] != "b-new" {
		t.Fatalf("pending list: %d %s", code, b)
	}
	_, b = e.do(t, "GET", "/v1/incidents?state=all", "")
	_ = json.Unmarshal(b, &list)
	if len(list.Incidents) != 3 {
		t.Fatalf("all list: %s", b)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents?state=bogus", ""); code != 400 {
		t.Errorf("bad state: %d", code)
	}

	code, b = e.do(t, "GET", "/v1/incidents/a-old", "")
	if code != 200 || !bytes.Contains(b, []byte("ワールド")) {
		t.Fatalf("get: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents/nope", ""); code != 404 {
		t.Errorf("missing incident: %d", code)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents/..%2F..%2Fetc", ""); code != 404 {
		t.Errorf("traversal id: %d", code)
	}

	if code, b := e.do(t, "GET", "/v1/incidents/a-old/images/damage.png", ""); code != 200 || string(b) != "PNGDATA" {
		t.Errorf("image: %d %q", code, b)
	}
	// 記録にないファイルは、同じフォルダにあっても返さない
	for _, name := range []string{"record.json", "report.md", "..%2Frecord.json"} {
		if code, _ := e.do(t, "GET", "/v1/incidents/a-old/images/"+name, ""); code != 404 {
			t.Errorf("image %s: %d, want 404", name, code)
		}
	}
}

func TestConnectorDismissNeedsReviewMode(t *testing.T) {
	e := newConnEnv(t, config.ModeDryRun)
	e.putRecord(t, &domain.Record{ID: "x1", Kind: "block", Name: "x", Created: 1, State: domain.StatePending})
	if code, _ := e.do(t, "POST", "/v1/incidents/x1/dismiss", `{"reason":"工事"}`); code != 403 {
		t.Fatalf("dry-run dismiss: %d, want 403", code)
	}

	e = newConnEnv(t, config.ModeReview)
	e.putRecord(t, &domain.Record{ID: "x1", Kind: "block", Name: "x", Created: 1, State: domain.StatePending})
	code, b := e.do(t, "POST", "/v1/incidents/x1/dismiss", "{\"reason\":\"工事\\nの人\"}")
	if code != 200 {
		t.Fatalf("dismiss: %d %s", code, b)
	}
	rec, err := e.g.Record("x1")
	if err != nil || rec.State != domain.StateDismissed || len(rec.History) != 1 || rec.History[0].By != "admin1 (id 42)" || strings.Contains(rec.History[0].Note, "\n") {
		t.Fatalf("record after dismiss: %+v err=%v", rec, err)
	}
	// 却下した事件は、ロールバックも通報もできない
	if code, _ := e.do(t, "POST", "/v1/incidents/x1/rollback", ""); code != 409 {
		t.Errorf("rollback after dismiss: %d, want 409", code)
	}
	if code, _ := e.do(t, "POST", "/v1/incidents/x1/report", `{"note":"x"}`); code != 409 {
		t.Errorf("report after dismiss: %d, want 409", code)
	}
	rec, err = e.g.Record("x1")
	if err != nil || rec.State != domain.StateDismissed || rec.Reported {
		t.Fatalf("却下の状態が上書きされた: %+v err=%v", rec, err)
	}
	// 操作は audit.jsonl に残る
	b, err = os.ReadFile(filepath.Join(e.dir, "audit.jsonl"))
	if err != nil || !bytes.Contains(b, []byte(`"path":"/v1/incidents/x1/dismiss"`)) || !bytes.Contains(b, []byte("admin1")) {
		t.Fatalf("audit: %v %s", err, b)
	}
}

func TestConnectorRollbackRefusals(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	now := time.Now().Unix()
	e.putRecord(t, &domain.Record{ID: "t1", Kind: "block", Name: "trusty", World: "main", Created: 1, Start: now, State: domain.StatePending})
	e.putRecord(t, &domain.Record{ID: "c1", Kind: "chat", Name: "talker", Created: 1, Start: now, State: domain.StatePending})
	e.putRecord(t, &domain.Record{ID: "d1", Kind: "block", Name: "done", World: "main", Created: 1, Start: now, State: domain.StatePending, RolledBack: true})
	e.putRecord(t, &domain.Record{ID: "n1", Kind: "block", Name: "bad name; stop", World: "main", Created: 1, Start: now, State: domain.StatePending})
	e.putRecord(t, &domain.Record{ID: "w1", Kind: "block", Name: "griefer", World: "bad world; stop", Created: 1, Start: now, State: domain.StatePending})
	e.putRecord(t, &domain.Record{ID: "o1", Kind: "block", Name: "griefer", World: "main", Created: 1, Start: now - domain.MaxRollbackAge - 60, State: domain.StatePending})
	for id, want := range map[string]int{"t1": 403, "c1": 403, "d1": 409, "n1": 403, "w1": 403, "o1": 409, "missing": 404} {
		if code, b := e.do(t, "POST", "/v1/incidents/"+id+"/rollback", ""); code != want {
			t.Errorf("rollback %s: %d %s, want %d", id, code, b, want)
		}
		// 断ったら、印が残らない
		if e.g.IsBusy(id) {
			t.Errorf("rollback %s: busy のまま", id)
		}
	}
	if len(e.con.Sent()) != 0 {
		t.Fatalf("console got commands: %v", e.con.Sent())
	}
}

// 同じ事件の操作は、重ならない。
func TestConnectorBusyIncidentConflicts(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	e.putRecord(t, &domain.Record{ID: "b1", Kind: "block", Name: "griefer", World: "main", Created: 1, Start: time.Now().Unix(), State: domain.StatePending})
	if err := e.g.Claim("b1", "rollback"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"rollback", "report", "dismiss"} {
		if code, b := e.do(t, "POST", "/v1/incidents/b1/"+op, ""); code != 409 {
			t.Errorf("%s: %d %s, want 409", op, code, b)
		}
	}
	e.g.Release("b1")
	if code, b := e.do(t, "POST", "/v1/incidents/b1/dismiss", ""); code != 200 {
		t.Errorf("dismiss: %d %s", code, b)
	}
}

// ロールバックは、コンソールの返事ではなく、データベースで確かめて記録する。
func TestConnectorRollbackIsVerifiedInDatabase(t *testing.T) {
	const line = `[12:35:53 INFO]: CoreProtect - Rollback completed for "griefer".`
	run := func(t *testing.T, con *testutil.FakeConsole, done, total int64) *domain.Record {
		t.Helper()
		sqldb, _ := testutil.NewFakeDB(t, testutil.FakeAnswer{Has: "rolled_back", Rows: [][]driver.Value{{done, total}}})
		e := newConnEnvCP(t, config.ModeReview, detect.NewCP(sqldb, "coreprotect_"))
		e.con.Reply = con.Reply
		e.con.Err = con.Err
		e.g.SetRollbackConfirmWait(time.Millisecond)
		e.putRecord(t, &domain.Record{ID: "r1", Kind: "block", Name: "griefer", World: "main", UserID: 7, Created: 1, Start: time.Now().Unix() - 60, End: 2, State: domain.StatePending})
		if code, b := e.do(t, "POST", "/v1/incidents/r1/rollback", ""); code != 202 {
			t.Fatalf("rollback: %d %s", code, b)
		}
		deadline := time.Now().Add(10 * time.Second)
		for e.g.IsBusy("r1") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		rec, err := e.g.Record("r1")
		if err != nil || rec.Busy != "" {
			t.Fatalf("%+v %v", rec, err)
		}
		if s := e.con.Sent(); len(s) != 1 || !strings.HasPrefix(s[0], "co rollback u:griefer t:") || !strings.HasSuffix(s[0], "s r:#main") {
			t.Fatalf("sent %q", s)
		}
		return rec
	}
	ok := &testutil.FakeConsole{Reply: map[string][]string{"co rollback": {line}}}

	// 両方よい
	rec := run(t, ok, 100, 100)
	if !rec.RolledBack || rec.History[0].Note != "100/100" {
		t.Fatalf("%+v", rec)
	}
	// コンソールが「完了」でも、データベースで戻っていなければ失敗
	rec = run(t, ok, 0, 100)
	if rec.RolledBack || !strings.HasPrefix(rec.History[0].Note, "失敗") {
		t.Fatalf("%+v", rec)
	}
	// 切断でも、データベースで戻っていれば成功。記録にコンソールの行は載らない
	rec = run(t, &testutil.FakeConsole{Err: io.ErrUnexpectedEOF}, 100, 100)
	if !rec.RolledBack || strings.Contains(rec.History[0].Note, "INFO") {
		t.Fatalf("%+v", rec)
	}
	// 切断で、データベースでも戻っていなければ失敗
	rec = run(t, &testutil.FakeConsole{Err: io.ErrUnexpectedEOF}, 0, 100)
	if rec.RolledBack {
		t.Fatalf("%+v", rec)
	}
}

func TestConnectorMuteAndKick(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	code, b := e.do(t, "POST", "/v1/players/spammer/mute", `{"reason":"連投"}`)
	if code != 200 {
		t.Fatalf("mute: %d %s", code, b)
	}
	// 成否だけを返す (コンソールの行を返さない)
	if strings.TrimSpace(string(b)) != `{"ok":true}` {
		t.Errorf("mute: %s", b)
	}
	code, b = e.do(t, "POST", "/v1/players/spammer/kick", "{\"reason\":\"荒らし\\nop everyone\"}")
	if code != 200 {
		t.Fatalf("kick: %d %s", code, b)
	}
	if strings.TrimSpace(string(b)) != `{"ok":true}` {
		t.Errorf("kick: %s", b)
	}
	if s := e.con.Sent(); len(s) != 2 || s[0] != "mute spammer 10m spam" || s[1] != "kick spammer 荒らし op everyone" {
		t.Fatalf("commands: %q", s)
	}
	e.con.Reset()
	for _, c := range []struct{ path string }{
		{"/v1/players/trusty/mute"}, {"/v1/players/trusty/kick"},
		{"/v1/players/a%20b/kick"}, {"/v1/players/x;op/mute"}, {"/v1/players/%0Aop/kick"},
	} {
		if code, _ := e.do(t, "POST", c.path, ""); code != 403 {
			t.Errorf("%s: %d, want 403", c.path, code)
		}
	}
	if s := e.con.Sent(); len(s) != 0 {
		t.Fatalf("unsafe commands were sent: %q", s)
	}

	e = newConnEnv(t, config.ModeObserve)
	if code, _ := e.do(t, "POST", "/v1/players/spammer/kick", ""); code != 403 {
		t.Errorf("observe kick: %d, want 403", code)
	}
}

// キックとミュートは、事件の処理 (g.opMu) を待たない。
func TestConnectorMuteAndKickDoNotWaitForOps(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	e.g.LockOp()
	defer e.g.UnlockOp()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if code, b := e.do(t, "POST", "/v1/players/spammer/kick", `{"reason":"x"}`); code != 200 {
			t.Errorf("kick: %d %s", code, b)
		}
		if code, b := e.do(t, "POST", "/v1/players/spammer/mute", `{"reason":"x"}`); code != 200 {
			t.Errorf("mute: %d %s", code, b)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("g.opMu を待っている")
	}
}

// コンソールの中継が断ったとき、コネクタは 403 や 503 を返す。
func TestConnectorKickRelayErrors(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	e.con.Err = fmt.Errorf("%w: busy", domain.ErrUnavailable)
	if code, _ := e.do(t, "POST", "/v1/players/spammer/kick", ""); code != 503 {
		t.Errorf("中継が使えない: %d", code)
	}
	e.con.Err = fmt.Errorf("%w: refused", domain.ErrForbidden)
	if code, _ := e.do(t, "POST", "/v1/players/spammer/mute", ""); code != 403 {
		t.Errorf("中継が断った: %d", code)
	}
}

// Dismiss も、ほかの操作と同じに、事件の ID の形を確かめる。
func TestConnectorDismissRejectsBadID(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	outside := filepath.Join(e.dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecord(outside, &domain.Record{ID: "outside", State: domain.StatePending}); err != nil {
		t.Fatal(err)
	}
	if code, b := e.do(t, "POST", "/v1/incidents/..%2Foutside/dismiss", `{"reason":"x"}`); code != 404 {
		t.Errorf("dismiss: %d %s, want 404", code, b)
	}
	rec, err := store.LoadRecord(outside)
	if err != nil || rec.State != domain.StatePending {
		t.Fatalf("外の記録が変わった: %+v %v", rec, err)
	}
}

func TestConnectorActivityValidatesHours(t *testing.T) {
	e := newConnEnv(t, config.ModeReview) // データベースが無いので、検査を通れば落ちる
	for _, h := range []string{"NaN", "nan", "Inf", "-Inf", "+Inf", "0", "-1", "73", "1e9", "x"} {
		if code, b := e.do(t, "GET", "/v1/players/griefer/activity?hours="+url.QueryEscape(h), ""); code != 400 {
			t.Errorf("hours=%s: %d %s, want 400", h, code, b)
		}
	}
}

func TestConnectorDiscordAllowlist(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	code, b := e.do(t, "GET", "/v1/discord/tools?query=channel", "")
	if code != 200 || bytes.Contains(b, []byte("delete_channel")) || !bytes.Contains(b, []byte("create_message")) {
		t.Fatalf("tools: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_delete_channel","pathParams":{"channel_id":"1"}}`); code != 403 {
		t.Errorf("delete: %d, want 403", code)
	}
	if code, _ := e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_ban_guild_member"}`); code != 403 {
		t.Errorf("ban: %d, want 403", code)
	}
	if len(e.mcp.Calls) != 1 { // 許可しないものは、Discord-MCP まで届かない (最初の 1 回は tools の検索)
		t.Fatalf("mcp calls: %v", e.mcp.Calls)
	}
	code, b = e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_create_message","pathParams":{"channel_id":"`+forumID+`"},"body":{"content":"hi","allowed_mentions":{"parse":["everyone"]}},"queryParams":{"limit":3},"reason":"test"}`)
	if code != 200 {
		t.Fatalf("create_message: %d %s", code, b)
	}
	args := e.mcp.Calls[1]["args"].(map[string]any)
	if args["toolName"] != "discord_create_message" || args["authOverride"] != "Bot TOKEN" || args["queryParams"] == nil {
		t.Fatalf("args: %v", args)
	}
	// 呼ぶ側が何を書いても、だれにも通知しない
	testutil.AssertNoMentions(t, args["body"].(map[string]any))
	// 頼んだ人の名前を先に書く (理由は 200 文字で切れる)
	if !strings.HasPrefix(args["auditLogReason"].(string), "admin1 (id 42): test") {
		t.Errorf("reason: %v", args["auditLogReason"])
	}
	long := `{"tool":"discord_create_message","pathParams":{"channel_id":"` + forumID + `"},"body":{"content":"x"},"reason":"` + strings.Repeat("r", 500) + `"}`
	if code, b := e.do(t, "POST", "/v1/discord/call", long); code != 200 {
		t.Fatalf("long reason: %d %s", code, b)
	}
	last := e.mcp.Calls[len(e.mcp.Calls)-1]["args"].(map[string]any)
	if r := last["auditLogReason"].(string); !strings.HasPrefix(r, "admin1 (id 42)") || len([]rune(r)) > 200 {
		t.Errorf("long reason: %q", r)
	}
}

// Discord のツールは、通報フォーラムとそのスレッドの中だけ。
func TestConnectorDiscordChannelScope(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	call := func(tool, pathParams, body string) int {
		t.Helper()
		code, _ := e.do(t, "POST", "/v1/discord/call", `{"tool":"`+tool+`","pathParams":`+pathParams+`,"body":`+body+`}`)
		return code
	}
	in := func(id string) string { return `{"channel_id":"` + id + `"}` }
	msg := `{"content":"hi"}`
	tags := `{"applied_tags":["4444444444444444444"]}`
	thread := `{"name":"x","message":{"content":"hi"}}`
	for _, c := range []struct {
		name, tool, params, body string
		want                     int
	}{
		{"フォーラムに書く", "discord_create_message", in(forumID), msg, 200},
		{"スレッドに書く", "discord_create_message", in(threadID), msg, 200},
		{"外に書く", "discord_create_message", in(outsideID), msg, 403},
		{"外を読む", "discord_list_channel_messages", in(outsideID), `null`, 403},
		{"スレッドを読む", "discord_list_channel_messages", in(threadID), `null`, 200},
		{"チャンネルの指定が無い", "discord_list_channel_messages", `{}`, `null`, 400},
		{"ID の形が違う", "discord_get_channel", in("5"), `null`, 400},
		{"ID に余計な文字", "discord_get_channel", in(forumID + "/../1"), `null`, 400},
		{"ID が文字列でない", "discord_get_channel", `{"channel_id":5}`, `null`, 400},
		{"ほかのパスの値の形が違う", "discord_get_message", `{"channel_id":"` + forumID + `","message_id":"x"}`, `null`, 400},
		{"パスの値の名前の形が違う", "discord_get_channel", `{"channel_id":"` + forumID + `","authOverride":"` + forumID + `"}`, `null`, 400},
		{"投稿を立てる", "discord_create_thread", in(forumID), thread, 200},
		{"スレッドの中に投稿は立てない", "discord_create_thread", in(threadID), thread, 403},
		{"投稿の本文が無い", "discord_create_thread", in(forumID), `{"name":"x"}`, 400},
		{"メッセージの本文が無い", "discord_create_message", in(forumID), `null`, 400},
		{"タグを変える", "discord_update_channel", in(threadID), tags, 200},
		{"名前も変える", "discord_update_channel", in(threadID), `{"name":"x","applied_tags":[]}`, 403},
		{"権限を変える", "discord_update_channel", in(threadID), `{"permission_overwrites":[]}`, 403},
		{"タグの形が違う", "discord_update_channel", in(threadID), `{"applied_tags":["x"]}`, 400},
		{"外のタグを変える", "discord_update_channel", in(outsideID), tags, 403},
		{"チャンネルを指定しないツール", "discord_get_my_user", `{}`, `null`, 200},
	} {
		if code := call(c.tool, c.params, c.body); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
}

func TestConnectorDiscordCallIsAudited(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	const secret = "投稿の本文そのもの"
	code, b := e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_create_message","pathParams":{"channel_id":"`+forumID+`"},"body":{"content":"`+secret+`"}}`)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	code, _ = e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_create_message","pathParams":{"channel_id":"`+outsideID+`"},"body":{"content":"x"}}`)
	if code != 403 {
		t.Fatalf("外: %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit: %s", raw)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["tool"] != "discord_create_message" || first["channel"] != forumID || len(first["body_sha256"].(string)) != 64 || first["status"] != float64(200) {
		t.Fatalf("audit: %v", first)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("本文そのものを残している: %s", raw)
	}
	// 断った呼び出しも、どのチャンネルかが残る
	if second["channel"] != outsideID || second["status"] != float64(403) {
		t.Fatalf("audit: %v", second)
	}
}

func TestConnectorStatus(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	e.putRecord(t, &domain.Record{ID: "p1", Kind: "block", Name: "p", Created: 1, State: domain.StatePending})
	code, b := e.do(t, "GET", "/v1/status", "")
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	if code != 200 || st["mode"] != "review" || st["pending"] != float64(1) || st["can_act"] != true || st["can_mute"] != true {
		t.Fatalf("status: %d %s", code, b)
	}
}

func TestConnectorScreenshotValidation(t *testing.T) {
	e := newConnEnv(t, config.ModeReview)
	if code, _ := e.do(t, "POST", "/v1/screenshot", `{"x":1,"z":2}`); code != 503 {
		t.Errorf("no shooter: %d, want 503", code)
	}
	e.g.SetShooter(&capture.Shooter{})
	for body, want := range map[string]int{
		`{"z":2}`:                        400,
		`{"x":1}`:                        400,
		`{"x":1,"z":2,"world":"nether"}`: 400,
		`{"x":1,"z":2,"distance":5000}`:  400,
		`{"x":1,"z":2,"distance":1}`:     400,
		`not json`:                       400,
	} {
		if code, b := e.do(t, "POST", "/v1/screenshot", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, b, want)
		}
	}
	if code, _ := e.do(t, "GET", "/v1/screenshots/0123456789abcdef", ""); code != 404 {
		t.Errorf("unknown job: %d, want 404", code)
	}
}
