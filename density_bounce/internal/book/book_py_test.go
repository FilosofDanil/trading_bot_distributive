package book

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

// TestMatchesPythonBook replays a 20-minute synthetic stream (snapshot, diff events with one gap and a resync,
// trades, density scans) that was run through the Python bounce/book.py, and compares every checkpoint:
// mid, best quotes, flows, turnover, ATR, mid / bar history, trend and the whole density table.
// Fixture: testdata/py_book.json.gz (generated from the Python code; quantities are float32-exact and trades
// print at whole seconds, so the compact Go types must give the same numbers).
func TestMatchesPythonBook(t *testing.T) {
	f, err := os.Open("testdata/py_book.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Tick   float64           `json:"tick"`
		Symbol string            `json:"symbol"`
		Log    []json.RawMessage `json:"log"`
		CumAt  int               `json:"py_cum_at"`
	}
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	b := New(fx.Symbol, fx.Tick)
	lastLoad := false
	checks := 0
	for i, raw := range fx.Log {
		var op struct {
			Op   string       `json:"op"`
			Rows [][4]float64 `json:"rows"`
			ID   int64        `json:"id"`
			Bids [][2]float64 `json:"bids"`
			Asks [][2]float64 `json:"asks"`
			U    int64        `json:"U"`
			LU   int64        `json:"u"`
			PU   int64        `json:"pu"`
			E    int64        `json:"E"`
			B    [][2]float64 `json:"b"`
			A    [][2]float64 `json:"a"`
			T    int64        `json:"T"`
			P    float64      `json:"p"`
			Q    float64      `json:"q"`
			M    bool         `json:"m"`
			Now  float64      `json:"now"`
			OK   bool         `json:"ok"`
			Dens []any        `json:"dens"`
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
		switch op.Op {
		case "seed_bars", "seed_h1":
			ks := make([]Kline, len(op.Rows))
			for j, r := range op.Rows {
				ks[j] = Kline{OpenMs: int64(r[0]), High: r[1], Low: r[2], Close: r[3]}
			}
			if op.Op == "seed_bars" {
				b.SeedBars(ks)
			} else {
				b.SeedH1(ks)
			}
		case "snapshot":
			lastLoad = b.LoadSnapshot(op.ID, b.Levels(op.Bids), b.Levels(op.Asks))
		case "loaded":
			if lastLoad != op.OK {
				t.Fatalf("log %d: load_snapshot = %v, python %v", i, lastLoad, op.OK)
			}
		case "depth":
			b.OnDepth(DepthEvent{FirstU: op.U, LastU: op.LU, PrevU: op.PU, E: op.E, Bids: b.Levels(op.B), Asks: b.Levels(op.A)})
		case "pending":
			b.Pending = append(b.Pending, DepthEvent{FirstU: op.U, LastU: op.LU, PrevU: op.PU, E: op.E, Bids: b.Levels(op.B), Asks: b.Levels(op.A)})
		case "trade":
			b.OnTrade(op.T, op.P, op.Q, op.M)
		case "scan":
			b.ScanDensities(op.Now)
		case "check":
			checks++
			compareCheck(t, i, b, raw)
		default:
			t.Fatalf("log %d: unknown op %q", i, op.Op)
		}
	}
	if checks < 50 {
		t.Fatalf("only %d checkpoints", checks)
	}
	t.Logf("%d calls, %d checkpoints equal; python kept %d cum_at prices, Go keeps %d tape prints and %d densities",
		len(fx.Log), checks, fx.CumAt, len(b.Tape()), len(b.Densities))
}

func compareCheck(t *testing.T, i int, b *Book, raw json.RawMessage) {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	now := c["now"].(float64)
	at := func(what string) string { return fmt.Sprintf("log %d (now %.1f) %s", i, now, what) }
	num := func(name string, got float64, ok bool, rel float64) {
		t.Helper()
		want, has := c[name].(float64)
		if !has {
			if ok {
				t.Fatalf("%s: got %v, python None", at(name), got)
			}
			return
		}
		if !ok || !close(got, want, rel) {
			t.Fatalf("%s: got %v (ok %v), python %v", at(name), got, ok, want)
		}
	}
	mid, ok := b.Mid()
	num("mid", mid, ok, 1e-12)
	bb, okB := b.BestBid()
	num("bb", float64(bb), okB, 0)
	ba, okA := b.BestAsk()
	num("ba", float64(ba), okA, 0)
	if c["synced"].(bool) != b.Synced {
		t.Fatalf("%s: synced %v", at("synced"), b.Synced)
	}
	num("flow60s", b.Flow(now, 60, true), true, 1e-12)
	num("flow60b", b.Flow(now, 60, false), true, 1e-12)
	num("flow180s", b.Flow(now, 180, true), true, 1e-12)
	num("flow180b", b.Flow(now, 180, false), true, 1e-12)
	num("rate", b.FlowRate(now, true, 60, 180), true, 1e-12)
	num("minute", b.MinuteTurnover(now), true, 1e-6) // float32 buckets
	num("turn180", b.Turnover(now, 180), true, 1e-6)
	atr, ok := b.ATR(now, 14)
	num("atr", atr, ok, 1e-9)
	m60, ok := b.MidAgo(now, 60)
	num("mid60", m60, ok, 1e-12)
	p600, ok := b.PriceAgo(now, 600)
	num("price600", p600, ok, 1e-9)
	b300, ok := b.BarAgo(now, 300)
	num("bar300", b300, ok, 1e-9)
	tr, ok := b.Trend(20)
	num("trend", float64(tr), ok, 0)
	tb, ok := b.TrendBefore(20, now-7200)
	num("trend_before", float64(tb), ok, 0)
	num("nbars", float64(len(b.Bars())), true, 0)
	num("nh1", float64(len(b.H1())), true, 0)

	want := c["dens"].([]any)
	got := make([]*Density, 0, len(b.Densities))
	for _, d := range b.Densities {
		got = append(got, d)
	}
	sort.Slice(got, func(x, y int) bool { // python sorted by the side's name: "ask" < "bid"
		if got[x].Side != got[y].Side {
			return got[x].Side.String() < got[y].Side.String()
		}
		return got[x].Tick < got[y].Tick
	})
	if len(got) != len(want) {
		t.Fatalf("%s: %d densities, python %d", at("dens"), len(got), len(want))
	}
	for j, w := range want {
		r := w.([]any)
		d := got[j]
		desc := fmt.Sprintf("density %s %d", d.Side, d.Tick)
		if r[0].(string) != d.Side.String() || int32(r[1].(float64)) != d.Tick {
			t.Fatalf("%s: %s, python %v %v", at("dens"), desc, r[0], r[1])
		}
		if r[2].(string) != d.Gone || int(r[3].(float64)) != d.Touches || r[4].(bool) != d.Near {
			t.Fatalf("%s: %s gone %q touches %d near %v, python %v", at("dens"), desc, d.Gone, d.Touches, d.Near, r)
		}
		for k, v := range map[int]float64{5: d.USD, 6: d.PeakUSD, 7: d.FirstSeen, 8: d.Drop(now, 10), 9: b.Eaten(d)} {
			if !close(v, r[k].(float64), 1e-9) && math.Abs(v-r[k].(float64)) > 1e-6 {
				t.Fatalf("%s: %s field %d = %v, python %v", at("dens"), desc, k, v, r[k])
			}
		}
	}
}

func close(a, b, rel float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= rel*math.Max(math.Abs(a), math.Abs(b))
}
