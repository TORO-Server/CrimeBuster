package detect

import (
	"context"
	"crime-buster/internal/testutil"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

func TestChatForLimitsAndOrders(t *testing.T) {
	sqldb, db := testutil.NewFakeDB(t, testutil.ChatRows(9))
	cp := NewCP(sqldb, "coreprotect_")
	msgs, err := cp.ChatFor(context.Background(), 9, 1, 2)
	if err != nil || len(msgs) != 2 || msgs[0].Msg != "一つ目" {
		t.Fatalf("%+v %v", msgs, err)
	}
	q := db.Find("chat WHERE user = ?")[0]
	if !strings.Contains(q.Q, "LIMIT ?") || q.Args[3] != int64(maxChatFor) {
		t.Fatalf("%+v", q)
	}
}

func TestRolledBackCountsTheWorld(t *testing.T) {
	sqldb, db := testutil.NewFakeDB(t, testutil.FakeAnswer{Has: "rolled_back", Rows: [][]driver.Value{{int64(3), int64(4)}}})
	cp := NewCP(sqldb, "coreprotect_")
	done, total, err := cp.RolledBack(context.Background(), 7, "main", 1, 2)
	if err != nil || done != 3 || total != 4 {
		t.Fatal(done, total, err)
	}
	q := db.Find("rolled_back")[0]
	if !strings.Contains(q.Q, "coreprotect_world WHERE world = ?") || q.Args[3] != "main" {
		t.Fatalf("%+v", q)
	}
}

// 接続が 1 本でも、マテリアルの名前を引くところで止まらない。
func TestStatsDoesNotHoldRowsWhileLookingUpMaterial(t *testing.T) {
	sqldb, _ := testutil.NewFakeDB(t,
		testutil.FakeAnswer{Has: "SELECT action, COUNT(*)", Rows: [][]driver.Value{{int64(actBreak), int64(10), int64(1000), int64(1010)}}},
		testutil.FakeAnswer{Has: "SELECT wid, COUNT(*)", Rows: [][]driver.Value{{int64(1), int64(10)}}},
		testutil.FakeAnswer{Has: "world GROUP BY", Rows: [][]driver.Value{{int64(1), "main"}}},
		testutil.FakeAnswer{Has: "SELECT MIN(x)", Rows: [][]driver.Value{{int64(0), int64(10), int64(60), int64(70), int64(0), int64(10)}}},
		testutil.FakeAnswer{Has: "SELECT time, COUNT(*)", Rows: [][]driver.Value{{int64(1005), int64(5)}}},
		testutil.FakeAnswer{Has: "SELECT type, COUNT(*)", Rows: [][]driver.Value{{int64(3), int64(10)}}},
		testutil.FakeAnswer{Has: "material_map GROUP BY", Rows: [][]driver.Value{{int64(3), "minecraft:stone"}}},
	)
	cp := NewCP(sqldb, "coreprotect_")
	cp.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := cp.Stats(ctx, UserInfo{ID: 7, Name: "x"}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if s.World != "main" || len(s.TopBreaks) != 1 || s.TopBreaks[0].Material != "stone" {
		t.Fatalf("%+v", s)
	}
}

func TestSummaryLimitsCommands(t *testing.T) {
	s := &Stats{Commands: []string{"/x " + strings.Repeat("a", 500) + "\nnext"}}
	got := SummaryOf(s)["commands"].([]string)
	if len(got) != 1 || len([]rune(got[0])) > 121 || strings.Contains(got[0], "\n") {
		t.Fatalf("%q", got)
	}
}
