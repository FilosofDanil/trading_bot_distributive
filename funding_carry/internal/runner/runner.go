// Package runner is the daily cycle: universe -> decision -> paper fills -> marks, once per UTC day after 00:05
// (carry/runner.py).
package runner

import (
	"encoding/json"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fundingcarry/internal/carry"
	"fundingcarry/internal/live"
	"fundingcarry/internal/market"
	"fundingcarry/internal/paper"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
	"fundingcarry/internal/universe"
)

const RetryAfterS = 15 * 60

// Busy is returned when a cycle is already running.
type Busy struct{}

func (Busy) Error() string  { return "busy" }
func (Busy) PyName() string { return "Busy" }

// ValueError is a refused request (HTTP 409).
type ValueError struct{ Msg string }

func (e *ValueError) Error() string  { return e.Msg }
func (e *ValueError) PyName() string { return "ValueError" }

// Runner holds the store, the mode and the (injectable) steps of the cycle.
type Runner struct {
	Store      *state.Store
	RunAfterMS int64
	Mode       string // paper | live | live-blocked
	Live       *live.Live
	Started    time.Time

	// the steps of the cycle; tests replace them (the Python tests monkeypatch the same functions)
	UpdateUniverse func(tags map[string][]string, day int64, log func(string)) (map[string][]string, error)
	Candidates     func(tags map[string][]string, day int64, spot map[string]bool) []string
	SpotSymbols    func() (map[string]bool, error)
	Decide         func(day int64, held state.HeldMap, equity float64, cands []string, cool map[string]int64) (*strategy.Plan, error)
	Orders         func(plan *strategy.Plan) ([]strategy.Order, error)

	busy    sync.Mutex
	busyOn  atomic.Bool
	mu      sync.Mutex // guards nextTry, watchErr
	nextTry float64
	watchE  string
	watchAt float64
}

// New is Runner(store, run_after_min, mode, live).
func New(store *state.Store, runAfterMin int, mode string, lv *live.Live) *Runner {
	return &Runner{
		Store: store, RunAfterMS: int64(runAfterMin) * 60_000, Mode: mode, Live: lv, Started: time.Now(),
		UpdateUniverse: universe.Update,
		Candidates:     universe.Candidates,
		SpotSymbols:    func() (map[string]bool, error) { return market.SpotSymbols() },
		Decide: func(day int64, held state.HeldMap, equity float64, cands []string, cool map[string]int64) (*strategy.Plan, error) {
			return strategy.Decide(day, held, equity, cands, nil, cool)
		},
		Orders: func(plan *strategy.Plan) ([]strategy.Order, error) { return strategy.Orders(plan, nil, nil) },
	}
}

// Busy reports whether a cycle is running.
func (r *Runner) IsBusy() bool { return r.busyOn.Load() }

func nowS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func today(now int64) int64 { return now - now%market.DayMS }

// ------------------------------------------------------------------ API view

// StrategyView is one element of GET /v1/strategies.
type StrategyView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Mode          string   `json:"mode"`
	State         string   `json:"state"`
	Desired       string   `json:"desired"`
	OpenPositions int      `json:"open_positions"`
	Book          []string `json:"book"`
	Capital0      float64  `json:"capital0"`
	Equity        float64  `json:"equity"`
	PnlTotal      float64  `json:"pnl_total"`
	PnlToday      float64  `json:"pnl_today"`
	LastRunDay    *string  `json:"last_run_day"`
	LastRunAt     *string  `json:"last_run_at"`
	NextRunAt     string   `json:"next_run_at"`
	LastError     *string  `json:"last_error"`
}

func fin(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

func eqOf(a *string) string {
	if a == nil {
		return ""
	}
	return *a
}

// Strategy is the strategy view.
func (r *Runner) Strategy() StrategyView {
	st := r.Store.Snapshot()
	now := carry.NowMS()
	td := today(now)
	var stateName string
	switch {
	case r.IsBusy():
		stateName = "busy"
	case st.LastError != nil && *st.LastError != "":
		stateName = "error"
	case st.Desired == "paused":
		stateName = "paused"
	default:
		stateName = "running"
	}
	pnlToday := 0.0
	if n := len(st.Daily); n > 0 {
		if last := st.Daily[n-1]; last.Day == carry.Day(td) {
			pnlToday = last.Funding + last.Basis + last.Fees
		}
	}
	equity := r.equity(st)
	due := td + r.RunAfterMS + market.DayMS
	if eqOf(st.LastRunDay) != carry.Day(td) || st.LastRunDay == nil {
		due = td + r.RunAfterMS
	}
	if due < now {
		due = now
	}
	book := st.Held.Keys()
	sort.Strings(book)
	return StrategyView{ID: carry.StrategyID, Name: carry.StrategyName, Mode: r.Mode, State: stateName, Desired: st.Desired,
		OpenPositions: st.Held.Len(), Book: book, Capital0: st.Capital0, Equity: fin(equity), PnlTotal: fin(equity - st.Capital0),
		PnlToday: fin(pnlToday), LastRunDay: st.LastRunDay, LastRunAt: st.LastRunAt, NextRunAt: carry.ISO(due),
		LastError: st.LastError}
}

// equity: paper books every mark into capital; live books a pair only when it closes, so add the open PnL.
func (r *Runner) equity(st *state.Data) float64 {
	if r.Mode == "paper" {
		return st.Capital
	}
	var v []float64
	for _, s := range st.Held.Keys() {
		h, _ := st.Held.Get(s)
		v = append(v, h.Funding+h.Basis-h.Fees)
	}
	return st.Capital + pyx.PySum(v)
}

// SetDesired persists paused / running and reports a change.
func (r *Runner) SetDesired(desired string, by any, reason string) StrategyView {
	var before string
	_ = r.Store.Update(func(d *state.Data) {
		before = d.Desired
		d.Desired = desired
	})
	if before != desired {
		who := ""
		if pyx.Truthy(by) {
			who = " by " + pyx.Str(by)
		}
		what := "resumed"
		if desired == "paused" {
			what = "paused: no new pairs, held pairs are still managed"
		}
		msg := fmt.Sprintf("%s %s%s", carry.StrategyID, what, who)
		if reason != "" {
			msg += " (" + reason + ")"
		}
		r.Store.Event("info", msg, true)
	}
	return r.Strategy()
}

// Position is one element of GET /positions.
type Position struct {
	Symbol  string  `json:"symbol"`
	N       float64 `json:"n"`
	Perp0   float64 `json:"perp0"`
	Spot0   float64 `json:"spot0"`
	Opened  string  `json:"opened"`
	Days    int64   `json:"days"`
	Funding float64 `json:"funding"`
	Basis   float64 `json:"basis"`
	Fees    float64 `json:"fees"`
	Pnl     float64 `json:"pnl"`
}

func (r *Runner) Positions() []Position {
	st := r.Store.Snapshot()
	now := carry.NowMS()
	keys := st.Held.Keys()
	sort.Strings(keys)
	out := []Position{}
	for _, s := range keys {
		h, _ := st.Held.Get(s)
		opened := h.Opened
		if opened == 0 {
			opened = now
		}
		out = append(out, Position{Symbol: s, N: h.N, Perp0: h.Perp0, Spot0: h.Spot0, Opened: carry.Day(opened),
			Days: floorDiv(now-opened, market.DayMS), Funding: h.Funding, Basis: h.Basis, Fees: -h.Fees,
			Pnl: h.Funding + h.Basis - h.Fees})
	}
	return out
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// CurvePoint is ["2026-10-10", 500.0].
type CurvePoint struct {
	Day    string
	Equity float64
}

func (c CurvePoint) MarshalJSON() ([]byte, error) { return json.Marshal([]any{c.Day, c.Equity}) }

// Report is GET /report.
type Report struct {
	Start       *string      `json:"start"`
	Days        int          `json:"days"`
	Capital0    float64      `json:"capital0"`
	Equity      float64      `json:"equity"`
	ReturnPct   float64      `json:"return_pct"`
	AnnualPct   float64      `json:"annual_pct"`
	MaxDDPct    float64      `json:"max_dd_pct"`
	Funding     float64      `json:"funding"`
	Basis       float64      `json:"basis"`
	Fees        float64      `json:"fees"`
	PairsOpened int          `json:"pairs_opened"`
	PairsClosed int          `json:"pairs_closed"`
	PairStops   int          `json:"pair_stops"`
	ActiveDays  int          `json:"active_days"`
	Curve       []CurvePoint `json:"curve"`
}

func (r *Runner) Report() Report {
	st := r.Store.Snapshot()
	daily := st.Daily
	peak, dd := st.Capital0, 0.0
	eq := []float64{st.Capital0}
	for _, d := range daily {
		eq = append(eq, d.Equity)
	}
	for _, e := range eq {
		if e > peak {
			peak = e
		}
		v := 0.0
		if peak > 0 {
			v = 1 - e/peak
		}
		if v > dd {
			dd = v
		}
	}
	days := len(daily)
	equity := r.equity(st)
	ret := 0.0
	if st.Capital0 != 0 {
		ret = equity/st.Capital0 - 1
	}
	annual := 0.0
	if days > 0 && ret > -1 {
		annual = math.Pow(1+ret, 365/float64(days)) - 1
	}
	var f, b, fe []float64
	active := 0
	for _, d := range daily {
		f, b, fe = append(f, d.Funding), append(b, d.Basis), append(fe, d.Fees)
		if len(d.Book) > 0 {
			active++
		}
	}
	curve := []CurvePoint{}
	tail := daily
	if len(tail) > 60 {
		tail = tail[len(tail)-60:]
	}
	for _, d := range tail {
		curve = append(curve, CurvePoint{d.Day, fin(d.Equity)})
	}
	return Report{Start: st.StartDay, Days: days, Capital0: st.Capital0, Equity: fin(equity), ReturnPct: fin(ret * 100),
		AnnualPct: fin(annual * 100), MaxDDPct: fin(dd * 100), Funding: pyx.PySum(f), Basis: pyx.PySum(b), Fees: pyx.PySum(fe),
		PairsOpened: st.Stats.PairsOpened, PairsClosed: st.Stats.PairsClosed, PairStops: st.Stats.PairStops,
		ActiveDays: active, Curve: curve}
}

// ------------------------------------------------------------------ the cycle

// TriggerResult is the answer of POST /run.
type TriggerResult struct {
	OK      bool `json:"ok"`
	Started bool `json:"started"`
	Preview bool `json:"preview"`
}

// Trigger starts today's cycle now in the background; a preview if today was already accepted.
func (r *Runner) Trigger() (TriggerResult, error) {
	if r.IsBusy() {
		return TriggerResult{}, Busy{}
	}
	day := today(carry.NowMS())
	preview := eqOf(r.Store.Snapshot().LastRunDay) == carry.Day(day)
	go r.SafeRun(day, preview)
	return TriggerResult{OK: true, Started: true, Preview: preview}, nil
}

// FlattenResult is the answer of POST /flatten.
type FlattenResult struct {
	OK      bool `json:"ok"`
	Started bool `json:"started"`
}

// Flatten is the kill switch: pause, then close every live pair in the background.
func (r *Runner) Flatten(by any) (FlattenResult, error) {
	if r.Live == nil {
		return FlattenResult{}, &ValueError{fmt.Sprintf("flatten needs live mode (mode is %s)", r.Mode)}
	}
	r.SetDesired("paused", by, "close all")
	go func() {
		n, err := r.Live.Flatten(by)
		if err != nil {
			fmt.Printf("close all failed: %s\n", pyx.Describe(err))
			r.Store.Event("error", pyx.Cut("close all failed: "+pyx.Describe(err), 500), true)
			return
		}
		r.Store.Event("warn", fmt.Sprintf("%s: close all done, %d pair(s) closed; the strategy stays paused", carry.StrategyID, n), true)
	}()
	return FlattenResult{OK: true, Started: true}, nil
}

// Watch is the live liquidation guard loop (Live.WatchOnce); errors are reported, not fatal.
func (r *Runner) Watch(stop <-chan struct{}, every time.Duration) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		err := r.watchOnce()
		if err == nil {
			r.mu.Lock()
			r.watchE, r.watchAt = "", 0
			r.mu.Unlock()
		} else { // Binance down, bad response...: keep watching
			msg := pyx.Cut(pyx.Describe(err), 300)
			r.mu.Lock()
			last, at := r.watchE, r.watchAt
			r.mu.Unlock()
			if msg != last || nowS()-at > 600 { // do not repeat the same alert every 30 s
				r.Store.Event("error", "watchdog: "+msg, true)
				r.mu.Lock()
				r.watchE, r.watchAt = msg, nowS()
				r.mu.Unlock()
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}

func (r *Runner) watchOnce() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			fmt.Printf("%s\n", debug.Stack())
		}
	}()
	return r.Live.WatchOnce()
}

// SafeRun runs a cycle and keeps the loop alive on failure; control alerts on last_error.
func (r *Runner) SafeRun(day int64, preview bool) {
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
				fmt.Printf("%s\n", debug.Stack())
			}
		}()
		_, err = r.RunDay(day, preview)
		return
	}()
	if err == nil {
		return
	}
	if _, ok := err.(Busy); ok {
		return
	}
	fmt.Printf("run failed: %s\n", pyx.Describe(err))
	desc := pyx.Describe(err)
	last := pyx.Cut(desc, 500)
	_ = r.Store.Update(func(d *state.Data) { d.LastError = &last })
	r.Store.Event("error", pyx.Cut(fmt.Sprintf("run %s failed: %s", carry.Day(day), desc), 500), true)
	r.mu.Lock()
	r.nextTry = nowS() + RetryAfterS
	r.mu.Unlock()
}

// Summary is the plan as GET /plan serves it.
type Summary struct {
	Day         string              `json:"day"`
	Preview     bool                `json:"preview"`
	Accepted    bool                `json:"accepted"`
	Paused      bool                `json:"paused"`
	Equity      float64             `json:"equity"`
	Book        []string            `json:"book"`
	Open        []string            `json:"open"`
	Close       []string            `json:"close"`
	Resize      strategy.ResizeList `json:"resize"`
	PairStopped []string            `json:"pair_stopped"`
	Rehedge     []string            `json:"rehedge"`
	Top         []strategy.Top      `json:"top"`
	WouldOpen   []string            `json:"would_open"`
	MinF8       float64             `json:"min_f8"`
	Skipped     map[string]string   `json:"skipped"`
	Candidates  int                 `json:"candidates"`
	Orders      []strategy.Order    `json:"orders"`
	Idle        float64             `json:"idle"`
	ComputedAt  string              `json:"computed_at"`
}

// RunDay runs one cycle. Busy when another cycle is running.
func (r *Runner) RunDay(day int64, preview bool) (*Summary, error) {
	if !r.busy.TryLock() {
		return nil, Busy{}
	}
	r.busyOn.Store(true)
	defer func() {
		r.busyOn.Store(false)
		r.busy.Unlock()
	}()
	return r.runDay(day, preview)
}

func join(s []string) string { return strings.Join(s, ", ") }

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (r *Runner) runDay(day int64, preview bool) (*Summary, error) {
	st := r.Store.Snapshot()
	t0 := time.Now()
	tags, err := r.UpdateUniverse(st.Tags, day, func(m string) { fmt.Println(m) })
	if err != nil {
		return nil, err
	}
	if err := r.Store.Update(func(d *state.Data) { d.Tags = tags }); err != nil {
		return nil, err
	}
	paused := st.Desired == "paused"
	spot, err := r.SpotSymbols()
	if err != nil {
		return nil, err
	}
	cands := r.Candidates(tags, day, spot)
	cool := st.Cool
	// Paused = no new pairs, held pairs still managed: the acted-on plan sees only the held pairs.
	// The full decision is still computed, so the plan keeps the funding top and what a resume would open.
	full, err := r.Decide(day, st.Held, st.Capital, cands, cool)
	if err != nil {
		return nil, err
	}
	plan := full
	if paused {
		if plan, err = r.Decide(day, st.Held, st.Capital, []string{}, cool); err != nil {
			return nil, err
		}
	}
	plan.HeldN = map[string]float64{}
	for _, s := range st.Held.Keys() {
		h, _ := st.Held.Get(s)
		plan.HeldN[s] = h.N
	}
	od, err := r.Orders(plan)
	if err != nil {
		return nil, err
	}
	var nv []float64
	for _, s := range plan.Book {
		if v, ok := plan.Notional[s]; ok {
			nv = append(nv, v)
		}
	}
	used := (1 + strategy.P.Buffer) * pyx.PySum(nv)
	skipped := map[string]string{}
	for k, v := range full.Skipped {
		skipped[k] = v
	}
	for k, v := range plan.Skipped {
		skipped[k] = v
	}
	wouldOpen := []string{}
	if paused {
		wouldOpen = full.Open
	}
	if od == nil {
		od = []strategy.Order{}
	}
	top := append([]strategy.Top{}, full.Top...)
	for i := range top {
		top[i].Score = fin(top[i].Score)
	}
	sum := &Summary{Day: carry.Day(day), Preview: preview, Accepted: !preview, Paused: paused, Equity: fin(plan.Equity),
		Book: nz(plan.Book), Open: nz(plan.Open), Close: nz(plan.Close), Resize: plan.Resize, PairStopped: nz(plan.PairStopped),
		Rehedge: nz(plan.Rehedge), Top: top, WouldOpen: nz(wouldOpen), MinF8: strategy.P.MinF8, Skipped: skipped,
		Candidates: len(cands), Orders: od, Idle: fin(plan.Equity - used), ComputedAt: carry.NowISO()}
	raw, err := json.Marshal(sum)
	if err != nil {
		return nil, err
	}
	var topParts []string
	for i, t := range full.Top {
		if i >= 3 {
			break
		}
		topParts = append(topParts, fmt.Sprintf("%s %.3f %%", t.Symbol, t.Score*100))
	}
	topS := orDefault(join(topParts), "none")
	secs := func() float64 { return time.Since(t0).Seconds() }

	if preview {
		if err := r.Store.Update(func(d *state.Data) { d.Plan = raw }); err != nil {
			return nil, err
		}
		note := ""
		if paused {
			note = "; paused, a resume would open " + orDefault(join(sum.WouldOpen), "nothing")
		}
		r.Store.Event("info", fmt.Sprintf("preview %s: book %s%s; top funding/8h %s (min %.3f %%); %d candidates, %.0f s",
			sum.Day, orDefault(join(plan.Book), "empty"), note, topS, strategy.P.MinF8*100, len(cands), secs()), true)
		return sum, nil
	}

	if r.Live != nil { // live: real orders, real marks
		if err := r.Live.Execute(plan); err != nil {
			return nil, err
		}
		row, err := r.Live.Mark(day)
		if err != nil {
			return nil, err
		}
		if err := r.Store.Update(func(d *state.Data) {
			d.Plan = raw
			if d.StartDay == nil || *d.StartDay == "" {
				d.StartDay = state.Str(sum.Day)
			}
			d.LastRunDay, d.LastRunAt, d.LastError = state.Str(sum.Day), state.Str(carry.NowISO()), nil
		}); err != nil {
			return nil, err
		}
		r.Store.Event("info", fmt.Sprintf("day %s (live): book %s, equity %.2f USDT; top funding/8h %s; %d candidates, %.0f s",
			row.Day, orDefault(join(row.Book), "empty"), row.Equity, topS, len(cands), secs()), true)
		return sum, nil
	}

	rates := map[string][]market.FundingRow{}
	for _, s := range st.Held.Keys() {
		h, _ := st.Held.Get(s)
		marked := h.Marked
		if marked == 0 {
			marked = day
		}
		f, err := market.Funding(s, marked, day)
		if err != nil {
			return nil, err
		}
		rates[s] = f
	}
	marks := paper.MarkAll(st.Held, plan.Prices, rates, day)
	var res paper.Result
	if err := r.Store.Update(func(d *state.Data) {
		d.Held = st.Held // the snapshot the marks were computed on
		res = paper.Apply(d, plan, marks)
		d.Plan = raw
		if d.StartDay == nil || *d.StartDay == "" {
			d.StartDay = state.Str(sum.Day)
		}
		d.LastRunDay, d.LastRunAt, d.LastError = state.Str(sum.Day), state.Str(carry.NowISO()), nil
	}); err != nil {
		return nil, err
	}
	for _, e := range res.Events {
		r.Store.Event(e.Level, e.Msg, e.Notify)
	}
	row := res.Row
	r.Store.Event("info", fmt.Sprintf("day %s: book %s, equity %.2f USDT (funding %+.2f, basis %+.2f, fees %+.2f); top funding/8h %s; %d candidates, %.0f s",
		row.Day, orDefault(join(row.Book), "empty"), row.Equity, row.Funding, row.Basis, row.Fees, topS, len(cands), secs()), true)
	return sum, nil
}

// Loop runs the daily cycle once per UTC day after RunAfterMS, checking every 30 s.
func (r *Runner) Loop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		now := carry.NowMS()
		day := today(now)
		st := r.Store.Snapshot()
		r.mu.Lock()
		next := r.nextTry
		r.mu.Unlock()
		if r.Mode != "live-blocked" && eqOf(st.LastRunDay) != carry.Day(day) && now >= day+r.RunAfterMS && nowS() >= next &&
			!r.IsBusy() {
			r.SafeRun(day, false)
		}
		select {
		case <-stop:
			return
		case <-time.After(30 * time.Second):
		}
	}
}
