package universe

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"

	"fundingcarry/internal/market"
	"fundingcarry/internal/testutil"
)

const hourMS = int64(3_600_000)

type screenFixture struct {
	T0      int64 `json:"t0"`
	LiquidN int   `json:"liquid_n"`
	Days    []int64
	Syms    []struct {
		S  string
		Qb float64
		H0 *int64
		C0 int64
		D  []int64
		Q  []int64
		H  []int64
		C  []int64
		O  []int64
	}
	Exp map[string]struct {
		Play   map[string][]string
		Liquid map[string][]string
	}
}

func (f *screenFixture) bars() map[string][]market.Bar {
	out := map[string][]market.Bar{}
	for _, s := range f.Syms {
		var hours, closes, opens []int64
		if s.H0 != nil {
			c := s.C0
			hours, closes, opens = []int64{*s.H0}, []int64{c}, []int64{c}
			for i, d := range s.D {
				opens = append(opens, c)
				c += d
				closes = append(closes, c)
				hours = append(hours, *s.H0+int64(i)+1)
			}
		} else {
			hours, closes, opens = s.H, s.C, s.O
		}
		var bars []market.Bar
		for i, h := range hours {
			o, c := float64(opens[i])/100.0, float64(closes[i])/100.0
			bars = append(bars, market.Bar{OpenTime: f.T0 + h*hourMS, Open: o, Close: c, High: math.Max(o, c), Low: math.Min(o, c),
				QuoteVol: float64(s.Q[i]) * s.Qb, CloseTime: f.T0 + h*hourMS + hourMS - 1})
		}
		out[s.S] = bars
	}
	return out
}

func setLiquid(t *testing.T, n int) {
	old := LiquidN
	LiquidN = n
	t.Cleanup(func() { LiquidN = old })
}

func TestScreensMatchPython(t *testing.T) {
	var fx screenFixture
	testutil.Load(t, "screens", &fx)
	setLiquid(t, fx.LiquidN)
	bars := fx.bars()
	var names []string
	for _, s := range fx.Syms {
		names = append(names, s.S)
	}
	idx := map[string]int{}
	for i, n := range names {
		idx[n] = i
	}
	oi := func(sym string, day int64) float64 { // the stub of the generator
		d := int((day - fx.T0) / market.DayMS)
		i := idx[sym]
		if (i+d)%5 == 0 {
			return math.NaN()
		}
		return float64((i*7+d*13)%11-3) / 10.0
	}
	var todo []int64
	for _, d := range fx.Days {
		todo = append(todo, fx.T0+d*market.DayMS)
	}
	res, err := Compute(names, todo, func(s string) ([]market.Bar, error) { return bars[s], nil }, oi, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range fx.Days {
		exp := fx.Exp[strconv.FormatInt(d, 10)]
		if df := testutil.Diff(testutil.Roundtrip(t, exp.Play), testutil.Roundtrip(t, res[i].Play), 0); df != "" {
			t.Fatalf("day %d in-play: %s", d, df)
		}
		if df := testutil.Diff(testutil.Roundtrip(t, exp.Liquid), testutil.Roundtrip(t, res[i].Liquid), 0); df != "" {
			t.Fatalf("day %d liquid: %s", d, df)
		}
	}
	if _, err := Compute([]string{"AAAUSDT"}, todo, func(string) ([]market.Bar, error) { return nil, nil }, nil, 1); err == nil {
		t.Fatal("no BTC bars must be an error")
	}
}

func TestMissingDaysAndCandidates(t *testing.T) {
	today := 40 * market.DayMS
	tags := map[string][]string{}
	for _, d := range []int64{10, 11, 25, 40} {
		tags[strconv.FormatInt(d*market.DayMS, 10)] = []string{"AUSDT"}
	}
	m := MissingDays(tags, today)
	if len(m) != 31-4 {
		t.Fatal(len(m))
	}
	tags[strconv.FormatInt(5*market.DayMS, 10)] = []string{"OLDUSDT"}
	tags[strconv.FormatInt(40*market.DayMS, 10)] = []string{"BUSDT", "NOSPOT"}
	got := Candidates(tags, today, map[string]bool{"AUSDT": true, "BUSDT": true, "OLDUSDT": true})
	if len(got) != 2 || got[0] != "AUSDT" || got[1] != "BUSDT" { // OLD is outside the 30 days, NOSPOT has no spot pair
		t.Fatal(got)
	}
}

// Update through a fake Binance: the streaming build (exchangeInfo stream, paged klines, features per symbol).
func TestUpdateStreamsTheBuild(t *testing.T) {
	var fx screenFixture
	testutil.Load(t, "screens", &fx)
	setLiquid(t, fx.LiquidN)
	bars := fx.bars()
	shift := 60 * market.DayMS // the bars must be closed: move the whole panel into the past
	for _, bs := range bars {
		for i := range bs {
			bs[i].OpenTime -= shift
			bs[i].CloseTime -= shift
		}
	}
	fx.T0 -= shift
	oldF, oldSleep := market.FAPI, market.Sleep
	market.Sleep = func(time.Duration) {}
	t.Cleanup(func() { market.FAPI, market.Sleep = oldF, oldSleep; market.ResetCache() })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			var syms []any
			for _, s := range fx.Syms {
				syms = append(syms, map[string]any{"symbol": s.S, "status": "TRADING", "contractType": "PERPETUAL", "quoteAsset": "USDT"})
			}
			json.NewEncoder(w).Encode(map[string]any{"symbols": syms})
		case "/fapi/v1/klines":
			lo, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			hi, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
			lim, _ := strconv.Atoi(q.Get("limit"))
			out := []any{}
			for _, b := range bars[q.Get("symbol")] {
				if b.OpenTime >= lo && b.OpenTime <= hi && len(out) < lim {
					f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
					out = append(out, []any{b.OpenTime, f(b.Open), f(b.High), f(b.Low), f(b.Close), "1", b.CloseTime, f(b.QuoteVol), 1, "1", "1", "0"})
				}
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	market.FAPI = ts.URL
	market.ResetCache()
	today := fx.T0 + 40*market.DayMS
	out, err := Update(map[string][]string{"1": {"X"}}, today, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 31 {
		t.Fatal(len(out))
	}
	checked := 0
	for _, d := range fx.Days {
		if d < 10 {
			continue
		}
		exp := fx.Exp[strconv.FormatInt(d, 10)]
		set := map[string]bool{}
		for s := range exp.Play {
			set[s] = true
		}
		for s := range exp.Liquid {
			set[s] = true
		}
		var want []string
		for s := range set {
			want = append(want, s)
		}
		sort.Strings(want)
		got := out[strconv.FormatInt(fx.T0+d*market.DayMS, 10)]
		if len(got) != len(want) {
			t.Fatalf("day %d: %v want %v", d, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("day %d: %v want %v", d, got, want)
			}
		}
		checked++
	}
	if checked < 7 {
		t.Fatal(checked)
	}
}
