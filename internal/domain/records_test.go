package domain

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestRecordTransition は、状態の遷移表の全組み合わせを確かめる。
// 許可した遷移では State と Reported が同期し、拒否した遷移ではどちらも変わらない。
func TestRecordTransition(t *testing.T) {
	states := []IncidentState{StatePending, StateReported, StateDismissed}
	allowed := map[[2]IncidentState]bool{
		{StatePending, StateReported}:  true,
		{StatePending, StateDismissed}: true,
	}
	for _, from := range states {
		for _, to := range states {
			r := &Record{State: from, Reported: from == StateReported}
			err := r.Transition(to)
			if allowed[[2]IncidentState{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s: 許可した遷移が失敗した: %v", from, to, err)
					continue
				}
				if r.State != to {
					t.Errorf("%s -> %s: State が %s になった", from, to, r.State)
				}
				if want := to == StateReported; r.Reported != want {
					t.Errorf("%s -> %s: Reported が %v (want %v)", from, to, r.Reported, want)
				}
			} else {
				if err == nil {
					t.Errorf("%s -> %s: 許可していない遷移が成功した", from, to)
				}
				if r.State != from || r.Reported != (from == StateReported) {
					t.Errorf("%s -> %s: 拒否したのに状態が変わった: %+v", from, to, r)
				}
			}
		}
	}
	// 空の状態からも、未知の状態へも、遷移できない
	r := &Record{}
	if err := r.Transition(StateReported); err == nil {
		t.Error("空の状態からの遷移が成功した")
	}
	r = &Record{State: StatePending}
	if err := r.Transition(IncidentState("unknown")); err == nil {
		t.Error("未知の状態への遷移が成功した")
	}
	if r.State != StatePending || r.Reported {
		t.Errorf("拒否したのに状態が変わった: %+v", r)
	}
}

// TestRecordJSONFormat は、record.json の形式 (キーと値) が従来どおりであることを確かめる。
func TestRecordJSONFormat(t *testing.T) {
	for _, tc := range []struct {
		state    IncidentState
		reported bool
	}{
		{StatePending, false},
		{StateReported, true},
		{StateDismissed, false},
	} {
		r := &Record{
			ID: "x1", Kind: "block", Name: "p", UUID: "u", UserID: 7,
			Created: 1, Start: 2, End: 3, Trigger: "burst",
			State: tc.state, Reported: tc.reported,
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(`{"id":"x1","kind":"block","name":"p","uuid":"u","user_id":7,"created":1,"start":2,"end":3,"trigger":"burst","state":%q,"reported":%v,"rolled_back":false}`, string(tc.state), tc.reported)
		if string(b) != want {
			t.Errorf("state %s: JSON が従来と違う\n got: %s\nwant: %s", tc.state, b, want)
		}
		// 従来形式の JSON を読み直しても、同じ状態になる
		var back Record
		if err := json.Unmarshal([]byte(want), &back); err != nil {
			t.Fatal(err)
		}
		if back.State != tc.state || back.Reported != tc.reported {
			t.Errorf("state %s: 読み直しで変わった: %+v", tc.state, back)
		}
	}
}
