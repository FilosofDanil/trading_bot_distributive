package setup_test

import (
	"encoding/json"
	"math"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	. "densitybounce/internal/testkit"
)

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func hold(t *testing.T, b *book.Book, d *book.Density) (float64, bool, float64) {
	t.Helper()
	return setup.HoldSecs(b, d, NOW)
}

func TestScanReturnsTheTradablePlanFirstAndPlanBounceIsItsFirstItem(t *testing.T) {
	PinP(t)
	b := LongBook(NOW)
	p, ghost := setup.Scan(b, NOW, nil, nil)
	if p == nil || ghost != nil || p.Skip != "" || p.Side != "long" {
		t.Fatalf("p %+v ghost %+v", p, ghost)
	}
	if plan(b, NOW).Entry != p.Entry {
		t.Error("plan_bounce differs from scan")
	}
	if p2, g2 := setup.Scan(b, NOW, map[book.DKey]bool{bidKey: true}, nil); p2 != nil || g2 != nil { // a skipped density gives nothing at all
		t.Error("skipped density gave a plan")
	}
}

func TestNoATRNoPlan(t *testing.T) {
	PinP(t)
	setup.P.StopATR = 2.0
	b := LongBook(NOW, NoBars()) // no 1m bars yet
	if p, g := setup.Scan(b, NOW, nil, nil); p != nil || g != nil || plan(b, NOW) != nil {
		t.Error("planned without bars")
	}
	b = LongBook(NOW, NBars(13)) // one bar short of atr_n = 14
	if p, g := setup.Scan(b, NOW, nil, nil); p != nil || g != nil {
		t.Error("planned with 13 bars")
	}
	b = LongBook(NOW)
	b.AppendBar(book.Bar{Min: int32(NOW / 60), Hi: b.T(1.2), Lo: b.T(0.8)}) // the running minute is not complete: it does not count
	atr, _ := b.ATR(NOW, 14)
	if plan(b, NOW) == nil || math.Abs(atr-0.0002) > 1e-9 {
		t.Errorf("running minute counted: atr %v", atr)
	}
	setup.P.StopATR = 0.0 // ATR off: the bare stop_buf is allowed again, as before
	near(t, "stop", plan(LongBook(NOW, NoBars()), NOW).Stop, 0.9985, 0)
}

func TestYoungDensityIsAShadowPlanAndYoungerThanShadowAgeIsNothing(t *testing.T) {
	PinP(t)
	setup.P.MinAge, setup.P.ShadowAge = 60.0, 20.0
	p, ghost := setup.Scan(LongBook(NOW, Age(30)), NOW, nil, nil) // 20 <= 30 < 60 s
	if p != nil || ghost == nil || ghost.Skip != "young" || ghost.Features.AgeS != 30.0 {
		t.Fatalf("p %v ghost %+v", p, ghost)
	}
	near(t, "ghost entry", ghost.Entry, 1.0002, 0) // a full plan, not traded
	near(t, "ghost stop", ghost.Stop, 0.9985, 0)
	if p, g := setup.Scan(LongBook(NOW, Age(10)), NOW, nil, nil); p != nil || g != nil { // < shadow_age
		t.Error("10 s old density gave a plan")
	}
	if p, g := setup.Scan(LongBook(NOW, Age(60)), NOW, nil, nil); p == nil || g != nil { // exactly min_age: trades
		t.Error("60 s old density did not trade")
	}
	setup.P.MinAge = 20.0 // the old rule: 30 s is old enough
	if plan(LongBook(NOW, Age(30)), NOW) == nil {
		t.Error("min_age 20 should trade")
	}
}

func TestWeakDensityIsAShadowPlanAStrongOneTradesWithHoldS(t *testing.T) {
	PinP(t)
	setup.P.HoldMin = 60.0
	b := LongBook(NOW)               // a 100k USDT bid density
	Sells(b, NOW-30, 120_000, 10030) // 2000 USDT/s of selling in the last minute: 50 s worth
	d := b.Densities[bidKey]
	h, ok, rate := hold(t, b, d)
	near(t, "rate", rate, 2000, 2000e-3)
	if !ok {
		t.Fatal("hold unknown")
	}
	near(t, "hold", h, 50, 50e-3)
	p, ghost := setup.Scan(b, NOW, nil, nil)
	if p != nil || ghost == nil || ghost.Skip != "weak" {
		t.Fatalf("p %v ghost %+v", p, ghost)
	}
	near(t, "hold_s", val(t, "hold_s", ghost.Features.HoldS), 50.0, 0.1)
	if ghost.Features.FlowS != 2000 {
		t.Errorf("flow_s %d", ghost.Features.FlowS)
	}
	b = LongBook(NOW)
	Sells(b, NOW-30, 30_000, 10030) // 500 USDT/s: 200 s worth
	p, ghost = setup.Scan(b, NOW, nil, nil)
	if p == nil || ghost != nil {
		t.Fatalf("strong: p %v ghost %v", p, ghost)
	}
	near(t, "hold_s", val(t, "hold_s", p.Features.HoldS), 200.0, 0.5)
	if p.Features.FlowS != 500 {
		t.Errorf("flow_s %d", p.Features.FlowS)
	}
	setup.P.HoldMin = 0.0 // the gate off: the weak density trades again
	b = LongBook(NOW)
	Sells(b, NOW-30, 120_000, 10030)
	p = plan(b, NOW)
	if p == nil {
		t.Fatal("hold_min 0 should trade")
	}
	near(t, "hold_s", val(t, "hold_s", p.Features.HoldS), 50.0, 0.1)
}

func TestHoldUsesTheFasterOfOneAndThreeMinutesAndOnlyTheAggressorThatHitsTheDensity(t *testing.T) {
	PinP(t)
	setup.P.HoldMin = 60.0
	b := LongBook(NOW)
	d := b.Densities[bidKey]
	if _, ok, rate := hold(t, b, d); ok || rate != 0 { // no tape: the gate passes
		t.Errorf("no tape: ok %v rate %v", ok, rate)
	}
	if plan(b, NOW) == nil {
		t.Error("no tape should trade")
	}
	Sells(b, NOW-120, 540_000, 10030) // nothing in the last minute, 3000 USDT/s over 3 min
	h, ok, rate := hold(t, b, d)
	near(t, "rate", rate, 3000, 3)
	near(t, "hold", h, 100_000.0/3000, 100_000.0/3000*1e-3)
	if !ok {
		t.Error("hold unknown")
	}
	if _, g := setup.Scan(b, NOW, nil, nil); g == nil || g.Skip != "weak" {
		t.Errorf("ghost %+v", g)
	}
	b = LongBook(NOW)
	Buys(b, NOW-30, 5_000_000, 9970) // buyers lift the asks: no pressure on a bid density
	if _, ok, rate := hold(t, b, b.Densities[bidKey]); ok || rate != 0 {
		t.Errorf("buys: ok %v rate %v", ok, rate)
	}
	if plan(b, NOW) == nil {
		t.Error("buyers should not stop a long")
	}
	s := ShortBook(NOW) // the mirror: an ask density feels the aggressive BUYS
	Buys(s, NOW-30, 120_000, 9970)
	h, ok, _ = setup.HoldSecs(s, s.Densities[askKey], NOW)
	if !ok {
		t.Fatal("short hold unknown")
	}
	near(t, "short hold", h, 50.0, 0.5)
	if _, g := setup.Scan(s, NOW, nil, nil); g == nil || g.Skip != "weak" {
		t.Errorf("short ghost %+v", g)
	}
	Sells(s, NOW-20, 9_000_000, 9970) // sellers do not hit an ask density
	h, _, _ = setup.HoldSecs(s, s.Densities[askKey], NOW)
	near(t, "short hold after sells", h, 50.0, 0.5)
}

func TestCancelReasonDensityWeak(t *testing.T) {
	PinP(t)
	setup.P.HoldCancel = 30.0
	b := LongBook(NOW)
	p := plan(b, NOW)
	// Python prints 200_000 here: the wall is exactly 30 s worth, not under it. With float32 trade quantities that
	// exact tie can fall either way (a documented narrowing, docs/go_port.md), so the test sits just above it.
	Sells(b, NOW-5, 199_000, 10030) // 3317 USDT/s: 30.1 s worth
	if got := setup.CancelReason(b, p, NOW, NOW); got != "" {
		t.Errorf("30 s worth: %q", got)
	}
	Sells(b, NOW-4, 150_000, 10030) // 5833 USDT/s: 17 s worth
	if got := setup.CancelReason(b, p, NOW, NOW); got != "density_weak" {
		t.Errorf("17 s worth: %q", got)
	}
	if !setup.IsWallFailed("density_weak") || !setup.IsWallFailed("unconfirmed") || setup.IsWallFailed("price_away") || setup.IsWallFailed("entry_timeout") {
		t.Error("WallFailed")
	}
	setup.P.HoldCancel = 0.0
	if got := setup.CancelReason(b, p, NOW, NOW); got != "" {
		t.Errorf("hold_cancel 0: %q", got)
	}
}

func f64(x float64) *float64 { return &x }

func TestMarketFilterLongAndShort(t *testing.T) {
	PinP(t)
	setup.P.MktMax = 0.01
	b := LongBook(NOW)
	p, ghost := setup.Scan(b, NOW, nil, f64(-0.012)) // the others fell 1.2 % in 30 min: every wall gets hit
	if p != nil || ghost == nil || ghost.Skip != "market" || val(t, "mkt", ghost.Features.Mkt) != -0.012 {
		t.Fatalf("p %v ghost %+v", p, ghost)
	}
	if _, g := setup.Scan(b, NOW, nil, f64(-0.01)); g == nil || g.Skip != "market" { // exactly mkt_max counts
		t.Error("exactly mkt_max")
	}
	for _, m := range []*float64{f64(-0.005), f64(0.03), nil} { // a small fall / a rising market / unknown
		if p, _ := setup.Scan(b, NOW, nil, m); p == nil {
			t.Errorf("mkt %v should trade", m)
		}
	}
	s := ShortBook(NOW)
	if _, g := setup.Scan(s, NOW, nil, f64(0.012)); g == nil || g.Skip != "market" { // a rising market against a short
		t.Error("short vs rising market")
	}
	for _, m := range []float64{-0.03, 0.004} {
		if p, _ := setup.Scan(s, NOW, nil, f64(m)); p == nil {
			t.Errorf("short mkt %v should trade", m)
		}
	}
	setup.P.MktMax = 0.0 // off
	if p, _ := setup.Scan(b, NOW, nil, f64(-0.05)); p == nil {
		t.Error("mkt_max 0 should trade")
	}
}

func TestSeveralSoftGatesAreJoinedAndHardGatesStillDropThePlan(t *testing.T) {
	PinP(t)
	b := LongBook(NOW, Age(30))
	Sells(b, NOW-30, 120_000, 10030)
	p, ghost := setup.Scan(b, NOW, nil, f64(-0.02))
	if p != nil || ghost == nil || ghost.Skip != "young+weak+market" {
		t.Fatalf("p %v ghost %+v", p, ghost)
	}
	b = LongBook(NOW, Age(30))
	b.Densities[bidKey].USD = 50_000 // half eaten: a hard gate, not a shadow plan
	if p, g := setup.Scan(b, NOW, nil, f64(-0.02)); p != nil || g != nil {
		t.Error("hard gate gave a plan")
	}
}

func TestTradablePlanAndShadowPlanAreReportedSeparately(t *testing.T) {
	PinP(t)
	// a second, nearer density 0.2 % below mid fails a soft gate (young); the far one trades
	b := LongBook(NOW)
	k := int32(10010)
	b.SetLevel(book.Bid, k, 100_000.0/1.001)
	b.Densities[book.DKey{Side: book.Bid, Tick: k}] = book.NewDensity(book.Bid, k, 1.0010, NOW-30, 100_000, 100_000, NOW)
	p, ghost := setup.Scan(b, NOW, nil, nil)
	if p == nil || p.DKey != bidKey {
		t.Fatalf("plan %+v", p)
	}
	if ghost == nil || ghost.DKey != (book.DKey{Side: book.Bid, Tick: k}) || ghost.Skip != "young" {
		t.Fatalf("ghost %+v", ghost)
	}
}

func TestTouchedAndConfirmLong(t *testing.T) {
	PinP(t)
	b := LongBook(NOW)
	p := plan(b, NOW)        // entry 1.0002 = tick 10002
	if setup.Touched(b, p) { // the best bid is 10029: price has not come down yet
		t.Error("touched too early")
	}
	filterSide(b, book.Bid, func(k int32) bool { return k <= 10002 })
	if !setup.Touched(b, p) { // the best bid is the entry tick: an order there is first in line
		t.Error("not touched")
	}
	conf := func(want string, wantTick int32, wantOK bool) {
		t.Helper()
		why, tick, ok := setup.Confirm(b, p)
		if why != want || tick != wantTick || ok != wantOK {
			t.Errorf("Confirm = (%q, %d, %v), want (%q, %d, %v)", why, tick, ok, want, wantTick, wantOK)
		}
	}
	conf("", 10002, true)
	b.SetSide(book.Ask, map[int32]float64{10002: 100}) // the best ask is 10002: a post-only bid there would cross
	conf("", 10001, true)                              // one tick below the ask
	b.SetSide(book.Ask, map[int32]float64{10001: 100}) // ... and 10000 is the density itself: wait
	conf("", 0, false)
	b.SetSide(book.Ask, map[int32]float64{10003: 100})
	d := b.Densities[bidKey]
	d.USD = 0.69 * d.PeakUSD // under 70 % of its peak
	conf("unconfirmed", 0, false)
	d.USD = 0.7 * d.PeakUSD
	conf("", 10002, true) // exactly intact
	d.Gone = "pulled"
	conf("unconfirmed", 0, false)
	delete(b.Densities, bidKey)
	conf("unconfirmed", 0, false)
}

func TestTouchedAndConfirmShort(t *testing.T) {
	PinP(t)
	b := ShortBook(NOW)
	p := plan(b, NOW) // entry 0.9998 = tick 9998
	if p.Side != "short" || setup.Touched(b, p) {
		t.Fatal("short plan / touched")
	}
	filterSide(b, book.Ask, func(k int32) bool { return k >= 9998 })
	why, tick, ok := setup.Confirm(b, p)
	if !setup.Touched(b, p) || why != "" || tick != 9998 || !ok {
		t.Errorf("confirm (%q, %d, %v)", why, tick, ok)
	}
	b.SetSide(book.Bid, map[int32]float64{9998: 100}) // a post-only ask there would cross: one tick above the bid
	if why, tick, ok := setup.Confirm(b, p); why != "" || tick != 9999 || !ok {
		t.Errorf("confirm (%q, %d, %v)", why, tick, ok)
	}
	b.SetSide(book.Bid, map[int32]float64{9999: 100})
	if why, _, ok := setup.Confirm(b, p); why != "" || ok { // 10000 is the density
		t.Errorf("confirm wait: %q %v", why, ok)
	}
	b.Densities[askKey].USD = 10
	if why, _, ok := setup.Confirm(b, p); why != "unconfirmed" || ok {
		t.Errorf("confirm weak: %q %v", why, ok)
	}
}

// book.py ---------------------------------------------------------------------
func TestFlowAndFlowRateSplitByAggressorAndWindow(t *testing.T) {
	b := book.New("XUSDT", 0.0001)
	b.PushTrade(book.NewTradeAt(900, 10000, 100, true)) // 100 x 1.0 sell, 100 s ago: 100 USDT
	b.PushTrade(book.NewTradeAt(990, 20000, 10, true))  // 10 x 2.0 sell, 10 s ago: 20 USDT
	b.PushTrade(book.NewTradeAt(995, 10000, 50, false)) // a buy: 50 USDT
	near(t, "sell 60", b.Flow(1000, 60, true), 20, 0)
	near(t, "sell 180", b.Flow(1000, 180, true), 120, 0)
	near(t, "buy 180", b.Flow(1000, 180, false), 50, 0)
	if b.Flow(1000, 4, false) != 0 {
		t.Error("buy 4")
	}
	near(t, "rate", b.FlowRate(1000, true, 60, 180), math.Max(20.0/60, 120.0/180), 0) // the 3 min rate is the faster
	near(t, "rate 5/8", b.FlowRate(1000, true, 5, 8), 0, 1e-12)                       // no sell in the last 8 s
	near(t, "rate 15/180", b.FlowRate(1000, true, 15, 180), math.Max(20.0/15, 120.0/180), 0)
	near(t, "rate buy", b.FlowRate(1000, false, 60, 180), 50.0/60, 0)
	if book.New("YUSDT", 0.0001).FlowRate(1000, true, 60, 180) != 0 {
		t.Error("empty book rate")
	}
}

func TestBarAgoIsTheMiddleOfThatMinuteAndNoneBeforeTheBars(t *testing.T) {
	b := book.New("XUSDT", 0.0001)
	b.AppendBar(book.Bar{Min: 10, Hi: 10200, Lo: 10000})
	b.AppendBar(book.Bar{Min: 11, Hi: 10500, Lo: 10300})
	b.AppendBar(book.Bar{Min: 12, Hi: 10400, Lo: 10400})
	at := func(secs, want float64) {
		t.Helper()
		v, ok := b.BarAgo(12*60+30, secs)
		if !ok {
			t.Errorf("BarAgo(%v) none", secs)
			return
		}
		near(t, "bar_ago", v, want, 1e-9)
	}
	at(30, 1.04)                               // minute 12
	at(90, 1.04)                               // minute 11 -> (1.05 + 1.03) / 2
	at(150, 1.01)                              // minute 10
	if _, ok := b.BarAgo(12*60+30, 3000); ok { // before the oldest bar
		t.Error("BarAgo(3000) should be none")
	}
}

func TestDensityBandIsThreePercentTheBaselineTwo(t *testing.T) {
	PinP(t)
	wall := func(priceK int32) *book.Book {
		b := book.New("XUSDT", 0.0001)
		for i := int32(1); i < 400; i++ { // a thin ladder, 100 units a level, 4 % deep
			b.SetLevel(book.Bid, 10000-i, 100)
			b.SetLevel(book.Ask, 10000+i, 100)
		}
		b.SetLevel(book.Bid, priceK, 30_000) // ~29k USDT: > 25k and > 8 x the median level
		b.LastTs = 1000
		return b
	}
	if book.DENS.Band != 0.03 || book.DENS.BaseBand != 0.02 {
		t.Fatalf("DENS %+v", book.DENS)
	}
	b := wall(9750) // 2.5 % below mid: inside the 3 % band, outside the 2 % baseline
	b.ScanDensities(1000)
	if _, ok := b.Densities[book.DKey{Side: book.Bid, Tick: 9750}]; !ok {
		t.Error("2.5 % wall not tracked")
	}
	b = wall(9650) // 3.5 % below: not tracked
	b.ScanDensities(1000)
	if len(b.Densities) != 0 {
		t.Error("3.5 % wall tracked")
	}
	book.DENS.Band = 0.02 // the old width would have missed the first one
	b = wall(9750)
	b.ScanDensities(1000)
	if len(b.Densities) != 0 {
		t.Error("band 2 % tracked the 2.5 % wall")
	}
}
