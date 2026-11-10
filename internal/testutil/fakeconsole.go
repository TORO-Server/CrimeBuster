package testutil

import (
	"context"
	"strings"
	"sync"
	"time"
)

// fakeConsole は Ops の試験用。
type FakeConsole struct {
	mu      sync.Mutex
	cmds    []string
	Reply   map[string][]string
	Err     error
	Longest time.Duration // いちばん長い timeout
}

func (f *FakeConsole) Run(_ context.Context, cmd string, timeout time.Duration, done func(string) bool) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	f.Longest = max(f.Longest, timeout)
	if f.Err != nil {
		return nil, f.Err
	}
	for prefix, lines := range f.Reply {
		if strings.HasPrefix(cmd, prefix) {
			for _, l := range lines {
				if done != nil && done(l) {
					break
				}
			}
			return lines, nil
		}
	}
	return nil, nil
}

// sent は、受けたコマンドの写し。
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

type FailConsole struct{ *FakeConsole }

func (f *FailConsole) Run(ctx context.Context, cmd string, to time.Duration, done func(string) bool) ([]string, error) {
	line := "[10:00:00 INFO]: CoreProtect - Rollback failed."
	done(line)
	return []string{line}, nil
}
