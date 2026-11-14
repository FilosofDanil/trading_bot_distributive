// Package trader is the trade lifecycle and the risk limits, shared by paper and live mode (bounce/trader.py).
//
// A Slot is one symbol's trade: entry armed (waiting for the confirmation, no order yet) -> entry working ->
// open (protected by a stop and reduce-only takes, one per part: tp1 / tp2 / run, see setup) -> closed. The
// stop always closes whatever is left of the position. The broker (sim.SimBroker, or the live broker) executes
// and reports back through OnEntryResult / OnPart / OnClosed. Tick is called every 0.5 s by the runner loop.
//
// Concurrency contract. The Python runner was one asyncio loop plus an API thread behind threading locks. Here
// there is ONE engine lock, owned by the runner: every call into a Trader, a SimBroker, a book.Book or a
// shadow.Shadow happens with that lock held (the feed, the 0.5 s loop, the HTTP handlers, the broker's
// background polls). Nothing in this package locks, so nothing here may be called without it. The state.Store
// has its own mutex and may also be used without the engine lock. Python's asyncio create_task and call_later
// become the Sched interface: Go queues a function to run soon, After runs it after a delay, and the runner
// implements both by running the function later UNDER THE SAME LOCK. The broker methods return an error where
// the Python ones raised; they are called synchronously with the lock held, so a live broker must not block in
// them (it starts its own goroutine, takes the engine lock to report, and calls OnEntryResult / OnClosed).
//
// Books, slots and shadows keep their INSERTION ORDER (omap): Python iterates dicts in that order and it decides
// which symbol takes a free slot first.
package trader

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"densitybounce/internal/book"
	"densitybounce/internal/meta"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/setup"
	"densitybounce/internal/shadow"
	"densitybounce/internal/state"
)

// Shadow timing constants.
const (
	ShadowEvery = 600.0 // s between two shadow plans of one density skipped by the same gates
	ShadowMax   = 60    // shadow plans followed at once
)

// Broker executes the trader's orders. Every call happens under the engine lock and returns an error where the
// Python coroutine raised. A -2021 rule: MoveStop may return an error with Code() == -2021 ("price is already
// past the new stop"), then the trader closes the position.
type Broker interface {
	PlaceEntry(s *Slot) error
	CancelEntry(s *Slot) error
	Protect(s *Slot) error
	MoveStop(s *Slot, price float64) error
	Close(s *Slot, why string) error
}

// Attacher is implemented by brokers that need the trader (Python's broker.trader = self).
type Attacher interface{ Attach(t *Trader) }

// Sched runs work later under the engine lock: Go ~ asyncio create_task, After ~ loop.call_later.
type Sched interface {
	Go(fn func())
	After(d time.Duration, fn func())
}

// SymInfo is the exchange info of one symbol.
type SymInfo struct {
	Tick        float64
	Step        float64
	MinQty      float64
	MinNotional float64
}

// Part is one take of a position.
type Part struct {
	Name   string
	Px     float64
	Qty    float64
	Done   bool
	ID     string
	FillPx float64
}

// Slot is one symbol's trade.
type Slot struct {
	Sym        string
	Plan       *setup.Plan
	Qty        float64
	State      string // arm | entry | open | closing
	PlacedAt   float64
	FilledQty  float64
	EntryPx    float64
	EntryFee   float64
	OpenedAt   float64
	Stop       float64
	Manage     *setup.Manage
	CancelSent bool
	CloseSent  bool
	OID        string // client order id of the entry (live)
	Parts      []*Part

	TouchAt      *float64 // when price reached the entry level (armed entries)
	ArmedS       float64
	CancelReason string
	ExitReason   string
	StopWhy      string // the last stop move that reached the exchange: stop_lock, stop_be
	LockDone     bool
	StopID       string // client id of the exchange stop (live)
}

// StopReason is the name of a stop-out, from the last stop move that reached the exchange: stop_lock, stop_be
// or stop.
func StopReason(s *Slot) string {
	if s.StopWhy != "" {
		return s.StopWhy
	}
	return "stop"
}

type attemptKey struct {
	Sym  string
	DKey book.DKey
}

// ShadowKey is (sym, dkey, skip): one density skipped by one gate.
type ShadowKey struct {
	Sym  string
	DKey book.DKey
	Skip string
}

type shadowKey = ShadowKey

// Trader holds the books, the slots and the risk state.
type Trader struct {
	Broker Broker
	Store  *state.Store
	Info   map[string]SymInfo // symbol -> {tick, step, min_qty, min_notional}

	books   *omap[string, *book.Book]
	slots   *omap[string, *Slot]
	shadows *omap[string, *shadow.Shadow] // pid -> the plain entry of that plan, followed on the tape

	Attempts   map[attemptKey]int     // (sym, dkey) -> attempts
	Failed     map[attemptKey]float64 // (sym, dkey) -> no entries at that density until this time
	ShadowSeen map[shadowKey]float64  // (sym, dkey, skip) -> time of its last shadow plan
	Cool       map[string]float64
	Slips      map[string][]float64 // sym -> times of stops that slipped > slip_max

	Live        bool // set by the runner: the guard on the mean R is for live money
	BlockedSyms map[string]bool
	Halt        string // set -> no new entries, ever (total loss)
	Notify      string // closes | all | none
	LastBlock   string

	// Now is the clock (Python's time.time()); Sched runs deferred work; Logf is the log (log.Printf by default).
	Now   func() float64
	Sched Sched
	Logf  func(format string, args ...any)

	guard    string
	guardOK  bool
	mktAt    float64
	mktCh    map[string]float64
	prunedAt float64
}

// prune drops the entries of ShadowSeen and Failed that no longer change a decision (Python kept them forever):
// a shadow seen ShadowEvery ago or earlier passes the check as if never seen, an expired block blocks nothing.
// Runs at most once per ShadowEvery.
func (t *Trader) prune(now float64) {
	if now-t.prunedAt < ShadowEvery {
		return
	}
	t.prunedAt = now
	for k, at := range t.ShadowSeen {
		if now-at >= ShadowEvery {
			delete(t.ShadowSeen, k)
		}
	}
	for k, until := range t.Failed {
		if until <= now {
			delete(t.Failed, k)
		}
	}
}

// New makes a trader and attaches it to the broker.
func New(broker Broker, store *state.Store, info map[string]SymInfo, notify string, sched Sched) *Trader {
	if sched == nil {
		panic("trader.New: a Sched is required")
	}
	if info == nil {
		info = map[string]SymInfo{}
	}
	t := &Trader{Broker: broker, Store: store, Info: info, Notify: notify, Sched: sched,
		books: newOmap[string, *book.Book](), slots: newOmap[string, *Slot](),
		shadows:  newOmap[string, *shadow.Shadow](),
		Attempts: map[attemptKey]int{}, Failed: map[attemptKey]float64{}, ShadowSeen: map[shadowKey]float64{},
		Cool: map[string]float64{}, Slips: map[string][]float64{}, BlockedSyms: map[string]bool{},
		mktCh: map[string]float64{},
		Now:   func() float64 { return float64(time.Now().UnixNano()) / 1e9 },
		Logf:  log.Printf}
	if a, ok := broker.(Attacher); ok {
		a.Attach(t)
	}
	return t
}

func (t *Trader) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// ------------------------------------------------------------------ books, slots

// AddBook watches a book (set again, a symbol keeps its place).
func (t *Trader) AddBook(sym string, b *book.Book) { t.books.Set(sym, b) }

// RemoveBook stops watching a symbol.
func (t *Trader) RemoveBook(sym string) { t.books.Delete(sym) }

// Book is the book of a symbol (nil when it is not watched).
func (t *Trader) Book(sym string) *book.Book {
	b, _ := t.books.Get(sym)
	return b
}

// BookSyms are the watched symbols in insertion order.
func (t *Trader) BookSyms() []string { return t.books.Keys() }

// Books are the watched books in insertion order.
func (t *Trader) Books() []*book.Book { return t.books.Values() }

// BookCount is the number of watched books.
func (t *Trader) BookCount() int { return t.books.Len() }

// Slot is the trade of a symbol (nil when there is none).
func (t *Trader) Slot(sym string) *Slot {
	s, _ := t.slots.Get(sym)
	return s
}

// Slots are the trades in insertion order.
func (t *Trader) Slots() []*Slot { return t.slots.Values() }

// SlotCount is the number of trades.
func (t *Trader) SlotCount() int { return t.slots.Len() }

// PutSlot registers a trade (tests, live recovery).
func (t *Trader) PutSlot(s *Slot) { t.slots.Set(s.Sym, s) }

// ClearSlots drops every trade (tests).
func (t *Trader) ClearSlots() { t.slots.Clear() }

// Shadows are the followed shadow plans in insertion order.
func (t *Trader) Shadows() []*shadow.Shadow { return t.shadows.Values() }

// ShadowByPID is the shadow plan of a plan id (nil when there is none).
func (t *Trader) ShadowByPID(pid string) *shadow.Shadow {
	s, _ := t.shadows.Get(pid)
	return s
}

// ResetGuard drops the cached verdict of the live guard (tests).
func (t *Trader) ResetGuard() { t.guardOK = false }

// GuardCached is the cached verdict of the live guard (ok false = none cached).
func (t *Trader) GuardCached() (string, bool) { return t.guard, t.guardOK }

// ResetMarket forgets the cached market change (tests).
func (t *Trader) ResetMarket() { t.mktAt, t.mktCh = 0, map[string]float64{} }

// FailedUntil is the time until which entries at a density are blocked because it let us down (false = not blocked).
func (t *Trader) FailedUntil(sym string, k book.DKey) (float64, bool) {
	v, ok := t.Failed[attemptKey{sym, k}]
	return v, ok
}

// SetFailed blocks a density until the given time (tests).
func (t *Trader) SetFailed(sym string, k book.DKey, until float64) {
	t.Failed[attemptKey{sym, k}] = until
}

// ClearAttempts forgets the attempts per density (tests).
func (t *Trader) ClearAttempts() { t.Attempts = map[attemptKey]int{} }

// ------------------------------------------------------------------ helpers

// Paused reports whether the desired state is "paused".
func (t *Trader) Paused() bool { return t.Store.Desired() == "paused" }

func (t *Trader) stat(key string) { t.Store.AddStat(key) }

// Size is the qty that loses RiskUSD() at stop (at most the notional; without a stop: the notional), on the lot
// grid. 0 means the step is too coarse for this notional (or the symbol has no info).
func (t *Trader) Size(sym string, price, stop float64) float64 {
	i, ok := t.Info[sym]
	if !ok {
		return 0
	}
	usd := RISK.Notional
	if stop != 0 && stop != price {
		usd = math.Min(usd, RiskUSD()/(math.Abs(price-stop)/price))
	}
	q := pyfmt.Round(math.Floor(usd/price/i.Step+1e-9)*i.Step, 10)
	if q < i.MinQty || q*price < i.MinNotional {
		q = pyfmt.Round(math.Ceil(i.MinNotional/price/i.Step-1e-9)*i.Step, 10)
	}
	if q*price > RISK.Notional*1.6 {
		return 0.0 // step too coarse for this notional
	}
	return q
}

// Split are the takes of a filled position: tp1 / tp2 / run quantities on the lot grid; a part too small for the
// exchange (min qty, min notional + 10 %) is merged into tp2, so a small position keeps a single take.
func (t *Trader) Split(s *Slot) []*Part {
	i := t.Info[s.Sym]
	step, q, px := i.Step, s.FilledQty, s.EntryPx
	floor := func(x float64) float64 { return pyfmt.Round(math.Floor(x/step+1e-9)*step, 10) }
	ok := func(x float64) bool { return x >= i.MinQty && x*px >= 1.1*i.MinNotional }
	p := setup.P
	q1, q3 := 0.0, 0.0
	if s.Plan.TP1 != 0 {
		q1 = floor(q * p.TP1Part)
	}
	if s.Plan.TPRun != 0 {
		q3 = floor(q * (1 - p.TP1Part - p.TP2Part))
	}
	if !ok(q1) {
		q1 = 0
	}
	if !ok(q3) {
		q3 = 0
	}
	q2 := pyfmt.Round(q-q1-q3, 10)
	if q2 <= 0 || !ok(q2) {
		q1, q3 = 0, 0
		q2 = q
	}
	var parts []*Part
	for _, c := range []struct {
		n  string
		px float64
		v  float64
	}{{"tp1", s.Plan.TP1, q1}, {"tp2", s.Plan.TP, q2}, {"run", s.Plan.TPRun, q3}} {
		if c.v > 0 {
			parts = append(parts, &Part{Name: c.n, Px: c.px, Qty: c.v})
		}
	}
	return parts
}

// Block is why no new entry may open now ("" = free).
func (t *Trader) Block(now float64) string {
	day := state.UTCDay(now)
	if pnl := t.Store.TotalPNL(); pnl <= -RISK.TotalLoss {
		return fmt.Sprintf("total loss %s <= -%s USDT: halted", pyfmt.F(pnl, 2), pyfmt.G(RISK.TotalLoss))
	}
	today, _ := t.Store.Day(day)
	if today.PNL <= -RISK.DayLoss {
		return fmt.Sprintf("day loss %s <= -%s USDT", pyfmt.F(today.PNL, 2), pyfmt.G(RISK.DayLoss))
	}
	if RISK.DayLossR > 0 && today.R <= -RISK.DayLossR {
		return fmt.Sprintf("day loss %sR <= -%sR", pyfmt.F(today.R, 2), pyfmt.G(RISK.DayLossR))
	}
	if t.Live && RISK.GuardN > 0 {
		if !t.guardOK {
			n, mean := t.Store.GuardStats(meta.Version)
			t.guard = ""
			if n >= RISK.GuardN && mean < RISK.GuardR {
				t.guard = fmt.Sprintf("mean %sR over %d trades of v%s < %sR: halted, back to paper",
					pyfmt.FS(mean, 2), n, meta.Version, pyfmt.G(RISK.GuardR))
			}
			t.guardOK = true
		}
		if t.guard != "" {
			return t.guard
		}
	}
	// raising MAX_CONSEC_LOSSES lifts a block set under a lower limit
	if bday, limit := t.Store.ConsecBlock(); bday == day && limit >= RISK.MaxConsec {
		return fmt.Sprintf("%d losses in a row today", limit)
	}
	if today.N >= RISK.MaxTradesDay {
		return fmt.Sprintf("%d trades today", RISK.MaxTradesDay)
	}
	return ""
}

// ------------------------------------------------------------------ main step

// Tick is called every 0.5 s: scan the densities and move every symbol's trade one step.
func (t *Trader) Tick(now float64) {
	for _, sym := range t.books.Keys() {
		b := t.Book(sym)
		if b == nil {
			continue
		}
		s := t.Slot(sym)
		if !b.Synced || now-b.LastTs > 5 { // blind book: no new decisions
			switch {
			case s != nil && s.State == "arm":
				t.EntryOver(s, "stale_book", now)
			case s != nil && s.State == "entry" && !s.CancelSent:
				s.CancelSent = true
				s.CancelReason = "stale_book"
				if err := t.Broker.CancelEntry(s); err != nil {
					t.tickError(sym, err)
				}
			}
			continue
		}
		b.ScanDensities(now)
		t.tickSymbol(sym, b, s, now)
	}
	t.StepShadows(now)
}

func (t *Trader) tickError(sym string, err error) {
	t.logf("tick %s: %v", sym, err)
	t.Store.Event("error", fmt.Sprintf("%s: tick: %v", sym, err), false)
}

// tickSymbol never lets one symbol kill the loop: a panic or a broker error becomes an error event.
func (t *Trader) tickSymbol(sym string, b *book.Book, s *Slot, now float64) {
	defer func() {
		if r := recover(); r != nil {
			t.tickError(sym, fmt.Errorf("panic: %v", r))
		}
	}()
	var err error
	switch {
	case s == nil:
		t.Seek(sym, b, now)
	case s.State == "arm":
		t.WatchArm(s, b, now)
	case s.State == "entry":
		err = t.WatchEntry(s, b, now)
	case s.State == "open":
		t.WatchOpen(s, b, now)
	}
	if err != nil {
		t.tickError(sym, err)
	}
}

// ------------------------------------------------------------------ market, shadow plans

// Keep are the symbols the feed must keep watching: the bot's trades and the shadow plans still being followed.
func (t *Trader) Keep() map[string]bool {
	out := map[string]bool{}
	for _, k := range t.slots.Keys() {
		out[k] = true
	}
	for _, sh := range t.shadows.Values() {
		out[sh.Sym] = true
	}
	return out
}

// Market is the median change of the other watched coins over mkt_secs (nil with fewer than 5 of them): < 0 =
// the market is falling. Refreshed every 10 s.
func (t *Trader) Market(sym string, now float64) *float64 {
	if now-t.mktAt > 10 {
		ch := map[string]float64{}
		for _, s := range t.books.Keys() {
			b := t.Book(s)
			mid, okM := b.Mid()
			old, okO := b.BarAgo(now, setup.P.MktSecs)
			if b.Synced && okM && mid != 0 && okO && old != 0 {
				ch[s] = mid/old - 1
			}
		}
		t.mktAt, t.mktCh = now, ch
	}
	var rs []float64
	for s, r := range t.mktCh {
		if s != sym {
			rs = append(rs, r)
		}
	}
	if len(rs) < 5 {
		return nil
	}
	sortFloats(rs)
	m := len(rs) / 2
	var v float64
	if len(rs)%2 == 1 {
		v = rs[m]
	} else {
		v = (rs[m-1] + rs[m]) / 2
	}
	return &v
}

func sortFloats(x []float64) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
		}
	}
}

// AddShadow starts following the plain entry of this plan. Skipped plans (final): one per density and gate set
// in 10 min.
func (t *Trader) AddShadow(sym string, plan *setup.Plan, now float64, act string, final bool) {
	if final {
		key := shadowKey{sym, plan.DKey, plan.Skip}
		if now-t.ShadowSeen[key] < ShadowEvery || t.shadows.Len() >= ShadowMax {
			return
		}
		t.ShadowSeen[key] = now
	}
	plan.PID = fmt.Sprintf("%s-%d", sym, int64(now*1000))
	t.shadows.Set(plan.PID, shadow.New(sym, plan, now, act, final))
}

// act is what the bot finally did with a plan it traded (cancel:<why> or filled).
func (t *Trader) act(pid, act string) {
	if sh := t.ShadowByPID(pid); sh != nil {
		sh.Act, sh.Final = act, true
	}
}

// StepShadows advances every shadow plan and journals the finished ones.
func (t *Trader) StepShadows(now float64) {
	t.prune(now)
	for _, pid := range t.shadows.Keys() {
		sh := t.ShadowByPID(pid)
		if sh == nil {
			continue
		}
		t.stepShadow(pid, sh, now)
	}
}

func (t *Trader) stepShadow(pid string, sh *shadow.Shadow, now float64) {
	defer func() {
		if r := recover(); r != nil {
			t.logf("shadow %s: %v", pid, r)
			t.shadows.Delete(pid)
		}
	}()
	b := t.Book(sh.Sym)
	switch {
	case b == nil:
		sh.Lost()
	case b.Synced:
		sh.Step(b, now, FEE)
	case now-sh.LastTS > 300:
		sh.Lost()
	}
	if sh.State == "done" && (sh.Final || now-sh.T0 > 2*setup.P.MaxHold) {
		t.shadows.Delete(pid)
		t.Store.Plan(sh.Record(meta.Version))
	}
}

// ------------------------------------------------------------------ entries

// Seek looks for a plan on a symbol without a trade.
func (t *Trader) Seek(sym string, b *book.Book, now float64) {
	if t.Halt != "" || t.Paused() || t.BlockedSyms[sym] || now < t.Cool[sym] {
		return
	}
	if t.slots.Len() >= RISK.MaxOpen {
		return
	}
	why := t.Block(now)
	if why != t.LastBlock {
		t.LastBlock = why
		if why != "" {
			t.Store.Event("warn", "no new entries: "+why, true)
		}
	}
	if why != "" {
		if strings.Contains(why, "halted") {
			t.Halt = why
		}
		return
	}
	skip := map[book.DKey]bool{}
	for k, n := range t.Attempts {
		if k.Sym == sym && n >= RISK.AttemptsPerDensity {
			skip[k.DKey] = true
		}
	}
	for k, until := range t.Failed {
		if k.Sym == sym && until > now {
			skip[k.DKey] = true
		}
	}
	plan, ghost := setup.Scan(b, now, skip, t.Market(sym, now))
	if ghost != nil { // stopped by a soft gate: journal what it would do
		t.AddShadow(sym, ghost, now, "skip:"+ghost.Skip, true)
	}
	if plan == nil {
		return
	}
	t.stat("plans")
	qty := t.Size(sym, plan.Entry, plan.Stop)
	if qty <= 0 {
		t.Cool[sym] = now + 600
		return
	}
	if k := setup.P.StopDepthK; k > 0 {
		depth := setup.StopDepth(b, plan)
		plan.Features.DepthX = setup.F64(pyfmt.Round(depth/qty, 1))
		if depth < k*qty { // a stop here would slide through an empty book
			t.stat("thin_stop")
			plan.Skip = "thin_stop"
			t.AddShadow(sym, plan, now, "skip:thin_stop", true)
			t.Cool[sym] = now + 30
			return
		}
	}
	s := &Slot{Sym: sym, Plan: plan, Qty: qty, State: "entry", PlacedAt: now}
	if setup.P.ConfirmSecs > 0 {
		s.State = "arm" // no order yet: wait for the density to be tested
	}
	t.slots.Set(sym, s)
	t.Attempts[attemptKey{sym, plan.DKey}]++
	t.AddShadow(sym, plan, now, "open", false)
	feat, _ := jsonString(plan.Features)
	t.logf("PLAN %s %s entry %s stop %s tp %s qty %s %s", sym, plan.Side, pyfmt.G(plan.Entry), pyfmt.G(plan.Stop),
		pyfmt.G(plan.TP), pyfmt.G(qty), feat)
	if s.State == "arm" {
		t.stat("armed")
	} else {
		t.place(s, now)
	}
}

func (t *Trader) place(s *Slot, now float64) {
	if err := t.Broker.PlaceEntry(s); err != nil {
		t.stat("rejected")
		t.logf("%s entry rejected: %v", s.Sym, err)
		t.EntryOver(s, "rejected", now)
		return
	}
	t.stat("placed")
}

// EntryOver ends an armed or working entry without a fill: free the slot, journal the reason, set the cooldowns.
func (t *Trader) EntryOver(s *Slot, why string, now float64) {
	t.slots.Delete(s.Sym)
	t.stat("cancelled")
	t.stat("cancel_" + why)
	t.act(s.Plan.PID, "cancel:"+why)
	t.logf("CANCEL %s %s entry %s: %s", s.Sym, s.Plan.Side, pyfmt.G(s.Plan.Entry), why)
	if setup.IsWallFailed(why) { // the density let us down: not that one again
		t.Failed[attemptKey{s.Sym, s.Plan.DKey}] = now + RISK.WallCool
		t.Cool[s.Sym] = now + 5
	} else if why == "rejected" {
		t.Cool[s.Sym] = now + 30
	} else {
		t.Cool[s.Sym] = now + RISK.SymCooldown
	}
}

// WatchArm is the confirmed entry: wait until price reaches the entry level, then confirm_secs more; place the
// order only if the density is still intact.
func (t *Trader) WatchArm(s *Slot, b *book.Book, now float64) {
	var why string
	switch {
	case t.Halt != "":
		why = "halt"
	case t.Paused():
		why = "paused"
	default:
		why = setup.CancelReason(b, s.Plan, s.PlacedAt, now)
	}
	if why != "" {
		t.EntryOver(s, why, now)
		return
	}
	if s.TouchAt == nil {
		if setup.Touched(b, s.Plan) {
			v := now
			s.TouchAt = &v
		}
		return
	}
	if now-*s.TouchAt < setup.P.ConfirmSecs {
		return
	}
	why, tick, ok := setup.Confirm(b, s.Plan)
	if why != "" {
		t.EntryOver(s, why, now)
		return
	}
	if !ok {
		return // no room for a post-only order yet
	}
	s.ArmedS = pyfmt.Round(now-s.PlacedAt, 1)
	s.Plan.Features.ArmedS = setup.F64(s.ArmedS)
	s.Plan.Entry = b.P(tick)
	s.State, s.PlacedAt = "entry", now
	t.place(s, now)
}

// WatchEntry cancels a working entry when its density failed. A broker error is returned.
func (t *Trader) WatchEntry(s *Slot, b *book.Book, now float64) error {
	if s.CancelSent {
		return nil
	}
	why := "halt"
	if t.Halt == "" {
		why = setup.CancelReason(b, s.Plan, s.PlacedAt, now)
	}
	if why != "" {
		s.CancelSent = true
		s.CancelReason = why
		return t.Broker.CancelEntry(s)
	}
	return nil
}

// WatchOpen manages an open position: the runner's lock, the break-even, the time exit.
func (t *Trader) WatchOpen(s *Slot, b *book.Book, now float64) {
	if s.CloseSent || s.Manage == nil {
		return
	}
	done, names := map[string]bool{}, map[string]bool{}
	for _, p := range s.Parts {
		names[p.Name] = true
		if p.Done {
			done[p.Name] = true
		}
	}
	if done["tp2"] && names["run"] && !s.LockDone {
		s.LockDone = true // the runner keeps the profit of tp1
		px := 0.0
		for _, p := range s.Parts {
			if p.Name == "tp1" {
				px = p.Px
				break
			}
		}
		if px == 0 {
			px = setup.BEPrice(s.Plan, s.EntryPx, b.TickSize)
		}
		s.Manage.BeDone = true
		t.MoveStop(s, px, "stop_lock")
		return
	}
	why := setup.ExitReason(b, s.Plan, s.EntryPx, s.Manage, now)
	switch {
	case why == "be" || (done["tp1"] && setup.P.BeOnTP1 && !s.Manage.BeDone):
		s.Manage.BeDone = true
		t.MoveStop(s, setup.BEPrice(s.Plan, s.EntryPx, b.TickSize), "stop_be")
	case why != "":
		t.Close(s, why)
	}
}

// codeErr is the error interface of an exchange error with a numeric code.
type codeErr interface{ Code() int }

// MoveStop moves the stop; a -2021 error (price already past the new stop) closes the position instead.
func (t *Trader) MoveStop(s *Slot, px float64, why string) {
	if err := t.Broker.MoveStop(s, px); err != nil {
		var ce codeErr
		if errors.As(err, &ce) && ce.Code() == -2021 {
			t.Close(s, why)
			return
		}
		t.Store.Event("error", fmt.Sprintf("%s: move stop: %v", s.Sym, err), false)
		return
	}
	s.Stop = px
	s.StopWhy = why
	t.logf("%s stop -> %s (%s)", s.Sym, pyfmt.G(px), why)
}

// OnPart is called when a partial take filled (the broker reports each part once).
func (t *Trader) OnPart(sym, name string, qty, px float64) {
	s := t.Slot(sym)
	if s == nil {
		return
	}
	for _, p := range s.Parts {
		if p.Name == name && !p.Done {
			p.Done, p.FillPx = true, px
			sgn := float64(s.Plan.Sgn())
			r := math.Abs(s.EntryPx - s.Plan.Stop)
			if r == 0 {
				r = s.Plan.Risk()
			}
			t.Store.Event("info", fmt.Sprintf("%s %s: %s %s @ %s (%sR)", s.Plan.Side, sym, name, pyfmt.G(qty),
				pyfmt.G(px), pyfmt.FS(sgn*(px-s.EntryPx)/r, 2)), t.Notify == "all")
		}
	}
}

// Close closes a position at market; a failure is reported and retried on the next tick.
func (t *Trader) Close(s *Slot, why string) {
	if s.CloseSent {
		return
	}
	s.CloseSent = true
	s.State = "closing"
	s.ExitReason = why
	if err := t.Broker.Close(s, why); err != nil {
		t.logf("close %s failed: %v", s.Sym, err)
		t.Store.Event("error", fmt.Sprintf("%s: close failed: %v", s.Sym, err), false)
		s.CloseSent = false // retry on the next tick
		s.State = "open"
	}
}

// Flatten cancels every working entry and closes every open position of the bot. It returns the slots touched.
func (t *Trader) Flatten() int {
	n := 0
	for _, s := range t.slots.Values() {
		n++
		switch {
		case s.State == "arm":
			t.EntryOver(s, "flatten", t.Now())
		case s.State == "entry" && !s.CancelSent:
			s.CancelSent = true
			s.CancelReason = "flatten"
			if err := t.Broker.CancelEntry(s); err != nil {
				t.Store.Event("error", fmt.Sprintf("%s: cancel failed: %v", s.Sym, err), false)
			}
		case s.State == "open":
			t.Close(s, "flatten")
		}
	}
	return n
}

// ------------------------------------------------------------------ broker callbacks

// OnEntryResult is called by the broker when the entry ended: filled > 0 = (partly) filled at avgPx, else
// cancelled / expired.
func (t *Trader) OnEntryResult(sym string, filled, avgPx, fee float64) {
	s := t.Slot(sym)
	if s == nil || s.State != "entry" {
		return
	}
	if filled <= 0 {
		why := s.CancelReason
		if why == "" {
			why = "expired"
		}
		t.EntryOver(s, why, t.Now())
		return
	}
	t.act(s.Plan.PID, "filled")
	s.FilledQty, s.EntryPx, s.EntryFee = filled, avgPx, fee
	s.State, s.OpenedAt, s.Stop = "open", t.Now(), s.Plan.Stop
	s.Manage = &setup.Manage{OpenedAt: s.OpenedAt}
	s.Parts = t.Split(s)
	t.stat("filled")
	var ps []string
	for _, p := range s.Parts {
		ps = append(ps, fmt.Sprintf("%s %s @ %s", p.Name, pyfmt.G(p.Qty), pyfmt.G(p.Px)))
	}
	t.Store.Event("info", fmt.Sprintf("opened %s %s %s @ %s, stop %s, take %s (density %s USDT); parts %s",
		s.Plan.Side, sym, pyfmt.G(filled), pyfmt.G(avgPx), pyfmt.G(s.Plan.Stop), pyfmt.G(s.Plan.TP),
		pyfmt.Comma(s.Plan.Features.DensityUSD), strings.Join(ps, ", ")), t.Notify == "all")
	t.Sched.Go(func() { t.protect(s) })
}

func (t *Trader) protect(s *Slot) {
	if err := t.Broker.Protect(s); err != nil {
		t.logf("PROTECT FAILED %s: %v -> closing at market", s.Sym, err)
		t.Store.Event("error", fmt.Sprintf("%s: stop not placed (%v) -> closing at market", s.Sym, err), true)
		t.Close(s, "protect_failed")
	}
}

// OnClosed is called by the broker when the position is flat: exit price, net pnl and fees (before the rebate).
func (t *Trader) OnClosed(sym string, exitPx, pnl, fees float64, why string) {
	s := t.Slot(sym)
	if s == nil {
		return
	}
	t.slots.Delete(sym)
	now := t.Now()
	sgn := float64(s.Plan.Sgn())
	rUSD := math.Abs(s.EntryPx-s.Plan.Stop) * s.FilledQty
	reason := why
	if reason == "" {
		reason = s.ExitReason
	}
	rebate := fees * RISK.FeeRebate // journal and limits count the commission net of it
	fees -= rebate
	pnl += rebate
	// slippage of a stop-out (> 0 = filled beyond the stop); with a part taken the exit price is an average
	var slip *float64
	anyDone := false
	for _, p := range s.Parts {
		anyDone = anyDone || p.Done
	}
	if (reason == "stop" || reason == "stop_be" || reason == "stop_lock") && s.Stop != 0 && !anyDone {
		v := pyfmt.Round(sgn*(s.Stop-exitPx)/s.Stop, 5)
		slip = &v
	}
	t.guardOK = false
	tr := state.Trade{Ver: meta.Version, PID: s.Plan.PID, TsOpen: pyfmt.Round(s.OpenedAt, 3),
		TsClose: pyfmt.Round(now, 3), Sym: sym, Side: s.Plan.Side, Entry: s.EntryPx, Exit: exitPx, Qty: s.FilledQty,
		Notional: pyfmt.Round(s.EntryPx*s.FilledQty, 4), Stop: s.Plan.Stop, TP: s.Plan.TP, Reason: reason,
		Gross: pyfmt.Round(sgn*(exitPx-s.EntryPx)*s.FilledQty, 6), Fees: pyfmt.Round(fees, 6),
		Rebate: pyfmt.Round(rebate, 6), PNL: pyfmt.Round(pnl, 6), Slip: slip,
		HoldS: pyfmt.Round(now-s.OpenedAt, 1), Features: s.Plan.Features, Parts: []state.TradePart{}}
	if rUSD != 0 {
		v := pyfmt.Round(pnl/rUSD, 3)
		tr.R = &v
	}
	for _, p := range s.Parts {
		tr.Parts = append(tr.Parts, state.TradePart{Name: p.Name, Px: p.Px, Qty: p.Qty, Done: p.Done, FillPx: p.FillPx})
	}
	t.Store.RecordTrade(tr)
	if t.Store.ConsecLosses() >= RISK.MaxConsec {
		t.Store.BlockConsec(state.UTCDay(now), RISK.MaxConsec)
	}
	t.Cool[sym] = now + RISK.SymCooldown
	p := setup.P
	if slip != nil && *slip > p.SlipMax {
		var hits []float64
		for _, x := range t.Slips[sym] {
			if now-x < p.SlipBanS {
				hits = append(hits, x)
			}
		}
		hits = append(hits, now)
		t.Slips[sym] = hits
		if len(hits) >= p.SlipBanN {
			t.Cool[sym] = now + p.SlipBanS
			t.Store.Event("warn", fmt.Sprintf("%s: %d stops slipped > %s within %s h -> no trades on it for %s h",
				sym, len(hits), pyfmt.Pct(p.SlipMax, 1), pyfmt.G(p.SlipBanS/3600), pyfmt.G(p.SlipBanS/3600)), true)
		}
	}
	icon := "❌"
	if pnl > 0 {
		icon = "✅"
	}
	var hit []string
	for _, p := range s.Parts {
		if p.Done {
			hit = append(hit, p.Name)
		}
	}
	var msg string
	if tr.R != nil {
		msg = fmt.Sprintf("%s %s %s closed (%s) %s -> %s: %s USDT (%sR), held %s s", icon, s.Plan.Side, sym, reason,
			pyfmt.G(s.EntryPx), pyfmt.G(exitPx), pyfmt.FS(pnl, 4), pyfmt.FS(*tr.R, 2), pyfmt.F(tr.HoldS, 0))
		if len(hit) > 0 && len(s.Parts) > 1 {
			msg += ", parts " + strings.Join(hit, " ")
		}
	} else {
		msg = fmt.Sprintf("%s %s closed (%s): %s USDT", icon, sym, reason, pyfmt.FS(pnl, 4))
	}
	t.Store.Event("info", msg, t.Notify != "none")
}

// ------------------------------------------------------------------ views

// SlotView is one trade as the API shows it.
type SlotView struct {
	Symbol   string         `json:"symbol"`
	Side     string         `json:"side"`
	State    string         `json:"state"`
	Entry    float64        `json:"entry"`
	Stop     float64        `json:"stop"`
	TP       float64        `json:"tp"`
	Qty      float64        `json:"qty"`
	N        float64        `json:"n"`
	Mark     *float64       `json:"mark"`
	UPnL     float64        `json:"upnl"`
	AgeS     int64          `json:"age_s"`
	Features setup.Features `json:"features"`
}

// View lists the trades; marks are the fallback prices of symbols whose book is gone.
func (t *Trader) View(marks map[string]float64) []SlotView {
	out := []SlotView{}
	for _, s := range t.slots.Values() {
		var mid *float64
		if b := t.Book(s.Sym); b != nil {
			if m, ok := b.Mid(); ok && m != 0 {
				mid = &m
			}
		}
		if mid == nil {
			if m, ok := marks[s.Sym]; ok {
				mid = &m
			}
		}
		sgn := float64(s.Plan.Sgn())
		upnl := 0.0
		if mid != nil && *mid != 0 && s.State != "arm" && s.State != "entry" {
			upnl = sgn*(*mid-s.EntryPx)*s.FilledQty - s.EntryFee
		}
		entry := s.EntryPx
		if entry == 0 {
			entry = s.Plan.Entry
		}
		qty := s.FilledQty
		if qty == 0 {
			qty = s.Qty
		}
		stop := s.Stop
		if stop == 0 {
			stop = s.Plan.Stop
		}
		since := s.OpenedAt
		if since == 0 {
			since = s.PlacedAt
		}
		out = append(out, SlotView{Symbol: s.Sym, Side: s.Plan.Side, State: s.State, Entry: entry, Stop: stop,
			TP: s.Plan.TP, Qty: qty, N: pyfmt.Round(entry*qty, 2), Mark: mid, UPnL: pyfmt.Round(upnl, 4),
			AgeS: pyfmt.Int(t.Now() - since), Features: s.Plan.Features})
	}
	return out
}
