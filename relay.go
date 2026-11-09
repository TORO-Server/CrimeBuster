package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// コンソールの中継 (crime-buster console-relay)。Docker のソケットを持つのは、このプロセスだけ。
//
//	POST /run {"cmd","timeout_ms"}  許可した形のコマンドだけを送り、コンソールの行を流す (timeout か、相手が切るまで)
//	GET  /healthz
//
// crime-buster と 2 つだけの internal ネットワークに置く。送れるのは、crime-buster が送る形のコマンドだけで、値もここで確かめる。
// 200 は「コマンドを送った」という意味。そのあと attach が切れたら、終わりの印を付けずに接続を切る (正しい終わりと区別できる)。

const (
	relayMaxBody       = 4 << 10
	relayMaxConcurrent = 4
	relayMaxTimeout    = 10 * time.Minute
	relayReadTimeout   = 10 * time.Second
	relayWriteTimeout  = 10 * time.Second
	attachTimeout      = 10 * time.Second
	consoleMaxLine     = 1 << 20
)

// RelayConfig は中継の設定。crime-buster の Config とは分ける (ソケットの場所を crime-buster に持たせない)。
type RelayConfig struct {
	Addr      string
	Socket    string
	Container string
	MuteCmd   string
}

var containerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func loadRelayConfig() (*RelayConfig, error) {
	c := &RelayConfig{
		Addr:      envStr("CONSOLE_ADDR", ":8091"),
		Socket:    envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		Container: envStr("MC_CONTAINER", "toro-main"),
		MuteCmd:   os.Getenv("CHAT_MUTE_CMD"),
	}
	if !containerRE.MatchString(c.Container) {
		return nil, fmt.Errorf("MC_CONTAINER: %q", c.Container)
	}
	return c, nil
}

func cmdConsoleRelay(log *slog.Logger) error {
	cfg, err := loadRelayConfig()
	if err != nil {
		return err
	}
	allow, err := newConsoleAllow(cfg.MuteCmd)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	relay := NewRelay(&DockerConsole{Socket: cfg.Socket, Container: cfg.Container}, allow, log)
	// ReadTimeout は付けない (期限が切断の監視に残って、流している途中で context が切れる)。本文の期限は run で付ける。
	// 書く期限も行ごとに付けるので、接続は 1 リクエストごとに閉じる (古い期限を次のリクエストに残さない)
	srv := &http.Server{Addr: cfg.Addr, Handler: relay.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	srv.SetKeepAlivesEnabled(false)
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	log.Info("コンソールの中継を開いた", "addr", cfg.Addr, "container", cfg.Container)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- 許可するコマンド ----

// maxKickReason は kick の理由の長さ。理由に制御文字と改行の仲間は入れない。
const maxKickReason = 100

var (
	blueMapUpdateCmdRE = regexp.MustCompile(`^bluemap update ` + idPattern + ` -?[0-9]{1,8} -?[0-9]{1,8} ([0-9]{1,6})$`)
	rollbackCmdRE      = regexp.MustCompile(`^co rollback u:` + namePattern + ` t:([0-9]{1,9})s r:#` + idPattern + `$`)
	kickCmdRE          = regexp.MustCompile(fmt.Sprintf(`^kick %s [^\p{C}\p{Zl}\p{Zp}]{1,%d}$`, namePattern, maxKickReason))
)

// ConsoleAllow は、送ってよいコマンドの形。文字列全体が一致するものだけを通す。
type ConsoleAllow struct {
	muteTmpl string
	mute     *regexp.Regexp
}

// newConsoleAllow は、CHAT_MUTE_CMD の形も許可に足す。空なら、ミュートは通さない。
func newConsoleAllow(muteTmpl string) (*ConsoleAllow, error) {
	a := &ConsoleAllow{muteTmpl: strings.TrimSpace(muteTmpl)}
	if a.muteTmpl == "" {
		return a, nil
	}
	if ctlRE.MatchString(a.muteTmpl) || !strings.Contains(a.muteTmpl, "{name}") {
		return nil, fmt.Errorf("CHAT_MUTE_CMD: {name} を含む 1 行にしてください")
	}
	parts := strings.Split(a.muteTmpl, "{name}")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("^" + parts[0] + "(" + namePattern + ")" + strings.Join(parts[1:], namePattern) + "$")
	if err != nil {
		return nil, fmt.Errorf("CHAT_MUTE_CMD: %w", err)
	}
	a.mute = re
	return a, nil
}

// Allowed は、cmd が crime-buster の送る形かどうか。
func (a *ConsoleAllow) Allowed(cmd string) bool {
	switch cmd {
	case "save-all flush", "bluemap":
		return true
	}
	if m := blueMapUpdateCmdRE.FindStringSubmatch(cmd); m != nil {
		return inRange(m[1], 1, maxBlueMapRadius)
	}
	if m := rollbackCmdRE.FindStringSubmatch(cmd); m != nil {
		return inRange(m[1], 1, maxRollbackSeconds)
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

// ---- 中継の本体 ----

// attacher はコンソールの入出力につなぐもの (本番は DockerConsole)。
type attacher interface {
	attach(ctx context.Context) (net.Conn, io.Reader, error)
}

// Relay は POST /run の本体。
type Relay struct {
	con          attacher
	allow        *ConsoleAllow
	slots        chan struct{}
	writeTimeout time.Duration // 1 行を書く期限 (読まない相手で止まらないように)
	log          *slog.Logger
}

func NewRelay(con attacher, allow *ConsoleAllow, log *slog.Logger) *Relay {
	return &Relay{con: con, allow: allow, slots: make(chan struct{}, relayMaxConcurrent), writeTimeout: relayWriteTimeout, log: log}
}

func (s *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", s.run)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

type relayRequest struct {
	Cmd       string `json:"cmd"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// readRelayRequest は本文を読む。読み切るので、そのあと相手の切断を context で知ることができる。
func readRelayRequest(w http.ResponseWriter, r *http.Request) (relayRequest, int, error) {
	var in relayRequest
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, relayMaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return in, http.StatusRequestEntityTooLarge, errors.New("本文が大きすぎます")
		}
		return in, http.StatusBadRequest, errors.New("本文を読めません")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.More() {
		return in, http.StatusBadRequest, errors.New("本文は {\"cmd\",\"timeout_ms\"} の JSON です")
	}
	if in.TimeoutMS < 1 || in.TimeoutMS > relayMaxTimeout.Milliseconds() {
		return in, http.StatusBadRequest, fmt.Errorf("timeout_ms は 1〜%d", relayMaxTimeout.Milliseconds())
	}
	return in, 0, nil
}

func (s *Relay) run(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(relayReadTimeout))
	in, code, err := readRelayRequest(w, r)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	if !s.allow.Allowed(in.Cmd) {
		s.log.Warn("許可していないコマンドを断った", "cmd", oneLine(in.Cmd, 200))
		http.Error(w, "許可していないコマンドです", http.StatusForbidden)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "同時に動かせる数を超えています", http.StatusTooManyRequests)
		return
	}
	s.log.Info("コマンドを送る", "cmd", in.Cmd, "timeout_ms", in.TimeoutMS)
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	conn, stream, err := s.con.attach(r.Context())
	if err != nil {
		s.log.Error("attach に失敗", "err", err)
		http.Error(w, "コンソールにつなげません", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout + relayGrace))
	// 相手が切ったら、止まっている書き込みも終わらせる
	stop := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer stop()

	started := false
	send := writerFunc(func(p []byte) (int, error) {
		n, err := conn.Write(p)
		if err == nil {
			started = true
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = rc.Flush()
		}
		return n, err
	})
	err = streamConsole(r.Context(), send, stream, in.Cmd, timeout, func(line string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return true
		}
		return rc.Flush() != nil
	})
	// 終わりの印も書けるように、期限を新しくする
	_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	switch {
	case err == nil, errors.Is(err, errConsoleTimeout), r.Context().Err() != nil:
		// 時間まで流した、または相手が切った
	case !started:
		s.log.Error("コマンドを送れない", "err", err)
		http.Error(w, "コンソールにつなげません", http.StatusBadGateway)
	default:
		s.log.Warn("コンソールの attach が切れた", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// ---- コンソールの入出力 ----

// DockerConsole は Docker Engine の API (unix ソケット) で、決めたコンテナの標準入出力に attach する。
type DockerConsole struct {
	Socket    string
	Container string
	dial      func(ctx context.Context) (net.Conn, error) // 試験でだけ差し替える
}

func (d *DockerConsole) attach(ctx context.Context) (net.Conn, io.Reader, error) {
	dial := d.dial
	if dial == nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", d.Socket)
		}
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("docker socket: %w", err)
	}
	// 手順の読み書きが止まっても、いつまでも待たない
	_ = conn.SetDeadline(time.Now().Add(attachTimeout))
	req := fmt.Sprintf("POST /containers/%s/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nUpgrade: tcp\r\nConnection: Upgrade\r\nContent-Length: 0\r\n\r\n", d.Container)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("docker attach: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, nil, fmt.Errorf("docker attach: HTTP %d", resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	var stream io.Reader = br
	if strings.Contains(resp.Header.Get("Content-Type"), "multiplexed") {
		stream = &demux{r: br}
	}
	return conn, stream, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[=>]`)

func cleanLine(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.TrimSpace(s)
}

// errConsoleTimeout は、timeout まで行を流したとき。中継では、正しい終わり方。
var errConsoleTimeout = errors.New("console timeout")

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// streamConsole は、つながった入出力に cmd を送り、出た行を 1 行ずつ emit に渡す。emit が true を返すと nil で終わる。
// 相手が切れたら io.ErrUnexpectedEOF、timeout まで来たら errConsoleTimeout を返す。
func streamConsole(ctx context.Context, w io.Writer, r io.Reader, cmd string, timeout time.Duration, emit func(string) bool) error {
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), consoleMaxLine)
		for sc.Scan() {
			if l := cleanLine(sc.Text()); l != "" {
				select {
				case lines <- l:
				default: // 読む側が遅いときは捨てる
				}
			}
		}
	}()

	// 端末の描き直しが落ち着くのを少し待つ
	settle := time.NewTimer(400 * time.Millisecond)
	defer settle.Stop()
drain:
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return io.ErrUnexpectedEOF
			}
		case <-settle.C:
			break drain
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if _, err := io.WriteString(w, cmd+"\r"); err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return io.ErrUnexpectedEOF
			}
			if emit(l) {
				return nil
			}
		case <-deadline.C:
			return errConsoleTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// demux は tty でないコンテナの、多重化された出力 (8 バイトの頭 + 本体) を 1 本にほどく。
type demux struct {
	r    io.Reader
	left int
}

func (d *demux) Read(p []byte) (int, error) {
	for d.left == 0 {
		var h [8]byte
		if _, err := io.ReadFull(d.r, h[:]); err != nil {
			return 0, err
		}
		d.left = int(binary.BigEndian.Uint32(h[4:]))
	}
	if len(p) > d.left {
		p = p[:d.left]
	}
	n, err := d.r.Read(p)
	d.left -= n
	return n, err
}

// relayHealth は、コンテナの中から中継が答えるかを確かめる (イメージに curl が無いので)。
func relayHealth() error {
	addr := envStr("CONSOLE_ADDR", ":8091")
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("CONSOLE_ADDR: %w", err)
	}
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := hc.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("healthz: HTTP %d", res.StatusCode)
	}
	return nil
}
