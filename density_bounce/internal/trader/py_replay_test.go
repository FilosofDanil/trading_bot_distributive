package trader_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	"densitybounce/internal/sim"
	"densitybounce/internal/state"
	. "densitybounce/internal/testkit"
	"densitybounce/internal/trader"
)

func pxf(t int) float64 {
	x, _ := strconv.ParseFloat(fmt.Sprintf("%.4f", float64(t)*0.0001), 64)
	return x
}

// cmp compares a decoded Go record with the Python one: strings, bools and null exactly, numbers within 1e-9
// (relative or absolute). The "ver" keys are skipped (the Python run was v0.4.5, this is 0.5.0).
func cmp(t *testing.T, path string, got, want any, bad *int) {
	t.Helper()
	if *bad > 20 {
		return
	}
	fail := func(f string, a ...any) {
		*bad++
		t.Errorf(path+": "+f, a...)
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			fail("go %v, python object", got)
			return
		}
		for k, wv := range w {
			if k == "ver" {
				continue
			}
			gv, has := g[k]
			if !has {
				fail("key %q missing in go", k)
				continue
			}
			cmp(t, path+"."+k, gv, wv, bad)
		}
		for k := range g {
			if _, has := w[k]; !has && k != "ver" {
				fail("extra key %q in go", k)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			fail("go %v, python list of %d", got, len(w))
			return
		}
		for i := range w {
			cmp(t, fmt.Sprintf("%s[%d]", path, i), g[i], w[i], bad)
		}
	case float64:
		g, ok := got.(float64)
		if !ok || math.Abs(g-w) > 1e-9*math.Max(1, math.Abs(w)) {
			fail("go %v, python %v", got, w)
		}
	default:
		if got != want {
			fail("go %v, python %v", got, want)
		}
	}
}

type rEvent struct {
	kind  int
	t     int64 // seconds from T0
	si    int
	a, b  []float64 // depth: [tick, qty, ...] pairs
	tick  int
	qty   float64
	bm    bool
	snapU int64
}

// TestReplayMatchesPython drives a 2 h, 6 symbol synthetic stream through Trader + SimBroker (lag 0) with the
// replay clock, a tick every 0.5 s and the queued work run after every step, and compares the closed trades, the
// plan journal, the counters and the event texts with what the Python code produced on the same stream
// (testdata/py_replay.json.gz, bounce_gen/gen_replay.py).
func TestReplayMatchesPython(t *testing.T) {
	f, err := os.Open("testdata/py_replay.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Tick     float64  `json:"tick"`
		T0       int64    `json:"t0"`
		Dur      int      `json:"dur"`
		Syms     []string `json:"syms"`
		Params   string   `json:"params"`
		Notional float64  `json:"notional"`
		Seeds    map[string]struct {
			K1 [][3]int `json:"k1"`
			KH [][2]int `json:"kh"`
		} `json:"seeds"`
		Stream [][]json.RawMessage `json:"stream"`
		Expect struct {
			Trades    []any              `json:"trades"`
			Plans     []any              `json:"plans"`
			Stats     map[string]float64 `json:"stats"`
			Events    [][3]any           `json:"events"`
			OpenSlots []string           `json:"open_slots"`
			Shadows   int                `json:"shadows"`
			Cool      map[string]float64 `json:"cool"`
		} `json:"expect"`
	}
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	SaveParams(t)
	PinRisk(t) // restores RISK; the values below are the Python run's
	setup.P = setup.Default()
	if err := setup.LoadParams(fx.Params); err != nil {
		t.Fatal(err)
	}
	trader.RISK = trader.DefaultRisk()
	trader.RISK.Notional = fx.Notional

	st, err := state.Open(filepath.Join(t.TempDir(), "replay.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	st.Out = io.Discard
	br := sim.New(0)
	sc := &Sched{}
	info := map[string]trader.SymInfo{}
	for _, s := range fx.Syms {
		info[s] = trader.SymInfo{Tick: fx.Tick, Step: 1, MinQty: 1, MinNotional: 5}
	}
	clock := float64(fx.T0)
	tr := trader.New(br, st, info, "closes", sc)
	tr.Logf = nil
	tr.Now = func() float64 { return clock }
	books := make([]*book.Book, len(fx.Syms))
	for i, s := range fx.Syms {
		b := book.New(s, fx.Tick)
		var k1, kh []book.Kline
		for _, r := range fx.Seeds[s].K1 {
			k1 = append(k1, book.Kline{OpenMs: int64(r[0]), High: pxf(r[1]), Low: pxf(r[2]), Close: pxf(r[1])})
		}
		for _, r := range fx.Seeds[s].KH {
			kh = append(kh, book.Kline{OpenMs: int64(r[0]), High: pxf(r[1]), Low: pxf(r[1]), Close: pxf(r[1])})
		}
		b.SeedBars(k1)
		b.SeedH1(kh)
		books[i] = b
		tr.AddBook(s, b)
	}

	// decode the stream
	var evs []rEvent
	num := func(raw json.RawMessage) float64 {
		var x float64
		_ = json.Unmarshal(raw, &x)
		return x
	}
	for _, row := range fx.Stream {
		switch int(num(row[0])) {
		case 0:
			si := int(num(row[1]))
			var bids, asks [][2]float64
			_ = json.Unmarshal(row[3], &bids)
			_ = json.Unmarshal(row[4], &asks)
			lv := func(rows [][2]float64) []book.Level {
				var out []book.Level
				for _, r := range rows {
					out = append(out, book.Level{Tick: int32(r[0]), Qty: float32(r[1])})
				}
				return out
			}
			if !books[si].LoadSnapshot(int64(num(row[2])), lv(bids), lv(asks)) {
				t.Fatal("snapshot")
			}
			books[si].LastTs = float64(fx.T0)
		case 1:
			var b, a []float64
			_ = json.Unmarshal(row[3], &b)
			_ = json.Unmarshal(row[4], &a)
			evs = append(evs, rEvent{kind: 1, t: int64(num(row[1])), si: int(num(row[2])), a: b, b: a})
		default:
			evs = append(evs, rEvent{kind: 2, t: int64(num(row[1])), si: int(num(row[2])), tick: int(num(row[3])),
				qty: num(row[4]), bm: num(row[5]) == 1})
		}
	}
	u := make([]int64, len(fx.Syms))
	for i := range u {
		u[i] = 1000
	}
	apply := func(e rEvent) {
		ms := (fx.T0 + e.t) * 1000
		clock = float64(ms) / 1000
		b := books[e.si]
		if e.kind == 1 {
			lv := func(x []float64) []book.Level {
				var out []book.Level
				for i := 0; i+1 < len(x); i += 2 {
					out = append(out, book.Level{Tick: int32(x[i]), Qty: float32(x[i+1])})
				}
				return out
			}
			u[e.si]++
			if !b.OnDepth(book.DepthEvent{FirstU: u[e.si], LastU: u[e.si], PrevU: u[e.si] - 1, E: ms, Bids: lv(e.a), Asks: lv(e.b)}) {
				t.Fatalf("depth gap on %s", fx.Syms[e.si])
			}
		} else {
			tk := b.OnTrade(ms, pxf(e.tick), e.qty, e.bm)
			br.OnTrade(fx.Syms[e.si], tk)
		}
		sc.Settle()
	}
	i := 0
	steps := 0
	for k := 1; float64(k)*0.5 <= float64(fx.Dur); k++ {
		now := float64(fx.T0) + 0.5*float64(k)
		for i < len(evs) && float64((fx.T0+evs[i].t)*1000) <= now*1000 {
			apply(evs[i])
			i++
		}
		clock = now
		tr.Tick(now)
		sc.Settle()
		steps++
	}

	bad := 0
	toAny := func(rs []json.RawMessage) []any {
		out := make([]any, len(rs))
		for i, r := range rs {
			_ = json.Unmarshal(r, &out[i])
		}
		return out
	}
	gotTrades := toAny(st.Trades(1000))
	gotPlans := toAny(st.Plans(1000))
	cmp(t, "trades", gotTrades, fx.Expect.Trades, &bad)
	cmp(t, "plans", gotPlans, fx.Expect.Plans, &bad)
	gotStats := map[string]any{}
	wantStats := map[string]any{}
	for k, v := range st.Stats() {
		gotStats[k] = float64(v)
	}
	for k, v := range fx.Expect.Stats {
		wantStats[k] = v
	}
	cmp(t, "stats", gotStats, wantStats, &bad)
	evl := st.Events()
	if len(evl) != len(fx.Expect.Events) {
		t.Errorf("%d events, python %d", len(evl), len(fx.Expect.Events))
	} else {
		for j, e := range evl {
			w := fx.Expect.Events[j]
			if e.Level != w[0].(string) || e.Msg != w[1].(string) || e.Notify != w[2].(bool) {
				t.Errorf("event %d: go [%s] %q %v, python [%s] %q %v", j, e.Level, e.Msg, e.Notify, w[0], w[1], w[2])
				break
			}
		}
	}
	var open []string
	for _, s := range tr.Slots() {
		open = append(open, s.Sym)
	}
	sort.Strings(open)
	want := append([]string(nil), fx.Expect.OpenSlots...)
	sort.Strings(want)
	if fmt.Sprint(open) != fmt.Sprint(want) || len(tr.Shadows()) != fx.Expect.Shadows {
		t.Errorf("open slots %v shadows %d, python %v %d", open, len(tr.Shadows()), want, fx.Expect.Shadows)
	}
	for sym, until := range fx.Expect.Cool {
		if math.Abs(tr.Cool[sym]-until) > 1e-6 {
			t.Errorf("cool[%s] = %v, python %v", sym, tr.Cool[sym], until)
		}
	}
	if len(tr.Cool) != len(fx.Expect.Cool) {
		t.Errorf("%d cooldowns, python %d", len(tr.Cool), len(fx.Expect.Cool))
	}
	// the stream must exercise the paths it is meant to (it is the Python run's numbers)
	acts := map[string]int{}
	hits := map[string]int{}
	for _, p := range gotPlans {
		m := p.(map[string]any)
		acts[m["act"].(string)]++
		hits[m["shadow"].(map[string]any)["hit"].(string)]++
	}
	if len(gotPlans) < 20 || st.Stat("filled") < 5 || len(gotTrades) < 3 || st.Stat("cancelled") < 1 || len(hits) < 3 {
		t.Errorf("replay too weak: %d plans, %d fills, %d trades, %d cancels, shadow outcomes %v", len(gotPlans), st.Stat("filled"), len(gotTrades), st.Stat("cancelled"), hits)
	}
	t.Logf("%d steps, %d events; %d plan records (acts %v, shadow outcomes %v), %d fills, %d closed trades, %d cancels: all equal to Python",
		steps, len(evs), len(gotPlans), acts, hits, st.Stat("filled"), len(gotTrades), st.Stat("cancelled"))
}
