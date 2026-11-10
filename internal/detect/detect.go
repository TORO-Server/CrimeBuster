package detect

import (
	"crime-buster/internal/config"
	"fmt"
)

// CoreProtect の coreprotect_block の action
const (
	actBreak = 0
	actPlace = 1
	actClick = 2
)

// Event は coreprotect_block の 1 行。
type Event struct {
	RowID   int64
	Time    int64 // unix 秒
	User    int
	Wid     int
	X, Y, Z int
	Type    int
	Action  int
}

// Trigger は、人の手では出せない速さでブロックを変えているのを見つけたとき。
type Trigger struct {
	User   int
	Time   int64
	Kind   string // "burst-break" / "burst-place" / "sustained-break"
	Breaks int
	Places int
	Window int // 秒
}

func (t Trigger) String() string {
	return fmt.Sprintf("%s: 壊 %d / 置 %d (%d 秒)", t.Kind, t.Breaks, t.Places, t.Window)
}

// Detector は、プレイヤーごとの 1 秒ごとの数を窓の長さだけ持って、窓の合計が閾値を超えたら知らせる。
type Detector struct {
	burst, sustained int // 窓 (秒)
	breakBurst       int
	placeBurst       int
	sustainedBreaks  int
	users            map[int]*userWin
}

type slot struct {
	sec    int64
	breaks int
	places int
}

type userWin struct {
	slots     []slot // sustained 秒ぶんの輪
	last      int64
	sumBreaks int // 輪の全部の壊した数
}

func NewDetector(c *config.Config) *Detector {
	burst := int(c.DetectConfig.BurstWindow.Seconds())
	if burst < 1 {
		burst = 1
	}
	sus := int(c.DetectConfig.SustainedWindow.Seconds())
	if sus < burst {
		sus = burst
	}
	return &Detector{
		burst: burst, sustained: sus,
		breakBurst: c.DetectConfig.BreakBurst, placeBurst: c.DetectConfig.PlaceBurst, sustainedBreaks: c.DetectConfig.SustainedBreaks,
		users: map[int]*userWin{},
	}
}

// Feed は 1 つの出来事を入れる。閾値を超えた瞬間だけ Trigger を返す (超えたままのあいだは返し続ける。
// 同じ人の続きの扱いは呼ぶ側で決める)。時刻がさかのぼった行は、窓の中なら足し、外なら捨てる。
func (d *Detector) Feed(e Event) *Trigger {
	if e.Action != actBreak && e.Action != actPlace {
		return nil
	}
	u := d.users[e.User]
	if u == nil {
		u = &userWin{slots: make([]slot, d.sustained)}
		d.users[e.User] = u
	}
	if e.Time > u.last {
		d.advance(u, e.Time)
	} else if u.last-e.Time >= int64(d.sustained) {
		return nil
	}
	s := &u.slots[int(e.Time%int64(d.sustained))]
	if s.sec != e.Time {
		u.sumBreaks -= s.breaks
		*s = slot{sec: e.Time}
	}
	if e.Action == actBreak {
		s.breaks++
		u.sumBreaks++
	} else {
		s.places++
	}

	breaks, places := 0, 0
	for i := 0; i < d.burst; i++ {
		sec := u.last - int64(i)
		s := u.slots[int(sec%int64(d.sustained))]
		if s.sec == sec {
			breaks += s.breaks
			places += s.places
		}
	}
	switch {
	case breaks >= d.breakBurst:
		return &Trigger{User: e.User, Time: e.Time, Kind: "burst-break", Breaks: breaks, Places: places, Window: d.burst}
	case places >= d.placeBurst:
		return &Trigger{User: e.User, Time: e.Time, Kind: "burst-place", Breaks: breaks, Places: places, Window: d.burst}
	case u.sumBreaks >= d.sustainedBreaks:
		return &Trigger{User: e.User, Time: e.Time, Kind: "sustained-break", Breaks: u.sumBreaks, Window: d.sustained}
	}
	return nil
}

// advance は時計を now まで進めて、窓から出た秒の輪を空にする。
func (d *Detector) advance(u *userWin, now int64) {
	if u.last == 0 {
		u.last = now
		return
	}
	gap := now - u.last
	if gap >= int64(d.sustained) {
		for i := range u.slots {
			u.slots[i] = slot{}
		}
		u.sumBreaks = 0
		u.last = now
		return
	}
	for sec := u.last + 1; sec <= now; sec++ {
		s := &u.slots[int(sec%int64(d.sustained))]
		u.sumBreaks -= s.breaks
		*s = slot{}
	}
	u.last = now
}

// Reset はその人の数え直し (事件を閉じたあと、同じ数で続けて鳴らないように)。
func (d *Detector) Reset(user int) { delete(d.users, user) }
