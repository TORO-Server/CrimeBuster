package testutil

import (
	"context"
	"strings"
	"sync"
)

// FakeConsole は Ops の試験用。Run は、コマンドの接頭辞に合う応答を返す。
type FakeConsole struct {
	mu    sync.Mutex
	cmds  []string
	Reply map[string][]string // 接頭辞 → 応答の行 (Run は改行でつないで返す)
	Err   error
}

func (f *FakeConsole) Run(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	if f.Err != nil {
		return "", f.Err
	}
	for prefix, lines := range f.Reply {
		if strings.HasPrefix(cmd, prefix) {
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", nil
}

// Sent は、受けたコマンドの写し。
func (f *FakeConsole) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *FakeConsole) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = nil
}

// FailConsole は、ロールバックの失敗の応答を返す。
type FailConsole struct{ *FakeConsole }

func (f *FailConsole) Run(context.Context, string) (string, error) {
	return "CoreProtect - Rollback failed.", nil
}
