package console

import (
	"context"
	"crime-buster/internal/domain"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRcon は、RCON のサーバー側の試験用。認証 → コマンド (応答は分割して返す) の順に話す。
// 応答の終わりはクライアント側が「パケットが来なくなる」ことで判定するので、接続は開いたままにする。
type fakeRcon struct {
	password string
	replies  map[string][]string // コマンド → 応答の断片 (そのままの順で返す)
	got      []string
}

func (f *fakeRcon) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return ln.Addr().String()
}

func readPkt(r io.Reader) (int32, int32, string, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, 0, "", err
	}
	n := int32(binary.LittleEndian.Uint32(head[:]))
	rest := make([]byte, n)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, 0, "", err
	}
	return int32(binary.LittleEndian.Uint32(rest[0:4])), int32(binary.LittleEndian.Uint32(rest[4:8])), string(rest[8 : len(rest)-2]), nil
}

func writePkt(w io.Writer, id, typ int32, body string) {
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
	_, _ = w.Write(buf)
}

func (f *fakeRcon) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	for {
		id, typ, body, err := readPkt(conn)
		if err != nil {
			return
		}
		switch {
		case typ == rconAuth:
			if body != f.password {
				writePkt(conn, -1, rconCommand, "")
				return
			}
			writePkt(conn, id, rconReply, "") // 認証の前に来ることがある空の応答
			writePkt(conn, id, rconCommand, "")
		case typ == rconCommand:
			f.got = append(f.got, body)
			for _, frag := range f.replies[body] {
				writePkt(conn, id, rconReply, frag)
			}
		}
	}
}

func TestRconRun(t *testing.T) {
	f := &fakeRcon{password: "pw", replies: map[string][]string{
		"save-all flush": {"§aSaving the game ", "(this may take a moment!)", "Saved the game"},
		"list":           {"There are 2 of a max of 20 players online"},
	}}
	rc := &RconConsole{Addr: f.serve(t), Password: "pw"}
	ctx := context.Background()
	got, err := rc.Run(ctx, "save-all flush")
	if err != nil {
		t.Fatal(err)
	}
	// 分割された応答がつながり、§ の書式コードが取れている
	if got != "Saving the game (this may take a moment!)Saved the game" {
		t.Fatalf("got %q", got)
	}
	if _, err := rc.Run(ctx, "list"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.got, ",") != "save-all flush,list" {
		t.Fatalf("server got %q", f.got)
	}
}

func TestRconAuthRejected(t *testing.T) {
	f := &fakeRcon{password: "pw"}
	rc := &RconConsole{Addr: f.serve(t), Password: "wrong"}
	if _, err := rc.Run(context.Background(), "list"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestRconAllowlistFirst(t *testing.T) {
	f := &fakeRcon{password: "pw"}
	allow, err := NewAllow("")
	if err != nil {
		t.Fatal(err)
	}
	rc := &RconConsole{Addr: f.serve(t), Password: "pw", Allow: allow}
	if _, err := rc.Run(context.Background(), "stop"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err=%v", err)
	}
	if len(f.got) != 0 {
		t.Fatalf("断ったのに送った: %q", f.got)
	}
}

func TestRconUnreachable(t *testing.T) {
	rc := &RconConsole{Addr: "127.0.0.1:1", Password: "pw"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rc.Run(ctx, "list"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}
