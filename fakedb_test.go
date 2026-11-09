package main

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

// fakeDB は database/sql のドライバーの代わり。クエリの文字列の一部で、返す行を決める (CP の試験用)。
type fakeDB struct {
	mu      sync.Mutex
	answers []fakeAnswer
	asked   []fakeQuery
}

type fakeAnswer struct {
	has  string // クエリにこの文字列があれば
	rows [][]driver.Value
	err  error
}

type fakeQuery struct {
	q    string
	args []driver.Value
}

func newFakeCP(t *testing.T, answers ...fakeAnswer) (*CP, *fakeDB) {
	t.Helper()
	f := &fakeDB{answers: answers}
	db := sql.OpenDB(f)
	t.Cleanup(func() { db.Close() })
	return &CP{db: db, p: "coreprotect_", users: map[int]UserInfo{}, worlds: map[int]string{}, mats: map[int]string{}}, f
}

// set は、has の答えを差し替える (無ければ足す)。
func (f *fakeDB) set(a fakeAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.answers {
		if f.answers[i].has == a.has {
			f.answers[i] = a
			return
		}
	}
	f.answers = append(f.answers, a)
}

func (f *fakeDB) find(has string) []fakeQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeQuery
	for _, q := range f.asked {
		if strings.Contains(q.q, has) {
			out = append(out, q)
		}
	}
	return out
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return f }
func (f *fakeDB) Open(string) (driver.Conn, error)             { return &fakeConn{db: f}, nil }

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{db: c.db, q: q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("読むだけ") }

type fakeStmt struct {
	db *fakeDB
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
	s.db.asked = append(s.db.asked, fakeQuery{q: s.q, args: args})
	for _, a := range s.db.answers {
		if strings.Contains(s.q, a.has) {
			if a.err != nil {
				return nil, a.err
			}
			return &fakeRows{rows: a.rows}, nil
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
