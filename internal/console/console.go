// Package console は、Minecraft のサーバーのコンソールを RCON で扱う。
//
// 以前は、Docker のソケットを持つ別プロセス (中継) に頼み、コンソールのログ行を
// 流してもらっていた。いまは Paper 標準の RCON で直接送る: ソケットは誰も持たず、
// ブラウザも持たない。送れるコマンドは、送る前に許可リスト (allow.go) で確かめる。
//
// RCON の応答は「送ったコマンドへの返事」だけが返る (コンソールのログ全体ではない)。
// そのため、成否の判定は応答の文面で行い、最終的な確認は呼び出し側がデータベースで
// 行う (ロールバックの確認は、従来からデータベースが基準)。
package console

import (
	"context"
	"crime-buster/internal/domain"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Console は Minecraft のサーバーのコンソール。
type Console interface {
	// Run は cmd を 1 つ送り、応答の全文を返す。許可した形でなければ送らない。
	// 応答の § 書式コードは取り除いてある。
	Run(ctx context.Context, cmd string) (string, error)
}

// Ops はサーバーに送るコマンドの組み立てと、応答の読み取り。
type Ops struct{ C Console }

// 応答の文面の判定。RCON の応答にはコマンドを送った相手への返事しか入らないので、
// 行の頭の時刻などを要求しない (プレイヤーの発言が混ざることはない)。
var (
	savedRE    = regexp.MustCompile(`(?i:saved the (?:game|world))`)
	rollbackNG = regexp.MustCompile(`(?i:rollback (?:failed|cancel+ed)|rollback or restore aborted|a rollback/restore is already in progress|no (?:results|data) found|database busy)`)
)

// SaveFlush はワールドをディスクに書き出す (Renderer はディスクの region ファイルを読むので)。
// 応答に「書き出した」の文面が無ければ、失敗として返す。
func (o Ops) SaveFlush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := o.C.Run(ctx, "save-all flush")
	if err != nil {
		return err
	}
	if !savedRE.MatchString(res) {
		return fmt.Errorf("save-all flush: 書き出しの確認が応答に無い")
	}
	return nil
}

// Ping は、読み取りだけのコマンドで疎通を確かめる。
func (o Ops) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := o.C.Run(ctx, "list")
	return err
}

// RollbackCmd は CoreProtect のロールバックのコマンド。時間は「いまからさかのぼる秒」。
// 名前かワールドが安全な形でないときは空 (コンソールに渡さず、報告文にも書かない)。
func RollbackCmd(player string, seconds int, world string) string {
	if !domain.ValidName(player) || !domain.ValidWorld(world) {
		return ""
	}
	return fmt.Sprintf("co rollback u:%s t:%ds r:#%s", player, seconds, world)
}

// RestoreCmd は、まちがえたときに戻すためのコマンド (報告に書く)。形の決まりは RollbackCmd と同じ。
func RestoreCmd(player string, seconds int, world string) string {
	if !domain.ValidName(player) || !domain.ValidWorld(world) {
		return ""
	}
	return fmt.Sprintf("co restore u:%s t:%ds r:#%s", player, seconds, world)
}

// ErrRollbackFailed は、応答に失敗の文面があったときのエラー (ロールバックは始まっていない)。
// 転送の失敗 (接続や認証) とは区別する: そちらは、コマンドが届いたかどうかが分からない。
var ErrRollbackFailed = errors.New("CoreProtect がロールバックの失敗を返しました")

// Rollback はコマンドを送る。応答に失敗の文面があれば、その場でエラーにする。
// ロールバックの完了そのものは応答では分からない (処理はサーバー側で続く) ので、
// 本当に戻ったかは、呼ぶ側がデータベースで確かめる (従来からその設計)。
func (o Ops) Rollback(ctx context.Context, player string, seconds int, world string) error {
	cmd := RollbackCmd(player, seconds, world)
	if cmd == "" {
		return fmt.Errorf("%w: 名前 %q かワールド %q はコンソールに安全に渡せません", domain.ErrForbidden, player, world)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, err := o.C.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if rollbackNG.MatchString(res) {
		return ErrRollbackFailed
	}
	return nil
}

func (o Ops) Kick(ctx context.Context, player, reason string) error {
	if !domain.ValidName(player) {
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", domain.ErrForbidden, player)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := o.C.Run(ctx, fmt.Sprintf("kick %s %s", player, reason))
	return err
}

// MuteCommand は ChatMuteCmd の {name} を名前にしたコマンド。名前が Minecraft の名前として正しくないときは空 (コマンドの注入を避ける)。
func MuteCommand(tmpl, name string) string {
	if strings.TrimSpace(tmpl) == "" || !domain.ValidName(name) {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(tmpl), "{name}", name)
}
