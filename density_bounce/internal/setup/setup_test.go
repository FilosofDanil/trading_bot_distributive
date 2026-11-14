package setup_test

import (
	"math"
	"strings"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	. "densitybounce/internal/testkit"
)

var NOW = 1000.0

var bidKey = book.DKey{Side: book.Bid, Tick: 10000}
var askKey = book.DKey{Side: book.Ask, Tick: 10000}

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol && math.Abs(got-want) > 1e-6*math.Abs(want) {
		t.Errorf("%s = %v, want %v (tol %v)", name, got, want, tol)
	}
}

func isNil(t *testing.T, name string, p *float64) {
	t.Helper()
	if p != nil {
		t.Errorf("%s = %v, want nil", name, *p)
	}
}

func val(t *testing.T, name string, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil", name)
	}
	return *p
}

func plan(b *book.Book, now float64) *setup.Plan { return setup.PlanBounce(b, now, nil, nil) }

func filterSide(b *book.Book, s book.Side, keep func(k int32) bool) {
	m := map[int32]float64{}
	b.RangeLevels(s, func(k int32, q float64) {
		if keep(k) {
			m[k] = q
		}
	})
	b.SetSide(s, m)
}

func bars(b *book.Book, now, rng float64, n int, mid float64) {
	cur := int32(now / 60)
	for m := cur - int32(n); m < cur; m++ {
		b.AppendBar(book.Bar{Min: m, Hi: b.T(mid + rng/2), Lo: b.T(mid - rng/2)})
	}
}

func hours(b *book.Book, closes []float64, lastHour int) {
	first := 0
	if lastHour >= 0 {
		first = lastHour - len(closes) + 1
	}
	for i, c := range closes {
		b.AppendH1(book.HourClose{Hour: int32(first + i), Close: b.T(c)})
	}
}

func series(start, step float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	return out
}

var (
	down = series(1.50, -0.02, 25) // 1h closes falling: last < EMA20 -> trend -1
	up   = series(1.00, 0.02, 25)
)

func TestLongEntryInFrontStopBehindTakeAtMiddleOfTheMove(t *testing.T) {
	PinP(t)
	p := plan(LongBook(NOW), NOW)
	if p == nil || p.Side != "long" {
		t.Fatalf("plan = %+v", p)
	}
	near(t, "entry", p.Entry, 1.0002, 0)
	near(t, "stop", p.Stop, 0.9985, 0)
	near(t, "tp", p.TP, 1.0200, 0)
	if p.DKey != bidKey {
		t.Errorf("dkey %v", p.DKey)
	}
	f := p.Features
	near(t, "move_start", f.MoveStart, 1.0400, 0)
	near(t, "move", f.Move, 0.0385, 1e-4)
	near(t, "approach", f.Approach, 0.0108, 1e-4)
	near(t, "move_s", f.MoveS, 1500, 0)
	near(t, "rr", f.RR, (1.0200-1.0002)/(1.0002-0.9985), 0.01)
	near(t, "tp1", p.TP1, 1.0019, 0) // half at +1R (risk 0.0017), no runner
	if p.TPRun != 0 {
		t.Errorf("tp_run %v", p.TPRun)
	}
	near(t, "atr", val(t, "atr", f.ATR), 0.0002/1.003, 1e-5)
	if f.Trend != nil { // tiny ATR, no hours
		t.Errorf("trend %v", *f.Trend)
	}
	near(t, "accel", val(t, "accel", f.Accel), 2.54, 0.02) // 3 min speed vs the move's
	if f.Counter {
		t.Error("counter")
	}
	isNil(t, "hold_s", f.HoldS) // no tape, no other coins
	isNil(t, "mkt", f.Mkt)
	if p.Skip != "" {
		t.Errorf("skip %q", p.Skip)
	}
}

func TestShortIsTheMirror(t *testing.T) {
	PinP(t)
	p := plan(ShortBook(NOW), NOW)
	if p == nil || p.Side != "short" {
		t.Fatalf("plan = %+v", p)
	}
	near(t, "entry", p.Entry, 0.9998, 0)
	near(t, "stop", p.Stop, 1.0015, 0)
	near(t, "tp", p.TP, 0.9800, 0)
	near(t, "tp1", p.TP1, 0.9981, 0)
	if p.TPRun != 0 {
		t.Errorf("tp_run %v", p.TPRun)
	}
}

func TestPartsWhenSwitchedOn(t *testing.T) {
	PinP(t)
	setup.P.TP1R, setup.P.TP1Frac, setup.P.RunFrac = 0, 0.25, 1.0
	p := plan(LongBook(NOW), NOW)
	if p == nil {
		t.Fatal("no plan")
	}
	near(t, "tp", p.TP, 1.0200, 0)
	near(t, "tp1", p.TP1, 1.0100, 0) // 25 % of the move
	near(t, "tp_run", p.TPRun, 1.0400, 0)
	p = plan(ShortBook(NOW), NOW)
	if p == nil {
		t.Fatal("no short plan")
	}
	near(t, "tp1", p.TP1, 0.9900, 0)
	near(t, "tp_run", p.TPRun, 0.9600, 0)
}

func TestABiggerMoveHasALongerTake(t *testing.T) {
	PinP(t)
	b := MakeBook([]Pt{{1500, 1.0800}, {180, 1.0140}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, NOW)
	p := plan(b, NOW)
	if p == nil {
		t.Fatal("no plan")
	}
	near(t, "tp", p.TP, 1.0400, 0)
}

func TestCoarseTickIsNotTraded(t *testing.T) {
	PinP(t)
	// tick 0.0001 at a price of 0.40 = 2.5 bp: a valid setup otherwise
	b := MakeBook([]Pt{{1500, 0.4200}, {180, 0.4060}, {60, 0.4028}, {0, 0.4012}}, 0.4000, book.Bid, NOW)
	if plan(b, NOW) != nil {
		t.Error("coarse tick traded")
	}
	setup.P.TickMax = 0.0004
	if plan(b, NOW) == nil {
		t.Error("tick_max 0.0004 should trade")
	}
}

func TestThinTurnoverIsNotTraded(t *testing.T) {
	PinP(t)
	if plan(LongBook(NOW, Turnover(99_000)), NOW) != nil {
		t.Error("99k traded")
	}
	if plan(LongBook(NOW, Turnover(101_000)), NOW) == nil {
		t.Error("101k not traded")
	}
}

func TestSlowApproachIsNotTraded(t *testing.T) {
	PinP(t)
	// a big move, but only 0.50 % of it in the last 3 min: the price drifts to the wall
	b := MakeBook([]Pt{{1500, 1.0400}, {180, 1.0080}, {60, 1.0060}, {0, 1.0030}}, 1.0000, book.Bid, NOW)
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestStalledApproachIsNotTraded(t *testing.T) {
	PinP(t)
	// 1.08 % in 3 min, but it has stopped: 0.05 % in the last 60 s
	b := MakeBook([]Pt{{1500, 1.0400}, {180, 1.0140}, {60, 1.0035}, {0, 1.0030}}, 1.0000, book.Bid, NOW)
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestNoHistoryForTheApproachIsNotTraded(t *testing.T) {
	PinP(t)
	b := MakeBook([]Pt{{60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, NOW) // just subscribed: no mid from 3 min ago
	b.AppendBar(book.Bar{Min: int32((NOW - 900) / 60), Hi: b.T(1.04), Lo: b.T(1.03)})
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestShortPreviousMoveIsNotTraded(t *testing.T) {
	PinP(t)
	// sharp (0.89 %/3 min) but the whole move is only 1.19 %: noise, not a move
	b := MakeBook([]Pt{{180, 1.0120}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, NOW)
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestAMoveOlderThanTheWindowDoesNotCount(t *testing.T) {
	PinP(t)
	// the 4 % high was 33 min ago; inside the 30 min window the move is only 1.96 %
	b := MakeBook([]Pt{{2000, 1.0400}, {1700, 1.0200}, {180, 1.0140}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, NOW)
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestSeededBarsMeasureAMoveOlderThanTheMidSamples(t *testing.T) {
	PinP(t)
	now := 100_000.0
	b := MakeBook([]Pt{{180, 1.0140}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, now)
	if plan(b, now) != nil { // 1.4 % seen: too short
		t.Fatal("traded on 1.4 %")
	}
	minute := int64((now - 1500) / 60)
	b.SeedBars([]book.Kline{{OpenMs: minute * 60_000, High: 1.0400, Low: 1.0300, Close: 1.03}})
	p := plan(b, now)
	if p == nil {
		t.Fatal("no plan after the seed")
	}
	near(t, "move_start", p.Features.MoveStart, 1.0400, 0)
	near(t, "tp", p.TP, 1.0200, 0)
}

func TestSeedKeepsOurOwnTapeAndGoesInFrontOfIt(t *testing.T) {
	b := book.New("XUSDT", 0.0001)
	b.OnTrade(120_000, 1.0000, 1, false)
	b.OnTrade(121_000, 1.0100, 1, false)
	b.SeedBars([]book.Kline{{OpenMs: 60_000, High: 1.05, Low: 0.95, Close: 1}, {OpenMs: 120_000, High: 9.00, Low: 0.10, Close: 1}}) // minute 2 is ours
	want := []book.Bar{{Min: 1, Hi: 10500, Lo: 9500}, {Min: 2, Hi: 10100, Lo: 10000}}
	got := b.Bars()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("bars %v, want %v", got, want)
	}
}

func TestYoungOrEatenDensityIsNotTraded(t *testing.T) {
	PinP(t)
	if plan(LongBook(NOW, Age(5)), NOW) != nil {
		t.Error("young traded")
	}
	b := LongBook(NOW)
	d := b.Densities[bidKey]
	d.USD = 0.5 * d.PeakUSD // half eaten already
	if plan(b, NOW) != nil {
		t.Error("eaten traded")
	}
}

func TestPriceTooFarFromDensityIsNotTraded(t *testing.T) {
	PinP(t)
	b := MakeBook([]Pt{{1500, 1.0400}, {180, 1.0200}, {60, 1.0110}, {0, 1.0060}}, 1.0000, book.Bid, NOW) // 0.6 % away
	if plan(b, NOW) != nil {
		t.Error("traded")
	}
}

func TestRewardRiskFilterAndSkipSet(t *testing.T) {
	PinP(t)
	setup.P.MinRR = 12.0 // reward 1.98 % / risk 0.17 % = 11.6
	if plan(LongBook(NOW), NOW) != nil {
		t.Error("rr 12 traded")
	}
	setup.P.MinRR = 1.0
	if setup.PlanBounce(LongBook(NOW), NOW, map[book.DKey]bool{bidKey: true}, nil) != nil {
		t.Error("skipped density traded")
	}
}

func TestExitsBreakEvenThenTime(t *testing.T) {
	PinP(t)
	b := LongBook(NOW) // mid 1.0030 = +1.65 R over the entry 1.0002
	p := &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.0050, DKey: bidKey}
	m := &setup.Manage{OpenedAt: NOW}
	exp := func(want string, entry, at float64) {
		t.Helper()
		if got := setup.ExitReason(b, p, entry, m, at); got != want {
			t.Errorf("ExitReason(entry %v, at %v) = %q, want %q", entry, at, got, want)
		}
	}
	exp("", 1.0002, NOW+60) // the default: break-even only when tp1 fills
	setup.P.BeAtR = 0.7
	exp("be", 1.0002, NOW+60) // the option: the bounce went, stop to break-even
	exp("", 1.0025, NOW+60)   // only +0.1 R from this entry
	m.BeDone = true
	delete(b.Densities, bidKey) // the wall is gone: no panic exit, the stop decides
	exp("", 1.0002, NOW+60)
	exp("time", 1.0002, NOW+setup.P.MaxHold+1)
	setup.P.ExitOnGone = true
	exp("", 1.0002, NOW+60)             // already +1.6 R: let it run
	exp("density_gone", 1.0030, NOW+60) // no progress yet
}

func TestBookATRTrendAndPriceAgo(t *testing.T) {
	PinP(t)
	b := LongBook(NOW, NoBars())
	if _, ok := b.ATR(NOW, 14); ok {
		t.Error("atr without bars")
	}
	bars(b, NOW, 0.004, 14, 1.0100)
	b.AppendBar(book.Bar{Min: int32(NOW / 60), Hi: b.T(1.2), Lo: b.T(0.8)}) // the current, unfinished minute does not count
	atr, ok := b.ATR(NOW, 14)
	if !ok {
		t.Fatal("no atr")
	}
	near(t, "atr", atr, 0.004, 1e-9)
	if _, ok := b.Trend(20); ok {
		t.Error("trend without hours")
	}
	hours(b, down, -1)
	if tr, ok := b.Trend(20); !ok || tr != -1 {
		t.Errorf("trend down = %v %v", tr, ok)
	}
	b.ClearH1()
	hours(b, up, -1)
	if tr, ok := b.Trend(20); !ok || tr != 1 {
		t.Errorf("trend up = %v %v", tr, ok)
	}
	if p, ok := b.PriceAgo(NOW, 60); !ok { // the mid samples
		t.Error("price_ago 60")
	} else {
		near(t, "price_ago(60)", p, 1.0070, 0)
	}
	if p, ok := b.PriceAgo(NOW, 600); !ok { // the newest sample at least 600 s old (1500 s)
		t.Error("price_ago 600")
	} else {
		near(t, "price_ago(600)", p, 1.0400, 0)
	}
	if _, ok := b.PriceAgo(NOW, 1800); ok { // older than the samples and every bar
		t.Error("price_ago 1800 should be none")
	}
	b.PrependBars([]book.Bar{{Min: int32(math.Floor((NOW-1800)/60)) - 1, Hi: b.T(1.06), Lo: b.T(1.04)}})
	if p, ok := b.PriceAgo(NOW, 1800); !ok {
		t.Error("price_ago 1800 after the bar")
	} else {
		near(t, "price_ago(1800)", p, 1.05, 0)
	}
}

func TestSeedH1KeepsOurOwnHours(t *testing.T) {
	b := book.New("XUSDT", 0.0001)
	b.OnTrade(7_200_500, 1.0100, 1, false)                                                     // hour 2
	b.SeedH1([]book.Kline{{OpenMs: 3_600_000, Close: 1.05}, {OpenMs: 7_200_000, Close: 9.99}}) // hour 2 is ours
	want := []book.HourClose{{Hour: 1, Close: 10500}, {Hour: 2, Close: 10100}}
	got := b.H1()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("h1 %v, want %v", got, want)
	}
}

func TestStopIsTwoATRBehindTheDensity(t *testing.T) {
	PinP(t)
	p := plan(LongBook(NOW, ATR(0.006)), NOW) // ATR 0.6 % > the 0.15 % floor
	if p == nil {
		t.Fatal("no plan")
	}
	near(t, "stop", p.Stop, 1.0000-0.0120, 0)
	near(t, "tp1", p.TP1, 1.0002+(1.0002-0.9880), 0) // +1R from the entry
	near(t, "atr", val(t, "atr", p.Features.ATR), 0.006/1.003, 1e-5)
	near(t, "stop_atr", val(t, "stop_atr", p.Features.StopATR), 2.03, 0.01)
	// a quiet coin: 2 x ATR < 0.15 %, the floor stays
	near(t, "quiet stop", plan(LongBook(NOW, ATR(0.0004)), NOW).Stop, 0.9985, 0)
	big := []Pt{{1500, 1.0800}, {180, 1.0140}, {60, 1.0070}, {0, 1.0030}} // an 8 % move: take 1.0400
	b3 := MakeBook(big, 1.0000, book.Bid, NOW, ATR(0.010))                // 2 x ATR = 2 %: wide, but inside risk_max 4 %
	near(t, "wide stop", plan(b3, NOW).Stop, 0.9800, 0)
	b4 := MakeBook(big, 1.0000, book.Bid, NOW, ATR(0.025)) // 5 %: more than risk_max
	setup.P.MinRR = 0.0
	if plan(b4, NOW) != nil {
		t.Error("5 % stop traded")
	}
	setup.P.StopATR = 0.0
	near(t, "bare stop", plan(b4, NOW).Stop, 0.9985, 0)
}

const (
	T0 = 100_000.0 // the move starts at T0 - 1500 = 98 500: hour 26 closed before it
	H0 = 26        // the last hour that closed before the move began
)

func counterBook(now, start float64) *book.Book {
	return MakeBook([]Pt{{1500, start}, {180, 1.0140}, {60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, now)
}

func TestAgainstThe1hTrendOnlyAfterASharpApproach(t *testing.T) {
	PinP(t)
	setup.P.CounterAccel = 1.0
	// 1.20 -> 1.003 in 25 min = 16.4 %: the last 3 min (1.08 %) run at 0.55 x the speed of the move
	b := counterBook(T0, 1.20)
	hours(b, down, H0) // a long into a falling 1h trend: counter
	p, ghost := setup.Scan(b, T0, nil, nil)
	if p != nil || ghost == nil || ghost.Skip != "counter_slow" { // not dropped: a shadow plan
		t.Fatalf("p %v ghost %+v", p, ghost)
	}
	if !ghost.Features.Counter {
		t.Error("counter")
	}
	near(t, "accel", val(t, "accel", ghost.Features.Accel), 0.55, 0.02)
	if plan(b, T0) != nil {
		t.Error("plan_bounce traded")
	}
	b.ClearH1()
	hours(b, up, H0) // the same entry with the trend: fine
	p = plan(b, T0)
	if p == nil || p.Features.Counter {
		t.Fatalf("with the trend: %+v", p)
	}
	near(t, "accel", val(t, "accel", p.Features.Accel), 0.55, 0.02)
	// 1.05: the move is 4.5 %, accel 2.0: a sharp approach, counter allowed
	b = counterBook(T0, 1.05)
	hours(b, down, H0)
	p = plan(b, T0)
	if p == nil || !p.Features.Counter || p.Features.Trend == nil || *p.Features.Trend != -1 || p.Skip != "" {
		t.Fatalf("sharp counter: %+v", p)
	}
	near(t, "accel", val(t, "accel", p.Features.Accel), 2.02, 0.02)
	setup.P.CounterAccel = 0.0 // the filter off
	b = counterBook(T0, 1.20)
	hours(b, down, H0)
	if plan(b, T0) == nil {
		t.Error("filter off should trade")
	}
	setup.P.CounterAccel = 2.5 // a stricter bar turns the 4.5 % move into a shadow plan too
	b = counterBook(T0, 1.05)
	hours(b, down, H0)
	if _, g := setup.Scan(b, T0, nil, nil); g == nil || g.Skip != "counter_slow" {
		t.Errorf("stricter bar: %+v", g)
	}
}

func TestCounterIsJudgedByTheTrendBeforeTheMoveBegan(t *testing.T) {
	PinP(t)
	setup.P.CounterAccel = 1.0
	b := counterBook(T0, 1.20)
	hours(b, down, H0)                                        // falling before the move: counter
	b.AppendH1(book.HourClose{Hour: H0 + 1, Close: b.T(3.0)}) // an hour that closes after the move began (28 * 3600 > 98 500)
	tr := func(before float64, n int) (int, bool) { return b.TrendBefore(n, before) }
	if v, ok := b.Trend(20); !ok || v != 1 { // measured now, the last close is far above the EMA
		t.Errorf("trend now %v", v)
	}
	if v, ok := tr(T0-1500, 20); !ok || v != -1 { // measured before the move: the hour does not count
		t.Errorf("trend before the move %v", v)
	}
	if _, g := setup.Scan(b, T0, nil, nil); g == nil || g.Skip != "counter_slow" {
		t.Errorf("ghost %+v", g)
	}
	if v, ok := tr((H0+1)*3600, 20); !ok || v != -1 { // the close of hour H0 + 1 is at (H0 + 2) * 3600: not yet
		t.Errorf("before (H0+1)*3600: %v", v)
	}
	if v, ok := tr((H0+2)*3600, 20); !ok || v != 1 {
		t.Errorf("before (H0+2)*3600: %v", v)
	}
	if _, ok := tr(T0, 30); ok { // fewer closes than the EMA length
		t.Error("30 closes")
	}
}

func TestAccelIsNoneWithoutHistoryOrWhenTheMoveIsNotTowardTheDensity(t *testing.T) {
	PinP(t)
	b := counterBook(T0, 1.20)
	a, ok := setup.Accel(b, T0, 1.0030, 1, 1.20, T0-1500)
	if !ok {
		t.Fatal("accel none")
	}
	near(t, "accel", a, 0.55, 0.02)
	if _, ok := setup.Accel(b, T0, 1.0030, 1, 1.0020, T0-1500); ok { // start below mid: no move toward it
		t.Error("start below mid")
	}
	if _, ok := setup.Accel(b, T0, 1.0030, -1, 1.20, T0-1500); ok { // the other side: moving away
		t.Error("moving away")
	}
	young := MakeBook([]Pt{{60, 1.0070}, {0, 1.0030}}, 1.0000, book.Bid, T0)
	if _, ok := setup.Accel(young, T0, 1.0030, 1, 1.04, T0-1500); ok { // no mid from 3 min ago
		t.Error("no history")
	}
	// a move shorter than approach_secs is judged over its own length: a straight drop is 1
	s := MakeBook([]Pt{{120, 1.0300}, {60, 1.0150}, {0, 1.0000}}, 0.99, book.Bid, T0)
	a, ok = setup.Accel(s, T0, 1.0000, 1, 1.0300, T0-120)
	if !ok {
		t.Fatal("straight drop none")
	}
	near(t, "straight drop", a, 1.0, 0.01)
}

func TestCounterEntryIsCancelledWhenTheApproachStalls(t *testing.T) {
	PinP(t)
	setup.P.CancelAfter = 600.0
	setup.P.CounterAccel = 1.0
	b := counterBook(T0, 1.05)
	hours(b, down, H0)
	p := plan(b, T0)
	if p == nil || !p.Features.Counter || p.Features.MoveStart != 1.05 {
		t.Fatalf("plan %+v", p)
	}
	if got := setup.CancelReason(b, p, T0, T0); got != "" {
		t.Errorf("reason %q", got)
	}
	b.PushMid(T0+100, 1.0031)
	b.PushMid(T0+300, 1.0030) // 3 min without progress toward the density
	if got := setup.CancelReason(b, p, T0, T0+300); got != "counter_stalled" {
		t.Errorf("reason %q", got)
	}
	p.Features.Counter = false // with the trend the stall is no reason
	if got := setup.CancelReason(b, p, T0, T0+300); got != "" {
		t.Errorf("reason %q", got)
	}
}

func TestADensityBeingEatenIsNotTradedAndCancelsTheEntry(t *testing.T) {
	PinP(t)
	b := LongBook(NOW)
	d := b.Densities[bidKey]
	p := plan(b, NOW)
	if p == nil {
		t.Fatal("no plan")
	}
	d.AddHist(NOW-8, 100_000)
	d.AddHist(NOW-4, 90_000)
	d.USD = 78_000 // -22 % in 8 s, still >= 70 % of the peak
	near(t, "drop", d.Drop(NOW, 10), 0.22, 0)
	if plan(b, NOW) != nil {
		t.Error("eaten traded")
	}
	if got := setup.CancelReason(b, p, NOW-1, NOW); got != "density_eaten" {
		t.Errorf("reason %q", got)
	}
	if d.Drop(NOW+20, 10) != 0 { // the drop is older than 10 s
		t.Error("old drop")
	}
	setup.P.EatCancel = 0.0
	if plan(b, NOW) == nil {
		t.Error("eat_cancel 0 should trade")
	}
}

func TestStopDepthCountsTheBookBeyondTheStop(t *testing.T) {
	PinP(t)
	p := plan(LongBook(NOW, Depth(10)), NOW)
	// stop 0.9985, band 0.3 % = 30 ticks: levels every 5 ticks behind the density at 9985 .. 9955 -> 7 x 10
	near(t, "long depth", setup.StopDepth(LongBook(NOW, Depth(10)), p), 70, 0)
	ps := plan(ShortBook(NOW, Depth(10)), NOW)
	near(t, "short depth", setup.StopDepth(ShortBook(NOW, Depth(10)), ps), 70, 0)
}

func TestParamsOverride(t *testing.T) {
	PinP(t)
	if err := setup.LoadParams(`{"take_frac": 0.618, "exit_on_gone": true, "usd_min": 80000, "be_on_tp1": true, "atr_n": 10}`); err != nil {
		t.Fatal(err)
	}
	if !setup.P.BeOnTP1 || setup.P.ATRN != 10 {
		t.Errorf("P %+v", setup.P)
	}
	if setup.P.TakeFrac != 0.618 || !setup.P.ExitOnGone {
		t.Errorf("P %+v", setup.P)
	}
	if book.DENS.USDMin != 80000.0 { // density keys go to book.DENS
		t.Errorf("DENS %+v", book.DENS)
	}
	err := setup.LoadParams(`{"tak_frac": 0.5}`)
	if err == nil || !strings.Contains(err.Error(), "unknown keys ['tak_frac']") || !strings.Contains(err.Error(), "'take_frac'") ||
		!strings.Contains(err.Error(), "'usd_min'") {
		t.Errorf("unknown key error: %v", err)
	}
}

func TestParamsCoercionTapeKeepAndErrors(t *testing.T) {
	PinP(t)
	if err := setup.LoadParams(`{"atr_n": 10.9, "max_touches": "3", "min_age": 45, "exit_on_gone": 0, "be_on_tp1": "yes", "approach_secs": 600, "band": 0.04}`); err != nil {
		t.Fatal(err)
	}
	p := setup.P
	if p.ATRN != 10 || p.MaxTouches != 3 || p.MinAge != 45.0 || p.ExitOnGone || !p.BeOnTP1 || book.DENS.Band != 0.04 {
		t.Errorf("coercion: %+v", p)
	}
	if book.TapeKeep != 610 { // max(approach_secs, impulse_secs, 300) + 10
		t.Errorf("TapeKeep %v", book.TapeKeep)
	}
	if setup.LoadParams("  ") != nil || setup.LoadParams(`{"atr_n": "x"}`) == nil || setup.LoadParams(`[1]`) == nil || setup.LoadParams(`{`) == nil {
		t.Error("empty / bad PARAMS")
	}
	setup.P = setup.Default()
	if err := setup.LoadParams(""); err != nil || book.TapeKeep != 310 {
		t.Errorf("default TapeKeep %v (%v)", book.TapeKeep, err)
	}
}

func TestFeaturesJSONKeysAndNulls(t *testing.T) {
	PinP(t)
	p := plan(LongBook(NOW), NOW)
	raw, err := jsonOf(p.Features)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"density_usd", "peak_usd", "age_s", "touches", "dist", "spread", "minute_usd", "dens_vs_minute",
		"moved60", "approach", "move", "move_start", "move_ts", "move_s", "risk", "reward", "rr", "atr", "stop_atr",
		"trend", "counter", "accel", "hold_s", "flow_s", "mkt", "vol_ratio", "ch24", "drop10"}
	last := -1
	for _, k := range order {
		i := strings.Index(raw, `"`+k+`":`)
		if i < 0 || i < last {
			t.Fatalf("key %q missing or out of order in %s", k, raw)
		}
		last = i
	}
	if strings.Contains(raw, "depth_x") || strings.Contains(raw, "armed_s") {
		t.Errorf("depth_x / armed_s present before they are set: %s", raw)
	}
	for _, k := range []string{`"trend":null`, `"hold_s":null`, `"mkt":null`, `"ch24":null`} {
		if !strings.Contains(raw, k) {
			t.Errorf("%s missing in %s", k, raw)
		}
	}
	p.Features.DepthX = setup.F64(3.1)
	p.Features.ArmedS = setup.F64(17)
	raw, _ = jsonOf(p.Features)
	if !strings.Contains(raw, `"depth_x":3.1,"armed_s":17}`) {
		t.Errorf("late keys: %s", raw)
	}
	if z, _ := jsonOf(setup.Features{}); z != "{}" {
		t.Errorf("zero features: %s", z)
	}
}
