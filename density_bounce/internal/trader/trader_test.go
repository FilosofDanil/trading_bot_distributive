package trader_test

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/meta"
	"densitybounce/internal/setup"
	"densitybounce/internal/sim"
	"densitybounce/internal/state"
	. "densitybounce/internal/testkit"
	"densitybounce/internal/trader"
)

const NOW = 1000.0
const T0 = 100_000.0
const clock0 = 1_700_000_000.0 // the wall clock of the tests (Python used time.time())

var bidKey = book.DKey{Side: book.Bid, Tick: 10000}

type env struct {
	t     *testing.T
	tr    *trader.Trader
	br    *sim.Broker
	st    *state.Store
	sc    *Sched
	clock float64
}

type envOpt struct {
	lag     float64
	book    *book.Book
	now     float64
	desired string
	broker  func(*sim.Broker) trader.Broker
}

func newEnv(t *testing.T, dir string, o envOpt) *env {
	t.Helper()
	PinP(t)
	PinRisk(t)
	if o.now == 0 {
		o.now = NOW
	}
	if dir == "" {
		dir = t.TempDir()
	}
	st, err := state.Open(filepath.Join(dir, "s.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	st.Out = io.Discard
	if o.desired != "" {
		st.SetDesired(o.desired)
	}
	br := sim.New(o.lag)
	var b trader.Broker = br
	if o.broker != nil {
		b = o.broker(br)
	}
	e := &env{t: t, br: br, st: st, sc: &Sched{}, clock: clock0}
	info := map[string]trader.SymInfo{"XUSDT": {Tick: 0.0001, Step: 1, MinQty: 1, MinNotional: 5}}
	e.tr = trader.New(b, st, info, "closes", e.sc)
	e.tr.Logf = nil
	e.tr.Now = func() float64 { return e.clock }
	bk := o.book
	if bk == nil {
		bk = LongBook(o.now)
	}
	e.tr.AddBook("XUSDT", bk)
	return e
}

func newDefault(t *testing.T) *env { return newEnv(t, "", envOpt{}) }

func (e *env) b() *book.Book { return e.tr.Book("XUSDT") }

func (e *env) stat(k string) int { return e.st.Stat(k) }

func (e *env) seek(now float64) { e.tr.Seek("XUSDT", e.b(), now) }

func (e *env) fill(ts float64, tick int32, qty float64, bm bool) {
	e.br.OnTrade("XUSDT", book.NewTradeAt(ts, tick, qty, bm))
	e.sc.Settle()
}

func (e *env) lastTrade() state.Trade {
	e.t.Helper()
	lt := e.st.LastTrades(1)
	if len(lt) != 1 {
		e.t.Fatal("no closed trade")
	}
	return lt[0]
}

func (e *env) hasEvent(sub ...string) bool {
	for _, ev := range e.st.Events() {
		ok := true
		for _, s := range sub {
			ok = ok && strings.Contains(ev.Msg, s)
		}
		if ok {
			return true
		}
	}
	return false
}

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v +- %v", name, got, want, tol)
	}
}

func sells(b *book.Book, ts, usd float64) { Sells(b, ts, usd, 10030) }

func touch(b *book.Book, top int32) { // price comes down to the entry level: no bid is left above top
	m := map[int32]float64{}
	b.RangeLevels(book.Bid, func(k int32, q float64) {
		if k <= top {
			m[k] = q
		}
	})
	b.SetSide(book.Bid, m)
}

func parts(e *env) {
	setup.P.TP1R, setup.P.TP1Frac, setup.P.TP1Part, setup.P.BeOnTP1, setup.P.RunFrac = 0, 0.25, 0.25, true, 1.0
}

func (e *env) openLong() *trader.Slot {
	e.t.Helper()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	if s == nil || s.State != "entry" || s.Qty != 9.0 { // 10 USDT / 1.0002, step 1
		e.t.Fatalf("slot %+v", s)
	}
	e.fill(NOW+1, 10001, 50, true) // a sell prints through our bid at 1.0002
	if s.State != "open" || math.Abs(s.EntryPx-1.0002) > 1e-9 {
		e.t.Fatalf("state %s entry %v", s.State, s.EntryPx)
	}
	if o := e.br.Order("XUSDT"); o == nil || o.Kind != "open" { // stop + take resting
		e.t.Fatal("no open order")
	}
	if len(s.Parts) != 1 || s.Parts[0].Name != "tp2" { // 9 USDT: too small to split, one take
		e.t.Fatalf("parts %+v", s.Parts)
	}
	return s
}

func (e *env) openBigLong() *trader.Slot {
	e.t.Helper()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	e.fill(NOW+1, 10001, 100, true)
	want := []trader.Part{{Name: "tp1", Qty: 14, Px: 1.0100}, {Name: "tp2", Qty: 36, Px: 1.0200}, {Name: "run", Qty: 9, Px: 1.0400}}
	if len(s.Parts) != 3 {
		e.t.Fatalf("parts %+v", s.Parts)
	}
	for i, w := range want {
		if p := s.Parts[i]; p.Name != w.Name || p.Qty != w.Qty || math.Abs(p.Px-w.Px) > 1e-9 {
			e.t.Fatalf("part %d = %+v, want %+v", i, *p, w)
		}
	}
	return s
}

func TestOneTakeForTheWholePositionAndNoStopMove(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	setup.P.TP1R = 0 // v0.3.0: no part at +1R
	b := e.b()       // mid 1.0030 = +1.65 R
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	e.fill(NOW+1, 10001, 100, true)
	if len(s.Parts) != 1 || s.Parts[0].Name != "tp2" || s.Parts[0].Qty != 59 || math.Abs(s.Parts[0].Px-1.0200) > 1e-9 {
		t.Fatalf("parts %+v", s.Parts)
	}
	e.tr.WatchOpen(s, b, NOW+5)
	if s.Manage.BeDone || e.br.Order("XUSDT").Stop != 9985 {
		t.Error("stop moved")
	}
	e.fill(NOW+30, 10201, 100, false)
	tr := e.lastTrade()
	if e.tr.SlotCount() != 0 || tr.Reason != "take" {
		t.Errorf("slots %d reason %s", e.tr.SlotCount(), tr.Reason)
	}
	near(t, "exit", tr.Exit, 1.0200, 1e-9)
}

func TestNoBreakEvenBeforeTheFirstTake(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	parts(e)
	s := e.openBigLong()
	e.tr.WatchOpen(s, e.b(), NOW+5)
	if s.Manage.BeDone || e.br.Order("XUSDT").Stop != 9985 {
		t.Error("break-even before tp1")
	}
}

func TestPartialTakesBreakEvenAndRunnerLock(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	parts(e)
	b := e.b()
	s := e.openBigLong()
	e.fill(NOW+5, 10101, 50, false) // through tp1
	if !s.Parts[0].Done || s.Parts[1].Done || s.Parts[2].Done {
		t.Fatal("parts done after tp1")
	}
	e.tr.WatchOpen(s, b, NOW+5)
	if !s.Manage.BeDone || e.br.Order("XUSDT").Stop != 10008 { // break-even: entry + 0.06 %
		t.Errorf("be: done %v stop %d", s.Manage.BeDone, e.br.Order("XUSDT").Stop)
	}
	e.fill(NOW+20, 10201, 50, false) // through tp2
	e.tr.WatchOpen(s, b, NOW+20)
	if !s.LockDone || e.br.Order("XUSDT").Stop != 10100 { // the runner keeps tp1's profit
		t.Errorf("lock: done %v stop %d", s.LockDone, e.br.Order("XUSDT").Stop)
	}
	e.fill(NOW+40, 10100, 50, true) // back to tp1: the runner stops out
	if e.tr.SlotCount() != 0 {
		t.Fatal("slot left")
	}
	tr := e.lastTrade()
	if tr.Reason != "stop_lock" || !tr.Parts[0].Done || !tr.Parts[1].Done || tr.Parts[2].Done {
		t.Errorf("trade %+v", tr)
	}
	stopPx := 1.0100 * (1 - 0.0002)
	gross := 14*(1.0100-1.0002) + 36*(1.0200-1.0002) + 9*(stopPx-1.0002)
	fees := 59*1.0002*0.0002 + (14*1.0100+36*1.0200)*0.0002 + 9*stopPx*0.0005
	near(t, "pnl", tr.PNL, gross-fees, 1e-6)
	near(t, "fees", tr.Fees, fees, 1e-6)
	near(t, "exit", tr.Exit, (14*1.0100+36*1.0200+9*stopPx)/59, 1e-6)
	if !e.hasEvent("closed (stop_lock)", "parts tp1 tp2") {
		t.Error("no close event")
	}
}

func TestRunnerTakeClosesTheLastPart(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	parts(e)
	e.openBigLong()
	e.fill(NOW+30, 10401, 50, false) // one print through every take
	tr := e.lastTrade()
	if e.tr.SlotCount() != 0 || tr.Reason != "take" {
		t.Fatalf("reason %s", tr.Reason)
	}
	for _, p := range tr.Parts {
		if !p.Done {
			t.Error("part not done")
		}
	}
	near(t, "exit", tr.Exit, (14*1.0100+36*1.0200+9*1.0400)/59, 1e-6)
}

func TestPartialThenInitialStop(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	parts(e)
	e.openBigLong()
	e.fill(NOW+5, 10101, 50, false) // tp1, but the stop is not moved yet
	e.fill(NOW+6, 9985, 50, true)   // the rest (45) stops out
	tr := e.lastTrade()
	stopPx := 0.9985 * (1 - 0.0002)
	gross := 14*(1.0100-1.0002) + 45*(stopPx-1.0002)
	fees := 59*1.0002*0.0002 + 14*1.0100*0.0002 + 45*stopPx*0.0005
	if tr.Reason != "stop" {
		t.Error(tr.Reason)
	}
	near(t, "pnl", tr.PNL, gross-fees, 1e-6)
}

func TestHalfAtOneRAndTheStopStays(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	b := e.b()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	e.fill(NOW+1, 10001, 100, true)
	if len(s.Parts) != 2 || s.Parts[0].Name != "tp1" || s.Parts[0].Qty != 29 || math.Abs(s.Parts[0].Px-1.0019) > 1e-9 ||
		s.Parts[1].Name != "tp2" || s.Parts[1].Qty != 30 || math.Abs(s.Parts[1].Px-1.0200) > 1e-9 {
		t.Fatalf("parts %+v %+v", s.Parts[0], s.Parts[len(s.Parts)-1])
	}
	e.fill(NOW+5, 10020, 50, false) // through tp1 at +1R
	e.tr.WatchOpen(s, b, NOW+5)
	if !s.Parts[0].Done || s.Manage.BeDone || e.br.Order("XUSDT").Stop != 9985 { // no break-even
		t.Error("break-even moved")
	}
	e.fill(NOW+9, 9985, 50, true) // the rest stops out where it started
	tr := e.lastTrade()
	stopPx := 0.9985 * (1 - 0.0002)
	gross := 29*(1.0019-1.0002) + 30*(stopPx-1.0002)
	fees := 59*1.0002*0.0002 + 29*1.0019*0.0002 + 30*stopPx*0.0005
	near(t, "pnl", tr.PNL, gross-fees, 1e-6)
	if tr.Reason != "stop" || tr.Slip != nil { // the exit price averages in the part
		t.Errorf("reason %s slip %v", tr.Reason, tr.Slip)
	}
}

func TestSizeKeepsTheUSDTRisk(t *testing.T) {
	e := newDefault(t)
	trader.RISK.Notional = 60
	if q := e.tr.Size("XUSDT", 1.0002, 0.9985); q != 59 { // default risk 0.12 USDT: the notional caps it
		t.Errorf("size %v", q)
	}
	trader.RISK.RiskUSD = 0.05
	if q := e.tr.Size("XUSDT", 1.0002, 0.9985); q != 29 { // 0.05 USDT / 0.17 % = 29.4 USDT
		t.Errorf("size %v", q)
	}
	if q := e.tr.Size("XUSDT", 1.0002, 0.9902); q != 5 { // 1 % stop: 5 USDT (the exchange minimum)
		t.Errorf("size %v", q)
	}
	if q := e.tr.Size("XUSDT", 1.0002, 0); q != 59 { // no stop: the notional
		t.Errorf("size %v", q)
	}
}

func TestNoEntryWhenTheBookBehindTheStopIsThin(t *testing.T) {
	e := newDefault(t)
	e.tr.AddBook("XUSDT", LongBook(NOW, Depth(1))) // 7 units within 0.3 % beyond the stop < 3 x 9
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || e.stat("thin_stop") != 1 || e.tr.Cool["XUSDT"] != NOW+30 {
		t.Fatalf("slots %d thin %d cool %v", e.tr.SlotCount(), e.stat("thin_stop"), e.tr.Cool["XUSDT"])
	}
	e.tr.Cool = map[string]float64{}
	e.tr.AddBook("XUSDT", LongBook(NOW, Depth(4))) // 28 >= 27
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	if s == nil || s.Plan.Features.DepthX == nil {
		t.Fatal("no slot")
	}
	near(t, "depth_x", *s.Plan.Features.DepthX, 3.1, 0.05)
}

func (e *env) stopped(exitPx float64) state.Trade {
	e.tr.PutSlot(&trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.0200, DKey: bidKey},
		Qty: 9, State: "open", FilledQty: 9, EntryPx: 1.0002, OpenedAt: e.clock, Stop: 0.9985})
	e.tr.OnClosed("XUSDT", exitPx, -0.05, 0.01, "stop")
	return e.lastTrade()
}

func TestTwoSlippedStopsBanTheSymbolForADay(t *testing.T) {
	e := newDefault(t)
	near(t, "slip 1", *e.stopped(0.9980).Slip, 0.0005, 1e-5) // 0.05 %: an ordinary fill
	near(t, "slip 2", *e.stopped(0.9950).Slip, 0.0035/0.9985, 1e-5)
	if e.tr.Cool["XUSDT"] >= e.clock+120 { // 0.35 %: the first slip
		t.Error("banned after one slip")
	}
	e.stopped(0.9950) // the second within 24 h: banned
	if e.tr.Cool["XUSDT"] <= e.clock+86_000 || !e.hasEvent("no trades on it for 24 h") {
		t.Error("not banned")
	}
}

func TestTakeAtMiddleOfTheMove(t *testing.T) {
	e := newDefault(t)
	e.openLong()
	e.fill(NOW+30, 10201, 50, false) // a buy prints through the take
	tr := e.lastTrade()
	if e.tr.SlotCount() != 0 || tr.Reason != "take" {
		t.Fatal(tr.Reason)
	}
	near(t, "exit", tr.Exit, 1.0200, 1e-9)
	gross := 9 * (1.0200 - 1.0002)
	near(t, "pnl", tr.PNL, gross-9*1.0002*0.0002-9*1.0200*0.0002, 1e-6)
	d := e.st.Snapshot()
	if d.Totals.N != 1 || d.Totals.Wins != 1 {
		t.Error("totals")
	}
	found := false
	for _, ev := range d.Events {
		found = found || (ev.Notify && strings.Contains(ev.Msg, "closed (take)"))
	}
	if !found {
		t.Error("no notified close event")
	}
}

func TestStopBehindTheDensity(t *testing.T) {
	e := newDefault(t)
	e.openLong()
	e.fill(NOW+5, 9985, 50, true) // trades at the stop
	tr := e.lastTrade()
	exit := 0.9985 * (1 - 0.0002) // stop price minus 0.02 % slippage
	pnl := 9*(exit-1.0002) - 9*1.0002*0.0002 - 9*exit*0.0005
	if tr.Reason != "stop" || tr.PNL >= 0 {
		t.Fatal(tr.Reason, tr.PNL)
	}
	near(t, "exit", tr.Exit, exit, 1e-9)
	near(t, "pnl", tr.PNL, pnl, 1e-6)
	near(t, "r", *tr.R, pnl/(9*0.0017), 0.01) // about -1.5 R: fees + slippage add ~0.5 R
	near(t, "slip", *tr.Slip, 0.0002, 1e-6)
	if e.st.ConsecLosses() != 1 {
		t.Error("consec")
	}
}

func TestFeeRebateCountsInTheJournalAndTheTotals(t *testing.T) {
	e := newDefault(t)
	trader.RISK.FeeRebate = 0.4
	e.openLong()
	e.fill(NOW+5, 9985, 50, true)
	tr := e.lastTrade()
	exit := 0.9985 * (1 - 0.0002)
	paid := 9*1.0002*0.0002 + 9*exit*0.0005
	near(t, "rebate", tr.Rebate, 0.4*paid, 1e-6)
	near(t, "fees", tr.Fees, 0.6*paid, 1e-6)
	near(t, "pnl", tr.PNL, 9*(exit-1.0002)-0.6*paid, 1e-6)
	d := e.st.Snapshot()
	near(t, "total pnl", d.Totals.PNL, tr.PNL, 1e-6)
	near(t, "total fees", d.Totals.Fees, tr.Fees, 1e-6)
}

func TestEntryCancelledWhenDensityGoes(t *testing.T) {
	e := newDefault(t)
	e.seek(NOW)
	e.b().Densities[bidKey].Gone = "pulled"
	if err := e.tr.WatchEntry(e.tr.Slot("XUSDT"), e.b(), NOW+1); err != nil {
		t.Fatal(err)
	}
	if e.tr.SlotCount() != 0 || e.stat("cancelled") != 1 || len(e.st.LastTrades(1)) != 0 {
		t.Error("cancel")
	}
}

func TestPauseAndRiskBlocks(t *testing.T) {
	e := newEnv(t, "", envOpt{desired: "paused"})
	e.seek(NOW)
	if e.tr.SlotCount() != 0 { // paused: no new entries
		t.Fatal("paused traded")
	}
	e.st.SetDesired("running")
	e.st.Mutate(func(d *state.Data) {
		d.Days[state.UTCDay(NOW)] = state.Agg{N: 3, PNL: -trader.RISK.DayLoss}
	})
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || !strings.Contains(e.tr.LastBlock, "day loss") {
		t.Fatalf("day loss: %q", e.tr.LastBlock)
	}
	e.st.Mutate(func(d *state.Data) {
		d.Days = map[string]state.Agg{}
		d.Totals.PNL = -trader.RISK.TotalLoss
	})
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || !strings.Contains(e.tr.Halt, "halted") {
		t.Errorf("halt %q", e.tr.Halt)
	}
}

func TestRaisingTheConsecLimitLiftsTodaysBlock(t *testing.T) {
	e := newDefault(t)
	trader.RISK.MaxConsec = 1
	e.openLong()
	e.fill(NOW+5, 9985, 50, true) // the stop: one loss hits the limit of 1
	if _, lim := e.st.ConsecBlock(); lim != 1 {
		t.Fatalf("limit %d", lim)
	}
	e.st.Mutate(func(d *state.Data) { day := state.UTCDay(NOW); d.ConsecBlockDay = &day }) // the trade closed on the wall clock's day
	if got := e.tr.Block(NOW); got != "1 losses in a row today" {
		t.Errorf("block %q", got)
	}
	trader.RISK.MaxConsec = 12
	if got := e.tr.Block(NOW); got != "" {
		t.Errorf("block %q", got)
	}
	trader.RISK.MaxConsec = 1 // same limit after a restart: still blocked
	if got := e.tr.Block(NOW); !strings.Contains(got, "in a row") {
		t.Errorf("block %q", got)
	}
	e.st.Mutate(func(d *state.Data) { d.ConsecBlockLimit = 0 }) // a block recorded before the limit was kept
	if got := e.tr.Block(NOW); got != "" {
		t.Errorf("block %q", got)
	}
}

func TestFlattenClosesOpenTrade(t *testing.T) {
	e := newDefault(t)
	e.openLong()
	if n := e.tr.Flatten(); n != 1 {
		t.Fatal(n)
	}
	if e.tr.SlotCount() != 0 || e.lastTrade().Reason != "flatten" {
		t.Error("flatten")
	}
}

func TestTickRunsOnASyncedBook(t *testing.T) {
	e := newDefault(t)
	e.b().LastTs = NOW
	e.tr.Tick(NOW)
	if e.tr.Slot("XUSDT") == nil { // scan + seek through the public entry point
		t.Error("no slot after Tick")
	}
}

// ------------------------------------------------------------------ v0.4.5

func TestVersionAndDefaults(t *testing.T) {
	if meta.Version != "0.5.0" || meta.Runner != "bounce" || meta.StrategyID != "bounce.density" {
		t.Error("meta")
	}
	e := newDefault(t)
	if e.tr.Live {
		t.Error("live by default")
	}
}

func TestArmedEntryIsPlaced15SAfterTheTouch(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	if s.State != "arm" || e.br.Order("XUSDT") != nil || e.stat("armed") != 1 || e.stat("placed") != 0 { // no order at the plan
		t.Fatal("armed state")
	}
	e.tr.WatchArm(s, b, NOW+1)
	if s.TouchAt != nil { // price has not reached the entry level
		t.Fatal("touched early")
	}
	touch(b, 10002)
	e.tr.WatchArm(s, b, NOW+2)
	if s.TouchAt == nil || *s.TouchAt != NOW+2 {
		t.Fatal("touch_at")
	}
	e.tr.WatchArm(s, b, NOW+16.9) // 14.9 s after the touch: still waiting
	if s.State != "arm" || e.br.Order("XUSDT") != nil {
		t.Fatal("placed too early")
	}
	e.tr.WatchArm(s, b, NOW+17) // 15 s
	if s.State != "entry" || s.PlacedAt != NOW+17 || e.br.Order("XUSDT").Kind != "entry" || e.stat("placed") != 1 ||
		*s.Plan.Features.ArmedS != 17.0 {
		t.Fatalf("not placed: %+v", s)
	}
	e.fill(NOW+18, 10001, 50, true)
	if s.State != "open" || math.Abs(s.EntryPx-1.0002) > 1e-9 {
		t.Error("not filled")
	}
}

func TestConfirmSecsZeroPlacesTheOrderAtThePlan(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 0
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	if s.State != "entry" || e.br.Order("XUSDT").Kind != "entry" || e.stat("placed") != 1 || e.stat("armed") != 0 {
		t.Error("immediate placement")
	}
}

func TestTheOrderGoesOneTickBelowTheBestAskWhenTheEntryWouldCross(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	touch(b, 10001)
	b.SetSide(book.Ask, map[int32]float64{10002: 100, 10003: 100}) // the ask came down to 1.0002 = the entry
	e.tr.WatchArm(s, b, NOW+1)
	e.tr.WatchArm(s, b, NOW+16)
	near(t, "entry", s.Plan.Entry, 1.0001, 1e-9)
	if s.State != "entry" || e.br.Order("XUSDT").Tick != 10001 {
		t.Error("tick")
	}
}

func lastPlan(t *testing.T, st *state.Store) map[string]any {
	t.Helper()
	rs := st.Plans(1)
	if len(rs) != 1 {
		t.Fatal("no plan record")
	}
	var m map[string]any
	if err := json.Unmarshal(rs[0], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUnconfirmedDensityCancelsTheEntryBlocksItAndIsJournaled(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	d := b.Densities[bidKey]
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	touch(b, 10002)
	e.tr.WatchArm(s, b, NOW+2)
	d.USD = 0.65 * d.PeakUSD // under 70 % at the confirmation, above 50 %
	e.tr.WatchArm(s, b, NOW+17)
	if e.tr.SlotCount() != 0 || e.br.Order("XUSDT") != nil { // never an order on the exchange
		t.Fatal("slot or order left")
	}
	if e.stat("cancel_unconfirmed") != 1 || e.stat("cancelled") != 1 {
		t.Error("stats")
	}
	if u, ok := e.tr.FailedUntil("XUSDT", bidKey); !ok || u != NOW+17+trader.RISK.WallCool || e.tr.Cool["XUSDT"] != NOW+17+5 {
		t.Errorf("failed %v %v cool %v", u, ok, e.tr.Cool["XUSDT"])
	}
	// the plain entry would have been filled and stopped: the journal shows what the cancel saved
	Print(b, NOW+20, 10001, 50, true)
	Print(b, NOW+25, 9985, 50, true)
	e.tr.StepShadows(NOW + 30)
	rec := lastPlan(t, e.st)
	sh := rec["shadow"].(map[string]any)
	if rec["act"] != "cancel:unconfirmed" || rec["pid"] != s.Plan.PID || rec["ver"] != meta.Version || sh["hit"] != "stop" ||
		sh["r"].(float64) >= -1.0 || rec["skip"] != "" {
		t.Errorf("record %v", rec)
	}
	// the density is blocked for wall_cool, a healthy one is not
	d.USD = d.PeakUSD
	e.tr.Cool = map[string]float64{}
	e.tr.AddBook("XUSDT", LongBook(NOW))
	e.tr.Seek("XUSDT", e.b(), NOW+10)
	if e.tr.Slot("XUSDT") != nil {
		t.Error("blocked wall traded")
	}
	e.tr.SetFailed("XUSDT", bidKey, NOW+10) // expired by then
	e.tr.Seek("XUSDT", e.b(), NOW+10)
	if e.tr.Slot("XUSDT") == nil {
		t.Error("expired block still blocks")
	}
}

func TestAnIntactDensityIsConfirmedEvenIfItLostALittle(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	d := b.Densities[bidKey]
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	touch(b, 10002)
	e.tr.WatchArm(s, b, NOW+2)
	d.USD = 0.75 * d.PeakUSD
	e.tr.WatchArm(s, b, NOW+17)
	if s.State != "entry" || len(e.tr.Failed) != 0 {
		t.Error("not confirmed")
	}
}

func TestArmedEntryWithoutATouchTimesOutWithoutBlockingTheDensity(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	e.seek(NOW)
	e.tr.WatchArm(e.tr.Slot("XUSDT"), b, NOW+100)
	if e.tr.Slot("XUSDT") == nil {
		t.Fatal("cancelled early")
	}
	e.tr.WatchArm(e.tr.Slot("XUSDT"), b, NOW+181)
	if e.tr.SlotCount() != 0 || e.br.Order("XUSDT") != nil || len(e.tr.Failed) != 0 ||
		e.stat("cancel_entry_timeout") != 1 || e.tr.Cool["XUSDT"] != NOW+181+trader.RISK.SymCooldown {
		t.Error("timeout")
	}
}

func TestArmedEntryFollowsCancelReasonHaltPauseAndStaleBook(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	b := e.b()
	e.seek(NOW)
	e.tr.Halt = "total loss"
	e.tr.WatchArm(e.tr.Slot("XUSDT"), b, NOW+1)
	if e.tr.SlotCount() != 0 || e.stat("cancel_halt") != 1 {
		t.Fatal("halt")
	}
	e.tr.Halt, e.tr.Cool = "", map[string]float64{}
	e.seek(NOW)
	e.st.SetDesired("paused")
	e.tr.WatchArm(e.tr.Slot("XUSDT"), b, NOW+1)
	if e.tr.SlotCount() != 0 || e.stat("cancel_paused") != 1 {
		t.Fatal("paused")
	}
	e.st.SetDesired("running")
	e.tr.Cool = map[string]float64{}
	e.tr.ClearAttempts()
	e.seek(NOW)
	e.tr.Tick(NOW + 10) // the book is 10 s old: blind
	if e.tr.SlotCount() != 0 || e.stat("cancel_stale_book") != 1 {
		t.Fatal("stale")
	}
	e.tr.Cool = map[string]float64{}
	e.tr.ClearAttempts()
	e.seek(NOW)
	if e.tr.Flatten() != 1 || e.tr.SlotCount() != 0 || e.stat("cancel_flatten") != 1 {
		t.Error("flatten")
	}
}

func TestArmedEntryWithAWeakWallIsCancelledAsDensityWeak(t *testing.T) {
	e := newDefault(t)
	setup.P.ConfirmSecs = 15.0
	setup.P.HoldCancel = 30.0
	b := e.b()
	e.seek(NOW)
	sells(b, NOW-2, 400_000) // 6.7k USDT/s against a 100k wall: 15 s worth
	e.tr.WatchArm(e.tr.Slot("XUSDT"), b, NOW+1)
	if _, ok := e.tr.FailedUntil("XUSDT", bidKey); e.tr.SlotCount() != 0 || e.stat("cancel_density_weak") != 1 || !ok {
		t.Error("density_weak")
	}
}

func TestEntryOverCountsJournalsAndSetsTheCooldown(t *testing.T) {
	whys := append(append([]string{}, setup.WallFailed...), "price_away", "entry_timeout", "counter_stalled", "rejected", "stale_book", "flatten", "expired")
	for _, why := range whys {
		t.Run(why, func(t *testing.T) {
			e := newDefault(t)
			e.seek(NOW)
			s := e.tr.Slot("XUSDT")
			e.tr.EntryOver(s, why, NOW)
			if e.tr.SlotCount() != 0 || e.stat("cancelled") != 1 || e.stat("cancel_"+why) != 1 {
				t.Fatal("counts")
			}
			sh := e.tr.ShadowByPID(s.Plan.PID)
			if sh == nil || sh.Act != "cancel:"+why || !sh.Final {
				t.Fatalf("shadow %+v", sh)
			}
			u, blocked := e.tr.FailedUntil("XUSDT", bidKey)
			if setup.IsWallFailed(why) {
				if !blocked || u != NOW+trader.RISK.WallCool || e.tr.Cool["XUSDT"] != NOW+5 {
					t.Error("wall failed")
				}
			} else {
				want := NOW + trader.RISK.SymCooldown
				if why == "rejected" {
					want = NOW + 30
				}
				if blocked || e.tr.Cool["XUSDT"] != want {
					t.Error("cooldown")
				}
			}
		})
	}
}

func TestAWorkingEntryCancelledBecauseTheWallWentBlocksThatWall(t *testing.T) {
	e := newDefault(t)
	b := e.b()
	e.seek(NOW)
	b.Densities[bidKey].Gone = "pulled"
	_ = e.tr.WatchEntry(e.tr.Slot("XUSDT"), b, NOW+1)
	if e.tr.SlotCount() != 0 || e.stat("cancel_density_gone") != 1 {
		t.Fatal("cancel")
	}
	if u, ok := e.tr.FailedUntil("XUSDT", bidKey); !ok || u <= e.clock+trader.RISK.WallCool-60 {
		t.Errorf("failed %v", u)
	}
	e.tr.Cool = map[string]float64{}
	e.tr.Seek("XUSDT", LongBook(NOW), NOW+10)
	if e.tr.SlotCount() != 0 { // the same wall is not traded again
		t.Error("traded again")
	}
}

type rejecting struct{ *sim.Broker }

func (rejecting) PlaceEntry(*trader.Slot) error { return errors.New("post-only would cross") }

func TestARejectedEntryIsCancelledWithAShortCooldown(t *testing.T) {
	e := newEnv(t, "", envOpt{broker: func(b *sim.Broker) trader.Broker { return rejecting{b} }})
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || e.stat("rejected") != 1 || e.stat("cancel_rejected") != 1 || e.tr.Cool["XUSDT"] != NOW+30 || len(e.tr.Failed) != 0 {
		t.Error("rejected")
	}
}

func TestAWorkingEntryThatExpiresUnfilledIsCancelledAsExpired(t *testing.T) {
	e := newDefault(t)
	e.seek(NOW)
	_ = e.br.CancelEntry(e.tr.Slot("XUSDT")) // no reason recorded
	if e.tr.SlotCount() != 0 || e.stat("cancel_expired") != 1 {
		t.Error("expired")
	}
}

func TestSkippedPlanIsJournaledWithItsShadowOutcome(t *testing.T) {
	e := newDefault(t)
	b := e.b()
	sells(b, NOW-30, 120_000) // a weak wall: 50 s of the flow
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || e.stat("plans") != 0 || len(e.tr.Shadows()) != 1 { // nothing traded, not even counted
		t.Fatal("skip")
	}
	sh := e.tr.Shadows()[0]
	if sh.Act != "skip:weak" || !sh.Final || !strings.HasPrefix(sh.Plan.PID, "XUSDT-") {
		t.Fatalf("shadow %+v", sh)
	}
	Print(b, NOW+1, 10001, 50, true) // the plain entry would have been filled ...
	Print(b, NOW+5, 9985, 50, true)  // ... and stopped
	e.tr.StepShadows(NOW + 6)
	if len(e.tr.Shadows()) != 0 {
		t.Fatal("shadow not journaled")
	}
	rs := e.st.Plans(10)
	if len(rs) != 1 {
		t.Fatalf("%d records", len(rs))
	}
	var rec map[string]any
	_ = json.Unmarshal(rs[0], &rec)
	so := rec["shadow"].(map[string]any)
	f := rec["features"].(map[string]any)
	if rec["act"] != "skip:weak" || rec["skip"] != "weak" || rec["ver"] != meta.Version || so["hit"] != "stop" ||
		so["r"].(float64) >= -1.0 || math.Abs(f["hold_s"].(float64)-50) > 0.2 {
		t.Errorf("record %v", rec)
	}
	for _, k := range []string{"pid", "sym", "side", "entry", "stop", "tp", "tp1", "ts"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("key %s missing", k)
		}
	}
	if filepath.Base(e.st.PlansPath()) != "s_plans.jsonl" {
		t.Error(e.st.PlansPath())
	}
}

func TestAStrongWallTradesAndASkippedPlanWithoutAFillIsANofill(t *testing.T) {
	e := newDefault(t)
	b := e.b()
	sells(b, NOW-30, 30_000) // 200 s worth: strong
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	if s == nil || s.Plan.Skip != "" {
		t.Fatal("no trade")
	}
	near(t, "hold_s", *s.Plan.Features.HoldS, 200, 0.5)
	if sh := e.tr.ShadowByPID(s.Plan.PID); sh.Act != "open" || sh.Final {
		t.Error("shadow")
	}
	e2 := newEnv(t, filepath.Join(t.TempDir(), "b"), envOpt{})
	sells(e2.b(), NOW-30, 120_000)
	e2.seek(NOW)
	e2.tr.StepShadows(NOW + 200) // the tape never went through the entry
	rec := lastPlan(t, e2.st)
	so := rec["shadow"].(map[string]any)
	if so["hit"] != "nofill" || so["r"] != nil || len(so) != 2 {
		t.Errorf("shadow %v", so)
	}
}

func TestYoungAndThinStopSkipsAreJournaled(t *testing.T) {
	e := newEnv(t, "", envOpt{book: LongBook(NOW, Age(30))})
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || e.tr.Shadows()[0].Act != "skip:young" {
		t.Error("young")
	}
	e = newEnv(t, "", envOpt{book: LongBook(NOW, Depth(1))})
	e.seek(NOW)
	sh := e.tr.Shadows()[0]
	if e.tr.SlotCount() != 0 || sh.Act != "skip:thin_stop" || sh.Plan.Skip != "thin_stop" || e.stat("thin_stop") != 1 {
		t.Error("thin stop")
	}
}

func TestSkippedPlansAreRateLimitedPerDensityAndGateSet(t *testing.T) {
	e := newDefault(t)
	_, ghost := setup.Scan(LongBook(NOW, Age(30)), NOW, nil, nil)
	e.tr.AddShadow("XUSDT", ghost, NOW, "skip:young", true)
	n := func() int { return len(e.tr.Shadows()) }
	if n() != 1 {
		t.Fatal(n())
	}
	e.tr.AddShadow("XUSDT", ghost, NOW+599, "skip:young", true) // the same density and gates within 10 min
	if n() != 1 {
		t.Fatal(n())
	}
	e.tr.AddShadow("XUSDT", ghost, NOW+601, "skip:young", true)
	if n() != 2 {
		t.Fatal(n())
	}
	_, ghost2 := setup.Scan(LongBook(NOW, Age(30)), NOW, nil, nil)
	ghost2.Skip = "young+weak" // other gates: its own timer
	e.tr.AddShadow("XUSDT", ghost2, NOW+602, "skip:young+weak", true)
	if n() != 3 {
		t.Fatal(n())
	}
	plan, _ := setup.Scan(LongBook(NOW), NOW, nil, nil) // a traded plan is never rate-limited
	e.tr.AddShadow("XUSDT", plan, NOW+603, "open", false)
	e.tr.AddShadow("XUSDT", plan, NOW+604, "open", false)
	if n() != 5 {
		t.Errorf("shadows %d", n())
	}
}

func TestATradedPlanIsJournaledAsFilledAndTheTradeCarriesVerAndPid(t *testing.T) {
	e := newDefault(t)
	b := e.b()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	fill := Print(b, NOW+1, 10001, 50, true)
	e.br.OnTrade("XUSDT", fill)
	e.sc.Settle()
	sh := e.tr.ShadowByPID(s.Plan.PID)
	if s.State != "open" || sh.Act != "filled" || !sh.Final {
		t.Fatal("fill")
	}
	take := Print(b, NOW+30, 10201, 50, false)
	e.br.OnTrade("XUSDT", take)
	e.sc.Settle()
	tr := e.lastTrade()
	if tr.Ver != "0.5.0" || tr.PID != s.Plan.PID || !strings.HasPrefix(tr.PID, "XUSDT-") {
		t.Errorf("trade %+v", tr)
	}
	e.tr.StepShadows(NOW + 31)
	rec := lastPlan(t, e.st)
	so := rec["shadow"].(map[string]any)
	if rec["pid"] != tr.PID || rec["act"] != "filled" || so["hit"] != "take" || so["r"].(float64) <= 5 || len(e.tr.Shadows()) != 0 {
		t.Errorf("record %v", rec)
	}
}

func TestTheFeedKeepsWatchingASymbolWithAShadowPlan(t *testing.T) {
	e := newDefault(t)
	if len(e.tr.Keep()) != 0 {
		t.Fatal("keep not empty")
	}
	plan, _ := setup.Scan(LongBook(NOW), NOW, nil, nil)
	e.tr.AddShadow("XUSDT", plan, NOW, "open", false)
	if k := e.tr.Keep(); len(k) != 1 || !k["XUSDT"] {
		t.Errorf("keep %v", k)
	}
}

func TestAShadowOfASymbolThatLeftTheWatchListIsLost(t *testing.T) {
	e := newDefault(t)
	plan, _ := setup.Scan(LongBook(NOW), NOW, nil, nil)
	e.tr.AddShadow("XUSDT", plan, NOW, "cancel:price_away", true)
	e.tr.RemoveBook("XUSDT")
	e.tr.StepShadows(NOW + 5)
	so := lastPlan(t, e.st)["shadow"].(map[string]any)
	if so["hit"] != "lost" || so["r"] != nil || len(e.tr.Shadows()) != 0 {
		t.Error("lost")
	}
}

// ------------------------------------------------------------------ the market filter
func others(e *env, changes []float64, now float64, synced bool) {
	for i, ch := range changes {
		b := LongBook(now, Sym("S"+string(rune('0'+i))+"USDT"))
		old := 1.003 / (1 + ch)
		b.PrependBars([]book.Bar{{Min: int32(math.Floor((now - 1800) / 60)), Hi: b.T(old + 0.0001), Lo: b.T(old - 0.0001)}})
		b.Synced = synced
		e.tr.AddBook("S"+string(rune('0'+i))+"USDT", b)
	}
}

func TestMarketIsTheMedianChangeOfTheOtherCoins(t *testing.T) {
	e := newEnv(t, "", envOpt{now: T0, book: LongBook(T0)})
	others(e, []float64{-0.02, -0.012, -0.011, -0.001, 0.003, 0.005}, T0, true)
	e.b().PrependBars([]book.Bar{{Min: int32(math.Floor((T0 - 1800) / 60)), Hi: 15000, Lo: 15000}}) // XUSDT itself fell 33 %: not part of its median
	m := e.tr.Market("XUSDT", T0)
	if m == nil {
		t.Fatal("nil")
	}
	near(t, "median of 6", *m, -0.006, 1e-4) // the mean of the middle two
	e.tr.RemoveBook("S0USDT")
	e.tr.ResetMarket()
	near(t, "median of 5", *e.tr.Market("XUSDT", T0), -0.001, 1e-4) // 5 others: the middle one
	e.tr.RemoveBook("S1USDT")
	e.tr.ResetMarket()
	if e.tr.Market("XUSDT", T0) != nil { // fewer than 5: no verdict
		t.Error("verdict with 4")
	}
}

func TestMarketIgnoresBlindBooksAndIsCachedFor10S(t *testing.T) {
	e := newEnv(t, "", envOpt{now: T0, book: LongBook(T0)})
	others(e, []float64{-0.02, -0.02, -0.02, -0.02, -0.02, -0.02}, T0, false)
	if e.tr.Market("XUSDT", T0) != nil {
		t.Fatal("blind books counted")
	}
	for _, b := range e.tr.Books() {
		b.Synced = true
	}
	if e.tr.Market("XUSDT", T0+5) != nil { // cached: refreshed every 10 s
		t.Fatal("not cached")
	}
	m := e.tr.Market("XUSDT", T0+11)
	if m == nil {
		t.Fatal("nil after refresh")
	}
	near(t, "market", *m, -0.02, 1e-4)
}

func TestAMarketWideFallSkipsTheLongAndACalmMarketDoesNot(t *testing.T) {
	e := newEnv(t, "", envOpt{now: T0, book: LongBook(T0)})
	setup.P.MktMax = 0.01
	others(e, []float64{-0.02, -0.02, -0.02, -0.02, -0.02, -0.02}, T0, true)
	e.seek(T0)
	sh := e.tr.Shadows()
	if e.tr.SlotCount() != 0 || len(sh) == 0 || sh[0].Act != "skip:market" {
		t.Fatal("not skipped")
	}
	near(t, "mkt", *sh[0].Plan.Features.Mkt, -0.02, 1e-4)
	e = newEnv(t, filepath.Join(t.TempDir(), "calm"), envOpt{now: T0, book: LongBook(T0)})
	others(e, []float64{-0.002, 0.001, 0.0, 0.003, -0.001, 0.002}, T0, true)
	e.seek(T0)
	if s := e.tr.Slot("XUSDT"); s == nil || s.Plan.Features.Mkt == nil {
		t.Error("calm market")
	}
	e = newEnv(t, filepath.Join(t.TempDir(), "few"), envOpt{now: T0, book: LongBook(T0)}) // no other coins: no filter
	e.seek(T0)
	if s := e.tr.Slot("XUSDT"); s == nil || s.Plan.Features.Mkt != nil {
		t.Error("few coins")
	}
}

// ------------------------------------------------------------------ risk
func TestDayLossInRBlocksNewEntries(t *testing.T) {
	e := newDefault(t)
	day := state.UTCDay(NOW)
	trader.RISK.DayLossR = 6.0
	setDay := func(r float64) {
		e.st.Mutate(func(d *state.Data) { d.Days = map[string]state.Agg{day: {N: 12, PNL: -0.1, R: r}} })
	}
	setDay(-5.9)
	if got := e.tr.Block(NOW); got != "" { // -5.9 R: still free
		t.Fatalf("block %q", got)
	}
	e.seek(NOW)
	if e.tr.Slot("XUSDT") == nil {
		t.Fatal("no entry")
	}
	e.tr.ClearSlots()
	setDay(-6.0)
	why := e.tr.Block(NOW)
	if !strings.Contains(why, "R") || !strings.Contains(why, "-6.00R") || strings.Contains(why, "USDT") { // the pnl limit (-1 USDT) was not reached
		t.Fatalf("block %q", why)
	}
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || !strings.Contains(e.tr.LastBlock, "-6.00R") {
		t.Error("seek while blocked")
	}
	e.st.Mutate(func(d *state.Data) { d.Days = map[string]state.Agg{"1969-12-31": {N: 12, PNL: -0.1, R: -9}} })
	if e.tr.Block(NOW) != "" { // yesterday's R does not count
		t.Error("yesterday counted")
	}
	setDay(-9)
	trader.RISK.DayLossR = 0 // off
	if e.tr.Block(NOW) != "" {
		t.Error("off")
	}
	trader.RISK.DayLossR = 10
	if e.tr.Block(NOW) != "" {
		t.Error("10")
	}
	trader.RISK.DayLossR = 9
	if !strings.Contains(e.tr.Block(NOW), "-9.00R") {
		t.Error("9")
	}
}

func ring(e *env, n int, r float64, ver string) {
	vers := make([]string, n)
	rs := make([]*float64, n)
	for i := range vers {
		x := r
		vers[i], rs[i] = ver, &x
	}
	e.st.SetGuardRing(vers, rs)
}

func TestLiveGuardHaltsOnABadMeanROnlyWhenLive(t *testing.T) {
	e := newDefault(t)
	trader.RISK.GuardN, trader.RISK.GuardR = 30, -0.3
	ring(e, 30, -0.5, meta.Version) // mean -0.5 R over 30 trades of this version
	e.tr.Live = false
	if e.tr.Block(NOW) != "" { // paper: the guard is not applied
		t.Fatal("applied in paper")
	}
	e.tr.Live = true
	e.tr.ResetGuard()
	why := e.tr.Block(NOW)
	if !strings.Contains(why, "halted") || !strings.Contains(why, "-0.50R over 30 trades") {
		t.Fatalf("block %q", why)
	}
	e.seek(NOW)
	if e.tr.SlotCount() != 0 || !strings.Contains(e.tr.Halt, "halted") { // the trader halts
		t.Error("no halt")
	}
}

func TestLiveGuardThresholdsAndVersions(t *testing.T) {
	e := newDefault(t)
	trader.RISK.GuardN, trader.RISK.GuardR = 30, -0.3
	e.tr.Live = true
	verdict := func() string { e.tr.ResetGuard(); return e.tr.Block(NOW) }
	ring(e, 29, -0.5, meta.Version)
	if verdict() != "" { // one trade short
		t.Error("29")
	}
	ring(e, 30, -0.29, meta.Version)
	if verdict() != "" { // not below the limit
		t.Error("-0.29")
	}
	ring(e, 30, 0.4, meta.Version)
	if verdict() != "" {
		t.Error("0.4")
	}
	ring(e, 30, -0.5, "0.4.0") // trades of an older version do not count
	if verdict() != "" {
		t.Error("old version")
	}
	x := -0.5
	vers := make([]string, 40)
	rs := make([]*float64, 40)
	for i := range vers {
		vers[i], rs[i] = "0.4.0", &x
	}
	for i := 10; i < 40; i++ {
		vers[i] = meta.Version
	}
	e.st.SetGuardRing(vers, rs)
	if !strings.Contains(verdict(), "halted") {
		t.Error("mixed versions")
	}
	vers = make([]string, 40)
	rs = make([]*float64, 40)
	for i := range vers {
		vers[i] = meta.Version
	}
	e.st.SetGuardRing(vers, rs) // no R, no verdict
	if verdict() != "" {
		t.Error("no r")
	}
	ring(e, 40, -0.5, meta.Version)
	trader.RISK.GuardN = 0 // off
	if verdict() != "" {
		t.Error("off")
	}
	trader.RISK.GuardN, trader.RISK.GuardR = 30, -1.0
	if verdict() != "" { // -0.5 is above -1
		t.Error("-1")
	}
}

func TestTheGuardVerdictIsCachedAndResetWhenATradeCloses(t *testing.T) {
	e := newDefault(t)
	trader.RISK.GuardN, trader.RISK.GuardR = 30, -0.3
	e.tr.Live = true
	ring(e, 29, -1.0, meta.Version)
	if v, ok := e.tr.GuardCached(); ok || v != "" {
		t.Fatal("cached before any block")
	}
	if e.tr.Block(NOW) != "" {
		t.Fatal("blocked at 29")
	}
	if v, ok := e.tr.GuardCached(); !ok || v != "" { // cached
		t.Fatal("not cached")
	}
	e.stopped(0.9980) // the 30th trade of this version: about -3 R
	if _, ok := e.tr.GuardCached(); ok {
		t.Fatal("cache not reset")
	}
	if !strings.Contains(e.tr.Block(NOW), "halted") {
		t.Error("no halt")
	}
}

// ------------------------------------------------------------------ SimBroker lag
func TestSimCancelLagLetsAFillThrough(t *testing.T) {
	e := newEnv(t, "", envOpt{lag: 0.4})
	b := e.b()
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	b.Densities[bidKey].Gone = "pulled"
	_ = e.tr.WatchEntry(s, b, NOW+1) // the cancel is on its way ...
	if !s.CancelSent || s.State != "entry" || e.br.Order("XUSDT") == nil {
		t.Fatal("cancel state")
	}
	e.fill(NOW+1.2, 10001, 50, true) // ... and the order still fills meanwhile
	if s.State != "open" {
		t.Fatal("did not fill")
	}
	e.sc.Advance(0.6) // the late cancel finds a position: ignored
	if e.tr.Slot("XUSDT").State != "open" || e.br.Order("XUSDT").Kind != "open" {
		t.Error("late cancel hurt")
	}
	if e.stat("cancelled") != 0 || e.stat("filled") != 1 {
		t.Error("stats")
	}
}

func TestSimCancelLagTakesEffectLaterWhenNothingFills(t *testing.T) {
	e := newEnv(t, "", envOpt{lag: 0.2})
	b := e.b()
	e.seek(NOW)
	b.Densities[bidKey].Gone = "pulled"
	_ = e.tr.WatchEntry(e.tr.Slot("XUSDT"), b, NOW+1)
	if e.tr.Slot("XUSDT") == nil { // not yet
		t.Fatal("cancelled at once")
	}
	e.sc.Advance(0.4)
	if e.tr.Slot("XUSDT") != nil || e.br.Order("XUSDT") != nil || e.stat("cancel_density_gone") != 1 {
		t.Error("late cancel")
	}
}

func TestSimWithoutLagCancelsAtOnceAndALatePrintChangesNothing(t *testing.T) {
	e := newDefault(t)
	b := e.b()
	e.seek(NOW)
	b.Densities[bidKey].Gone = "pulled"
	_ = e.tr.WatchEntry(e.tr.Slot("XUSDT"), b, NOW+1)
	if e.tr.SlotCount() != 0 || e.br.Order("XUSDT") != nil {
		t.Fatal("not cancelled")
	}
	e.fill(NOW+1.2, 10001, 50, true)
	if e.stat("filled") != 0 || e.stat("cancel_density_gone") != 1 {
		t.Error("stats")
	}
}

func TestSimNewEntryIgnoresTradesOlderThanTheLag(t *testing.T) {
	e := newEnv(t, "", envOpt{lag: 0.5})
	e.seek(NOW)
	s := e.tr.Slot("XUSDT")
	near(t, "live_at", e.br.Order("XUSDT").LiveAt, NOW+0.5, 1e-9)
	e.fill(NOW+0.3, 10001, 50, true) // printed before the order reached the exchange
	if s.State != "entry" {
		t.Fatal("filled early")
	}
	e.fill(NOW+0.6, 10001, 50, true)
	if s.State != "open" {
		t.Fatal("not filled")
	}
	e = newEnv(t, filepath.Join(t.TempDir(), "nolag"), envOpt{})
	e.seek(NOW)
	e.fill(NOW+0.3, 10001, 50, true)
	if e.tr.Slot("XUSDT").State != "open" {
		t.Error("no-lag fill")
	}
}

func TestPlansJSONIsValidOneObjectALine(t *testing.T) {
	e := newDefault(t)
	sells(e.b(), NOW-30, 120_000)
	e.seek(NOW)
	e.tr.StepShadows(NOW + 200)
	rs := e.st.Plans(10)
	var rec map[string]any
	if len(rs) != 1 || json.Unmarshal(rs[0], &rec) != nil || rec["act"] != "skip:weak" {
		t.Errorf("plans %v", rs)
	}
}

type codeError struct{}

func (codeError) Error() string { return "-2021 would trigger immediately" }
func (codeError) Code() int     { return -2021 }

type stopFail struct {
	*sim.Broker
	err error
}

func (s stopFail) MoveStop(*trader.Slot, float64) error { return s.err }

func TestMoveStopMinus2021ClosesThePosition(t *testing.T) {
	var sf *stopFail
	e := newEnv(t, "", envOpt{broker: func(b *sim.Broker) trader.Broker { sf = &stopFail{Broker: b, err: codeError{}}; return sf }})
	trader.RISK.Notional = 60
	parts(e)
	s := e.openBigLong()
	e.fill(NOW+5, 10101, 50, false) // tp1: break-even is due
	e.tr.WatchOpen(s, e.b(), NOW+5)
	if e.tr.SlotCount() != 0 || e.lastTrade().Reason != "stop_be" { // the -2021 rule: close instead of moving
		t.Errorf("slots %d", e.tr.SlotCount())
	}
	// any other error is only reported
	sf.err = errors.New("network")
	e2 := newEnv(t, filepath.Join(t.TempDir(), "x"), envOpt{broker: func(b *sim.Broker) trader.Broker { return stopFail{Broker: b, err: errors.New("network")} }})
	trader.RISK.Notional = 60
	s2 := e2.openBigLong()
	e2.fill(NOW+5, 10101, 50, false)
	e2.tr.WatchOpen(s2, e2.b(), NOW+5)
	if e2.tr.SlotCount() != 1 || !e2.hasEvent("move stop: network") {
		t.Error("other error")
	}
}

func TestLoadRiskReadsTheEnvironment(t *testing.T) {
	PinRisk(t)
	env := map[string]string{"NOTIONAL": "25", "MAX_OPEN": "3.9", "GUARD_R": "-0.5", "FEE_REBATE": " 0.4 "}
	if err := trader.LoadRisk(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	r := trader.RISK
	if r.Notional != 25 || r.MaxOpen != 3 || r.GuardR != -0.5 || r.FeeRebate != 0.4 || r.Leverage != 2 {
		t.Errorf("risk %+v", r)
	}
	if trader.RiskUSD() != 25*0.002 {
		t.Error("risk usd")
	}
	if err := trader.LoadRisk(func(k string) string { return map[string]string{"DAY_LOSS": "x"}[k] }); err == nil {
		t.Error("bad value accepted")
	}
}

func TestOrderedBooksAndSlotsKeepInsertionOrder(t *testing.T) {
	e := newDefault(t)
	for _, s := range []string{"BUSDT", "AUSDT", "CUSDT"} {
		e.tr.AddBook(s, LongBook(NOW, Sym(s)))
	}
	e.tr.AddBook("XUSDT", LongBook(NOW)) // set again: keeps its (first) place
	e.tr.RemoveBook("AUSDT")
	e.tr.AddBook("AUSDT", LongBook(NOW, Sym("AUSDT")))
	if got := strings.Join(e.tr.BookSyms(), ","); got != "XUSDT,BUSDT,CUSDT,AUSDT" {
		t.Errorf("book order %s", got)
	}
	if v := e.tr.View(nil); len(v) != 0 {
		t.Error("view")
	}
}

func TestViewListsTradesInOrder(t *testing.T) {
	e := newDefault(t)
	e.openLong()
	v := e.tr.View(nil)
	if len(v) != 1 || v[0].Symbol != "XUSDT" || v[0].State != "open" || v[0].Mark == nil || v[0].N != 9.0 {
		t.Fatalf("view %+v", v)
	}
	near(t, "upnl", v[0].UPnL, 9*(1.0030-1.0002)-9*1.0002*0.0002, 1e-3)
	if v[0].AgeS != 0 {
		t.Errorf("age %d", v[0].AgeS)
	}
}
