package setup_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	. "densitybounce/internal/testkit"
)

type pyPlan struct {
	Side     string         `json:"side"`
	Entry    float64        `json:"entry"`
	Stop     float64        `json:"stop"`
	TP       float64        `json:"tp"`
	TP1      float64        `json:"tp1"`
	TPRun    float64        `json:"tp_run"`
	DKey     [2]any         `json:"dkey"`
	Skip     string         `json:"skip"`
	Features map[string]any `json:"features"`
}

type pyCase struct {
	Now    float64      `json:"now"`
	Params string       `json:"params"`
	Mkt    *float64     `json:"mkt"`
	Skip   [][2]any     `json:"skip"`
	K1     [][3]int     `json:"k1"`
	KH     [][2]int     `json:"kh"`
	Mids   [][2]int     `json:"mids"`
	M2     int          `json:"m2"`
	Trades [][4]int     `json:"trades"`
	Bids   [][2]float64 `json:"bids"`
	Asks   [][2]float64 `json:"asks"`
	Dens   []struct {
		Side      string       `json:"side"`
		Tick      int32        `json:"tick"`
		FirstSeen float64      `json:"first_seen"`
		Peak      float64      `json:"peak"`
		USD       float64      `json:"usd"`
		LastSeen  float64      `json:"last_seen"`
		Touches   int          `json:"touches"`
		Gone      string       `json:"gone"`
		Hist      [][2]float64 `json:"hist"`
	} `json:"dens"`
	Ch24   *float64 `json:"ch24"`
	Expect struct {
		Plan  *pyPlan `json:"plan"`
		Ghost *pyPlan `json:"ghost"`
	} `json:"expect"`
}

func pxf(t int) float64 {
	x, _ := strconv.ParseFloat(fmt.Sprintf("%.4f", float64(t)*0.0001), 64)
	return x
}

func sideOf(s string) book.Side {
	if s == "bid" {
		return book.Bid
	}
	return book.Ask
}

func buildBook(c *pyCase, tick float64) *book.Book {
	b := book.New("XUSDT", tick)
	var k1, kh []book.Kline
	for _, r := range c.K1 {
		k1 = append(k1, book.Kline{OpenMs: int64(r[0]) * 60_000, High: pxf(r[1]), Low: pxf(r[2]), Close: pxf(r[1])})
	}
	for _, r := range c.KH {
		kh = append(kh, book.Kline{OpenMs: int64(r[0]) * 3_600_000, High: pxf(r[1]), Low: pxf(r[1]), Close: pxf(r[1])})
	}
	b.SeedBars(k1)
	b.SeedH1(kh)
	for _, m := range c.Mids {
		b.PushMid(c.Now-float64(m[0])/4, float64(c.M2+m[1])/2*tick)
	}
	for _, tr := range c.Trades {
		b.OnTrade(int64(tr[0])*1000, pxf(tr[1]), float64(tr[2]), tr[3] == 1)
	}
	lv := func(rows [][2]float64) map[int32]float64 {
		m := map[int32]float64{}
		for _, r := range rows {
			m[int32(r[0])] = r[1]
		}
		return m
	}
	b.SetSide(book.Bid, lv(c.Bids))
	b.SetSide(book.Ask, lv(c.Asks))
	for _, d := range c.Dens {
		o := book.NewDensity(sideOf(d.Side), d.Tick, b.P(d.Tick), d.FirstSeen, d.Peak, d.USD, d.LastSeen)
		o.Touches, o.Gone = d.Touches, d.Gone
		for _, h := range d.Hist {
			o.AddHist(h[0], h[1])
		}
		b.Densities[o.Key()] = o
	}
	b.Ch24 = c.Ch24
	return b
}

// looseKeys are the features that come from float32 tape quantities: they may differ by one unit of the rounding.
var looseKeys = map[string]float64{"minute_usd": 1, "flow_s": 1, "dens_vs_minute": 0.01, "vol_ratio": 0.01, "hold_s": 0.1, "stop_atr": 0.01, "atr": 0.00001}

func comparePlan(t *testing.T, name string, got *setup.Plan, want *pyPlan, loose *int) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Errorf("%s: go %v, python %v", name, got != nil, want != nil)
		return
	}
	if got == nil {
		return
	}
	if got.Side != want.Side || got.Skip != want.Skip || got.DKey.Side.String() != want.DKey[0].(string) ||
		float64(got.DKey.Tick) != want.DKey[1].(float64) {
		t.Errorf("%s: side/skip/dkey go %v %q %v, python %v %q %v", name, got.Side, got.Skip, got.DKey, want.Side, want.Skip, want.DKey)
	}
	for _, c := range []struct {
		n    string
		g, w float64
	}{{"entry", got.Entry, want.Entry}, {"stop", got.Stop, want.Stop}, {"tp", got.TP, want.TP}, {"tp1", got.TP1, want.TP1}, {"tp_run", got.TPRun, want.TPRun}} {
		if math.Abs(c.g-c.w) > 1e-12 {
			t.Errorf("%s: %s go %v python %v", name, c.n, c.g, c.w)
		}
	}
	raw, err := json.Marshal(got.Features)
	if err != nil {
		t.Fatal(err)
	}
	var gf map[string]any
	_ = json.Unmarshal(raw, &gf)
	if len(gf) != len(want.Features) {
		t.Errorf("%s: %d features, python %d", name, len(gf), len(want.Features))
	}
	for k, w := range want.Features {
		g, ok := gf[k]
		if !ok {
			t.Errorf("%s: feature %s missing", name, k)
			continue
		}
		if w == nil || g == nil {
			if w != g {
				t.Errorf("%s: feature %s go %v python %v", name, k, g, w)
			}
			continue
		}
		gv, gok := g.(float64)
		wv, wok := w.(float64)
		if !gok || !wok {
			if g != w {
				t.Errorf("%s: feature %s go %v python %v", name, k, g, w)
			}
			continue
		}
		if gv == wv || (k == "move_start" && math.Abs(gv-wv) <= 1e-12) { // the unrounded bar price: 1 ulp (tick * size vs parsed)
			continue
		}
		if unit, ok := looseKeys[k]; ok && math.Abs(gv-wv) <= unit*1.0001+1e-6*math.Abs(wv) {
			*loose++
			continue
		}
		t.Errorf("%s: feature %s go %v python %v", name, k, gv, wv)
	}
}

// TestScanMatchesPython runs setup.Scan on 300 random synthetic books (paths, density side / size / age, turnover,
// ATR bars, hour closes, tape, skip sets, market change, per-case PARAMS) and compares the plan, the ghost and
// every feature with what the Python setup.scan returned (testdata/py_scan.json.gz, bounce_gen/gen_scan.py).
func TestScanMatchesPython(t *testing.T) {
	f, err := os.Open("testdata/py_scan.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Tick  float64  `json:"tick"`
		Cases []pyCase `json:"cases"`
	}
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	SaveParams(t)
	plans, ghosts, nones, loose := 0, 0, 0, 0
	for i := range fx.Cases {
		c := &fx.Cases[i]
		setup.P = setup.Default()
		book.DENS = book.Dens{Band: 0.03, BaseBand: 0.02, Ratio: 8.0, USDMin: 25_000.0, MinuteK: 0.5, GoneFrac: 0.30}
		if err := setup.LoadParams(c.Params); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		b := buildBook(c, fx.Tick)
		skip := map[book.DKey]bool{}
		for _, s := range c.Skip {
			skip[book.DKey{Side: sideOf(s[0].(string)), Tick: int32(s[1].(float64))}] = true
		}
		plan, ghost := setup.Scan(b, c.Now, skip, c.Mkt)
		comparePlan(t, "case "+strconv.Itoa(i)+" plan", plan, c.Expect.Plan, &loose)
		comparePlan(t, "case "+strconv.Itoa(i)+" ghost", ghost, c.Expect.Ghost, &loose)
		switch {
		case plan != nil:
			plans++
		case ghost != nil:
			ghosts++
		default:
			nones++
		}
		if t.Failed() && i > 5 {
			break
		}
	}
	if len(fx.Cases) < 300 || plans < 100 || ghosts < 50 {
		t.Fatalf("fixture too weak: %d cases, %d plans, %d ghosts", len(fx.Cases), plans, ghosts)
	}
	t.Logf("%d cases equal: %d plans, %d ghosts only, %d empty; %d float32-tape features within one rounding unit",
		len(fx.Cases), plans, ghosts, nones, loose)
}
