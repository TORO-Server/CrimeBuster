package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// IncidentState は事件の状態。コネクタ (control) は、pending のものを「確認待ち」として見せる。
// record.json には、この文字列のまま保存する。
type IncidentState string

const (
	StatePending   IncidentState = "pending"   // 確認待ち (通報も却下もまだ)
	StateReported  IncidentState = "reported"  // フォーラムに通報した
	StateDismissed IncidentState = "dismissed" // 却下した (荒らしではなかった、など)
)

// Transition は、事件の状態を変える唯一の場所。許可する遷移は
// pending → reported と pending → dismissed だけとし、それ以外はエラーを返して状態を変えない。
// Reported は State の派生なので、ここで同期させる (reported は、State が reported のときだけ true)。
func (r *Record) Transition(to IncidentState) error {
	if r.State != StatePending || (to != StateReported && to != StateDismissed) {
		return fmt.Errorf("事件の状態は %s から %s へ変えられません", r.State, to)
	}
	r.State = to
	r.Reported = to == StateReported
	return nil
}

// Decision は、事件に対して誰が何をしたかの記録。
type Decision struct {
	At     int64  `json:"at"`
	By     string `json:"by"`
	Action string `json:"action"` // report / rollback / dismiss / mute
	Note   string `json:"note,omitempty"`
}

// Record は、reports/<id>/record.json に置く、事件 1 件の状態。画像と報告文は同じフォルダにある。
type Record struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "block" か "chat"
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	UserID  int    `json:"user_id"`
	Created int64  `json:"created"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	World   string `json:"world,omitempty"`
	Trigger string `json:"trigger"`

	State      IncidentState `json:"state"`
	Busy       string        `json:"busy,omitempty"` // 裏で動いている操作 (rollback)
	Reported   bool          `json:"reported"`
	Thread     string        `json:"thread,omitempty"`
	RolledBack bool          `json:"rolled_back"`
	Muted      bool          `json:"muted,omitempty"`

	Images      []string `json:"images,omitempty"` // 同じフォルダの中のファイル名
	Breaks      int      `json:"breaks,omitempty"`
	Places      int      `json:"places,omitempty"`
	Messages    int      `json:"messages,omitempty"`
	RollbackCmd string   `json:"rollback_cmd,omitempty"`
	RestoreCmd  string   `json:"restore_cmd,omitempty"`
	AI          *Verdict `json:"ai,omitempty"`

	History []Decision `json:"history,omitempty"`
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,100}$`)

// validID は事件の ID として安全か (フォルダ名に使うので、区切りや .. を通さない)。
func ValidID(id string) bool { return idRE.MatchString(id) && !strings.Contains(id, "..") }

func (r *Record) Decide(at int64, by, action, note string) {
	r.History = append(r.History, Decision{At: at, By: by, Action: action, Note: note})
}
