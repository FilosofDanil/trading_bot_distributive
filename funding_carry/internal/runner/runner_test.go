package runner

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fundingcarry/internal/live"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
)

const DAY = int64(1_760_054_400_000) // 2025-10-10 00:00 UTC

func newStore(t *testing.T) *state.Store {
	s, err := state.New(filepath.Join(t.TempDir(), "s.json"), 500)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fakeDecide(calls *[][]string, notional float64, score float64) func(int64, state.HeldMap, float64, []string, map[string]int64) (*strategy.Plan, error) {
	return func(day int64, held state.HeldMap, equity float64, cands []string, cool map[string]int64) (*strategy.Plan, error) {
		*calls = append(*calls, append([]string{}, cands...))
		var opens []string
		p := &strategy.Plan{Day: day, Equity: equity, Notional: map[string]float64{}, Score: map[string]float64{}, Prices: map[string]strategy.Pair{},
			Skipped: map[string]string{}}
		for _, s := range cands {
			if !held.Has(s) {
				opens = append(opens, s)
				p.Notional[s] = notional
				p.Score[s] = score
			}
		}
		p.Open = opens
		p.Book = append(append([]string{}, opens...), held.Keys()...)
		if len(cands) > 0 {
			p.Skipped["ZZZUSDT"] = "spot younger than 7 days"
		}
		for _, s := range cands {
			p.Top = append(p.Top, strategy.Top{Symbol: s, Score: 0.0003})
		}
		return p, nil
	}
}

func patch(r *Runner, calls *[][]string, cands []string) {
	r.UpdateUniverse = func(tags map[string][]string, day int64, log func(string)) (map[string][]string, error) {
		return tags, nil
	}
	r.Candidates = func(map[string][]string, int64, map[string]bool) []string { return cands }
	r.SpotSymbols = func() (map[string]bool, error) { return map[string]bool{}, nil }
	r.Decide = fakeDecide(calls, 10, 0.0003)
	r.Orders = func(*strategy.Plan) ([]strategy.Order, error) { return nil, nil }
}

func TestPausedPreviewKeepsTopAndWouldOpen(t *testing.T) {
	var calls [][]string
	r := New(newStore(t), 5, "paper", nil)
	patch(r, &calls, []string{"AAAUSDT", "BBBUSDT"})
	r.SetDesired("paused", nil, "")
	plan, err := r.RunDay(DAY, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(calls[0]) != 2 || len(calls[1]) != 0 { // full decision for info, acted-on one without new pairs
		t.Fatal(calls)
	}
	if !plan.Paused || len(plan.Open) != 0 || len(plan.Book) != 0 || len(plan.Top) != 2 || plan.Top[0].Symbol != "AAAUSDT" ||
		strings.Join(plan.WouldOpen, ",") != "AAAUSDT,BBBUSDT" || plan.Candidates != 2 || plan.Skipped["ZZZUSDT"] == "" {
		t.Fatalf("%+v", plan)
	}
	evs := r.Store.Snapshot().Events
	ev := evs[len(evs)-1]
	if !ev.Notify || !strings.Contains(ev.Msg, "a resume would open AAAUSDT, BBBUSDT") || !strings.Contains(ev.Msg, "top funding") {
		t.Fatal(ev)
	}
}

func TestRunningPreviewOpensAndDecidesOnce(t *testing.T) {
	var calls [][]string
	r := New(newStore(t), 5, "paper", nil)
	patch(r, &calls, []string{"AAAUSDT", "BBBUSDT"})
	plan, err := r.RunDay(DAY, true)
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	if plan.Paused || strings.Join(plan.Open, ",") != "AAAUSDT,BBBUSDT" || len(plan.WouldOpen) != 0 {
		t.Fatalf("%+v", plan)
	}
	if !strings.Contains(string(r.Store.Snapshot().Plan), `"open":["AAAUSDT","BBBUSDT"]`) {
		t.Fatal(string(r.Store.Snapshot().Plan))
	}
}

func TestRunnerFlattenPausesAndCloses(t *testing.T) {
	ex := live.NewFake(1000, 1)
	store := newStore(t)
	lv := live.New(ex, store, live.DefaultProtect(), true)
	r := New(store, 5, "live", lv)
	if h, err := lv.OpenPair("XUSDT", 100, "test"); h == nil || err != nil {
		t.Fatal(h, err)
	}
	if s := r.Strategy(); s.Mode != "live" || s.OpenPositions != 1 {
		t.Fatal(s)
	}
	if _, err := r.Flatten(1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && store.Snapshot().Held.Len() > 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	st := store.Snapshot()
	if st.Held.Len() != 0 || st.Desired != "paused" {
		t.Fatal(st.Desired, st.Held.Keys())
	}
	if _, ok := ex.Pos["XUSDT"]; ok {
		t.Fatal("position left")
	}
}

func TestRunnerLiveDayOpensForRealAndMarks(t *testing.T) {
	var calls [][]string
	ex := live.NewFake(1000, 1)
	store := newStore(t)
	r := New(store, 5, "live", live.New(ex, store, live.DefaultProtect(), true))
	patch(r, &calls, []string{"XUSDT"})
	r.Decide = fakeDecide(&calls, 100, 0.0003)
	if _, err := r.RunDay(DAY, false); err != nil {
		t.Fatal(err)
	}
	st := store.Snapshot()
	if math.Abs(ex.Pos["XUSDT"].Amt+100) > 1e-6 || !st.Held.Has("XUSDT") {
		t.Fatal(ex.Pos)
	}
	if st.LastRunDay == nil || *st.LastRunDay != "2025-10-10" || len(st.Daily) != 1 || st.Daily[0].Book[0] != "XUSDT" {
		t.Fatal(st.LastRunDay, st.Daily)
	}
	found := false
	for _, e := range st.Events {
		found = found || strings.Contains(e.Msg, "(live)")
	}
	if !found || r.Strategy().Equity >= 500 { // opening fees are in the open PnL
		t.Fatal(found, r.Strategy().Equity)
	}
}

func TestPaperRunnerRefusesFlatten(t *testing.T) {
	r := New(newStore(t), 5, "paper", nil)
	if _, err := r.Flatten(nil); err == nil {
		t.Fatal("paper must refuse flatten")
	} else if _, ok := err.(*ValueError); !ok {
		t.Fatal(err)
	}
}

func TestBusyAndReport(t *testing.T) {
	r := New(newStore(t), 5, "paper", nil)
	r.busy.Lock()
	r.busyOn.Store(true)
	if _, err := r.RunDay(DAY, true); err == nil {
		t.Fatal("busy expected")
	}
	if _, err := r.Trigger(); err == nil {
		t.Fatal("busy expected")
	}
	if r.Strategy().State != "busy" {
		t.Fatal(r.Strategy().State)
	}
	r.busyOn.Store(false)
	r.busy.Unlock()
	rep := r.Report()
	if rep.Days != 0 || rep.Equity != 500 || len(rep.Curve) != 0 {
		t.Fatalf("%+v", rep)
	}
}
