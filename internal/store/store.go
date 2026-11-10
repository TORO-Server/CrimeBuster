package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Incident は 1 人の荒らしの 1 まとまり。最後の変更から Quiet たつか MaxIncident になったら閉じて、報告する。
type Incident struct {
	ID      string `json:"id"`
	UserID  int    `json:"user_id"`
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Start   int64  `json:"start"`
	Last    int64  `json:"last"`
	Trigger string `json:"trigger"`
	Queued  bool   `json:"queued,omitempty"`
	Kind    string `json:"kind,omitempty"`    // "" はブロックの荒らし、"chat" はチャットのスパム
	AIOnly  bool   `json:"ai_only,omitempty"` // AI の判定だけで開いた事件 (チャット)。ミュートせず、通報だけにする
}

// Key は Incidents の鍵。同じ人でも、ブロックの荒らしとチャットのスパムは別の事件にする。
func (i *Incident) Key() string {
	if i.Kind == "chat" {
		return ChatKey(i.UserID)
	}
	return fmt.Sprint(i.UserID)
}

func ChatKey(user int) string { return fmt.Sprintf("chat:%d", user) }

// ThreadRef は、その人について立てたフォーラムの投稿。同じ人の続きは、新しく立てずにこの投稿に書き足す。
type ThreadRef struct {
	ThreadID string `json:"thread_id"`
	At       int64  `json:"at"`
}

type State struct {
	Cursor    int64                `json:"cursor"`
	ChatCur   int64                `json:"chat_cursor"`
	Incidents map[string]*Incident `json:"incidents"` // key: user id
	Threads   map[string]ThreadRef `json:"threads"`   // key: 小文字の名前
}

type Store struct {
	path string
	S    State // 触るときは Guard.mu を持つ
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, "state.json")}
	st.S.Incidents = map[string]*Incident{}
	st.S.Threads = map[string]ThreadRef{}
	b, err := os.ReadFile(st.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &st.S); err != nil {
		return nil, err
	}
	if st.S.Incidents == nil {
		st.S.Incidents = map[string]*Incident{}
	}
	if st.S.Threads == nil {
		st.S.Threads = map[string]ThreadRef{}
	}
	return st, nil
}

// Save は一時ファイルに書いてから置き換える (途中で止まっても壊れない)。
func (s *Store) Save() error {
	b, err := json.MarshalIndent(&s.S, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func ThreadKey(name string) string { return strings.ToLower(name) }
