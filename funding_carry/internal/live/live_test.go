package live

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
)

const SYM = "XUSDT"

func approx(a, b, rel float64) bool {
	return math.Abs(a-b) <= rel*math.Max(math.Abs(a), math.Abs(b))+1e-12
}

func setup(t *testing.T) (*Live, *FakeExchange, *state.Store) {
	ex := NewFake(1000, 1)
	store, err := state.New(filepath.Join(t.TempDir(), "live.json"), 500)
	if err != nil {
		t.Fatal(err)
	}
	return New(ex, store, DefaultProtect(), true), ex, store
}

// a key without Universal Transfer: the owner funded both wallets
func setupNoTransfer(t *testing.T) (*Live, *FakeExchange, *state.Store) {
	ex := NewFake(110, 1)
	ex.Fut = 250
	store, _ := state.New(filepath.Join(t.TempDir(), "live.json"), 300)
	return New(ex, store, DefaultProtect(), false), ex, store
}

func hasEvent(s *state.Store, level, sub string) bool {
	for _, e := range s.Snapshot().Events {
		if (level == "" || e.Level == level) && strings.Contains(e.Msg, sub) {
			return true
		}
	}
	return false
}

func mustOpen(t *testing.T, lv *Live, n float64) *state.Held {
	h, err := lv.OpenPair(SYM, n, "test")
	if err != nil || h == nil {
		t.Fatal(h, err)
	}
	return h
}

func TestNoTransferOpenUsesEachWallet(t *testing.T) {
	lv, ex, _ := setupNoTransfer(t)
	mustOpen(t, lv, 100)
	if ex.Called("transfer") != 0 || !approx(ex.Pos[SYM].Margin, strategy.P.Buffer*100, 0.02) ||
		ex.Spot["USDT"] >= 10 || ex.Fut >= 50 || len(ex.StopOrders[SYM]) == 0 {
		t.Fatal(ex.Pos[SYM], ex.Spot, ex.Fut)
	}
}

func TestNoTransferRefusesWithoutFuturesMargin(t *testing.T) {
	lv, ex, store := setupNoTransfer(t)
	ex.Fut = 150 // 2N = 200 needed
	if h, _ := lv.OpenPair(SYM, 100, "test"); h != nil {
		t.Fatal("must refuse")
	}
	if ex.Called("perp_order") != 0 || ex.Called("transfer") != 0 || !hasEvent(store, "", "margin on futures") {
		t.Fatal(ex.Calls)
	}
}

func TestNoTransferCloseKeepsMarginOnFuturesAndTopupUsesFutures(t *testing.T) {
	lv, ex, store := setupNoTransfer(t)
	mustOpen(t, lv, 100)
	ex.Fut += 100 // spare margin on futures
	ex.Pos[SYM].Margin = 160
	ex.Px[SYM] = [2]float64{1.75, 1.75} // liquidation ~49 % away -> top-up
	lv.WatchOnce()
	if ex.Pos[SYM].Margin <= 160 || ex.Called("transfer") != 0 {
		t.Fatal(ex.Pos[SYM].Margin)
	}
	before := ex.Fut
	h, _ := store.Snapshot().Held.Get(SYM)
	rec, err := lv.ClosePair(SYM, h, "test close", false)
	if err != nil || rec == nil || ex.Called("transfer") != 0 || ex.Fut <= before+100 {
		t.Fatal(rec, err, ex.Fut, before)
	}
}

func TestOpenPairHedgesMarginsAndPlacesTheStop(t *testing.T) {
	lv, ex, store := setup(t)
	h := mustOpen(t, lv, 100)
	if !approx(h.Q, 100, 1e-6) || !approx(ex.Pos[SYM].Amt, -100, 1e-6) {
		t.Fatal(h, ex.Pos[SYM])
	}
	if ex.Spot["X"] < h.Q*0.999 || !approx(h.QSpot, ex.Spot["X"], 1e-6) { // fee in the coin
		t.Fatal(ex.Spot, h)
	}
	if !approx(ex.Pos[SYM].Margin, strategy.P.Buffer*100, 0.02) || ex.Pos[SYM].Liq <= 2.9 { // total 2N, ~ +195 %
		t.Fatal(ex.Pos[SYM])
	}
	if len(ex.StopOrders[SYM]) != 1 || !approx(ex.StopOrders[SYM][0].Trigger, 2.2, 1e-6) { // +120 %
		t.Fatal(ex.StopOrders)
	}
	if !store.Snapshot().Held.Has(SYM) {
		t.Fatal("not held")
	}
}

func TestSpotFailureUnwindsTheShort(t *testing.T) {
	lv, ex, store := setup(t)
	ex.Fail["spot_order"] = true
	if h, _ := lv.OpenPair(SYM, 100, "test"); h != nil {
		t.Fatal("must not open")
	}
	if ex.Pos[SYM] != nil || math.Abs(ex.Spot["USDT"]-1000) > 0.2 || ex.Fut >= 1 || store.Snapshot().Held.Len() != 0 {
		t.Fatal(ex.Pos, ex.Spot, ex.Fut)
	}
	if !hasEvent(store, "error", "unwound") {
		t.Fatal("no event")
	}
}

func TestPerpFailureReturnsTheMargin(t *testing.T) {
	lv, ex, _ := setup(t)
	ex.Fail["perp_order"] = true
	if h, _ := lv.OpenPair(SYM, 100, "test"); h != nil {
		t.Fatal("must not open")
	}
	if !approx(ex.Spot["USDT"], 1000, 1e-9) || math.Abs(ex.Fut) > 1e-9 {
		t.Fatal(ex.Spot, ex.Fut)
	}
}

func TestRefusesExistingPositionAndMissingCash(t *testing.T) {
	lv, ex, _ := setup(t)
	ex.Pos[SYM] = &Perp{Amt: 0.5, Entry: 1, Mark: 1, Margin: 1} // the owner's own long
	if h, _ := lv.OpenPair(SYM, 100, "test"); h != nil {
		t.Fatal("must refuse")
	}
	delete(ex.Pos, SYM)
	ex.Spot["USDT"] = 50
	if h, _ := lv.OpenPair(SYM, 100, "test"); h != nil || ex.Called("perp_order") != 0 {
		t.Fatal("must refuse")
	}
}

func TestClosePairUnwindsBothLegsAndBooksPnl(t *testing.T) {
	lv, ex, store := setup(t)
	h := mustOpen(t, lv, 100)
	ex.Px[SYM] = [2]float64{1.1, 1.1} // +10 %: short -10, spot +10
	rec, err := lv.ClosePair(SYM, h, "test close", false)
	if err != nil || rec == nil || ex.Pos[SYM] != nil || len(ex.StopOrders[SYM]) != 0 {
		t.Fatal(rec, err)
	}
	if ex.Spot["X"] >= 0.2 || math.Abs(rec.Pnl) >= 1.5 { // only dust left; hedged: fees + rounding only
		t.Fatal(ex.Spot["X"], rec.Pnl)
	}
	st := store.Snapshot()
	if st.Held.Len() != 0 || !approx(st.Capital, 500+rec.Pnl, 1e-6) || ex.Fut >= 1 {
		t.Fatal(st.Capital, ex.Fut)
	}
}

func TestWatchdogAddsMarginWhenLiquidationNears(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	ex.Pos[SYM].Margin = 160 // liq at 2.6
	ex.Px[SYM] = [2]float64{1.75, 1.75}
	before := ex.Pos[SYM].Margin
	lv.WatchOnce()
	if ex.Pos[SYM].Margin <= before || !hasEvent(store, "", "added") {
		t.Fatal(ex.Pos[SYM].Margin)
	}
	lv.WatchOnce()                    // rate limited: no second top-up at once
	if ex.Called("add_margin") != 2 { // one at open, one top-up
		t.Fatal(ex.Calls)
	}
}

func TestWatchdogClosesBeforeLiquidation(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	ex.Px[SYM] = [2]float64{1.95, 1.95} // +95 % >= close_rise
	lv.WatchOnce()
	st := store.Snapshot()
	if ex.Pos[SYM] != nil || st.Held.Len() != 0 || st.Cool[SYM] == 0 || !hasEvent(store, "", "liquidation protection") {
		t.Fatal(st.Cool)
	}
}

func TestWatchdogReplacesAMissingStop(t *testing.T) {
	lv, ex, _ := setup(t)
	mustOpen(t, lv, 100)
	ex.StopOrders = map[string][]StopOrder{}
	lv.WatchOnce()
	if len(ex.StopOrders[SYM]) != 1 {
		t.Fatal(ex.StopOrders)
	}
}

func TestWatchdogSellsSpotWhenThePerpLegIsGone(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	ex.Fut += ex.Pos[SYM].Margin
	delete(ex.Pos, SYM) // the exchange stop fired
	lv.WatchOnce()
	st := store.Snapshot()
	if st.Held.Len() != 0 || ex.Spot["X"] >= 0.2 || st.Cool[SYM] == 0 {
		t.Fatal(ex.Spot, st.Cool)
	}
}

func TestWatchdogWaitsForTheDailyCycle(t *testing.T) {
	lv, ex, _ := setup(t)
	mustOpen(t, lv, 100)
	ex.Px[SYM] = [2]float64{1.95, 1.95}
	done := make(chan struct{})
	lv.mu.Lock()
	go func() { lv.WatchOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchOnce must return at once while the lock is held")
	}
	if ex.Pos[SYM] == nil { // skipped: the lock is held
		t.Fatal("acted while the cycle was trading")
	}
	lv.mu.Unlock()
	lv.WatchOnce()
	if ex.Pos[SYM] != nil {
		t.Fatal("must close now")
	}
}

func TestExecuteClosesThenOpens(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	plan := &strategy.Plan{Close: []string{SYM}, PairStopped: []string{}, Skipped: map[string]string{}, Rehedge: []string{},
		Open: []string{"YUSDT"}, Notional: map[string]float64{"YUSDT": 80}, Score: map[string]float64{"YUSDT": 0.0003}}
	if err := lv.Execute(plan); err != nil {
		t.Fatal(err)
	}
	held := store.Snapshot().Held
	if held.Len() != 1 || !held.Has("YUSDT") || ex.Pos[SYM] != nil || !approx(ex.Pos["YUSDT"].Amt, -40, 1e-6) {
		t.Fatal(held.Keys(), ex.Pos)
	}
}

func TestMarkBooksOpenPnlAndKeepsCapital(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	ex.Px[SYM] = [2]float64{1.1, 1.1}
	row, err := lv.Mark(1_760_054_400_000)
	if err != nil || row.Day != "2025-10-10" || len(row.Book) != 1 {
		t.Fatal(row, err)
	}
	st := store.Snapshot()
	h, _ := st.Held.Get(SYM)
	if st.Capital != 500 || !h.HasBooked || h.Marked != 1_760_054_400_000 || !approx(row.Equity, 500+h.Funding+h.Basis-h.Fees, 1e-9) {
		t.Fatal(st.Capital, h, row)
	}
}

func TestReconcileReportsAMissingPerpLeg(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	delete(ex.Pos, SYM)
	if err := lv.Reconcile(); err != nil || !hasEvent(store, "warn", "perp leg is missing for XUSDT") {
		t.Fatal(err)
	}
}

func TestFlattenClosesEverything(t *testing.T) {
	lv, ex, store := setup(t)
	mustOpen(t, lv, 100)
	n, err := lv.Flatten(7)
	if err != nil || n != 1 || ex.Pos[SYM] != nil || store.Snapshot().Held.Len() != 0 || !hasEvent(store, "", "close all by 7") {
		t.Fatal(n, err)
	}
}

func TestProtectFromEnv(t *testing.T) {
	t.Setenv("LIQ_TOPUP_DIST", "0.5")
	t.Setenv("LIQ_CLOSE_RISE", "bad")
	p := ProtectFromEnv()
	if p.TopupDist != 0.5 || p.CloseRise != 0.90 || p.StopRise != 1.20 || p.CloseDist != 0.25 {
		t.Fatal(p)
	}
	if FloorStep(1.2345, 0.01) != 1.23 || CeilStep(1.231, 0.01) != 1.24 || BaseAsset("XUSDT") != "X" {
		t.Fatal()
	}
}
