// Package testkit holds the helpers shared by the tests of setup, shadow, trader and sim (tests/helpers.py):
// synthetic order books (a price path and one density), the pinned parameters, and a deterministic Sched.
package testkit

import (
	"io"
	"log"
	"sort"
	"testing"
	"time"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	"densitybounce/internal/trader"
)

// Tick is the tick size of the synthetic books.
const Tick = 0.0001

// Pt is one point of a price path: {seconds before now, mid}.
type Pt [2]float64

type opts struct {
	usd, age, turnover, depth, atr float64
	sym                            string
	bars                           bool
	nBars                          int
}

// Opt tunes MakeBook.
type Opt func(*opts)

// USD is the density size.
func USD(x float64) Opt { return func(o *opts) { o.usd = x } }

// Age is the density age in seconds.
func Age(x float64) Opt { return func(o *opts) { o.age = x } }

// Turnover is the USDT per minute of turnover.
func Turnover(x float64) Opt { return func(o *opts) { o.turnover = x } }

// Depth is the qty every 5 ticks behind the density.
func Depth(x float64) Opt { return func(o *opts) { o.depth = x } }

// ATR is the range of the seeded 1m bars.
func ATR(x float64) Opt { return func(o *opts) { o.atr = x } }

// Sym is the symbol name.
func Sym(s string) Opt { return func(o *opts) { o.sym = s } }

// NoBars leaves the book without 1m bars.
func NoBars() Opt { return func(o *opts) { o.bars = false } }

// NBars is the number of complete 1m bars.
func NBars(n int) Opt { return func(o *opts) { o.nBars = n } }

// MakeBook builds a book for a price path (oldest first, the last mid is the current one) and one density.
// depth = qty every 5 ticks up to 1 % beyond the density (what a stop behind it fills against). Bars: nBars
// complete 1m bars of range atr centred on the last mid (so the ATR exists, 2 x ATR stays below the 0.15 % stop
// floor for the default range, and they never become the extreme of the move).
func MakeBook(path []Pt, densPrice float64, side book.Side, now float64, os ...Opt) *book.Book {
	o := opts{usd: 100_000, age: 100, turnover: 500_000, depth: 1000, atr: 0.0002, sym: "XUSDT", bars: true, nBars: 16}
	for _, f := range os {
		f(&o)
	}
	b := book.New(o.sym, Tick)
	for _, p := range path {
		b.PushMid(now-p[0], p[1])
	}
	mid := path[len(path)-1][1]
	if o.bars {
		cur := int32(now / 60)
		for m := cur - int32(o.nBars); m < cur; m++ {
			b.AppendBar(book.Bar{Min: m, Hi: b.T(mid + o.atr/2), Lo: b.T(mid - o.atr/2)})
		}
	}
	m := b.T(mid)
	b.SetSide(book.Bid, map[int32]float64{m - 1: 100, m - 2: 100})
	b.SetSide(book.Ask, map[int32]float64{m + 1: 100, m + 2: 100})
	k := b.T(densPrice)
	sgn := int32(1)
	behind := book.Bid
	if side == book.Ask {
		sgn, behind = -1, book.Ask
	}
	for i := int32(5); i < 101; i += 5 {
		b.SetLevel(behind, k-sgn*i, o.depth)
	}
	b.SetLevel(behind, k, o.usd/densPrice)
	b.Densities[book.DKey{Side: side, Tick: k}] = book.NewDensity(side, k, b.P(k), now-o.age, o.usd, o.usd, now)
	b.AddTurnover(now, o.turnover) // one print = this USDT per minute of turnover
	b.Synced, b.LastTs = true, now
	return b
}

// LongBook is a drop from 1.0400 (25 min ago) through 1.0140 (3 min ago) and 1.0070 (60 s ago) to 1.0030, onto a
// bid density at 1.0000.
func LongBook(now float64, os ...Opt) *book.Book {
	return MakeBook([]Pt{{1500, 1.0400}, {180, 1.0140}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, now, os...)
}

// ShortBook is the mirror: a rally from 0.9600 through 0.9860 and 0.9930 to 0.9970, into an ask density at 1.0000.
func ShortBook(now float64, os ...Opt) *book.Book {
	return MakeBook([]Pt{{1500, 0.9600}, {180, 0.9860}, {60, 0.9930}, {0, 0.9970}}, 1.0000, book.Ask, now, os...)
}

// Sells books one aggressive SELL of usd USDT printed at tick on the book's tape (it hits a bid density's side).
func Sells(b *book.Book, ts, usd float64, tick int32) {
	b.PushTrade(book.NewTradeAt(ts, tick, usd/(float64(tick)*b.TickSize), true))
}

// Buys books one aggressive BUY of usd USDT.
func Buys(b *book.Book, ts, usd float64, tick int32) {
	b.PushTrade(book.NewTradeAt(ts, tick, usd/(float64(tick)*b.TickSize), false))
}

// Print books one trade print of qty at tick.
func Print(b *book.Book, ts float64, tick int32, qty float64, buyerMaker bool) book.Trade {
	tr := book.NewTradeAt(ts, tick, qty, buyerMaker)
	b.PushTrade(tr)
	return tr
}

// PinP applies the pinned setup parameters (the tuned values the tests depend on; the defaults of counter_accel /
// mkt_max may change after a study) and restores P and book.DENS when the test ends.
func PinP(t testing.TB) {
	t.Helper()
	saved, dens := setup.P, book.DENS
	tk := book.TapeKeep
	t.Cleanup(func() { setup.P, book.DENS, book.TapeKeep = saved, dens, tk })
	p := &setup.P
	p.MinAge, p.ShadowAge, p.HoldMin, p.HoldCancel = 60, 20, 60, 30
	p.MktMax, p.MktSecs, p.CounterAccel, p.ConfirmSecs = 0.01, 1800, 1, 0
	p.Intact, p.StopATR, p.ATRN, p.TrendEMA, p.CancelAfter = 0.7, 2, 14, 20, 180
}

// SaveParams restores setup.P, book.DENS and book.TapeKeep when the test ends (without pinning anything).
func SaveParams(t testing.TB) {
	t.Helper()
	saved, dens, tk := setup.P, book.DENS, book.TapeKeep
	t.Cleanup(func() { setup.P, book.DENS, book.TapeKeep = saved, dens, tk })
}

// PinRisk applies the pinned risk limits and restores RISK when the test ends.
func PinRisk(t testing.TB) {
	t.Helper()
	saved := trader.RISK
	t.Cleanup(func() { trader.RISK = saved })
	r := &trader.RISK
	r.DayLossR, r.GuardN, r.GuardR, r.WallCool, r.SymCooldown = 6, 30, -0.3, 1800, 60
}

// Silence stops the trader's log output.
func Silence() { log.SetOutput(io.Discard) }

// Sched is a deterministic trader.Sched: Go queues, After sets a timer on the Clock; Settle runs the queued
// work, Advance moves the clock and runs the timers that are due.
type Sched struct {
	Clock  float64
	q      []func()
	timers []timer
	seq    int
}

type timer struct {
	at  float64
	seq int
	fn  func()
}

// Go queues fn.
func (s *Sched) Go(fn func()) { s.q = append(s.q, fn) }

// After runs fn d from the Clock.
func (s *Sched) After(d time.Duration, fn func()) {
	s.seq++
	s.timers = append(s.timers, timer{s.Clock + d.Seconds(), s.seq, fn})
}

// Settle runs the queued work (and what it queues) until nothing is left.
func (s *Sched) Settle() {
	for len(s.q) > 0 {
		fn := s.q[0]
		s.q = s.q[1:]
		fn()
	}
}

// Advance moves the Clock by secs and runs the due timers in order, settling after each.
func (s *Sched) Advance(secs float64) {
	s.AdvanceTo(s.Clock + secs)
}

// AdvanceTo moves the Clock to at and runs the due timers in order, settling after each.
func (s *Sched) AdvanceTo(at float64) {
	for {
		sort.SliceStable(s.timers, func(i, j int) bool {
			if s.timers[i].at != s.timers[j].at {
				return s.timers[i].at < s.timers[j].at
			}
			return s.timers[i].seq < s.timers[j].seq
		})
		if len(s.timers) == 0 || s.timers[0].at > at {
			break
		}
		tm := s.timers[0]
		s.timers = s.timers[1:]
		if tm.at > s.Clock {
			s.Clock = tm.at
		}
		tm.fn()
		s.Settle()
	}
	if at > s.Clock {
		s.Clock = at
	}
	s.Settle()
}

// Pending is the number of timers not yet run.
func (s *Sched) Pending() int { return len(s.timers) }
