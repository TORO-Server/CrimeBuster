package testutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// FakeDB は database/sql のドライバーの代わり。クエリの文字列の一部で、返す行を決める (CP の試験用)。
type FakeDB struct {
	mu      sync.Mutex
	answers []FakeAnswer
	asked   []FakeQuery
}

type FakeAnswer struct {
	Has  string // クエリにこの文字列があれば
	Rows [][]driver.Value
	Err  error
}

type FakeQuery struct {
	Q    string
	Args []driver.Value
}

// NewFakeDB は、代わりのドライバーで開いたデータベースと、答えを差し替えられる FakeDB を返す。
func NewFakeDB(t *testing.T, answers ...FakeAnswer) (*sql.DB, *FakeDB) {
	t.Helper()
	f := &FakeDB{answers: answers}
	db := sql.OpenDB(f)
	t.Cleanup(func() { db.Close() })
	return db, f
}

// Set は、Has の答えを差し替える (無ければ足す)。
func (f *FakeDB) Set(a FakeAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.answers {
		if f.answers[i].Has == a.Has {
			f.answers[i] = a
			return
		}
	}
	f.answers = append(f.answers, a)
}

func (f *FakeDB) Find(has string) []FakeQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeQuery
	for _, q := range f.asked {
		if strings.Contains(q.Q, has) {
			out = append(out, q)
		}
	}
	return out
}

func (f *FakeDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: f}, nil }
func (f *FakeDB) Driver() driver.Driver                        { return f }
func (f *FakeDB) Open(string) (driver.Conn, error)             { return &fakeConn{db: f}, nil }

type fakeConn struct{ db *FakeDB }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{db: c.db, q: q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("読むだけ") }

type fakeStmt struct {
	db *FakeDB
	q  string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("読むだけ")
}
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.asked = append(s.db.asked, FakeQuery{Q: s.q, Args: args})
	for _, a := range s.db.answers {
		if strings.Contains(s.q, a.Has) {
			if a.Err != nil {
				return nil, a.Err
			}
			return &fakeRows{rows: a.Rows}, nil
		}
	}
	return &fakeRows{}, nil
}

type fakeRows struct {
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string {
	if len(r.rows) == 0 {
		return nil
	}
	return make([]string, len(r.rows[0]))
}
func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}
