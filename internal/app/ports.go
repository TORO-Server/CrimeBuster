package app

import (
	"context"
	"crime-buster/internal/capture"
	"crime-buster/internal/console"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
)

// このファイルは、Guard とそのサービスが使う依存の境界 (ポート) を定義する。
// ポートは使う側 (このパッケージ) が持ち、メソッドは実際に使うものだけにする。
// 依存が無いことは「その機能は使えない」という判定に使うので、具象型が nil のときは
// ポート自体も nil に保つ (型付き nil を入れると、nil 判定が壊れる。NewGuard の変換を見よ)。

// Reader は、CoreProtect の読み取りのうち、このパッケージが使うもの。
type Reader interface {
	MaxRowID(ctx context.Context) (int64, error)
	MaxChatRowID(ctx context.Context) (int64, error)
	Poll(ctx context.Context, after int64, limit int) ([]detect.Event, error)
	PollChat(ctx context.Context, after int64, limit int) ([]detect.ChatMsg, error)
	User(ctx context.Context, id int) (detect.UserInfo, error)
	RecentEditCommand(ctx context.Context, user int, since int64) (string, error)
	Stats(ctx context.Context, user detect.UserInfo, from, to int64) (*detect.Stats, error)
	RolledBack(ctx context.Context, userID int, world string, from, to int64) (done, total int, err error)
	ChatFor(ctx context.Context, user int, from, to int64) ([]detect.ChatMsg, error)
}

// Judge は、AI の判定のうち、このパッケージが使うもの。
type Judge interface {
	ReviewIncident(ctx context.Context, s *detect.Stats, trigger string, images []string) (*domain.Verdict, error)
	ReviewChat(ctx context.Context, msgs []detect.ChatMsg, names map[int]string) ([]notify.ChatFlag, error)
}

// Reporter は、Discord への通報のうち、このパッケージが使うもの。
type Reporter interface {
	Post(ctx context.Context, existing, title, content string, files []notify.ReportFile, rolledBack bool) (string, error)
	PostTags(ctx context.Context, existing, title, content string, files []notify.ReportFile, tags []string) (string, error)
	TagID() string
}

// Shooter は、Renderer の撮影のうち、このパッケージが使うもの。
type Shooter interface {
	Shot(ctx context.Context, mapID string, cam capture.Cam, out string, ann *capture.Annot) error
	OverlayResult() string
}

// Console は、Minecraft のコンソール操作のうち、このパッケージが使うもの。
// 生のコマンド実行 (Run) は console.Ops の土台のものなので、opsConsole がつなぐ。
type Console interface {
	Run(ctx context.Context, cmd string) (string, error)
	Rollback(ctx context.Context, player string, seconds int, world string) error
	Kick(ctx context.Context, player, reason string) error
	SaveFlush(ctx context.Context) error
	Ping(ctx context.Context) error
}

// opsConsole は、console.Ops を Console として使うためのつなぎ。
type opsConsole struct{ ops *console.Ops }

func (c opsConsole) Run(ctx context.Context, cmd string) (string, error) {
	return c.ops.C.Run(ctx, cmd)
}

func (c opsConsole) Rollback(ctx context.Context, player string, seconds int, world string) error {
	return c.ops.Rollback(ctx, player, seconds, world)
}

func (c opsConsole) Kick(ctx context.Context, player, reason string) error {
	return c.ops.Kick(ctx, player, reason)
}

func (c opsConsole) SaveFlush(ctx context.Context) error { return c.ops.SaveFlush(ctx) }

func (c opsConsole) Ping(ctx context.Context) error { return c.ops.Ping(ctx) }

// 具象型がポートを満たすことの確認。
var (
	_ Reader   = (*detect.CP)(nil)
	_ Judge    = (*notify.AI)(nil)
	_ Reporter = (*notify.Reporter)(nil)
	_ Shooter  = (*capture.Shooter)(nil)
	_ Console  = opsConsole{}
)
