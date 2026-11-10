package relay

import (
	"bufio"
	"context"
	"crime-buster/internal/domain"
	"crime-buster/internal/testutil"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsoleAllow(t *testing.T) {
	a, err := newConsoleAllow("mute {name} 10m spam")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("あ", MaxKickReason)
	for _, cmd := range []string{
		"save-all flush",
		"bluemap",
		"bluemap update overworld -5650 -10125 256",
		"bluemap update overworld 0 0 1",
		"co rollback u:fuuty119 t:415s r:#main",
		"co rollback u:a.b_c t:86405s r:#main_nether",
		"kick spammer 荒らし op everyone",
		"kick spammer " + long,
		"mute Steve_1 10m spam",
	} {
		if !a.Allowed(cmd) {
			t.Errorf("通るはず: %q", cmd)
		}
	}
	for _, cmd := range []string{
		"",
		"save-all",
		"save-all flush ",
		" bluemap",
		"bluemap reload",
		"bluemap update overworld 1 2 257",
		"bluemap update overworld 1 2 0",
		"bluemap update overworld 1 2 -3",
		"bluemap update over/world 1 2 3",
		"bluemap update overworld 1 2 x",
		"bluemap update overworld 123456789 2 3",
		"co restore u:x t:10s r:#main",
		"co rollback u:x t:10s",
		"co rollback u:x t:10s r:#main extra",
		"co rollback u:a;b t:10s r:#main",
		"co rollback u:x t:0s r:#main",
		"co rollback u:x t:999999s r:#main",
		"co rollback u:x t:10s r:#ma in",
		"co rollback u:x t:10s r:#main\rstop",
		"kick x",
		"kick x ",
		"kick a;b reason",
		"kick x line\nnext",
		"kick x line\rnext",
		"kick x line next",
		"kick x " + long + "あ",
		"mute Steve_1 10m spam extra",
		"mute a b 10m spam",
		"mute Steve_1 10m spamx",
		"mute Steve_1\n10m spam",
		"stop",
		"say hello",
		"op someone",
	} {
		if a.Allowed(cmd) {
			t.Errorf("断るはず: %q", cmd)
		}
	}
}

func TestConsoleAllowMuteTemplate(t *testing.T) {
	a, err := newConsoleAllow("")
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed("mute Steve 10m spam") {
		t.Fatal("CHAT_MUTE_CMD が空なら、ミュートは通さない")
	}
	for _, bad := range []string{"mute all", "mute {name}\nstop"} {
		if _, err := newConsoleAllow(bad); err == nil {
			t.Errorf("テンプレート %q は断るはず", bad)
		}
	}
	// {name} が 2 つなら、同じ名前のときだけ通す
	a, err = newConsoleAllow("tempmute {name} 5m ({name})")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Allowed("tempmute Steve 5m (Steve)") || a.Allowed("tempmute Steve 5m (Alex)") {
		t.Fatal("{name} の照合")
	}
}

// crime-buster が送るコマンドは、すべて中継の許可を通る。
func TestRelayAllowsWhatOpsSend(t *testing.T) {
	const muteTmpl = "mute {name} 10m spam"
	a, err := newConsoleAllow(muteTmpl)
	if err != nil {
		t.Fatal(err)
	}
	con := &testutil.FakeConsole{}
	ops := Ops{C: con}
	ctx := context.Background()
	_ = ops.SaveFlush(ctx)
	_ = ops.BlueMapUpdate(ctx, "overworld", -5650, -10125, domain.MaxBlueMapRadius)
	_, _ = ops.BlueMapIdle(ctx, "overworld")
	_ = ops.Rollback(ctx, "fuuty119", domain.MaxRollbackAge+5, "main")
	_ = ops.Kick(ctx, "fuuty119", "ブロックを不自然な速さで変えたため")
	_ = ops.Kick(ctx, "fuuty119", domain.CleanText("理由\nつき"+strings.Repeat("字", 200), MaxKickReason))
	_, _ = con.Run(ctx, MuteCommand(muteTmpl, "fuuty119"), time.Second, nil)
	sent := con.Sent()
	if len(sent) != 7 {
		t.Fatalf("commands: %q", sent)
	}
	for _, cmd := range sent {
		if !a.Allowed(cmd) {
			t.Errorf("中継に断られる: %q", cmd)
		}
	}
	// いちばん長く待つのはロールバック。中継の上限に収まる
	if con.Longest != 10*time.Minute || con.Longest > relayMaxTimeout {
		t.Errorf("待つ時間 %s, 上限 %s", con.Longest, relayMaxTimeout)
	}
}

// pipeConsole は attach の代わり。net.Pipe の片側を中継に渡し、もう片側で試験が話す。
type pipeConsole struct {
	conns   chan net.Conn
	err     error
	attachs atomic.Int32

	mu  sync.Mutex
	all []net.Conn
}

func newPipeConsole() *pipeConsole { return &pipeConsole{conns: make(chan net.Conn, 16)} }

func (p *pipeConsole) attach(context.Context) (net.Conn, io.Reader, error) {
	p.attachs.Add(1)
	if p.err != nil {
		return nil, nil, p.err
	}
	a, b := net.Pipe()
	p.mu.Lock()
	p.all = append(p.all, b)
	p.mu.Unlock()
	p.conns <- b
	return a, a, nil
}

// next は、次に attach された接続のコンソール側を返す。来なければ、閉じた接続 (読み書きはエラーになる)。
func (p *pipeConsole) next() net.Conn {
	select {
	case c := <-p.conns:
		return c
	case <-time.After(5 * time.Second):
		a, b := net.Pipe()
		a.Close()
		b.Close()
		return a
	}
}

func (p *pipeConsole) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.all {
		c.Close()
	}
}

// readCmd は、コンソール側で 1 行 (\r まで) 受ける。
func readCmd(c net.Conn) (string, error) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	s, err := bufio.NewReader(c).ReadString('\r')
	return s, err
}

func newRelayEnv(t *testing.T, muteTmpl string) (*pipeConsole, *httptest.Server) {
	t.Helper()
	a, err := newConsoleAllow(muteTmpl)
	if err != nil {
		t.Fatal(err)
	}
	p := newPipeConsole()
	srv := httptest.NewServer(NewRelay(p, a, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(func() {
		p.closeAll()
		srv.Close()
	})
	return p, srv
}

func TestRelayRefusals(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"許可していない":  {`{"cmd":"stop","timeout_ms":1000}`, http.StatusForbidden},
		"改行":       {`{"cmd":"bluemap\nstop","timeout_ms":1000}`, http.StatusForbidden},
		"JSON でない": {`bluemap`, http.StatusBadRequest},
		"余計な項目":    {`{"cmd":"bluemap","timeout_ms":1000,"container":"x"}`, http.StatusBadRequest},
		"続きがある":    {`{"cmd":"bluemap","timeout_ms":1000}{"cmd":"bluemap"}`, http.StatusBadRequest},
		"時間が無い":    {`{"cmd":"bluemap"}`, http.StatusBadRequest},
		"時間が 0":    {`{"cmd":"bluemap","timeout_ms":0}`, http.StatusBadRequest},
		"時間が負":     {`{"cmd":"bluemap","timeout_ms":-1}`, http.StatusBadRequest},
		"時間が長すぎる":  {fmt.Sprintf(`{"cmd":"bluemap","timeout_ms":%d}`, relayMaxTimeout.Milliseconds()+1), http.StatusBadRequest},
		"時間が大きすぎる": {`{"cmd":"bluemap","timeout_ms":9223372036854775807}`, http.StatusBadRequest},
		"本文が大きい":   {`{"cmd":"` + strings.Repeat("x", relayMaxBody) + `","timeout_ms":1000}`, http.StatusRequestEntityTooLarge},
	} {
		res, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, res.StatusCode, c.want)
		}
	}
	if res, err := http.Get(srv.URL + "/run"); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /run: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	if res, err := http.Get(srv.URL + "/healthz"); err != nil || res.StatusCode != http.StatusNoContent {
		t.Errorf("healthz: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	if n := p.attachs.Load(); n != 0 {
		t.Fatalf("断ったのに attach した: %d", n)
	}
}

func TestRelayStreamsLinesUntilTimeout(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	got := make(chan string, 1)
	go func() {
		c := p.next()
		s, _ := readCmd(c)
		got <- s
		_, _ = io.WriteString(c, "\x1b[K[10:00:00 INFO]: one\r\n[10:00:01 INFO]: two\r\n")
	}()
	res, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"cmd":"bluemap","timeout_ms":300}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("timeout は正しい終わり方のはず: %v", err)
	}
	if res.StatusCode != 200 || string(b) != "[10:00:00 INFO]: one\n[10:00:01 INFO]: two\n" {
		t.Fatalf("%d %q", res.StatusCode, b)
	}
	if s := <-got; s != "bluemap\r" {
		t.Fatalf("sent %q", s)
	}
}

// 最後の行から時間がたって timeout になっても、正しい終わり方になる (切断と取り違えない)。
func TestRelayCleanEndAfterIdle(t *testing.T) {
	a, err := newConsoleAllow("")
	if err != nil {
		t.Fatal(err)
	}
	p := newPipeConsole()
	relay := NewRelay(p, a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay.writeTimeout = 300 * time.Millisecond
	srv := httptest.NewServer(relay.Handler())
	defer func() {
		p.closeAll()
		srv.Close()
	}()
	go func() {
		c := p.next()
		_, _ = readCmd(c)
		_, _ = io.WriteString(c, "[10:00:00 INFO]: one\r\n")
	}()
	res, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"cmd":"bluemap","timeout_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || string(b) != "[10:00:00 INFO]: one\n" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestRelayConcurrencyLimit(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < relayMaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/run", strings.NewReader(`{"cmd":"bluemap","timeout_ms":60000}`))
			if res, err := http.DefaultClient.Do(req); err == nil {
				res.Body.Close()
			}
		}()
	}
	for i := 0; i < relayMaxConcurrent; i++ {
		p.next() // 全部がつながるまで待つ (コマンドは読まないので、止まったまま)
	}
	if n := p.attachs.Load(); n != relayMaxConcurrent {
		t.Fatalf("attach %d", n)
	}
	res, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"cmd":"bluemap","timeout_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("5 本目: %d", res.StatusCode)
	}
	// 許可していないコマンドは、枠が埋まっていても 403
	res, err = http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"cmd":"stop","timeout_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("許可していないコマンド: %d", res.StatusCode)
	}
	// 相手が切れば、止まっていても枠が空く
	cancel()
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		go func() {
			c := p.next()
			_, _ = readCmd(c)
		}()
		res, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"cmd":"bluemap","timeout_ms":1}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("枠が空かない: %d", res.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelayConsoleStopsAtDone(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	conn := make(chan net.Conn, 1)
	go func() {
		c := p.next()
		_, _ = readCmd(c)
		_, _ = io.WriteString(c, "[10:00:00 INFO]: noise\r\n[10:00:01 INFO]: Saved the game\r\n")
		conn <- c
	}()
	rc := NewRelayConsole(srv.URL)
	lines, err := rc.Run(context.Background(), "save-all flush", 30*time.Second, func(l string) bool { return savedRE.MatchString(l) })
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
	// 中継は attach を閉じる
	c := <-conn
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("attach が閉じられていない: %v", err)
	}
}

// attach が切れたら、done があるときはエラーにする (完了と取り違えない)。
func TestRelayConsoleStreamDisconnect(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	drop := func() {
		c := p.next()
		_, _ = readCmd(c)
		_, _ = io.WriteString(c, "[10:00:00 INFO]: one\r\n")
		time.Sleep(100 * time.Millisecond) // 行が届いてから切る
		c.Close()
	}
	rc := NewRelayConsole(srv.URL)

	go drop()
	lines, err := rc.Run(context.Background(), "save-all flush", 30*time.Second, func(string) bool { return false })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("done あり: err=%v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines=%q", lines)
	}

	// done が無ければ、200 の時点でコマンドは送ってあるので、エラーにしない
	go drop()
	lines, err = rc.Run(context.Background(), "bluemap", 30*time.Second, nil)
	if err != nil || len(lines) != 1 {
		t.Fatalf("done なし: lines=%q err=%v", lines, err)
	}
}

func TestRelayConsoleTimeoutWithDone(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	go func() {
		c := p.next()
		_, _ = readCmd(c)
	}()
	_, err := NewRelayConsole(srv.URL).Run(context.Background(), "save-all flush", 200*time.Millisecond, func(string) bool { return false })
	if err == nil || errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err=%v", err)
	}
}

func TestRelayConsoleErrors(t *testing.T) {
	p, srv := newRelayEnv(t, "")
	rc := NewRelayConsole(srv.URL)
	ctx := context.Background()
	if _, err := rc.Run(ctx, "stop", time.Second, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("断られたとき: %v", err)
	}
	p.err = errors.New("no container")
	if _, err := rc.Run(ctx, "bluemap", time.Second, nil); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("attach できないとき: %v", err)
	}
	srv.Close()
	if _, err := rc.Run(ctx, "bluemap", time.Second, nil); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("中継につなげないとき: %v", err)
	}
}

func TestDockerConsoleAttach(t *testing.T) {
	daemon := func(status, contentType string, frames []byte, gotPath chan<- string) func(context.Context) (net.Conn, error) {
		return func(context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			go func() {
				defer b.Close()
				req, err := http.ReadRequest(bufio.NewReader(b))
				if err != nil {
					return
				}
				gotPath <- req.Method + " " + req.URL.Path
				fmt.Fprintf(b, "HTTP/1.1 %s\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", status, contentType)
				_, _ = b.Write(frames)
			}()
			return a, nil
		}
	}
	// 多重化された出力をほどく
	body := "hello\n"
	frame := append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(body))}, body...)
	paths := make(chan string, 2)
	d := &DockerConsole{Container: "toro-main", dial: daemon("101 UPGRADED", "application/vnd.docker.multiplexed-stream", frame, paths)}
	conn, stream, err := d.attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if p := <-paths; p != "POST /containers/toro-main/attach" {
		t.Fatalf("path %q", p)
	}
	line, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil || line != body {
		t.Fatalf("%q %v", line, err)
	}
	// コンテナが無いときは、エラー
	d = &DockerConsole{Container: "toro-main", dial: daemon("404 Not Found", "text/plain", nil, paths)}
	if _, _, err := d.attach(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadRelayConfig(t *testing.T) {
	t.Setenv("MC_CONTAINER", "")
	c, err := loadRelayConfig()
	if err != nil || c.Container != "toro-main" || c.Addr != ":8091" || c.Socket != "/var/run/docker.sock" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("MC_CONTAINER", "a/../b")
	if _, err := loadRelayConfig(); err == nil {
		t.Fatal("コンテナの名前の形を確かめるはず")
	}
}
