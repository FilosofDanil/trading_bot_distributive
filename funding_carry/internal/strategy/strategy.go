// Package strategy is the carry decision and the orders it implies, ported from Strategy_tester/lab/carry_live.py.
//
// Decide and Orders are kept line-for-line with the Python port (carry/strategy.py), which the lab's `check`
// proved equal to run_carry's book 92/92 days on run2; only the data access goes through the market package.
// The pandas parts (rolling median, NaN-skipping mean / max) are plain loops that reproduce pandas.
// Rules (safe carry, "buffer_calm"):
//
//	score        mean funding of the last 3 days, scaled to 8 h; open >= 0.02 %, keep while >= 0.01 %, top 8
//	filters      a NEW pair only while |perp / spot - 1| <= 2 % and no daily range > 35 % in the last 14 days
//	size         both legs N each, perp margin 2N (isolated, 1x): a coin ties up 3N; capital split over max(coins, 2);
//	             a held pair is resized only when its target differs by > 60 %; never more than the capital
//	margin       a held pair whose perp rose >= +100 % since entry / last re-hedge: re-hedge (alert)
//	pair stop    close a pair whose result since entry, from the basis change alone, is <= -2 % of the leg;
//	             no re-entry for 7 days
//	delisting    leave a pair a week before its perp delists (deliveryDate in exchangeInfo)
package strategy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/state"
)

// Params is the strategy parameter set P.
type Params struct {
	K          int // max coins (live: MAX_COINS)
	Kmin       int
	MinF8      float64
	ExitF8     float64
	Lookback   int
	Buffer     float64
	Calm       float64
	Band       float64
	Margin     float64
	DelistDays int
	PairStop   float64
}

// P is the live parameter set; main changes P.K for live mode.
var P = Params{K: 8, Kmin: 2, MinF8: 0.0002, ExitF8: 0.0001, Lookback: 3,
	Buffer: 2.0, Calm: 0.35, Band: 0.6, Margin: 0.5, DelistDays: 7, PairStop: 0.02}

const (
	SpotMinDays   = 7    // spot must have traded at least this long before a hedge is opened
	MaxEntryBasis = 0.02 // no new hedge while |perp / spot - 1| > 2 %
	DayMS         = market.DayMS
	delistNone    = int64(1) << 62
)

// FeeSide is one entry (or exit) of the hedged pair, per unit of N (lab/carry.py): 0.0010 + 0.0005 + 2 * 0.0002,
// computed at run time in float64 like Python does.
var FeeSide = func() float64 {
	a, b, c := 0.0010, 0.0005, 0.0002
	return a + b + 2*c
}()

// ------------------------------------------------------------------ funding

// F8Row is a funding settlement with its rate scaled to 8 h.
type F8Row struct {
	Time int64
	Rate float64
	F8   float64
}

// Funding8h is strategy.funding_8h: settlements in the window with f8 = rate * 8 / gap hours.
func Funding8h(sym string, start, end int64) ([]F8Row, error) {
	endDay := end - end%DayMS + DayMS // day-aligned window (live windows end every hour)
	rows, err := market.Funding(sym, start-10*DayMS, endDay)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var f []market.FundingRow
	for _, r := range rows {
		if r.Time >= start-10*DayMS && r.Time < end {
			f = append(f, r)
		}
	}
	sort.SliceStable(f, func(i, j int) bool { return f[i].Time < f[j].Time })
	return ComputeF8(f), nil
}

// ComputeF8 adds f8 to settlements sorted by time:
//
//	gap = fundingTime.diff().rolling(3, min_periods=1).median().bfill() / 3_600_000, clip(1, 8), fillna(8)
//	f8 = rate * 8 / gap
func ComputeF8(f []market.FundingRow) []F8Row {
	n := len(f)
	out := make([]F8Row, n)
	if n == 0 {
		return out
	}
	gaps := make([]float64, n)
	gaps[0] = math.NaN()
	for i := 1; i < n; i++ {
		gaps[i] = float64(f[i].Time - f[i-1].Time)
	}
	med := make([]float64, n)
	for i := 0; i < n; i++ {
		var w []float64
		for j := i - 2; j <= i; j++ {
			if j >= 0 && !math.IsNaN(gaps[j]) {
				w = append(w, gaps[j])
			}
		}
		if len(w) < 1 {
			med[i] = math.NaN()
			continue
		}
		sort.Float64s(w)
		if len(w)%2 == 1 {
			med[i] = w[len(w)/2]
		} else {
			med[i] = (w[len(w)/2-1] + w[len(w)/2]) / 2
		}
	}
	for i := n - 2; i >= 0; i-- { // bfill
		if math.IsNaN(med[i]) {
			med[i] = med[i+1]
		}
	}
	for i := 0; i < n; i++ {
		g := med[i] / 3_600_000
		switch {
		case math.IsNaN(g):
			g = 8
		case g < 1:
			g = 1
		case g > 8:
			g = 8
		}
		out[i] = F8Row{Time: f[i].Time, Rate: f[i].Rate, F8: f[i].Rate * 8 / g}
	}
	return out
}

// MarketData is what decide reads per symbol (tests pass it in, otherwise it is fetched).
type MarketData struct {
	PerpOpen  float64
	SpotOpen  float64
	SpotFirst int64
	Range14   float64
	F8        []F8Row
}

func perpDaily(sym string, day int64, days int64) ([]market.Bar, error) {
	return market.FetchKlines(sym, "1d", day-days*DayMS, day+DayMS)
}

func fetchMarket(s string, day int64) (*MarketData, string, error) {
	f, err := Funding8h(s, day-int64(P.Lookback+1)*DayMS, day)
	if err != nil {
		return nil, "", err
	}
	pk, err := perpDaily(s, day, 15)
	if err != nil {
		return nil, "", err
	}
	sp, err := market.SpotDailyOpen(s, day-(SpotMinDays+1)*DayMS, day+DayMS)
	if err != nil {
		return nil, "", err
	}
	var today, yday *market.Bar
	for i := range pk {
		switch pk[i].OpenTime {
		case day:
			if today == nil {
				today = &pk[i]
			}
		case day - DayMS:
			if yday == nil {
				yday = &pk[i]
			}
		}
	}
	_, haveSpot := sp[day]
	if len(pk) == 0 || (today == nil && yday == nil) || !haveSpot {
		return nil, "no price for today", nil
	}
	todayOpen := 0.0
	if today != nil {
		todayOpen = today.Open
	} else {
		todayOpen = yday.Close
	}
	first := int64(math.MaxInt64)
	for k := range sp {
		if k < first {
			first = k
		}
	}
	r14, have := math.NaN(), false
	for _, b := range pk {
		if b.OpenTime < day && b.OpenTime >= day-14*DayMS {
			v := b.High/b.Low - 1
			if math.IsNaN(v) {
				continue
			}
			if !have || v > r14 {
				r14, have = v, true
			}
		}
	}
	anyPast := false
	for _, b := range pk {
		if b.OpenTime < day && b.OpenTime >= day-14*DayMS {
			anyPast = true
			break
		}
	}
	if !anyPast {
		r14 = 0.0
	}
	return &MarketData{PerpOpen: todayOpen, SpotOpen: sp[day], SpotFirst: first, Range14: r14, F8: f}, "", nil
}

// ------------------------------------------------------------------ the plan

// Pair is a (perp, spot) price pair, written as [perp, spot].
type Pair [2]float64

// Top is one entry of the funding top list, written as [symbol, score].
type Top struct {
	Symbol string
	Score  float64
}

func (t Top) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{t.Symbol, t.Score})
}

func (t *Top) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 2 {
		return fmt.Errorf("top entry: want [symbol, score]")
	}
	if err := json.Unmarshal(raw[0], &t.Symbol); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &t.Score)
}

// ResizeItem is a held pair whose notional changes from Old to New.
type ResizeItem struct {
	Symbol   string
	Old, New float64
}

// ResizeList keeps the book order; it is written as the JSON object {"SYM": [old, new]}.
type ResizeList []ResizeItem

func (r ResizeList) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, it := range r {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(it.Symbol)
		v, err := json.Marshal([2]float64{it.Old, it.New})
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (r *ResizeList) UnmarshalJSON(b []byte) error {
	*r = ResizeList{}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var v [2]float64
		if err := dec.Decode(&v); err != nil {
			return err
		}
		*r = append(*r, ResizeItem{kt.(string), v[0], v[1]})
	}
	return nil
}

// Plan is what decide returns (the Python dict).
type Plan struct {
	Day         int64              `json:"day"`
	Equity      float64            `json:"equity"`
	Book        []string           `json:"book"`
	Notional    map[string]float64 `json:"notional"`
	Score       map[string]float64 `json:"score"`
	Open        []string           `json:"open"`
	Close       []string           `json:"close"`
	Resize      ResizeList         `json:"resize"`
	Rehedge     []string           `json:"rehedge"`
	PairStopped []string           `json:"pair_stopped"`
	Skipped     map[string]string  `json:"skipped"`
	Top         []Top              `json:"top"`
	Prices      map[string]Pair    `json:"prices"`
	HeldN       map[string]float64 `json:"held_n,omitempty"` // set by the runner for orders()
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type scored struct {
	score float64
	sym   string
}

// Decide is strategy.decide. held is the held pairs in their order; marketData (tests) may be nil, which fetches.
func Decide(day int64, held state.HeldMap, equity float64, cands []string, marketData map[string]*MarketData,
	cool map[string]int64) (*Plan, error) {
	var scores []scored
	info := map[string]*MarketData{}
	skipped := map[string]string{}

	symSet := map[string]bool{}
	for _, s := range cands {
		symSet[s] = true
	}
	for _, s := range held.Keys() {
		symSet[s] = true
	}
	syms := make([]string, 0, len(symSet))
	for s := range symSet {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	for _, s := range syms {
		m := marketData[s]
		if m == nil {
			var why string
			var err error
			m, why, err = fetchMarket(s, day)
			if err != nil {
				return nil, err
			}
			if m == nil {
				skipped[s] = why
				continue
			}
		}
		info[s] = m
		var w []float64
		for _, r := range m.F8 {
			if r.Time < day && r.Time >= day-int64(P.Lookback)*DayMS {
				w = append(w, r.F8)
			}
		}
		if len(w) >= 2 {
			scores = append(scores, scored{mean(w), s})
		}
	}
	sort.SliceStable(scores, func(i, j int) bool { // sort(reverse=True) on (score, symbol)
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return scores[i].sym > scores[j].sym
	})
	scoreOf := map[string]float64{}
	for _, sc := range scores {
		scoreOf[sc.sym] = sc.score
	}
	var keep, newSyms []string
	for _, sc := range scores {
		if held.Has(sc.sym) && sc.score >= P.ExitF8 {
			keep = append(keep, sc.sym)
		}
	}
	for _, sc := range scores {
		if !held.Has(sc.sym) && sc.score >= P.MinF8 && cool[sc.sym] <= day {
			newSyms = append(newSyms, sc.sym)
		}
	}
	perps, err := market.Perpetuals()
	if err != nil {
		return nil, err
	}
	delist := map[string]int64{}
	for _, x := range perps {
		delist[x.Symbol] = x.Delivery() // set when announced
	}
	var book []string
	cand := append(append([]string{}, keep...), newSyms...)
	if len(cand) > P.K {
		cand = cand[:P.K]
	}
	for _, s := range cand {
		m := info[s]
		if marketData == nil {
			dl, ok := delist[s]
			if !ok {
				dl = delistNone
			}
			if day+int64(P.DelistDays)*DayMS >= dl {
				skipped[s] = "perp delisting announced (deliveryDate within 7 days)"
				continue
			}
		}
		if m.SpotFirst > day-SpotMinDays*DayMS {
			skipped[s] = "spot younger than 7 days"
			continue
		}
		if !held.Has(s) && math.Abs(m.PerpOpen/m.SpotOpen-1) > MaxEntryBasis {
			skipped[s] = fmt.Sprintf("basis %+.2f%% > 2 %%", (m.PerpOpen/m.SpotOpen-1)*100)
			continue
		}
		if !held.Has(s) && m.Range14 > P.Calm {
			skipped[s] = fmt.Sprintf("daily range %s in 14 d > 35 %%", pyx.Pct0(m.Range14, false))
			continue
		}
		book = append(book, s)
	}
	tie := 1 + P.Buffer
	ns := map[string]float64{}
	sumNs := func(syms []string) float64 {
		v := make([]float64, 0, len(syms))
		for _, s := range syms {
			v = append(v, ns[s])
		}
		return pyx.PySum(v)
	}
	if len(book) > 0 {
		total := equity * float64(len(book)) / float64(maxInt(len(book), P.Kmin))
		for _, s := range book {
			ns[s] = total / float64(len(book)) / tie
		}
		for _, s := range book {
			if h, ok := held.Get(s); ok && math.Abs(ns[s]-h.N) <= P.Band*h.N {
				ns[s] = h.N
			}
		}
		excess := tie*sumNs(book) - equity
		if excess > 1e-9 {
			var fresh, heldBook []string
			for _, s := range book {
				if held.Has(s) {
					heldBook = append(heldBook, s)
				} else {
					fresh = append(fresh, s)
				}
			}
			room := tie * sumNs(fresh)
			cut := excess // min(excess, room)
			if room < cut {
				cut = room
			}
			for _, s := range fresh {
				if room > 0 {
					ns[s] *= 1 - cut/room
				} else {
					ns[s] *= 1
				}
			}
			excess -= cut
			if excess > 1e-9 {
				old := tie * sumNs(heldBook)
				for _, s := range heldBook {
					ns[s] *= 1 - excess/old
				}
			}
		}
	}
	var rehedge []string
	for _, s := range book {
		if h, ok := held.Get(s); ok && info[s].PerpOpen/h.Ref-1 >= P.Margin*P.Buffer {
			rehedge = append(rehedge, s)
		}
	}
	var stopped []string
	for _, s := range append([]string{}, book...) {
		h, ok := held.Get(s)
		if ok && h.Perp0 != 0 && h.Spot0 != 0 {
			pair := (info[s].SpotOpen/h.Spot0 - 1) - (info[s].PerpOpen/h.Perp0 - 1)
			if pair <= -P.PairStop {
				stopped = append(stopped, s)
				book = removeStr(book, s)
				delete(ns, s)
			}
		}
	}
	inBook := map[string]bool{}
	for _, s := range book {
		inBook[s] = true
	}
	plan := &Plan{Day: day, Equity: equity, Book: nz(book), Notional: ns, Score: map[string]float64{},
		Open: []string{}, Close: []string{}, Resize: ResizeList{}, Rehedge: nz(rehedge), PairStopped: nz(stopped),
		Skipped: skipped, Top: []Top{}, Prices: map[string]Pair{}}
	for _, s := range book {
		plan.Score[s] = scoreOf[s]
		if !held.Has(s) {
			plan.Open = append(plan.Open, s)
		} else if h, _ := held.Get(s); math.Abs(ns[s]-h.N) > 1e-9 {
			plan.Resize = append(plan.Resize, ResizeItem{s, h.N, ns[s]})
		}
	}
	for _, s := range held.Keys() {
		if !inBook[s] {
			plan.Close = append(plan.Close, s)
		}
	}
	for i, sc := range scores {
		if i >= 5 {
			break
		}
		plan.Top = append(plan.Top, Top{sc.sym, sc.score})
	}
	for s, m := range info {
		if inBook[s] || held.Has(s) {
			plan.Prices[s] = Pair{m.PerpOpen, m.SpotOpen}
		}
	}
	return plan, nil
}

// mean is pandas Series.mean(): numpy pairwise sum / count of the non-NaN values (NaN when none).
func mean(w []float64) float64 {
	vals := make([]float64, 0, len(w))
	for _, v := range w {
		if !math.IsNaN(v) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return math.NaN()
	}
	return pyx.NPSum(vals) / float64(len(vals))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func removeStr(s []string, x string) []string {
	for i, v := range s {
		if v == x {
			return append(s[:i:i], s[i+1:]...)
		}
	}
	return s
}

// ------------------------------------------------------------------ orders it WOULD place

// Filters returns the perp and spot symbol infos by symbol.
func Filters() (map[string]market.Symbol, map[string]market.Symbol, error) {
	perps, err := market.Perpetuals()
	if err != nil {
		return nil, nil, err
	}
	spotInfo, err := market.SpotInfo()
	if err != nil {
		return nil, nil, err
	}
	fut := make(map[string]market.Symbol, len(perps))
	for _, s := range perps {
		fut[s.Symbol] = s
	}
	spot := make(map[string]market.Symbol, len(spotInfo))
	for _, s := range spotInfo {
		spot[s.Symbol] = s
	}
	return fut, spot, nil
}

// StepMin is _step_min: lot step and minimum notional of a symbol.
func StepMin(info market.Symbol, spot bool) (step, mn float64) {
	mn = 5.0
	for _, f := range info.Filters {
		if f.Type == "LOT_SIZE" {
			if v, ok := f.StepSize.Float(); ok {
				step = v
			}
		}
		if spot && (f.Type == "NOTIONAL" || f.Type == "MIN_NOTIONAL") {
			if v, ok := f.MinNotional.Float(); ok {
				mn = v
			}
		}
		if !spot && f.Type == "MIN_NOTIONAL" {
			if v, ok := f.Notional.Float(); ok {
				mn = v
			}
		}
	}
	return step, mn
}

func qty(notional, price, step float64) float64 {
	q := notional / price
	if step > 0 {
		return math.Floor(q/step) * step
	}
	return q
}

// Leg is an order leg, written as [side, qty].
type Leg struct {
	Side string
	Qty  float64
}

func (l Leg) MarshalJSON() ([]byte, error) { return json.Marshal([]any{l.Side, l.Qty}) }

func (l *Leg) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 2 {
		return fmt.Errorf("order leg: want [side, qty]")
	}
	if err := json.Unmarshal(raw[0], &l.Side); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &l.Qty)
}

// Order is one order pair the plan implies.
type Order struct {
	Symbol          string     `json:"symbol"`
	Why             string     `json:"why"`
	Spot            Leg        `json:"spot"`
	Perp            Leg        `json:"perp"`
	Notional        float64    `json:"notional"`
	MarginToFutures float64    `json:"margin_to_futures"`
	OK              bool       `json:"ok"`
	Min             [2]float64 `json:"min"` // spot, perp minimum notional
}

// Orders is strategy.orders; fut / spot may be nil, then the exchange info is read.
func Orders(plan *Plan, fut, spot map[string]market.Symbol) ([]Order, error) {
	if fut == nil && spot == nil {
		var err error
		if fut, spot, err = Filters(); err != nil {
			return nil, err
		}
	}
	out := []Order{}
	pair := func(s string, n float64, side, why string) {
		pr := plan.Prices[s]
		pp, sp := pr[0], pr[1]
		fs, fm := StepMin(fut[s], false)
		ss, sm := StepMin(spot[s], true)
		q := qty(n, sp, ss) // equal quantity on both legs
		if q2 := qty(n, pp, fs); q2 < q {
			q = q2
		}
		ok := q*sp >= sm && q*pp >= fm
		o := Order{Symbol: s, Why: why, Notional: q * sp, OK: ok, Min: [2]float64{sm, fm}}
		if side == "open" {
			o.Spot, o.Perp = Leg{"BUY", q}, Leg{"SELL", q}
			o.MarginToFutures = P.Buffer * q * pp
		} else {
			o.Spot, o.Perp = Leg{"SELL", q}, Leg{"BUY", q}
		}
		out = append(out, o)
	}
	for _, s := range plan.Open {
		pair(s, plan.Notional[s], "open", fmt.Sprintf("funding %.3f %%/8h", plan.Score[s]*100))
	}
	for _, s := range plan.Close {
		if _, ok := plan.Prices[s]; ok {
			pair(s, plan.HeldN[s], "close", "funding below 0.01 %/8h or out of the book")
		}
	}
	for _, r := range plan.Resize {
		side := "close"
		if r.New > r.Old {
			side = "open"
		}
		pair(r.Symbol, math.Abs(r.New-r.Old), side, fmt.Sprintf("resize %.2f -> %.2f", r.Old, r.New))
	}
	return out, nil
}
