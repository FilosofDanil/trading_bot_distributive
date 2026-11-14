package live

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"densitybounce/internal/state"
	"densitybounce/internal/trader"
)

// Safety rules (bounce/live.py):
//   - only symbols with NO position and NO open order / algo order the bot did not place (refreshed every 30 s) are
//     traded, never BTCUSDT / ETHUSDT; every client id starts with "dbb" and cancel / flatten only touch those ids;
//   - isolated margin, the entry is a post-only (GTX) limit;
//   - the moment an entry fills: an exchange-side STOP_MARKET closePosition algo order + one reduce-only limit take
//     per part; if the stop cannot be placed the trader closes the position at market;
//   - the bot's open slots are kept in the state file; on start leftovers are cancelled / closed.
//
// Locking. Every method that touches the trader is called with the engine lock held (Mu), as the trader does. A
// network call or a sleep releases the lock and takes it again afterwards (Python's await): the state may have
// changed meanwhile, exactly as in the asyncio loop. Background goroutines take the lock when they start.

var never = map[string]bool{"BTCUSDT": true, "ETHUSDT": true}

type unknownEntry struct {
	side string
	qty  float64
}

// LiveBroker implements trader.Broker with real orders.
type LiveBroker struct {
	Rest *Rest
	// Mu is the engine lock of the runner.
	Mu sync.Locker
	// Sleep is time.Sleep (tests replace it); PollEntryEvery / PollOpenEvery / ForeignEvery are the loop periods.
	Sleep          func(time.Duration)
	PollEntryEvery time.Duration
	PollOpenEvery  time.Duration
	ForeignEvery   time.Duration
	Now            func() float64

	t          *trader.Trader
	foreign    map[string]bool
	configured map[string]bool
	seq        int
	unknown    map[string]unknownEntry // entries whose placement result was lost (network)
	issued     map[string]float64      // client id -> time issued
	stopped    atomic.Bool
}

// NewBroker makes a live broker that works under the engine lock mu.
func NewBroker(rest *Rest, mu sync.Locker) *LiveBroker {
	b := &LiveBroker{Rest: rest, Mu: mu, Sleep: time.Sleep, PollEntryEvery: 400 * time.Millisecond,
		PollOpenEvery: time.Second, ForeignEvery: 30 * time.Second,
		Now:     func() float64 { return float64(time.Now().UnixNano()) / 1e9 },
		foreign: map[string]bool{"BTCUSDT": true, "ETHUSDT": true}, configured: map[string]bool{},
		unknown: map[string]unknownEntry{}, issued: map[string]float64{}}
	return b
}

// Attach is called by trader.New.
func (b *LiveBroker) Attach(t *trader.Trader) { b.t = t }

// Stop ends the background loops (they exit at their next wake-up).
func (b *LiveBroker) Stop() { b.stopped.Store(true) }

// Foreign is the set of symbols the bot never trades (tests).
func (b *LiveBroker) Foreign() map[string]bool { return b.foreign }

func (b *LiveBroker) unlocked(fn func()) {
	b.Mu.Unlock()
	defer b.Mu.Lock()
	fn()
}

func (b *LiveBroker) sleep(d time.Duration) { b.unlocked(func() { b.Sleep(d) }) }

// call is a signed request with the lock released.
func (b *LiveBroker) call(out any, method, path string, q Q) (err error) {
	b.unlocked(func() {
		if out == nil {
			_, err = b.Rest.Call(method, path, true, "", q)
		} else {
			err = b.Rest.JSON(out, method, path, true, "", q)
		}
	})
	return err
}

// goTask runs fn in its own goroutine under the engine lock.
func (b *LiveBroker) goTask(fn func()) {
	go func() {
		b.Mu.Lock()
		defer b.Mu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR live task: %v", r)
			}
		}()
		fn()
	}()
}

// ------------------------------------------------------------------ ids / state

func (b *LiveBroker) cid(kind string) string {
	b.seq++
	now := b.Now()
	ms := int64(now * 1000)
	c := fmt.Sprintf("%s%s%d%03d", PREFIX, kind, ms%100_000_000_000, b.seq%1000) // <= 36 chars
	b.issued[c] = now
	return c
}

func (b *LiveBroker) saveState() {
	st := map[string]state.SlotRec{}
	for _, s := range b.t.Slots() {
		qty := s.FilledQty
		if qty == 0 {
			qty = s.Qty
		}
		ids := []string{}
		for _, p := range s.Parts {
			ids = append(ids, p.ID)
		}
		var stop *string
		if s.StopID != "" {
			v := s.StopID
			stop = &v
		}
		st[s.Sym] = state.SlotRec{Side: s.Plan.Side, State: s.State, Qty: qty, Entry: s.EntryPx, OID: s.OID,
			StopID: stop, TPIDs: ids, PlacedAt: s.PlacedAt}
	}
	b.t.Store.SetSlots(st)
}

// ------------------------------------------------------------------ lifecycle

// Start cancels / closes the leftovers of an earlier run, reads the foreign symbols and starts the 30 s loop.
// Called with the engine lock held.
func (b *LiveBroker) Start() error {
	if err := b.reconcile(); err != nil {
		return err
	}
	if err := b.refreshForeign(); err != nil {
		return err
	}
	b.goTask(b.foreignLoop)
	return nil
}

type posRisk struct {
	Symbol      string `json:"symbol"`
	PositionAmt num    `json:"positionAmt"`
}

type openOrder struct {
	Symbol        string `json:"symbol"`
	ClientOrderID string `json:"clientOrderId"`
	ClientAlgoID  string `json:"clientAlgoId"`
}

// RefreshForeign recomputes the symbols with a position or order the bot did not place.
func (b *LiveBroker) RefreshForeign() error { return b.refreshForeign() }

func (b *LiveBroker) refreshForeign() error {
	var pos []posRisk
	if err := b.call(&pos, "GET", "/fapi/v2/positionRisk", nil); err != nil {
		return err
	}
	f := map[string]bool{}
	for k := range never {
		f[k] = true
	}
	for _, p := range pos {
		if p.PositionAmt != 0 && b.t.Slot(p.Symbol) == nil {
			f[p.Symbol] = true
		}
	}
	var oo, ao []openOrder
	if err := b.call(&oo, "GET", "/fapi/v1/openOrders", nil); err != nil {
		return err
	}
	for _, o := range oo {
		if !strings.HasPrefix(o.ClientOrderID, PREFIX) {
			f[o.Symbol] = true
		}
	}
	if err := b.call(&ao, "GET", "/fapi/v1/openAlgoOrders", nil); err != nil {
		return err
	}
	for _, o := range ao {
		if !strings.HasPrefix(o.ClientAlgoID, PREFIX) {
			f[o.Symbol] = true
		}
	}
	if !sameSet(f, b.foreign) {
		log.Printf("INFO foreign symbols (never traded): %s", strings.Join(sortedKeys(f), " "))
	}
	b.foreign = f
	b.t.BlockedSyms = f
	return nil
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b *LiveBroker) foreignLoop() {
	for !b.stopped.Load() {
		b.sleep(b.ForeignEvery)
		if b.stopped.Load() {
			return
		}
		err := b.sweep()
		if err == nil {
			err = b.refreshForeign()
		}
		if err != nil {
			log.Printf("WARNING foreign refresh / sweep: %v", err)
		}
	}
}

// Sweep cancels bot orders that belong to no live slot and closes positions left by entries of unknown state.
func (b *LiveBroker) Sweep() error { return b.sweep() }

func (b *LiveBroker) sweep() error {
	now := b.Now()
	live := map[string]bool{}
	for c, t := range b.issued {
		if now-t < 60 { // just issued: maybe not recorded yet
			live[c] = true
		}
	}
	for c, t := range b.issued {
		if now-t >= 3600 {
			delete(b.issued, c)
		}
	}
	for _, s := range b.t.Slots() {
		live[s.OID] = true
		live[s.StopID] = true
		for _, p := range s.Parts {
			live[p.ID] = true
		}
	}
	var oo, ao []openOrder
	if err := b.call(&oo, "GET", "/fapi/v1/openOrders", nil); err != nil {
		return err
	}
	for _, o := range oo {
		if strings.HasPrefix(o.ClientOrderID, PREFIX) && !live[o.ClientOrderID] {
			log.Printf("WARNING sweep: cancelling orphan order %s %s", o.Symbol, o.ClientOrderID)
			if err := b.cancelOrder(o.Symbol, o.ClientOrderID); err != nil {
				return err
			}
		}
	}
	if err := b.call(&ao, "GET", "/fapi/v1/openAlgoOrders", nil); err != nil {
		return err
	}
	for _, o := range ao {
		if strings.HasPrefix(o.ClientAlgoID, PREFIX) && !live[o.ClientAlgoID] {
			log.Printf("WARNING sweep: cancelling orphan algo order %s %s", o.Symbol, o.ClientAlgoID)
			if err := b.cancelAlgo(o.ClientAlgoID); err != nil {
				return err
			}
		}
	}
	for _, sym := range sortedKeys(b.unknown) {
		r := b.unknown[sym]
		if b.t.Slot(sym) == nil {
			var err error
			b.unlocked(func() { err = CloseOwnPosition(b.Rest, sym, r.side, r.qty) })
			if err != nil {
				return err
			}
		}
		delete(b.unknown, sym)
	}
	return nil
}

func (b *LiveBroker) reconcile() error {
	var n int
	var err error
	b.unlocked(func() { n, err = CancelOwnOrders(b.Rest) })
	if err != nil {
		return err
	}
	st := b.t.Store.Slots()
	syms := sortedKeys(st) // Python kept the file's order; a Go map has none
	for _, sym := range syms {
		r := st[sym]
		if r.State == "open" || r.State == "closing" || r.State == "entry" {
			b.unlocked(func() { err = CloseOwnPosition(b.Rest, sym, r.Side, r.Qty) })
			if err != nil {
				return err
			}
		}
	}
	if n > 0 || len(st) > 0 {
		who := strings.Join(syms, ", ")
		if who == "" {
			who = "no symbol"
		}
		b.t.Store.Event("warn", fmt.Sprintf("restart: cancelled %d leftover bot order(s), closed leftovers on %s", n, who), true)
	}
	b.t.Store.SetSlots(map[string]state.SlotRec{})
	return nil
}

func (b *LiveBroker) ensureSymbol(sym string) error {
	if b.configured[sym] {
		return nil
	}
	err := b.call(nil, "POST", "/fapi/v1/marginType", Q{"symbol": sym, "marginType": "ISOLATED"})
	if err != nil {
		if c, ok := code(err); !ok || c != -4046 { // -4046 = already isolated
			return err
		}
	}
	if err := b.call(nil, "POST", "/fapi/v1/leverage", Q{"symbol": sym, "leverage": strconv.Itoa(trader.RISK.Leverage)}); err != nil {
		return err
	}
	b.configured[sym] = true
	return nil
}

// ------------------------------------------------------------------ entry

type orderResp struct {
	Status      string `json:"status"`
	ExecutedQty num    `json:"executedQty"`
	AvgPrice    num    `json:"avgPrice"`
}

// PlaceEntry places the post-only limit and starts watching it.
func (b *LiveBroker) PlaceEntry(s *trader.Slot) error {
	if b.foreign[s.Sym] || never[s.Sym] {
		return fmt.Errorf("%s has a foreign position/order", s.Sym)
	}
	i := b.t.Info[s.Sym]
	if err := b.ensureSymbol(s.Sym); err != nil {
		return err
	}
	s.OID = b.cid("e")
	side := "SELL"
	if s.Plan.Side == "long" {
		side = "BUY"
	}
	b.saveState()
	var r orderResp
	err := b.call(&r, "POST", "/fapi/v1/order", Q{"symbol": s.Sym, "side": side, "type": "LIMIT", "timeInForce": "GTX",
		"price": Fmt(s.Plan.Entry, i.Tick), "quantity": Fmt(s.Qty, i.Step), "newClientOrderId": s.OID,
		"newOrderRespType": "RESULT"})
	if err != nil {
		if _, ok := code(err); ok {
			return err // a definite rejection: nothing was placed
		}
		log.Printf("WARNING %s entry request failed (%v); looking the order up", s.Sym, err) // the order may exist
		found := false
		for k := 0; k < 5; k++ {
			b.sleep(time.Second)
			err2 := b.call(&r, "GET", "/fapi/v1/order", Q{"symbol": s.Sym, "origClientOrderId": s.OID})
			if err2 == nil {
				found = true
				break
			}
			if c, ok := code(err2); ok && c == -2013 { // order does not exist: it was never placed
				return errors.New("entry not placed")
			}
		}
		if !found {
			b.unknown[s.Sym] = unknownEntry{side: s.Plan.Side, qty: s.Qty}
			return errors.New("entry state unknown; the leftover sweep will cancel / close it")
		}
	}
	if r.Status == "EXPIRED" && r.ExecutedQty == 0 {
		return errors.New("post-only expired (would cross)")
	}
	b.goTask(func() { b.pollEntry(s) })
	return nil
}

func (b *LiveBroker) pollEntry(s *trader.Slot) {
	cancelAsked := false
	for s.State == "entry" && b.t.Slot(s.Sym) == s && !b.stopped.Load() {
		b.sleep(b.PollEntryEvery)
		if b.stopped.Load() {
			return
		}
		var o orderResp
		if err := b.call(&o, "GET", "/fapi/v1/order", Q{"symbol": s.Sym, "origClientOrderId": s.OID}); err != nil {
			log.Printf("WARNING poll entry %s: %v", s.Sym, err)
			continue
		}
		filled := float64(o.ExecutedQty)
		if o.Status == "PARTIALLY_FILLED" && !cancelAsked {
			cancelAsked = true // take what we got, protect it
			if err := b.cancelOrder(s.Sym, s.OID); err != nil {
				log.Printf("WARNING poll entry %s: cancel: %v", s.Sym, err) // Python's task died here; we keep polling
			}
			continue
		}
		switch o.Status {
		case "FILLED", "CANCELED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED":
			avg := float64(o.AvgPrice)
			b.t.OnEntryResult(s.Sym, filled, avg, avg*filled*trader.FEE.Maker)
			b.saveState()
			return
		}
	}
}

func (b *LiveBroker) cancelOrder(sym, cid string) error {
	err := b.call(nil, "DELETE", "/fapi/v1/order", Q{"symbol": sym, "origClientOrderId": cid})
	if c, ok := code(err); ok && (c == -2011 || c == -2013) { // unknown order = already filled / cancelled
		return nil
	}
	return err
}

// CancelEntry asks for the cancel; pollEntry reports the final result.
func (b *LiveBroker) CancelEntry(s *trader.Slot) error { return b.cancelOrder(s.Sym, s.OID) }

// ------------------------------------------------------------------ protection

func (b *LiveBroker) placeStop(s *trader.Slot, price float64) (string, error) {
	i := b.t.Info[s.Sym]
	caid := b.cid("s")
	side := "BUY"
	if s.Plan.Side == "long" {
		side = "SELL"
	}
	var r struct {
		AlgoStatus string `json:"algoStatus"`
	}
	err := b.call(&r, "POST", "/fapi/v1/algoOrder", Q{"algoType": "CONDITIONAL", "symbol": s.Sym, "side": side,
		"type": "STOP_MARKET", "triggerPrice": Fmt(price, i.Tick), "closePosition": "true",
		"workingType": "CONTRACT_PRICE", "priceProtect": "false", "clientAlgoId": caid})
	if err != nil {
		return "", err
	}
	if r.AlgoStatus != "NEW" && r.AlgoStatus != "" {
		return "", fmt.Errorf("stop status %s", r.AlgoStatus)
	}
	return caid, nil
}

func (b *LiveBroker) cancelAlgo(caid string) error {
	if caid == "" {
		return nil
	}
	err := b.call(nil, "DELETE", "/fapi/v1/algoOrder", Q{"clientAlgoId": caid})
	if _, ok := code(err); ok {
		log.Printf("INFO cancel algo %s: %v", caid, err) // an exchange answer is logged; a network error propagates
		return nil
	}
	return err
}

// Protect places the exchange-side stop and one reduce-only take per part. An error makes the trader close the
// position at market.
func (b *LiveBroker) Protect(s *trader.Slot) error {
	// watch the position first: if protection fails, the trader's market close is reported by this poll
	b.goTask(func() { b.pollOpen(s) })
	id, err := b.placeStop(s, s.Plan.Stop)
	if err != nil {
		c, ok := code(err)
		if !ok || c == -2021 { // -2021: would trigger immediately, price is past the stop; a network error: no retry
			return err
		}
		b.sleep(300 * time.Millisecond)
		id, err = b.placeStop(s, s.Plan.Stop) // one retry, then the trader flattens
		if err != nil {
			return err
		}
	}
	s.StopID = id
	b.saveState()
	i := b.t.Info[s.Sym]
	side := "BUY"
	if s.Plan.Side == "long" {
		side = "SELL"
	}
	for _, p := range s.Parts {
		tpID := b.cid("t")
		err := b.call(nil, "POST", "/fapi/v1/order", Q{"symbol": s.Sym, "side": side, "type": "LIMIT", "timeInForce": "GTC",
			"price": Fmt(p.Px, i.Tick), "quantity": Fmt(p.Qty, i.Step), "reduceOnly": "true", "newClientOrderId": tpID})
		if err == nil {
			p.ID = tpID
			continue
		}
		if _, ok := code(err); !ok {
			return err
		}
		log.Printf("WARNING %s take %s not placed (%v); the stop protects the position", s.Sym, p.Name, err)
	}
	b.saveState()
	return nil
}

// checkParts reports the takes that filled; with cancel (the position is flat) it cancels the ones that did not.
func (b *LiveBroker) checkParts(s *trader.Slot, cancel bool) {
	for _, p := range s.Parts {
		if p.Done || p.ID == "" {
			continue
		}
		var o orderResp
		err := b.call(&o, "GET", "/fapi/v1/order", Q{"symbol": s.Sym, "origClientOrderId": p.ID})
		if err == nil {
			if o.Status == "FILLED" {
				px := float64(o.AvgPrice)
				if px == 0 {
					px = p.Px
				}
				b.t.OnPart(s.Sym, p.Name, float64(o.ExecutedQty), px)
			} else if cancel {
				err = b.cancelOrder(s.Sym, p.ID)
			}
		}
		if err != nil {
			log.Printf("WARNING take check %s %s: %v", s.Sym, p.Name, err)
		}
	}
}

// MoveStop replaces the stop. Binance keeps one closePosition stop per side (-4130), so the old one goes first; if
// the new one fails, the old one comes back, and if that fails too the position is closed: it never stays without
// a stop.
func (b *LiveBroker) MoveStop(s *trader.Slot, price float64) error {
	oldPx := s.Stop
	if oldPx == 0 {
		oldPx = s.Plan.Stop
	}
	if err := b.cancelAlgo(s.StopID); err != nil {
		return err
	}
	id, err := b.placeStop(s, price)
	if err == nil {
		s.StopID = id
		b.saveState()
		return nil
	}
	c, _ := code(err)
	if c == -4130 { // the cancel failed: the old stop still stands
		return err
	}
	if c == -2021 {
		s.StopID = ""
	} else if id, err2 := b.placeStop(s, oldPx); err2 != nil {
		log.Printf("WARNING %s stop not restored (%v): closing", s.Sym, err2)
		b.t.Close(s, "stop_lost")
	} else {
		s.StopID = id
	}
	b.saveState()
	return err // -2021: price is past the new stop, the trader closes
}

// Close cancels the takes and closes what is left at market; pollOpen sees the flat position and reports.
func (b *LiveBroker) Close(s *trader.Slot, why string) error {
	i := b.t.Info[s.Sym]
	for _, p := range s.Parts {
		if p.ID != "" && !p.Done {
			if err := b.cancelOrder(s.Sym, p.ID); err != nil {
				return err
			}
		}
	}
	var amt float64
	var err error
	b.unlocked(func() { amt, err = PositionAmt(b.Rest, s.Sym) })
	if err != nil {
		return err
	}
	if amt != 0 {
		side := "BUY"
		if amt > 0 {
			side = "SELL"
		}
		return b.call(nil, "POST", "/fapi/v1/order", Q{"symbol": s.Sym, "side": side, "type": "MARKET",
			"quantity": Fmt(math.Min(math.Abs(amt), s.FilledQty), i.Step), "reduceOnly": "true",
			"newClientOrderId": b.cid("x")})
	}
	return nil
}

func (b *LiveBroker) pollOpen(s *trader.Slot) {
	zeros := 0
	left := s.FilledQty
	step := b.t.Info[s.Sym].Step
	for b.t.Slot(s.Sym) == s && !b.stopped.Load() {
		b.sleep(b.PollOpenEvery)
		if b.stopped.Load() {
			return
		}
		var amt float64
		var err error
		b.unlocked(func() { amt, err = PositionAmt(b.Rest, s.Sym) })
		if err != nil {
			log.Printf("WARNING poll position %s: %v", s.Sym, err)
			continue
		}
		if a := math.Abs(amt); a > 0 && a < left-step/2 { // smaller: a take filled (partly)
			left = a
			b.checkParts(s, false)
		}
		if amt == 0 {
			zeros++
		} else {
			zeros = 0
		}
		if zeros < 2 { // two flat readings in a row: no stale-read race
			continue
		}
		if err := b.cancelAlgo(s.StopID); err != nil {
			log.Printf("WARNING cancel stop %s: %v", s.Sym, err) // Python's task died here; we go on and report
		}
		b.checkParts(s, true)
		reason := s.ExitReason
		if reason == "" {
			all := len(s.Parts) > 0
			for _, p := range s.Parts {
				all = all && p.Done
			}
			if all {
				reason = "take"
			} else {
				reason = trader.StopReason(s)
			}
		}
		px, pnl, fees := b.realized(s)
		b.t.OnClosed(s.Sym, px, pnl, fees, reason)
		b.saveState()
		return
	}
}

type userTrade struct {
	Side            string `json:"side"`
	Qty             num    `json:"qty"`
	Price           num    `json:"price"`
	Commission      num    `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	RealizedPnl     num    `json:"realizedPnl"`
}

// realized is the exit price, net pnl and fees of this trade from the account's own fills.
func (b *LiveBroker) realized(s *trader.Slot) (px, pnl, fees float64) {
	var tr []userTrade
	err := b.call(&tr, "GET", "/fapi/v1/userTrades", Q{"symbol": s.Sym,
		"startTime": strconv.FormatInt(int64((s.PlacedAt-5)*1000), 10), "limit": "100"})
	if err != nil {
		log.Printf("WARNING realized %s: %v (pnl estimated)", s.Sym, err)
		return s.EntryPx, -s.EntryFee, s.EntryFee
	}
	exit := "BUY"
	if s.Plan.Side == "long" {
		exit = "SELL"
	}
	var q, qp, rp float64
	for _, t := range tr {
		if t.Side == exit {
			q += float64(t.Qty)
			qp += float64(t.Qty) * float64(t.Price)
		}
		if t.CommissionAsset == "USDT" {
			fees += float64(t.Commission)
		}
		rp += float64(t.RealizedPnl)
	}
	if q != 0 {
		px = qp / q
	}
	return px, rp - fees, fees
}

// ---------------------------------------------------------------------- helpers shared with reconcile

// PositionAmt is the signed position size of a symbol.
func PositionAmt(r *Rest, sym string) (float64, error) {
	var pos []posRisk
	if err := r.JSON(&pos, "GET", "/fapi/v2/positionRisk", true, "", Q{"symbol": sym}); err != nil {
		return 0, err
	}
	sum := 0.0
	for _, p := range pos {
		sum += float64(p.PositionAmt)
	}
	return sum, nil
}

// CancelOwnOrders cancels every open order and algo order whose client id starts with "dbb".
func CancelOwnOrders(r *Rest) (int, error) {
	n := 0
	var oo, ao []openOrder
	if err := r.JSON(&oo, "GET", "/fapi/v1/openOrders", true, "", nil); err != nil {
		return n, err
	}
	for _, o := range oo {
		if strings.HasPrefix(o.ClientOrderID, PREFIX) {
			if _, err := r.Call("DELETE", "/fapi/v1/order", true, "", Q{"symbol": o.Symbol, "origClientOrderId": o.ClientOrderID}); err != nil {
				if _, ok := code(err); !ok {
					return n, err
				}
				log.Printf("INFO cancel %s: %v", o.ClientOrderID, err)
				continue
			}
			n++
		}
	}
	if err := r.JSON(&ao, "GET", "/fapi/v1/openAlgoOrders", true, "", nil); err != nil {
		return n, err
	}
	for _, o := range ao {
		if strings.HasPrefix(o.ClientAlgoID, PREFIX) {
			if _, err := r.Call("DELETE", "/fapi/v1/algoOrder", true, "", Q{"clientAlgoId": o.ClientAlgoID}); err != nil {
				if _, ok := code(err); !ok {
					return n, err
				}
				log.Printf("INFO cancel algo %s: %v", o.ClientAlgoID, err)
				continue
			}
			n++
		}
	}
	return n, nil
}

// CloseOwnPosition closes at most qty of the bot's recorded position on sym (reduce-only, never flips, never more).
func CloseOwnPosition(r *Rest, sym, side string, qty float64) error {
	if never[sym] {
		return nil
	}
	amt, err := PositionAmt(r, sym)
	if err != nil {
		return err
	}
	if amt == 0 || (amt > 0) != (side == "long") {
		return nil
	}
	var info struct {
		Symbols []struct {
			Symbol  string `json:"symbol"`
			Filters []struct {
				FilterType string `json:"filterType"`
				StepSize   string `json:"stepSize"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := r.JSON(&info, "GET", "/fapi/v1/exchangeInfo", false, "", nil); err != nil {
		return err
	}
	step := 0.0
	for _, s := range info.Symbols {
		if s.Symbol != sym {
			continue
		}
		for _, f := range s.Filters {
			if f.FilterType == "LOT_SIZE" {
				step, _ = strconv.ParseFloat(f.StepSize, 64)
			}
		}
		break
	}
	if step == 0 {
		return fmt.Errorf("no LOT_SIZE step for %s", sym)
	}
	q := math.Min(math.Abs(amt), qty)
	o := "BUY"
	if amt > 0 {
		o = "SELL"
	}
	if _, err := r.Call("POST", "/fapi/v1/order", true, "", Q{"symbol": sym, "side": o, "type": "MARKET",
		"quantity": Fmt(q, step), "reduceOnly": "true", "newClientOrderId": fmt.Sprintf("%sx%d", PREFIX, time.Now().Unix())}); err != nil {
		return err
	}
	log.Printf("WARNING closed leftover bot position %s %s %g", sym, side, q)
	return nil
}
