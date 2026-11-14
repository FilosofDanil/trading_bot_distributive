package runner

import (
	"io"
	"strings"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/feed"
	"densitybounce/internal/setup"
	"densitybounce/internal/state"
	"densitybounce/internal/testkit"
	"densitybounce/internal/trader"
)

func newRunner(t *testing.T) *Runner {
	t.Helper()
	testkit.Silence()
	testkit.PinP(t)
	testkit.PinRisk(t)
	store, err := state.Open(t.TempDir()+"/s.json", 100)
	if err != nil {
		t.Fatal(err)
	}
	store.Out = io.Discard
	r := New(store, "paper", "closes", "", nil)
	r.Trader.Logf = func(string, ...any) {}
	return r
}

func f64(x float64) *float64 { return &x }

func TestStrategyViewBookIsEmptyForLossGuard(t *testing.T) {
	r := newRunner(t)
	s := r.Strategy()
	if s.ID != "bounce.density" || len(s.Book) != 0 || s.Book == nil || s.Equity != 100 || s.State != "running" || s.Mode != "paper" {
		t.Fatalf("%+v", s)
	}
	if s.Symbols == nil || s.Working == nil || s.LastRunAt != nil || s.LastError != nil {
		t.Fatalf("%+v", s)
	}
	r.Trader.Halt = "total loss"
	if s := r.Strategy(); s.State != "error" || s.LastError == nil || *s.LastError != "total loss" {
		t.Fatalf("%+v", s)
	}
}

func TestPlanReportAndPositionsViews(t *testing.T) {
	r := newRunner(t)
	r.Store.RecordTrade(state.Trade{TsClose: 1000, Side: "long", Sym: "XUSDT", Reason: "take", Gross: 0.05, Fees: 0.004,
		PNL: 0.046, R: f64(1.8), Parts: []state.TradePart{}})
	p := r.Plan()
	found := false
	for _, l := range p.Text {
		found = found || strings.HasPrefix(l, "rules: approach >= 0.80%/180s")
	}
	if len(p.Book) != 0 || p.Book == nil || !found || p.Open == nil || !p.Accepted || p.Preview {
		t.Fatalf("%+v", p)
	}
	if !strings.HasPrefix(p.Text[0], "mode paper · running · watching 0 (0 synced, lag 0 s) · 0 densities") {
		t.Fatal(p.Text[0])
	}
	rep := r.Report()
	if rep.Trades != 1 || rep.Equity != 100.046 || !strings.HasPrefix(rep.Text[0], "trades 1 · win 100.0% · avg +1.80R · pnl +0.0460 USDT (gross +0.0500, fees 0.0040)") {
		t.Fatalf("%+v", rep)
	}
	if len(rep.Curve) != 1 || rep.Curve[0][0] != "1970-01-01" || rep.PairStops != 0 {
		t.Fatalf("curve %v", rep.Curve)
	}
	last := rep.Text[len(rep.Text)-1]
	if last != "01-01T00:16 long  XUSDT        take     +0.0460 +1.80R" {
		t.Fatalf("last line %q", last)
	}
	if pos := r.Positions(); pos == nil || len(pos) != 0 {
		t.Fatalf("positions %v", pos)
	}
	if got := r.DaySummary("1970-01-01"); got != "bounce.density 1970-01-01: 1 trades, win 100%, pnl +0.0460 USDT (fees 0.0040), avg +1.80R" {
		t.Fatal(got)
	}
	if got := r.DaySummary("2000-01-01"); got != "bounce.density 2000-01-01: no trades" {
		t.Fatal(got)
	}
}

func TestOpenTradeAppearsInTheViews(t *testing.T) {
	r := newRunner(t)
	b := testkit.LongBook(1000)
	r.Trader.AddBook("XUSDT", b)
	s := &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.0050}, Qty: 9, State: "open",
		FilledQty: 9, EntryPx: 1.0002, OpenedAt: 1000}
	w := &trader.Slot{Sym: "YUSDT", Plan: &setup.Plan{Side: "short", Entry: 1, Stop: 1.01, TP: 0.9}, Qty: 5, State: "entry"}
	r.Trader.PutSlot(s)
	r.Trader.PutSlot(w)
	st := r.Strategy()
	if len(st.Symbols) != 1 || st.Symbols[0] != "XUSDT" || len(st.Working) != 1 || st.Working[0] != "YUSDT" || st.OpenPositions != 1 {
		t.Fatalf("%+v", st)
	}
	pos := r.Positions()
	if len(pos) != 1 || pos[0].Symbol != "XUSDT" || !strings.HasPrefix(pos[0].Text, "XUSDT long 9 @ 1.0002 stop 0.9985 take 1.005 mark 1.003 upnl +0.0") {
		t.Fatalf("%+v", pos)
	}
	lines := strings.Join(r.PlanLines(), "\n")
	for _, want := range []string{"OPEN long XUSDT entry 1.0002 stop 0.9985 take 1.005 upnl", "ENTRY short YUSDT entry 1 stop 1.01 take 0.9",
		"nearest densities:", "  XUSDT bid 1  100k USDT"} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in\n%s", want, lines)
		}
	}
	if p := r.Plan(); strings.Join(p.Open, ",") != "XUSDT,YUSDT" || p.Candidates != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestSetDesiredEventsAndFlattenNeedsALoop(t *testing.T) {
	r := newRunner(t)
	s := r.SetDesired("paused", nil, "tg")
	if s.Desired != "paused" {
		t.Fatal(s)
	}
	ev := r.Store.Events()
	last := ev[len(ev)-1]
	if !last.Notify || last.Msg != "bounce.density paused: no new entries, open trades keep their stop and take (tg)" {
		t.Fatalf("%+v", last)
	}
	n := len(r.Store.Events())
	r.SetDesired("paused", nil, "") // no change, no event
	if len(r.Store.Events()) != n {
		t.Fatal("an unchanged desired state must not log")
	}
	if _, err := r.Flatten(1); err != ErrNotRunning {
		t.Fatalf("err %v", err)
	}
	if r.Trigger().Preview != true {
		t.Fatal("trigger is a preview")
	}
	ev = r.Store.Events()
	if m := ev[len(ev)-1].Msg; !strings.HasPrefix(m, "bounce.density now:\nmode paper") {
		t.Fatalf("%q", m)
	}
}

func TestFlattenWithARunningLoopClosesTrades(t *testing.T) {
	r := newRunner(t)
	r.running.Store(true)
	s := &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1, Stop: 0.99, TP: 1.1}, Qty: 5, State: "arm"}
	r.Trader.PutSlot(s)
	res, err := r.Flatten(891477091)
	if err != nil || res.Slots != 1 || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	ev := r.Store.Events()
	if m := ev[len(ev)-1].Msg; m != "bounce.density: flatten by 891477091: 1 trade(s) cancelled / closed, paused" {
		t.Fatal(m)
	}
}

func TestHeartbeatFeedDownAndDayChange(t *testing.T) {
	r := newRunner(t)
	b := book.New("XUSDT", 0.0001)
	r.Trader.AddBook("XUSDT", b)
	r.day = "2000-01-01"
	r.beat(1e9)
	ev := r.Store.Events()
	if m := ev[len(ev)-1].Msg; m != "bounce.density 2000-01-01: no trades" {
		t.Fatalf("%q", m)
	}
	r.beat(1e9 + 200) // down for 200 s
	ev = r.Store.Events()
	if m := ev[len(ev)-1]; m.Level != "error" || m.Msg != "no order book synced for 200 s (1 symbols)" || !m.Notify {
		t.Fatalf("%+v", m)
	}
	b.Synced = true
	r.beat(1e9 + 300)
	ev = r.Store.Events()
	if m := ev[len(ev)-1].Msg; m != "market data back" || r.feedDownSince != 0 {
		t.Fatalf("%q", m)
	}
}

func TestLagString(t *testing.T) {
	for _, c := range []struct {
		w   int
		lag float64
		s   string
	}{{0, 0, "0"}, {2, 0, "0.0"}, {2, 0.3, "0.3"}, {2, 12, "12.0"}} {
		if got := lagStr(feed.Health{Watching: c.w, MaxLagS: c.lag}); got != c.s {
			t.Errorf("lagStr(%d, %v) = %q, want %q", c.w, c.lag, got, c.s)
		}
	}
}
