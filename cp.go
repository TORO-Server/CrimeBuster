package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// CP は CoreProtect のデータ (TiDB) を読む。書き込みはしない (ロールバックは Minecraft のコンソールで行う)。
type CP struct {
	db *sql.DB
	p  string

	mu     sync.Mutex
	users  map[int]UserInfo
	worlds map[int]string
	mats   map[int]string
}

type UserInfo struct {
	ID   int
	Name string
	UUID string
}

func OpenCP(c *Config) (*CP, error) {
	q := url.Values{}
	q.Set("timeout", "10s")
	q.Set("readTimeout", "60s")
	q.Set("tidb_mem_quota_query", "2147483648")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?%s", c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName, q.Encode())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &CP{db: db, p: c.TablePrefix, users: map[int]UserInfo{}, worlds: map[int]string{}, mats: map[int]string{}}, nil
}

func (c *CP) Close() error { return c.db.Close() }

func (c *CP) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// MaxRowID は今ある最後の行。起動したときは、ここから後ろだけを見る (起動前のことは報告しない)。
func (c *CP) MaxRowID(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := c.db.QueryRowContext(ctx, "SELECT MAX(rowid) FROM "+c.p+"block").Scan(&n)
	return n.Int64, err
}

// Poll は after より後ろの行を、rowid 順に最大 limit 件返す。
func (c *CP) Poll(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT rowid, time, user, wid, x, y, z, type, action FROM "+c.p+"block WHERE rowid > ? ORDER BY rowid LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t, u, w, x, y, z, ty, a sql.NullInt64
		if err := rows.Scan(&e.RowID, &t, &u, &w, &x, &y, &z, &ty, &a); err != nil {
			return nil, err
		}
		e.Time, e.User, e.Wid = t.Int64, int(u.Int64), int(w.Int64)
		e.X, e.Y, e.Z, e.Type, e.Action = int(x.Int64), int(y.Int64), int(z.Int64), int(ty.Int64), int(a.Int64)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RowIDAtTime は、時刻 ts 以降で最初の行の rowid。行は時刻順に増えるので、主キーで二分探索する (時刻だけの索引は無い)。
func (c *CP) RowIDAtTime(ctx context.Context, ts int64) (int64, error) {
	hi, err := c.MaxRowID(ctx)
	if err != nil {
		return 0, err
	}
	lo := int64(1)
	for lo < hi {
		mid := (lo + hi) / 2
		var rid, t sql.NullInt64
		err := c.db.QueryRowContext(ctx, "SELECT rowid, time FROM "+c.p+"block WHERE rowid >= ? ORDER BY rowid LIMIT 1", mid).Scan(&rid, &t)
		if err == sql.ErrNoRows {
			hi = mid
			continue
		}
		if err != nil {
			return 0, err
		}
		if t.Int64 >= ts {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}

// EventsFor は 1 人の [from, to] の変更を、rowid 順に返す (replay で、閾値に当てはめて確かめるため)。
func (c *CP) EventsFor(ctx context.Context, user int, from, to int64) ([]Event, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT rowid, time, user, wid, x, y, z, type, action FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? ORDER BY rowid LIMIT 1000000", user, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t, u, w, x, y, z, ty, a sql.NullInt64
		if err := rows.Scan(&e.RowID, &t, &u, &w, &x, &y, &z, &ty, &a); err != nil {
			return nil, err
		}
		e.Time, e.User, e.Wid = t.Int64, int(u.Int64), int(w.Int64)
		e.X, e.Y, e.Z, e.Type, e.Action = int(x.Int64), int(y.Int64), int(z.Int64), int(ty.Int64), int(a.Int64)
		out = append(out, e)
	}
	return out, rows.Err()
}

// User は rowid から名前と UUID を引く (覚えておく)。
func (c *CP) User(ctx context.Context, id int) (UserInfo, error) {
	c.mu.Lock()
	if u, ok := c.users[id]; ok {
		c.mu.Unlock()
		return u, nil
	}
	c.mu.Unlock()
	var name, uuid sql.NullString
	err := c.db.QueryRowContext(ctx, "SELECT user, uuid FROM "+c.p+"user WHERE rowid = ?", id).Scan(&name, &uuid)
	if err != nil {
		return UserInfo{}, err
	}
	u := UserInfo{ID: id, Name: name.String, UUID: uuid.String}
	c.mu.Lock()
	c.users[id] = u
	c.mu.Unlock()
	return u, nil
}

// UserByName は名前から引く (replay 用)。
func (c *CP) UserByName(ctx context.Context, name string) (UserInfo, error) {
	var id int
	var uuid sql.NullString
	err := c.db.QueryRowContext(ctx, "SELECT rowid, uuid FROM "+c.p+"user WHERE user = ? ORDER BY rowid DESC LIMIT 1", name).Scan(&id, &uuid)
	if err != nil {
		return UserInfo{}, err
	}
	return UserInfo{ID: id, Name: name, UUID: uuid.String}, nil
}

// World は co_world の id から名前を引く。同じ id の行が 2 つあることがあるので、まとめて 1 つにする。
func (c *CP) World(ctx context.Context, id int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.worlds) == 0 {
		rows, err := c.db.QueryContext(ctx, "SELECT id, MIN(world) FROM "+c.p+"world GROUP BY id")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var i int
				var w sql.NullString
				if rows.Scan(&i, &w) == nil {
					c.worlds[i] = w.String
				}
			}
		}
	}
	return c.worlds[id]
}

// Material は type の id から "minecraft:stone" を引く。
func (c *CP) Material(ctx context.Context, id int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.mats) == 0 {
		rows, err := c.db.QueryContext(ctx, "SELECT id, MIN(material) FROM "+c.p+"material_map GROUP BY id")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var i int
				var m sql.NullString
				if rows.Scan(&i, &m) == nil {
					c.mats[i] = m.String
				}
			}
		}
	}
	return c.mats[id]
}

// weCommandLike は、大きな編集をする道具のコマンド。これを最近使った人は、速いのが当たり前なので検知しない。
var weCommandLike = []string{"//%", "/we %", "/worldedit%", "/fill %", "/clone %", "/setblock %", "/br %", "/brush%", "/tool%", "/replacenear%"}

// RecentEditCommand は since 以降に、大きな編集のコマンドを使っていたら、その 1 つを返す。
func (c *CP) RecentEditCommand(ctx context.Context, user int, since int64) (string, error) {
	conds := make([]string, len(weCommandLike))
	args := []any{user, since}
	for i, l := range weCommandLike {
		conds[i] = "message LIKE ?"
		args = append(args, l)
	}
	var msg sql.NullString
	err := c.db.QueryRowContext(ctx,
		"SELECT message FROM "+c.p+"command WHERE user = ? AND time >= ? AND ("+strings.Join(conds, " OR ")+") LIMIT 1", args...).Scan(&msg)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return msg.String, err
}

type MatCount struct {
	Material string `json:"material"`
	Count    int    `json:"count"`
}

type Column struct {
	X, Z, N int
	Y0, Y1  int // 壊した高さの最低と最高
}

// Stats は 1 人の [from, to] のブロックの変更のまとめ。
type Stats struct {
	User       UserInfo
	World      string
	Wid        int
	From, To   int64 // 実際の最初と最後の変更
	Breaks     int
	Places     int
	Clicks     int
	PeakPerS   int
	PeakAt     int64
	X0, X1     int
	Y0, Y1     int
	Z0, Z1     int
	TopBreaks  []MatCount
	Columns    []Column // 壊した場所 (x, z ごとの数)
	Commands   []string
	Containers int
}

func (s *Stats) Empty() bool { return s.Breaks+s.Places+s.Clicks == 0 }

func (c *CP) Stats(ctx context.Context, user UserInfo, from, to int64) (*Stats, error) {
	s := &Stats{User: user}
	// 動作ごとの数と範囲、いちばん多い世界
	rows, err := c.db.QueryContext(ctx,
		"SELECT action, COUNT(*), MIN(time), MAX(time) FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? GROUP BY action", user.ID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, n int
		var t0, t1 int64
		if err := rows.Scan(&a, &n, &t0, &t1); err != nil {
			rows.Close()
			return nil, err
		}
		switch a {
		case actBreak:
			s.Breaks = n
		case actPlace:
			s.Places = n
		case actClick:
			s.Clicks = n
		}
		if s.From == 0 || t0 < s.From {
			s.From = t0
		}
		if t1 > s.To {
			s.To = t1
		}
	}
	rows.Close()
	if s.Empty() {
		return s, nil
	}

	err = c.db.QueryRowContext(ctx,
		"SELECT wid, COUNT(*) c FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? AND action IN (0,1) GROUP BY wid ORDER BY c DESC LIMIT 1", user.ID, from, to).Scan(&s.Wid, new(int))
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	s.World = c.World(ctx, s.Wid)

	var x0, x1, y0, y1, z0, z1 sql.NullInt64
	err = c.db.QueryRowContext(ctx,
		"SELECT MIN(x), MAX(x), MIN(y), MAX(y), MIN(z), MAX(z) FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? AND wid = ? AND action IN (0,1)", user.ID, from, to, s.Wid).
		Scan(&x0, &x1, &y0, &y1, &z0, &z1)
	if err != nil {
		return nil, err
	}
	s.X0, s.X1, s.Y0, s.Y1, s.Z0, s.Z1 = int(x0.Int64), int(x1.Int64), int(y0.Int64), int(y1.Int64), int(z0.Int64), int(z1.Int64)

	err = c.db.QueryRowContext(ctx,
		"SELECT time, COUNT(*) c FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? AND action = 0 GROUP BY time ORDER BY c DESC LIMIT 1", user.ID, from, to).Scan(&s.PeakAt, &s.PeakPerS)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	mrows, err := c.db.QueryContext(ctx,
		"SELECT type, COUNT(*) c FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? AND action = 0 GROUP BY type ORDER BY c DESC LIMIT 8", user.ID, from, to)
	if err != nil {
		return nil, err
	}
	type typeCount struct{ t, n int }
	var tops []typeCount
	for mrows.Next() {
		var tc typeCount
		if err := mrows.Scan(&tc.t, &tc.n); err != nil {
			mrows.Close()
			return nil, err
		}
		tops = append(tops, tc)
	}
	mrows.Close()
	// 名前を引くクエリは、行を読み終えて接続を返してから (接続の上限とキャッシュのロックで止まらないように)
	for _, tc := range tops {
		s.TopBreaks = append(s.TopBreaks, MatCount{Material: trimNS(c.Material(ctx, tc.t)), Count: tc.n})
	}

	crows, err := c.db.QueryContext(ctx,
		"SELECT x, z, COUNT(*), MIN(y), MAX(y) FROM "+c.p+"block WHERE user = ? AND time BETWEEN ? AND ? AND wid = ? AND action = 0 GROUP BY x, z LIMIT 300000", user.ID, from, to, s.Wid)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var col Column
		if err := crows.Scan(&col.X, &col.Z, &col.N, &col.Y0, &col.Y1); err != nil {
			crows.Close()
			return nil, err
		}
		s.Columns = append(s.Columns, col)
	}
	crows.Close()

	cmdrows, err := c.db.QueryContext(ctx,
		"SELECT message FROM "+c.p+"command WHERE user = ? AND time BETWEEN ? AND ? ORDER BY time LIMIT 20", user.ID, from, to)
	if err == nil {
		for cmdrows.Next() {
			var m sql.NullString
			if cmdrows.Scan(&m) == nil {
				s.Commands = append(s.Commands, m.String)
			}
		}
		cmdrows.Close()
	}
	// コンテナの出し入れ (盗みの目安)
	_ = c.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+c.p+"container WHERE user = ? AND time BETWEEN ? AND ?", user.ID, from, to).Scan(&s.Containers)
	return s, nil
}

// RolledBack は [from, to] の変更のうち、ロールバックされた数と全部の数。
// ロールバックはワールドを指定して行うので、数えるのも同じワールドだけ (world が空なら、すべて)。
func (c *CP) RolledBack(ctx context.Context, userID int, world string, from, to int64) (done, total int, err error) {
	q := "SELECT COALESCE(SUM(CASE WHEN rolled_back = 1 THEN 1 ELSE 0 END), 0), COUNT(*) FROM " + c.p + "block WHERE user = ? AND time BETWEEN ? AND ? AND action IN (0,1)"
	args := []any{userID, from, to}
	if world != "" {
		q += " AND wid IN (SELECT id FROM " + c.p + "world WHERE world = ?)"
		args = append(args, world)
	}
	err = c.db.QueryRowContext(ctx, q, args...).Scan(&done, &total)
	return
}

func trimNS(s string) string { return strings.TrimPrefix(s, "minecraft:") }

// TopBreakColumns は壊した場所を、数が多い順に n 個 (点の絵の確認に使う)。
func (s *Stats) TopBreakColumns(n int) []Column {
	cols := append([]Column(nil), s.Columns...)
	sort.Slice(cols, func(i, j int) bool { return cols[i].N > cols[j].N })
	if len(cols) > n {
		cols = cols[:n]
	}
	return cols
}
