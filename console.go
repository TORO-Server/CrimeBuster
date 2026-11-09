package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Console は Minecraft のサーバーのコンソール。
// toro-main には shell が無く、RCON のコマンドは Paper の非同期の制限で失敗するので、コンソールには attach でつなぐ。
// Docker のソケットは crime-buster に置かず、中継 (crime-buster console-relay、relay.go) に頼む。
type Console interface {
	// Run は cmd を 1 行送り、出力の行を done が true を返すまで (または timeout まで) 集めて返す。
	// done が nil なら、timeout まで集める。done があるのに、出力が途中で切れたり timeout になったりしたら、エラーを返す。
	Run(ctx context.Context, cmd string, timeout time.Duration, done func(line string) bool) ([]string, error)
}

const (
	relayGrace      = 15 * time.Second // 中継の timeout より、これだけ長く待つ
	maxRunLines     = 2000             // 1 回の Run で覚える行の数 (多いときは古いほうを捨てる)
	sendOnlyTimeout = time.Second      // 出力を使わないコマンド (キック・ミュート) は、中継の枠をすぐ空ける
)

// RelayConsole は、中継の POST /run で Console を実装する。
type RelayConsole struct {
	URL string
	hc  *http.Client
}

func NewRelayConsole(url string) *RelayConsole {
	// 内部のネットワークだけで話すので、プロキシの設定は見ない。行を流す時間が長いので、全体の期限は context で持つ
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	return &RelayConsole{URL: strings.TrimRight(url, "/"), hc: &http.Client{Transport: tr}}
}

func (c *RelayConsole) Run(ctx context.Context, cmd string, timeout time.Duration, done func(string) bool) ([]string, error) {
	body, err := json.Marshal(relayRequest{Cmd: cmd, TimeoutMS: timeout.Milliseconds()})
	if err != nil {
		return nil, err
	}
	// 返ると接続が切れて、中継の attach も終わる
	rctx, cancel := context.WithTimeout(ctx, timeout+relayGrace)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.URL+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: コンソールの中継につなげません: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, relayError(resp)
	}

	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), consoleMaxLine)
	for sc.Scan() {
		l := sc.Text()
		out = append(out, l)
		if len(out) > maxRunLines {
			out = out[1:]
		}
		if done != nil && done(l) {
			return out, nil
		}
	}
	err = sc.Err()
	switch {
	case ctx.Err() != nil:
		return out, ctx.Err()
	case done == nil:
		return out, nil // 200 なら、コマンドは送ってある
	case err != nil:
		return out, fmt.Errorf("console stream of %q: %w", cmd, io.ErrUnexpectedEOF)
	default:
		return out, fmt.Errorf("timeout waiting for output of %q", cmd)
	}
}

// relayError は、中継が断ったときのエラー。コネクタの HTTP API の状態コードに合わせて分ける。
func relayError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := oneLine(string(b), 200)
	switch resp.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%w: コンソールの中継が断りました: %s", ErrForbidden, msg)
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return fmt.Errorf("%w: コンソールの中継: %s", ErrUnavailable, msg)
	}
	return fmt.Errorf("console relay: HTTP %d: %s", resp.StatusCode, msg)
}

// ---- よく使うコマンド ----

// Ops はサーバーに送るコマンドの組み立てと、返事の読み取り。
type Ops struct{ C Console }

// コンソールの行は、プレイヤーの発言も混ざる。発言には <名前> などが頭に付くので、判定の文言は、行の頭 (端末のプロンプトの残りと、時刻とレベル) の直後だけで見る。
const (
	linePrompt = `^(?:>\s*)*`
	logPrefix  = linePrompt + `\[\d{2}:\d{2}:\d{2} INFO\]: `
	// BlueMap の状態は複数行の出力で、2 行目からは時刻が付かず、頭に印が 1 文字付く
	blueMapPrefix = linePrompt + `(?:\[\d{2}:\d{2}:\d{2} INFO\]: )?(?:\S )?`
)

var (
	savedRE       = regexp.MustCompile(logPrefix + `(?i:saved the (?:game|world)|flushed)`)
	rollbackOK    = regexp.MustCompile(logPrefix + `(?i:CoreProtect - Rollback completed for) "([^"]+)"`)
	rollbackNG    = regexp.MustCompile(logPrefix + `(?:CoreProtect - )?(?i:rollback (?:failed|cancel+ed)|rollback or restore aborted|a rollback/restore is already in progress|no (?:results|data) found|database busy)`)
	blueMapIdleRE = regexp.MustCompile(blueMapPrefix + `(?i:\d+ render-threads are idle)`)
	blueMapDoneRE = regexp.MustCompile(blueMapPrefix + `(?i:map (\S+) is updated)`)
)

// SaveFlush はワールドをディスクに書き出す (BlueMap はディスクの region ファイルを読むので)。
func (o Ops) SaveFlush(ctx context.Context) error {
	_, err := o.C.Run(ctx, "save-all flush", 90*time.Second, func(l string) bool { return savedRE.MatchString(l) })
	return err
}

// BlueMapUpdate は範囲を描き直す仕事を作る。
func (o Ops) BlueMapUpdate(ctx context.Context, mapID string, x, z, radius int) error {
	_, err := o.C.Run(ctx, fmt.Sprintf("bluemap update %s %d %d %d", mapID, x, z, radius), 8*time.Second, nil)
	return err
}

// BlueMapIdle は BlueMap が仕事を終えて、地図が最新になっているか。
func (o Ops) BlueMapIdle(ctx context.Context, mapID string) (bool, error) {
	lines, err := o.C.Run(ctx, "bluemap", 4*time.Second, nil)
	if err != nil {
		return false, err
	}
	return parseBlueMapIdle(lines, mapID), nil
}

func parseBlueMapIdle(lines []string, mapID string) bool {
	idle, updated := false, false
	for _, l := range lines {
		if blueMapIdleRE.MatchString(l) {
			idle = true
		}
		if m := blueMapDoneRE.FindStringSubmatch(l); m != nil && strings.EqualFold(m[1], mapID) {
			updated = true
		}
	}
	return idle && updated
}

// WaitBlueMap は BlueMap が描き終わるまで待つ。2 回続けて「終わっている」を見たら終わりとする。
func (o Ops) WaitBlueMap(ctx context.Context, mapID string, max time.Duration) error {
	end := time.Now().Add(max)
	okRuns := 0
	for time.Now().Before(end) {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		idle, err := o.BlueMapIdle(ctx, mapID)
		if err != nil {
			return err
		}
		if idle {
			okRuns++
			if okRuns >= 2 {
				return nil
			}
		} else {
			okRuns = 0
		}
	}
	return fmt.Errorf("bluemap did not finish within %s", max)
}

// RollbackCmd は CoreProtect のロールバックのコマンド。時間は「いまからさかのぼる秒」。
// 名前かワールドが安全な形でないときは空 (コンソールに渡さず、報告文にも書かない)。
func RollbackCmd(player string, seconds int, world string) string {
	if !validName(player) || !validWorld(world) {
		return ""
	}
	return fmt.Sprintf("co rollback u:%s t:%ds r:#%s", player, seconds, world)
}

// RestoreCmd は、まちがえたときに戻すためのコマンド (報告に書く)。形の決まりは RollbackCmd と同じ。
func RestoreCmd(player string, seconds int, world string) string {
	if !validName(player) || !validWorld(world) {
		return ""
	}
	return fmt.Sprintf("co restore u:%s t:%ds r:#%s", player, seconds, world)
}

var errRollbackFailed = errors.New("CoreProtect がロールバックの失敗を返しました")

// rollbackFinished は、その人のロールバックが終わったという CoreProtect の行か。
func rollbackFinished(line, player string) bool {
	m := rollbackOK.FindStringSubmatch(line)
	return m != nil && strings.EqualFold(m[1], player)
}

// Rollback はコマンドを送り、「完了」の行まで待つ。本当に戻ったかは、呼ぶ側がデータベースで確かめる (rollbackVerified)。
func (o Ops) Rollback(ctx context.Context, player string, seconds int, world string) error {
	cmd := RollbackCmd(player, seconds, world)
	if cmd == "" {
		return fmt.Errorf("%w: 名前 %q かワールド %q はコンソールに安全に渡せません", ErrForbidden, player, world)
	}
	failed := false
	_, err := o.C.Run(ctx, cmd, 10*time.Minute, func(l string) bool {
		if rollbackFinished(l, player) {
			return true
		}
		if rollbackNG.MatchString(l) {
			failed = true
			return true
		}
		return false
	})
	if err == nil && failed {
		err = errRollbackFailed
	}
	return err
}

func (o Ops) Kick(ctx context.Context, player, reason string) error {
	if !validName(player) {
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", ErrForbidden, player)
	}
	_, err := o.C.Run(ctx, fmt.Sprintf("kick %s %s", player, reason), sendOnlyTimeout, nil)
	return err
}
