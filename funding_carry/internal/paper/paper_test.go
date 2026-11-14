package paper

import (
	"testing"

	"fundingcarry/internal/market"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
	"fundingcarry/internal/testutil"
)

const D0 = int64(1_791_590_400_000) - int64(1_791_590_400_000)%market.DayMS
const D1 = D0 + market.DayMS

func plan(day int64, book []string, notional map[string]float64, prices map[string]strategy.Pair, stopped ...string) *strategy.Plan {
	score := map[string]float64{}
	for _, s := range book {
		score[s] = 0.0003
	}
	return &strategy.Plan{Day: day, Book: book, Notional: notional, Prices: prices, Score: score, Rehedge: []string{},
		PairStopped: stopped, Skipped: map[string]string{}}
}

func near(a, b float64) bool { return testutil.Close(a, b, 1e-9) }

func TestOpenMarkCloseCycle(t *testing.T) {
	st := state.Fresh(600)
	px := map[string]strategy.Pair{"AUSDT": {10, 10}}
	res := Apply(st, plan(D0, []string{"AUSDT"}, map[string]float64{"AUSDT": 100}, px), nil)
	if !near(st.Capital, 600-strategy.FeeSide*100) {
		t.Fatal(st.Capital)
	}
	h, _ := st.Held.Get("AUSDT")
	if h.Opened != D0 || !res.Events[0].Notify {
		t.Fatal(h, res.Events)
	}
	// next day: 3 settlements of 0.01 % on the short, spot +1 %, perp +1.5 % -> basis -0.5 % of N
	var rates []market.FundingRow
	for _, hh := range []int64{0, 8, 16} {
		rates = append(rates, market.FundingRow{Time: D0 + hh*3_600_000, Rate: 0.0001})
	}
	rates = append(rates, market.FundingRow{Time: D1, Rate: 0.0001})
	px2 := map[string]strategy.Pair{"AUSDT": {10.15, 10.1}}
	marks := MarkAll(st.Held, px2, map[string][]market.FundingRow{"AUSDT": rates}, D1)
	if !near(marks[0].Funding, 0.03) || !near(marks[0].Basis, 100*0.01-100*0.015) {
		t.Fatal(marks)
	}
	before := st.Capital
	Apply(st, plan(D1, []string{}, map[string]float64{}, px2), marks)
	if st.Held.Len() != 0 || !near(st.Capital, before+0.03-0.5-strategy.FeeSide*100) {
		t.Fatal(st.Capital)
	}
	c := st.Closed[len(st.Closed)-1]
	if c.Symbol != "AUSDT" || !near(c.Pnl, 0.03-0.5-2*strategy.FeeSide*100) {
		t.Fatal(c)
	}
	if st.Stats != (state.Stats{PairsOpened: 1, PairsClosed: 1, PairStops: 0}) {
		t.Fatal(st.Stats)
	}
}

func TestPairStopSetsCooldown(t *testing.T) {
	st := state.Fresh(600)
	Apply(st, plan(D0, []string{"AUSDT"}, map[string]float64{"AUSDT": 100}, map[string]strategy.Pair{"AUSDT": {10, 10}}), nil)
	px := map[string]strategy.Pair{"AUSDT": {10.3, 10}}
	marks := MarkAll(st.Held, px, nil, D1)
	Apply(st, plan(D1, []string{}, map[string]float64{}, px, "AUSDT"), marks)
	if st.Cool["AUSDT"] != D1+7*market.DayMS || st.Stats.PairStops != 1 {
		t.Fatal(st.Cool, st.Stats)
	}
}

func TestResizeChargesOnlyTheDifference(t *testing.T) {
	st := state.Fresh(600)
	px := map[string]strategy.Pair{"AUSDT": {10, 10}}
	Apply(st, plan(D0, []string{"AUSDT"}, map[string]float64{"AUSDT": 100}, px), nil)
	c := st.Capital
	marks := MarkAll(st.Held, px, nil, D1)
	Apply(st, plan(D1, []string{"AUSDT"}, map[string]float64{"AUSDT": 50}, px), marks)
	h, _ := st.Held.Get("AUSDT")
	if !near(st.Capital, c-strategy.FeeSide*50) || h.N != 50 || h.Perp0 != 10 {
		t.Fatal(st.Capital, h)
	}
}

type paperFixture struct {
	D0        int64 `json:"d0"`
	Scenarios []struct {
		Capital0 float64
		Days     []struct {
			Plan struct {
				Day         int64
				Book        []string
				Notional    map[string]float64
				Prices      map[string][2]float64
				Score       map[string]float64
				Rehedge     []string
				PairStopped []string `json:"pair_stopped"`
				Skipped     map[string]string
			}
			Rates   map[string][][2]float64
			Marks   []any
			Row     any
			Events  []any
			Held    any
			Capital float64
			Cool    any
			Stats   any
			ClosedN int `json:"closed_n"`
		}
		Final any
	}
}

func TestPaperScenariosMatchPython(t *testing.T) {
	var fx paperFixture
	testutil.Load(t, "paper", &fx)
	for si, sc := range fx.Scenarios {
		st := state.Fresh(sc.Capital0)
		for di, d := range sc.Days {
			p := d.Plan
			prices := map[string]strategy.Pair{}
			for s, v := range p.Prices {
				prices[s] = strategy.Pair{v[0], v[1]}
			}
			pl := &strategy.Plan{Day: p.Day, Book: nz(p.Book), Notional: p.Notional, Prices: prices, Score: p.Score,
				Rehedge: nz(p.Rehedge), PairStopped: nz(p.PairStopped), Skipped: p.Skipped}
			rates := map[string][]market.FundingRow{}
			for s, rr := range d.Rates {
				for _, r := range rr {
					rates[s] = append(rates[s], market.FundingRow{Time: fx.D0 + int64(r[0]), Rate: r[1]})
				}
			}
			marks := MarkAll(st.Held, prices, rates, p.Day)
			if df := testutil.Diff(any(d.Marks), testutil.Roundtrip(t, marks), 1e-9); df != "" {
				t.Fatalf("scenario %d day %d marks: %s", si, di, df)
			}
			res := Apply(st, pl, marks)
			var evs []any
			for _, e := range res.Events {
				evs = append(evs, []any{e.Level, e.Msg, e.Notify})
			}
			if evs == nil {
				evs = []any{}
			}
			if df := testutil.Diff(any(d.Events), any(evs), 1e-9); df != "" {
				t.Fatalf("scenario %d day %d events: %s", si, di, df)
			}
			if df := testutil.Diff(d.Row, testutil.Roundtrip(t, res.Row), 1e-9); df != "" {
				t.Fatalf("scenario %d day %d row: %s", si, di, df)
			}
			if df := testutil.Diff(d.Held, testutil.Roundtrip(t, st.Held), 1e-9); df != "" {
				t.Fatalf("scenario %d day %d held: %s", si, di, df)
			}
			if !near(st.Capital, d.Capital) || len(st.Closed) != d.ClosedN {
				t.Fatalf("scenario %d day %d: capital %v want %v", si, di, st.Capital, d.Capital)
			}
			if df := testutil.Diff(d.Cool, testutil.Roundtrip(t, st.Cool), 1e-9); df != "" {
				t.Fatalf("cool: %s", df)
			}
			if df := testutil.Diff(d.Stats, testutil.Roundtrip(t, st.Stats), 0); df != "" {
				t.Fatalf("stats: %s", df)
			}
		}
		if df := testutil.Diff(sc.Final, testutil.Roundtrip(t, st), 1e-9); df != "" {
			t.Fatalf("scenario %d final state: %s", si, df)
		}
	}
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
