package testutil

import (
	"database/sql/driver"
)

func ChatRows(user int64) FakeAnswer {
	// ChatFor は新しい順に読む
	return FakeAnswer{Has: "chat WHERE user = ?", Rows: [][]driver.Value{
		{int64(2), int64(1793757150), user, "二つ目"},
		{int64(1), int64(1793757149), user, "一つ目"},
	}}
}
