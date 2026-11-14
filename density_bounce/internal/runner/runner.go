// Package runner is the engine (bounce/runner.py): the 0.5 s tick loop, the feed and the heartbeat, and the views
// the control API serves.
//
// Concurrency: the Python runner was one asyncio loop. Here Mu is the engine lock; every access to the Trader, the
// Books, the SimBroker and the LiveBroker happens with it held. Runner implements trader.Sched: Go runs a function
// soon under the lock (create_task), After runs it later under the lock (call_later). Network I/O of the feed and
// of the live broker releases the lock around the call (Python's await). The API takes the lock for its views
// (Python's in_loop).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"densitybounce/internal/book"
	"densitybounce/internal/feed"
	"densitybounce/internal/live"
	"densitybounce/internal/meta"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/setup"
	"densitybounce/internal/sim"
	"densitybounce/internal/state"
	"densitybounce/internal/trader"
)

// Tick is the period of the trader loop.
const Tick = 500 * time.Millisecond

// ErrNotRunning is returned by Flatten while the loop is not running (the API answers 409).
var ErrNotRunning = errors.New("runner loop is not running")

// Runner owns the engine.
type Runner struct {
	Mu     sync.Mutex
	Store  *state.Store
	Mode   string // paper | live | live-blocked
	Trader *trader.Trader
	Feed   *feed.Feed
	Live   *live.LiveBroker
	// Started is the start time (uptime_s).
	Started time.Time
	// FlattenTimeout is how long Flatten waits for the loop (15 s).
	FlattenTimeout time.Duration

	day           string
	feedDownSince float64
	running       atomic.Bool
	cancel        context.CancelFunc
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// New builds a runner. rest != nil makes the broker a live broker; otherwise the paper broker (also for
// live-blocked, which opens nothing).
func New(store *state.Store, mode, notify, blocked string, rest *live.Rest) *Runner {
	r := &Runner{Store: store, Mode: mode, Started: time.Now(), FlattenTimeout: 15 * time.Second, day: state.UTCDay(now())}
	var broker trader.Broker
	var sb *sim.SimBroker
	if rest != nil {
		r.Live = live.NewBroker(rest, &r.Mu)
		broker = r.Live
	} else {
		lag := 0.3
		if v := os.Getenv("SIM_LAG"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
				lag = f
			}
		}
		sb = sim.New(lag)
		broker = sb
	}
	r.Trader = trader.New(broker, store, nil, notify, r)
	r.Trader.Live = mode == "live"
	if blocked != "" {
		r.Trader.Halt = "live trading is OFF: " + blocked
	}
	var onTrade func(string, book.Trade)
	if sb != nil {
		onTrade = sb.OnTrade
	}
	r.Feed = feed.New(r.Trader, &r.Mu, onTrade, 0)
	return r
}

// Go runs fn soon under the engine lock (asyncio create_task).
func (r *Runner) Go(fn func()) { go r.locked(fn) }

// After runs fn under the engine lock after d (asyncio call_later).
func (r *Runner) After(d time.Duration, fn func()) {
	time.AfterFunc(d, func() { r.locked(fn) })
}

func (r *Runner) locked(fn func()) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	defer func() {
		if p := recover(); p != nil {
			log.Printf("ERROR scheduled task: %v", p)
		}
	}()
	fn()
}

// ------------------------------------------------------------------ loop

// Start runs the loop in the background.
func (r *Runner) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.Feed.SetContext(ctx)
	r.running.Store(true)
	go r.mainLoop(ctx)
}

// Stop ends the loops.
func (r *Runner) Stop() {
	r.running.Store(false)
	if r.cancel != nil {
		r.cancel()
	}
	if r.Live != nil {
		r.Live.Stop()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (r *Runner) mainLoop(ctx context.Context) {
	if r.Live != nil {
		r.Mu.Lock()
		err := r.Live.Start()
		r.Mu.Unlock()
		if err != nil {
			r.Mu.Lock()
			r.Trader.Halt = fmt.Sprintf("live start failed: %v", err)
			r.Mu.Unlock()
			r.Store.Event("error", fmt.Sprintf("%s: live start failed (%v); no new entries", meta.StrategyID, err), true)
		}
	}
	go r.Feed.Screener(func() map[string]bool { return r.Trader.BlockedSyms })
	go r.Feed.Watchdog()
	go r.heartbeat(ctx)
	for ctx.Err() == nil {
		t0 := time.Now()
		r.tick(float64(t0.UnixNano()) / 1e9)
		if !sleepCtx(ctx, max(50*time.Millisecond, Tick-time.Since(t0))) {
			return
		}
	}
}

func (r *Runner) tick(t0 float64) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	defer func() { // the loop must survive anything
		if p := recover(); p != nil {
			log.Printf("ERROR tick: %v", p)
		}
	}()
	r.Trader.Tick(t0)
}

func (r *Runner) heartbeat(ctx context.Context) {
	for sleepCtx(ctx, 60*time.Second) {
		r.beat(now())
	}
}

// beat is one heartbeat (exported to the package's tests through the lowercase name).
func (r *Runner) beat(t float64) {
	if err := r.Store.Save(); err != nil { // persists the plan / fill counters
		log.Printf("WARNING state save: %v", err)
	}
	day := state.UTCDay(t)
	if day != r.day {
		r.Store.Event("info", r.DaySummary(r.day), true)
		r.day = day
	}
	r.Mu.Lock()
	h := r.Feed.Health()
	r.Mu.Unlock()
	down := h.Watching > 0 && h.Synced == 0
	switch {
	case down && r.feedDownSince == 0:
		r.feedDownSince = t
	case !down && r.feedDownSince != 0:
		if t-r.feedDownSince > 180 {
			r.Store.Event("info", "market data back", true)
		}
		r.feedDownSince = 0
	}
	if r.feedDownSince != 0 && 180 < t-r.feedDownSince && t-r.feedDownSince <= 240 {
		r.Store.Event("error", fmt.Sprintf("no order book synced for %s s (%d symbols)", pyfmt.F(t-r.feedDownSince, 0), h.Watching), true)
	}
}

// ------------------------------------------------------------------ commands

// pyStr is Python's str() of a JSON value (the "by" of a command is a Telegram user id).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case bool:
		return x
	}
	return true
}

// SetDesired pauses or resumes (persisted) and returns the strategy.
func (r *Runner) SetDesired(desired string, by any, reason string) Strategy {
	prev := r.Store.Desired()
	r.Store.SetDesired(desired)
	if prev != desired {
		what := "resumed"
		if desired == "paused" {
			what = "paused: no new entries, open trades keep their stop and take"
		}
		who := ""
		if truthy(by) {
			who = " by " + pyStr(by)
		}
		extra := ""
		if reason != "" {
			extra = " (" + reason + ")"
		}
		r.Store.Event("info", fmt.Sprintf("%s %s%s%s", meta.StrategyID, what, who, extra), true)
	}
	return r.Strategy()
}

// RunResult is the answer of POST /run.
type RunResult struct {
	OK      bool `json:"ok"`
	Started bool `json:"started"`
	Preview bool `json:"preview"`
}

// Trigger reports what the bot sees right now (as a Telegram message) and re-screens the symbols in the background.
func (r *Runner) Trigger() RunResult {
	r.Mu.Lock()
	lines := r.planLines()
	r.Mu.Unlock()
	if len(lines) > 14 {
		lines = lines[:14]
	}
	r.Store.Event("info", strings.Join(append([]string{meta.StrategyID + " now:"}, lines...), "\n"), true)
	if r.running.Load() {
		go func() {
			if err := r.Feed.Rescreen(); err != nil {
				log.Printf("WARNING rescreen: %v", err)
			}
		}()
	}
	return RunResult{OK: true, Started: true, Preview: true}
}

// FlattenResult is the answer of POST /flatten.
type FlattenResult struct {
	OK      bool `json:"ok"`
	Started bool `json:"started"`
	Slots   int  `json:"slots"`
}

// Flatten pauses, cancels the working entries and closes the open trades (15 s timeout).
func (r *Runner) Flatten(by any) (FlattenResult, error) {
	r.SetDesired("paused", by, "flatten")
	if !r.running.Load() {
		return FlattenResult{}, ErrNotRunning
	}
	ch := make(chan int, 1)
	go func() {
		r.Mu.Lock()
		defer r.Mu.Unlock()
		ch <- r.Trader.Flatten()
	}()
	select {
	case n := <-ch:
		r.Store.Event("warn", fmt.Sprintf("%s: flatten by %s: %d trade(s) cancelled / closed, paused", meta.StrategyID, pyStr(by), n), true)
		return FlattenResult{OK: true, Started: true, Slots: n}, nil
	case <-time.After(r.FlattenTimeout):
		return FlattenResult{}, errors.New("TimeoutError: ")
	}
}

// ------------------------------------------------------------------ views

// Strategy is one element of GET /v1/strategies.
type Strategy struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Mode          string   `json:"mode"`
	State         string   `json:"state"`
	Desired       string   `json:"desired"`
	OpenPositions int      `json:"open_positions"`
	Book          []string `json:"book"`
	Symbols       []string `json:"symbols"`
	Working       []string `json:"working"`
	Capital0      float64  `json:"capital0"`
	Equity        float64  `json:"equity"`
	PnlTotal      float64  `json:"pnl_total"`
	PnlToday      float64  `json:"pnl_today"`
	TradesTotal   int      `json:"trades_total"`
	TradesToday   int      `json:"trades_today"`
	LastRunDay    string   `json:"last_run_day"`
	LastRunAt     *string  `json:"last_run_at"`
	NextRunAt     *string  `json:"next_run_at"`
	LastError     *string  `json:"last_error"`
}

// Strategy is the strategy view (takes the lock).
func (r *Runner) Strategy() Strategy {
	d := r.Store.Snapshot()
	r.Mu.Lock()
	held, working := []string{}, []string{}
	upnl := 0.0
	slots := r.Trader.Slots()
	for _, s := range slots {
		if s.State != "arm" && s.State != "entry" {
			held = append(held, s.Sym)
		} else {
			working = append(working, s.Sym)
		}
	}
	if len(slots) > 0 {
		for _, v := range r.Trader.View(nil) {
			upnl += v.UPnL
		}
	}
	halt := r.Trader.Halt
	r.Mu.Unlock()
	t := now()
	today := d.Days[state.UTCDay(t)]
	st := d.Desired
	if halt != "" {
		st = "error"
	}
	var lastAt *string
	if ts, ok := r.Store.LastTradeClose(); ok {
		v := state.NowISO(ts)
		lastAt = &v
	}
	lastErr := d.LastError
	if halt != "" {
		lastErr = &halt
	}
	return Strategy{ID: meta.StrategyID, Name: meta.StrategyName, Mode: r.Mode, State: st, Desired: d.Desired,
		OpenPositions: len(held),
		// `book` stays empty on purpose: control's LossGuard leaves `book` symbols of live strategies out of its
		// loss measure (they are hedged carry legs); scalp positions must count.
		Book: []string{}, Symbols: held, Working: working, Capital0: d.Capital0,
		Equity:   pyfmt.Round(d.Capital0+d.Totals.PNL+upnl, 4),
		PnlTotal: pyfmt.Round(d.Totals.PNL, 4), PnlToday: pyfmt.Round(today.PNL, 4),
		TradesTotal: d.Totals.N, TradesToday: today.N, LastRunDay: state.UTCDay(t), LastRunAt: lastAt,
		LastError: lastErr}
}

// pyFloat is str(float) of a rounded lag: "0.3", "12.0", and "0" when there is no book at all.
func lagStr(h feed.Health) string {
	if h.Watching == 0 {
		return "0"
	}
	s := strconv.FormatFloat(h.MaxLagS, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// PlanLines is what the bot sees now (takes the lock).
func (r *Runner) PlanLines() []string {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return r.planLines()
}

func (r *Runner) planLines() []string {
	t := now()
	h := r.Feed.Health()
	lines := []string{fmt.Sprintf("mode %s · %s · watching %d (%d synced, lag %s s) · %d densities", r.Mode,
		r.Store.Desired(), h.Watching, h.Synced, lagStr(h), h.Densities)}
	why := r.Trader.Halt
	if why == "" {
		why = r.Trader.Block(t)
	}
	if why != "" {
		lines = append(lines, "no new entries: "+why)
	}
	p := setup.P
	lines = append(lines, fmt.Sprintf("rules: approach >= %s/%ss, move >= %s/%smin, density >= %s s of the flow and %s s old, "+
		"market move < %s, entry %s s after the touch, stop max(%s, %s x ATR) behind, %s at +%sR, take %s of the move, RR >= %s",
		pyfmt.Pct(p.ApproachMin, 2), pyfmt.F(p.ApproachSecs, 0), pyfmt.Pct(p.MoveMin, 2), pyfmt.F(p.MoveSecs/60, 0),
		pyfmt.F(p.HoldMin, 0), pyfmt.F(p.MinAge, 0), pyfmt.Pct(p.MktMax, 1), pyfmt.F(p.ConfirmSecs, 0),
		pyfmt.Pct(p.StopBuf, 2), pyfmt.G(p.StopATR), pyfmt.Pct(p.TP1Part, 0), pyfmt.G(p.TP1R), pyfmt.Pct(p.TakeFrac, 0),
		pyfmt.G(p.MinRR)))
	rk := trader.RISK
	lines = append(lines, fmt.Sprintf("risk: %s USDT a stop, <= %s USDT/trade x%d, max %d at once, day stop -%s USDT or -%sR, total stop -%s USDT",
		pyfmt.G(trader.RiskUSD()), pyfmt.G(rk.Notional), rk.Leverage, rk.MaxOpen, pyfmt.G(rk.DayLoss), pyfmt.G(rk.DayLossR),
		pyfmt.G(rk.TotalLoss)))
	st := r.Store.Stats()
	lines = append(lines, fmt.Sprintf("plans %d · armed %d · placed %d · filled %d · cancelled %d · shadow plans followed %d",
		st["plans"], st["armed"], st["placed"], st["filled"], st["cancelled"], len(r.Trader.Shadows())))
	for _, v := range r.Trader.View(nil) {
		l := fmt.Sprintf("%s %s %s entry %s stop %s take %s", strings.ToUpper(v.State), v.Side, v.Symbol, pyfmt.G(v.Entry),
			pyfmt.G(v.Stop), pyfmt.G(v.TP))
		if v.State != "arm" && v.State != "entry" {
			l += " upnl " + pyfmt.FS(v.UPnL, 4)
		}
		lines = append(lines, l)
	}
	type near struct {
		dist float64
		sym  string
		d    *book.Density
	}
	var ns []near
	for _, b := range r.Trader.Books() {
		mid, ok := b.Mid()
		if !ok || mid == 0 {
			continue
		}
		for _, d := range b.Alive() {
			dist := (mid - d.Price) / mid
			if dist < 0 {
				dist = -dist
			}
			ns = append(ns, near{dist, b.Symbol, d})
		}
	}
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].dist < ns[j].dist })
	if len(ns) > 0 {
		lines = append(lines, "nearest densities:")
	}
	for i, n := range ns {
		if i >= 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("  %s %s %s  %sk USDT  %s away  age %ss  touches %d", n.sym, n.d.Side, pyfmt.G(n.d.Price),
			pyfmt.FComma(n.d.USD/1000, 0), pyfmt.Pct(n.dist, 2), pyfmt.F(n.d.Age(t), 0), n.d.Touches))
	}
	return lines
}

// Plan is GET /plan.
type Plan struct {
	Day        string   `json:"day"`
	Preview    bool     `json:"preview"`
	Accepted   bool     `json:"accepted"`
	Paused     bool     `json:"paused"`
	Equity     float64  `json:"equity"`
	Book       []string `json:"book"`
	Open       []string `json:"open"`
	Candidates int      `json:"candidates"`
	ComputedAt string   `json:"computed_at"`
	Text       []string `json:"text"`
}

// Plan is the plan view (takes the lock).
func (r *Runner) Plan() Plan {
	d := r.Store.Snapshot()
	r.Mu.Lock()
	lines := r.planLines()
	syms := []string{}
	for _, s := range r.Trader.Slots() {
		syms = append(syms, s.Sym)
	}
	cand := r.Trader.BookCount()
	r.Mu.Unlock()
	sort.Strings(syms)
	return Plan{Day: state.UTCDay(now()), Accepted: true, Paused: d.Desired == "paused",
		Equity: pyfmt.Round(d.Capital0+d.Totals.PNL, 4), Book: []string{}, Open: syms, Candidates: cand,
		ComputedAt: state.NowISO(-1), Text: lines}
}

// Position is one element of GET /positions.
type Position struct {
	Symbol string   `json:"symbol"`
	N      float64  `json:"n"`
	Days   int      `json:"days"`
	PNL    float64  `json:"pnl"`
	Side   string   `json:"side"`
	Entry  float64  `json:"entry"`
	Stop   float64  `json:"stop"`
	TP     float64  `json:"tp"`
	Mark   *float64 `json:"mark"`
	Text   string   `json:"text"`
}

// Positions are the open trades (takes the lock).
func (r *Runner) Positions() []Position {
	r.Mu.Lock()
	views := r.Trader.View(nil)
	r.Mu.Unlock()
	out := []Position{}
	for _, v := range views {
		if v.State == "arm" || v.State == "entry" {
			continue
		}
		mark := 0.0
		if v.Mark != nil {
			mark = *v.Mark
		}
		out = append(out, Position{Symbol: v.Symbol, N: v.N, PNL: v.UPnL, Side: v.Side, Entry: v.Entry, Stop: v.Stop,
			TP: v.TP, Mark: v.Mark,
			Text: fmt.Sprintf("%s %s %s @ %s stop %s take %s mark %s upnl %s (%d s)", v.Symbol, v.Side, pyfmt.G(v.Qty),
				pyfmt.G(v.Entry), pyfmt.G(v.Stop), pyfmt.G(v.TP), pyfmt.G(mark), pyfmt.FS(v.UPnL, 4), v.AgeS)})
	}
	return out
}

// DaySummary is the day's line for the day-change event.
func (r *Runner) DaySummary(day string) string {
	a, ok := r.Store.Day(day)
	if !ok || a.N == 0 {
		return fmt.Sprintf("%s %s: no trades", meta.StrategyID, day)
	}
	n := float64(a.N)
	return fmt.Sprintf("%s %s: %d trades, win %s, pnl %s USDT (fees %s), avg %sR", meta.StrategyID, day, a.N,
		pyfmt.Pct(float64(a.Wins)/n, 0), pyfmt.FS(a.PNL, 4), pyfmt.F(a.Fees, 4), pyfmt.FS(a.R/n, 2))
}

// Report is GET /report.
type Report struct {
	Start       string   `json:"start"`
	Days        int      `json:"days"`
	ActiveDays  int      `json:"active_days"`
	Capital0    float64  `json:"capital0"`
	Equity      float64  `json:"equity"`
	ReturnPct   float64  `json:"return_pct"`
	AnnualPct   float64  `json:"annual_pct"`
	MaxDDPct    float64  `json:"max_dd_pct"`
	Funding     float64  `json:"funding"`
	Basis       float64  `json:"basis"`
	Fees        float64  `json:"fees"`
	PairsOpened int      `json:"pairs_opened"`
	PairsClosed int      `json:"pairs_closed"`
	PairStops   int      `json:"pair_stops"`
	Trades      int      `json:"trades"`
	Wins        int      `json:"wins"`
	Curve       [][2]any `json:"curve"`
	Text        []string `json:"text"`
}

func padR(s string, n int) string {
	for len([]rune(s)) < n {
		s += " "
	}
	return s
}

func padL(s string, n int) string {
	for len([]rune(s)) < n {
		s = " " + s
	}
	return s
}

func winPct(wins, n int) string {
	if n == 0 {
		return "0%"
	}
	return pyfmt.Pct(float64(wins)/float64(n), 0)
}

// Report is the report view (the store has its own lock).
func (r *Runner) Report() Report {
	d := r.Store.Snapshot()
	t := d.Totals
	cap := d.Capital0
	eq, peak, dd := cap, cap, 0.0
	curve := [][2]any{}
	days := make([]string, 0, len(d.Days))
	for k := range d.Days {
		days = append(days, k)
	}
	sort.Strings(days)
	for _, day := range days {
		eq += d.Days[day].PNL
		peak = max(peak, eq)
		x := 0.0
		if peak != 0 {
			x = (peak - eq) / peak * 100
		}
		dd = max(dd, x)
		curve = append(curve, [2]any{day, pyfmt.Round(eq, 4)})
	}
	n := t.N
	var lines []string
	if n > 0 {
		fn := float64(n)
		lines = append(lines, fmt.Sprintf("trades %d · win %s · avg %sR · pnl %s USDT (gross %s, fees %s)", n,
			pyfmt.Pct(float64(t.Wins)/fn, 1), pyfmt.FS(t.R/fn, 2), pyfmt.FS(t.PNL, 4), pyfmt.FS(t.Gross, 4), pyfmt.F(t.Fees, 4)))
	} else {
		lines = append(lines, "no closed trades yet")
	}
	for _, g := range []map[string]state.Group{t.Reasons, t.Sides} {
		keys := make([]string, 0, len(g))
		for k := range g {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { // by count, then by name (Python kept the insertion order for ties)
			if g[keys[i]].N != g[keys[j]].N {
				return g[keys[i]].N > g[keys[j]].N
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			x := g[k]
			lines = append(lines, fmt.Sprintf("  %s n %s  win %s  pnl %s", padR(k, 14), padL(strconv.Itoa(x.N), 4),
				padL(winPct(x.Wins, x.N), 4), pyfmt.FS(x.PNL, 4)))
		}
	}
	last := days
	if len(last) > 7 {
		last = last[len(last)-7:]
	}
	for _, day := range last {
		a := d.Days[day]
		lines = append(lines, fmt.Sprintf("%s  n %s  win %s  pnl %s", day, padL(strconv.Itoa(a.N), 3), padL(winPct(a.Wins, a.N), 4),
			pyfmt.FS(a.PNL, 4)))
	}
	for _, tr := range r.Store.LastTrades(5) {
		l := fmt.Sprintf("%s %s %s %s %s", state.NowISO(tr.TsClose)[5:16], padR(tr.Side, 5), padR(tr.Sym, 12), padR(tr.Reason, 8),
			pyfmt.FS(tr.PNL, 4))
		if tr.R != nil {
			l += " " + pyfmt.FS(*tr.R, 2) + "R"
		}
		lines = append(lines, l)
	}
	start := state.UTCDay(now())
	if d.StartDay != nil && *d.StartDay != "" {
		start = *d.StartDay
	}
	ret := 0.0
	if cap != 0 {
		ret = pyfmt.Round(t.PNL/cap*100, 3)
	}
	if len(curve) > 60 {
		curve = curve[len(curve)-60:]
	}
	return Report{Start: start, Days: len(days), ActiveDays: len(days), Capital0: cap, Equity: pyfmt.Round(cap+t.PNL, 4),
		ReturnPct: ret, MaxDDPct: pyfmt.Round(dd, 3), Basis: pyfmt.Round(t.Gross, 4), Fees: pyfmt.Round(t.Fees, 4),
		PairsOpened: n, PairsClosed: t.Wins, PairStops: t.Reasons["stop"].N, Trades: n, Wins: t.Wins, Curve: curve, Text: lines}
}

// HealthExtra is the runner's part of GET /v1/health.
type HealthExtra struct {
	Feed         feed.Health `json:"feed"`
	UniverseDays int         `json:"universe_days"`
}

// HealthExtra reads the feed health (takes the lock).
func (r *Runner) HealthExtra() HealthExtra {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	h := r.Feed.Health()
	return HealthExtra{Feed: h, UniverseDays: h.Watching}
}
