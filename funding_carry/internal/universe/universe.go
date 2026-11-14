// Package universe builds the daily candidate tags, ported from Strategy_tester/lab/universe.py (screens unchanged).
//
// Carry candidates are the coins tagged on any of the last 30 days by either universe the lab's paper loop
// builds: "coins in play" (gainers / losers / decorrelated / volume_in) and the liquid top-40. The runner keeps
// those daily tags in its state and computes only the days it is missing (all 31 at the first start).
//
// Memory: the Python build held the hourly panel of every perp at once. Here BTCUSDT is fetched first, then
// every symbol (4 workers); each symbol's bars are reduced to the few screen features each missing day needs
// (eligibility, 24 h volume, 24 h change, volume ratio, correlation with BTC) and dropped. Only the features
// are kept until the days are ranked, which is done exactly as screen() / screen_liquid() rank.
package universe

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"

	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
)

const (
	PerScreen    = 5
	MinQuoteVol  = 20_000_000 // SOURCES.md: boards filter on >= 20M USD 24h volume
	LookbackDays = 8

	CausalDays = 30
	DayMS      = market.DayMS
)

// LiquidN is the size of the liquid universe (a variable so that a test can make the cut bite on a small panel).
var LiquidN = 40

// Exclude are never candidates.
var Exclude = map[string]bool{"BTCUSDT": true, "USDCUSDT": true, "FDUSDUSDT": true, "BTCDOMUSDT": true}

// BarSource returns the hourly bars of a symbol (an error or no bars leaves the symbol out of the panel).
type BarSource func(sym string) ([]market.Bar, error)

// OIFunc is the open-interest growth of a symbol over the 24 h before day (NaN when unknown).
type OIFunc func(sym string, day int64) float64

// btcSeries is BTC's log return series: ret[j] = log(close[j]) - log(close[j-1]), NaN for the first bar.
type btcSeries struct {
	times []int64
	ret   []float64
}

func newBTC(bars []market.Bar) *btcSeries {
	b := &btcSeries{times: make([]int64, len(bars)), ret: make([]float64, len(bars))}
	for j, x := range bars {
		b.times[j] = x.OpenTime
		if j == 0 {
			b.ret[j] = math.NaN()
		} else {
			b.ret[j] = math.Log(x.Close) - math.Log(bars[j-1].Close)
		}
	}
	return b
}

// window is btc.iloc[bars before day] -> diff().iloc[-72:].
func (b *btcSeries) window(day int64) ([]int64, []float64) {
	k := sort.Search(len(b.times), func(i int) bool { return b.times[i] >= day })
	lo := k - 72
	if lo < 0 {
		lo = 0
	}
	return b.times[lo:k], b.ret[lo:k]
}

// Feat is what the screens read of one symbol on one day.
type Feat struct {
	Liq  bool    // eligible for the liquid screen (>= 30 days of bars)
	QV24 float64 // quote volume of the last 24 bars
	Play bool    // eligible for the in-play screen
	Chg  float64 // 24 h change
	VR   float64 // 24 h volume / mean daily volume of the 7 days before (NaN when that is 0)
	Corr float64 // Pearson correlation of hourly log returns with BTC over its last 72 (NaN when unknown)
}

// corrcoef is np.corrcoef(x, y)[0, 1].
func corrcoef(x, y []float64) float64 {
	n := len(x)
	mx, my := pyx.NPSum(x)/float64(n), pyx.NPSum(y)/float64(n)
	var c00, c01, c11 float64
	for i := 0; i < n; i++ {
		a, b := x[i]-mx, y[i]-my
		c00 += a * a
		c01 += a * b
		c11 += b * b
	}
	inv := 1 / float64(n-1)
	c00, c01, c11 = c00*inv, c01*inv, c11*inv
	sd0, sd1 := math.Sqrt(c00), math.Sqrt(c11)
	c := c01 / sd0 / sd1
	switch {
	case c > 1:
		return 1
	case c < -1:
		return -1
	}
	return c
}

// symbolFeats reduces one symbol's bars (sorted by open time) to the features of every day.
func symbolFeats(bars []market.Bar, days []int64, btc *btcSeries) []Feat {
	n := len(bars)
	ret := make([]float64, n)
	for j := 0; j < n; j++ {
		if j == 0 {
			ret[j] = math.NaN()
		} else {
			ret[j] = math.Log(bars[j].Close) - math.Log(bars[j-1].Close)
		}
	}
	out := make([]Feat, len(days))
	buf := make([]float64, 0, 24*LookbackDays)
	sumQV := func(lo, hi int) float64 {
		buf = buf[:0]
		for i := lo; i < hi; i++ {
			buf = append(buf, bars[i].QuoteVol)
		}
		return pyx.NPSum(buf)
	}
	for di, day := range days {
		k := sort.Search(n, func(i int) bool { return bars[i].OpenTime >= day })
		f := &out[di]
		if k >= 24*30 {
			f.Liq = true
			f.QV24 = sumQV(k-24, k)
		}
		if k < 24*7+24 {
			continue // too young: needs a full week of history
		}
		qv24 := sumQV(k-24, k)
		if qv24 < MinQuoteVol {
			continue
		}
		avgDay := sumQV(k-24*8, k-24) / 7
		f.Play = true
		f.QV24 = qv24
		f.Chg = bars[k-1].Close/bars[k-24].Open - 1
		if avgDay > 0 {
			f.VR = qv24 / avgDay
		} else {
			f.VR = math.NaN()
		}
		// inner join of the symbol's returns with BTC's last 72, NaN rows dropped
		bt, br := btc.window(day)
		var xs, ys []float64
		for i, t := range bt {
			j := sort.Search(k, func(m int) bool { return bars[m].OpenTime >= t })
			if j >= k || bars[j].OpenTime != t {
				continue
			}
			if math.IsNaN(ret[j]) || math.IsNaN(br[i]) {
				continue
			}
			xs, ys = append(xs, ret[j]), append(ys, br[i])
		}
		if len(xs) >= 48 {
			f.Corr = corrcoef(xs, ys)
		} else {
			f.Corr = math.NaN()
		}
	}
	return out
}

// topN is DataFrame.nlargest / nsmallest(keep="first"): a stable sort, NaN rows left out.
func topN(idx []int, n int, key func(int) float64, largest bool) []int {
	var v []int
	for _, i := range idx {
		if !math.IsNaN(key(i)) {
			v = append(v, i)
		}
	}
	sort.SliceStable(v, func(a, b int) bool {
		if largest {
			return key(v[a]) > key(v[b])
		}
		return key(v[a]) < key(v[b])
	})
	if len(v) > n {
		v = v[:n]
	}
	return v
}

// DayTags are the screens' tags of one day (symbol -> screen names).
type DayTags struct {
	Play   map[string][]string
	Liquid map[string][]string
}

// rank is screen_liquid + screen for one day, from the features of every symbol in panel order.
func rank(syms []string, feats [][]Feat, di int, day int64, oi OIFunc) DayTags {
	res := DayTags{Play: map[string][]string{}, Liquid: map[string][]string{}}
	var liq, play []int
	for si := range syms {
		if feats[si] == nil {
			continue
		}
		f := feats[si][di]
		if f.Liq {
			liq = append(liq, si)
		}
		if f.Play {
			play = append(play, si)
		}
	}
	for _, si := range topN(liq, LiquidN, func(i int) float64 { return feats[i][di].QV24 }, true) {
		res.Liquid[syms[si]] = []string{"liquid"}
	}
	if len(play) == 0 {
		return res
	}
	chg := func(i int) float64 { return feats[i][di].Chg }
	qv := func(i int) float64 { return feats[i][di].QV24 }
	corr := func(i int) float64 { return feats[i][di].Corr }
	picks := [][2]any{}
	picks = append(picks, [2]any{"gainers", topN(play, PerScreen, chg, true)})
	picks = append(picks, [2]any{"losers", topN(play, PerScreen, chg, false)})
	liquid := topN(play, 100, qv, true)
	picks = append(picks, [2]any{"decorrelated", topN(liquid, PerScreen, corr, false)})
	var flat []int
	for _, si := range play {
		f := feats[si][di]
		if math.Abs(f.Chg) <= 0.03 && f.VR >= 2.0 {
			flat = append(flat, si)
		}
	}
	score := map[int]float64{}
	for _, si := range flat {
		vr := feats[si][di].VR
		if oi != nil {
			g := oi(syms[si], day)
			if math.IsNaN(g) {
				g = 0
			}
			if g < 0 {
				g = 0
			}
			score[si] = vr * (1 + g*10)
		} else {
			score[si] = vr
		}
	}
	picks = append(picks, [2]any{"volume_in", topN(flat, PerScreen, func(i int) float64 { return score[i] }, true)})
	for _, p := range picks {
		name := p[0].(string)
		for _, si := range p[1].([]int) {
			res.Play[syms[si]] = append(res.Play[syms[si]], name)
		}
	}
	return res
}

// Compute builds the screen tags of the days in todo (sorted) over syms. The bars of a symbol live only while
// its features are computed. BTCUSDT is fetched first (the screens need it).
func Compute(syms []string, todo []int64, fetch BarSource, oi OIFunc, workers int) ([]DayTags, error) {
	btcIdx := -1
	for i, s := range syms {
		if s == "BTCUSDT" {
			btcIdx = i
		}
	}
	var btcBars []market.Bar
	if btcIdx >= 0 {
		if b, err := fetch("BTCUSDT"); err == nil {
			btcBars = b
		}
	}
	if len(btcBars) == 0 {
		return nil, &market.RuntimeError{Msg: "universe: no BTCUSDT hourly bars"}
	}
	btc := newBTC(btcBars)
	btcBars = nil

	feats := make([][]Feat, len(syms))
	var wg sync.WaitGroup
	jobs := make(chan int)
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				bars, err := fetch(syms[i])
				if err != nil || len(bars) == 0 {
					continue // not in the panel
				}
				feats[i] = symbolFeats(bars, todo, btc)
			}
		}()
	}
	for i, s := range syms {
		if Exclude[s] {
			continue // never a candidate; BTC was fetched above
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	out := make([]DayTags, len(todo))
	for di, day := range todo {
		out[di] = rank(syms, feats, di, day, oi)
	}
	return out, nil
}

// OIGrowth returns the open-interest growth function (cached per symbol, errors count as unknown).
func OIGrowth(start, end int64) OIFunc {
	var mu sync.Mutex
	cache := map[string][]market.OIRow{}
	failed := map[string]bool{}
	return func(sym string, day int64) float64 {
		mu.Lock()
		rows, ok := cache[sym]
		if !ok && !failed[sym] {
			r, err := market.OpenInterest(sym, start, end, "1h")
			if err != nil {
				failed[sym] = true
			} else {
				cache[sym], rows = r, r
			}
		}
		mu.Unlock()
		if len(rows) == 0 {
			return math.NaN()
		}
		var d []market.OIRow
		for _, r := range rows {
			if r.TS < day {
				d = append(d, r)
			}
		}
		if len(d) < 25 {
			return math.NaN()
		}
		return d[len(d)-1].Val/d[len(d)-25].Val - 1
	}
}

// MissingDays are the days of the causal window [today - 30 d, today] that have no tags yet.
func MissingDays(tags map[string][]string, today int64) []int64 {
	var out []int64
	for d := today - CausalDays*DayMS; d <= today; d += DayMS {
		if _, ok := tags[strconv.FormatInt(d, 10)]; !ok {
			out = append(out, d)
		}
	}
	return out
}

// Update fills the missing days of the window (both universes, union of tags) and drops older days.
func Update(tags map[string][]string, today int64, log func(string)) (map[string][]string, error) {
	todo := MissingDays(tags, today)
	out := map[string][]string{}
	for k, v := range tags {
		if n, err := strconv.ParseInt(k, 10, 64); err == nil && n >= today-CausalDays*DayMS {
			out[k] = v
		}
	}
	if len(todo) == 0 {
		return out, nil
	}
	perps, err := market.Perpetuals()
	if err != nil {
		return nil, err
	}
	syms := make([]string, len(perps))
	for i, s := range perps {
		syms[i] = s.Symbol
	}
	if log != nil {
		log(fmt.Sprintf("universe: %d day(s) to build over %d perps", len(todo), len(syms)))
	}
	// one hourly fetch per symbol covers both screens: the liquid screen needs 30 days before its day, in-play 8
	start, end := todo[0]-31*DayMS, todo[len(todo)-1]
	fetch := func(sym string) ([]market.Bar, error) { return market.FetchKlines(sym, "1h", start, end) }
	res, err := Compute(syms, todo, fetch, OIGrowth(todo[0]-2*DayMS, todo[len(todo)-1]), 4)
	if err != nil {
		return nil, err
	}
	for i, d := range todo {
		set := map[string]bool{}
		for s := range res[i].Play {
			set[s] = true
		}
		for s := range res[i].Liquid {
			set[s] = true
		}
		list := make([]string, 0, len(set))
		for s := range set {
			list = append(list, s)
		}
		sort.Strings(list)
		out[strconv.FormatInt(d, 10)] = list
	}
	return out, nil
}

// Candidates are the coins tagged in the last 30 days up to `day` that have a USDT spot pair (= lab candidates_today).
func Candidates(tags map[string][]string, day int64, spot map[string]bool) []string {
	set := map[string]bool{}
	for k, v := range tags {
		n, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		if day-CausalDays*DayMS <= n && n <= day {
			for _, s := range v {
				set[s] = true
			}
		}
	}
	out := []string{}
	for s := range set {
		if spot[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
