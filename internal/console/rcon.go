package console

import (
	"context"
	"crime-buster/internal/config"
	"crime-buster/internal/domain"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

// RCON (Source のプロトコル) のクライアント。コマンドごとに接続する (接続の使い回しはしない。
// サーバーは複数の接続を受け付けるので、キックやミュートが長い処理を待つことはない)。
//
// パケットは、長さ (4) + ID (4) + 種別 (4) + 本文 + 終端 (2 つの 0)。
// 種別は、認証が 3、コマンドが 2、応答が 0 (認証の応答は 2)。
// 1 つのコマンドの応答が複数のパケットに分かれることがあるが、目印のパケットは送らない:
// Paper の RCON は、コマンドに続く 2 つ目のパケットを受けると、接続を閉じることがある
// (検証で実測。目印なしでは全件成功した)。応答の終わりは、パケットが来なくなってから
// しばらく (rconQuiet) 待って判定する。
const (
	rconAuth    = 3
	rconCommand = 2
	rconReply   = 0

	rconMaxPacket   = 1 << 20 // 1 パケットの本文の上限
	rconMaxResponse = 8 << 20 // 1 コマンドの応答全体の上限
	rconDefaultWait = 60 * time.Second
	// rconQuiet は、応答のパケットが来なくなってから、応答の終わりと判定するまでの待ち。
	rconQuiet = 300 * time.Millisecond
)

// RconConsole は RCON で Console を実装する。
type RconConsole struct {
	Addr     string
	Password string
	Allow    *Allow

	// mu は、同時に 1 つのコマンドだけを送るためのもの。サーバー側の応答の置き場は
	// 接続をまたいで共有されることがあり、並行すると応答が混ざるため。
	mu   sync.Mutex
	dial func(ctx context.Context, network, addr string) (net.Conn, error) // 試験でだけ差し替える
}

// NewRconConsole は、設定から RconConsole を作る。許可リストにはミュートの形も入れる。
func NewRconConsole(cfg *config.Config) (*RconConsole, error) {
	allow, err := NewAllow(cfg.ChatConfig.ChatMuteCmd)
	if err != nil {
		return nil, err
	}
	return &RconConsole{Addr: cfg.ConsoleConfig.RconAddr, Password: cfg.ConsoleConfig.RconPassword, Allow: allow}, nil
}

var sectionRE = regexp.MustCompile(`§.`)

// Run は、許可を確かめてから、接続して認証し、コマンドを送って応答の全文を返す。
func (c *RconConsole) Run(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Allow != nil && !c.Allow.Allowed(cmd) {
		return "", fmt.Errorf("%w: 許可していないコマンドです: %s", domain.ErrForbidden, domain.OneLine(cmd, 200))
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rconDefaultWait)
		defer cancel()
	}
	dial := c.dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", c.Addr)
	if err != nil {
		return "", fmt.Errorf("%w: RCON につなげません: %v", domain.ErrUnavailable, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	done := context.AfterFunc(ctx, func() { conn.Close() })
	defer done()

	s := &rconSession{conn: conn}
	if err := s.auth(c.Password); err != nil {
		return "", err
	}
	res, err := s.exec(cmd)
	if err != nil {
		return "", err
	}
	return sectionRE.ReplaceAllString(res, ""), nil
}

type rconSession struct {
	conn net.Conn
	id   int32
}

func (s *rconSession) nextID() int32 {
	s.id++
	return s.id
}

func (s *rconSession) writePacket(id, typ int32, body string) error {
	if len(body) > rconMaxPacket {
		return fmt.Errorf("RCON: コマンドが長すぎます")
	}
	buf := make([]byte, 0, len(body)+14)
	var num [4]byte
	put := func(v int32) {
		binary.LittleEndian.PutUint32(num[:], uint32(v))
		buf = append(buf, num[:]...)
	}
	put(int32(len(body) + 10))
	put(id)
	put(typ)
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := s.conn.Write(buf)
	return err
}

// readPacket は 1 パケットを読む。本文と、ID と種別を返す。
func (s *rconSession) readPacket() (int32, int32, string, error) {
	var head [4]byte
	if _, err := io.ReadFull(s.conn, head[:]); err != nil {
		return 0, 0, "", err
	}
	n := int64(int32(binary.LittleEndian.Uint32(head[:])))
	if n < 10 || n > rconMaxPacket+10 {
		return 0, 0, "", fmt.Errorf("RCON: パケットの長さが正しくありません: %d", n)
	}
	rest := make([]byte, n)
	if _, err := io.ReadFull(s.conn, rest); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(rest[0:4]))
	typ := int32(binary.LittleEndian.Uint32(rest[4:8]))
	body := rest[8:]
	if len(body) > 0 && body[len(body)-1] == 0 {
		body = body[:len(body)-1]
	}
	if len(body) > 0 && body[len(body)-1] == 0 {
		body = body[:len(body)-1]
	}
	return id, typ, string(body), nil
}

// auth は認証する。失敗は ID -1 の応答で返る。
func (s *rconSession) auth(password string) error {
	id := s.nextID()
	if err := s.writePacket(id, rconAuth, password); err != nil {
		return fmt.Errorf("%w: RCON の認証を送れません: %v", domain.ErrUnavailable, err)
	}
	for {
		rid, typ, _, err := s.readPacket()
		if err != nil {
			return fmt.Errorf("%w: RCON の認証の応答を読めません: %v", domain.ErrUnavailable, err)
		}
		if typ == rconReply {
			continue // 認証の前に、空の応答が 1 つ来ることがある
		}
		if rid == -1 {
			return fmt.Errorf("%w: RCON の認証が断られました", domain.ErrUnavailable)
		}
		if rid == id && typ == rconCommand {
			return nil
		}
		return fmt.Errorf("%w: RCON の認証の応答の形が正しくありません", domain.ErrUnavailable)
	}
}

// exec はコマンドを送り、応答の全文を返す。応答は、コマンドへの返事のパケットを
// 集めたもので、終わりは「パケットが来なくなって rconQuiet が過ぎる」ことで判定する
// (目印のパケットは送らない。package の冒頭の注記を見よ)。
// 応答が途中で切れた場合は、文面の判定 (書き出しの確認など) とデータベースの確認が
// 安全側に倒すので、ここでは集まった分を返す。
func (s *rconSession) exec(cmd string) (string, error) {
	id := s.nextID()
	if err := s.writePacket(id, rconCommand, cmd); err != nil {
		return "", fmt.Errorf("%w: RCON にコマンドを送れません: %v", domain.ErrUnavailable, err)
	}
	var out []byte
	for {
		if len(out) > 0 {
			// 応答が始まっているので、短い待ちで終わりを判定する
			_ = s.conn.SetReadDeadline(time.Now().Add(rconQuiet))
		}
		rid, _, body, err := s.readPacket()
		if err != nil {
			if len(out) > 0 {
				if isTimeout(err) || errors.Is(err, io.EOF) {
					return string(out), nil
				}
			}
			return "", fmt.Errorf("%w: RCON の応答を読めません: %v", domain.ErrUnavailable, err)
		}
		if rid == id {
			out = append(out, body...)
			if len(out) > rconMaxResponse {
				return "", fmt.Errorf("RCON: 応答が長すぎます")
			}
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
