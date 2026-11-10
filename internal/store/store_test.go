package store

import (
	"testing"
)

func TestIncidentKeys(t *testing.T) {
	if (&Incident{UserID: 7}).Key() != "7" || (&Incident{UserID: 7, Kind: "chat"}).Key() != "chat:7" {
		t.Fatal("事件の鍵は、ブロックとチャットで別")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.S.Cursor = 42
	st.S.Incidents["7"] = &Incident{ID: "a", UserID: 7, Name: "x", Start: 1, Last: 2}
	st.S.Threads["x"] = ThreadRef{ThreadID: "99", At: 5}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.S.Cursor != 42 || st2.S.Incidents["7"].Name != "x" || st2.S.Threads["x"].ThreadID != "99" {
		t.Fatalf("%+v", st2.S)
	}
}
