// Package live is the live execution of the carry plan and the intraday liquidation guard (MODE=live), carry/live.py.
//
// The decision is the same strategy.Decide; here its plan is carried out with real orders on the owner's
// Binance account, through the Exchange interface (internal/exchange implements it for Binance; tests use a
// fake). A pair is a spot long and an isolated 1x perp short of the same quantity q.
//
// Opening a pair (in this order, each step undone if a later one fails):
//  1. move the perp margin (buffer x notional = 2N) from spot to the futures wallet
//  2. isolated margin, leverage 1; market SELL q on the perp
//  3. market BUY on spot, q grossed up for the taker fee charged in the coin
//  4. add the rest of the margin to the isolated position (total 2N: liquidation about +195 %)
//  5. exchange-side STOP_MARKET that closes the short at ref x (1 + stop_rise): works while the bot is down
//
// Liquidation protection, checked every WatchEveryS seconds (WatchOnce), for each held pair:
//
//	L1 liquidation price within topup_dist above the mark -> add isolated margin (futures wallet, then spot)
//	L2 perp up close_rise since ref, or liquidation within close_dist -> close the pair, 7-day cooldown
//	L3 the exchange stop is missing -> place it again (or close the pair if the price is already past it)
//	L4 a leg is gone (stop fired, liquidated, sold by hand) -> close the other leg, 7-day cooldown
//
// Every action is an event with notify=true, so control forwards it to Telegram.
//
// Without the Universal Transfer permission (transfers=false) nothing is moved between the wallets: the spot leg
// is paid from spot USDT, the perp margin from the futures wallet, a closed pair's margin stays on futures for the
// next one, and L1 top-ups come from the futures wallet only. The owner keeps about 1/3 of the capital on spot and
// 2/3 on futures.
package live

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fundingcarry/internal/carry"
	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
)

const (
	SpotFee     = 0.001  // spot taker, charged in the bought coin on a BUY (unless paid in BNB)
	PerpFee     = 0.0005 // perp taker, in USDT
	WatchEveryS = 30
	CooldownMS  = 7 * market.DayMS
)

func envFloat(name string, def float64) float64 {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

// Protect holds the liquidation-guard levels.
type Protect struct {
	TopupDist   float64 // L1: add margin when the liquidation price is less than +60 % above the mark
	CloseDist   float64 // L2: close when it is less than +25 % above the mark
	CloseRise   float64 // L2: close when the perp is +90 % above ref (entry / last re-hedge)
	StopRise    float64 // L3: exchange stop at +120 % above ref
	LegTol      float64 // L4: a leg below 95 % of its size counts as gone
	TopupEveryS float64 // at most one top-up per pair per 10 min
}

// DefaultProtect are the defaults.
func DefaultProtect() Protect {
	return Protect{TopupDist: 0.60, CloseDist: 0.25, CloseRise: 0.90, StopRise: 1.20, LegTol: 0.05, TopupEveryS: 600}
}

// ProtectFromEnv reads LIQ_TOPUP_DIST / LIQ_CLOSE_DIST / LIQ_CLOSE_RISE / LIQ_STOP_RISE.
func ProtectFromEnv() Protect {
	p := DefaultProtect()
	p.TopupDist = envFloat("LIQ_TOPUP_DIST", p.TopupDist)
	p.CloseDist = envFloat("LIQ_CLOSE_DIST", p.CloseDist)
	p.CloseRise = envFloat("LIQ_CLOSE_RISE", p.CloseRise)
	p.StopRise = envFloat("LIQ_STOP_RISE", p.StopRise)
	return p
}

// Fill is an executed order.
type Fill struct {
	Qty     float64 // base quantity: for a spot BUY the quantity received after the fee
	Price   float64 // average price
	FeeUSDT float64 // fee in USDT terms
}

// Perp is an open futures position.
type Perp struct {
	Amt    float64 // signed position amount (short < 0)
	Entry  float64
	Mark   float64
	Liq    float64 // liquidation price (0 = none)
	Margin float64 // isolated margin
}

// ExchangeError is a failed exchange call.
type ExchangeError struct{ Msg string }

func (e *ExchangeError) Error() string  { return e.Msg }
func (e *ExchangeError) PyName() string { return "ExchangeError" }

// Errorf makes an ExchangeError.
func Errorf(format string, a ...any) error { return &ExchangeError{fmt.Sprintf(format, a...)} }

// Exchange is what the executor needs from Binance.
type Exchange interface {
	Prices(sym string) (perpMark, spotLast float64, err error)
	Filters(sym string) (spotStep, spotMin, perpStep, perpMin float64, err error)
	SpotFree(asset string) (float64, error)
	FuturesAvailable() (float64, error) // USDT available in the futures wallet
	Transfer(amount float64, toFutures bool) error
	PrepareShort(sym string) error // isolated + leverage 1
	PerpOrder(sym, side string, qty float64, reduce bool) (Fill, error)
	SpotOrder(sym, side string, qty float64) (Fill, error)
	AddMargin(sym string, amount float64) error
	Positions() (map[string]Perp, error)              // non-zero positions
	Stops(syms []string) (map[string][]string, error) // this bot's open exchange stops
	PlaceStop(sym string, trigger float64) (string, error)
	CancelStops(sym string) error
	Funding(sym string, sinceMS int64) (float64, error) // funding received since
}

func FloorStep(q, step float64) float64 {
	if step <= 0 {
		return q
	}
	return math.Floor(q/step+1e-9) * step
}

func CeilStep(q, step float64) float64 {
	if step <= 0 {
		return q
	}
	return math.Ceil(q/step-1e-9) * step
}

func BaseAsset(sym string) string {
	if strings.HasSuffix(sym, "USDT") {
		return sym[:len(sym)-4]
	}
	return sym
}

func minF(a, b float64) float64 { // Python min(a, b)
	if b < a {
		return b
	}
	return a
}

func maxF(a, b float64) float64 { // Python max(a, b)
	if b > a {
		return b
	}
	return a
}

func nowS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Live is the executor and the watchdog.
type Live struct {
	ex        Exchange
	store     *state.Store
	P         Protect
	transfers bool       // false: the key cannot move USDT between spot and futures
	mu        sync.Mutex // serialises the daily cycle, the watchdog and flatten
	topupAt   map[string]float64
}

// New makes the executor.
func New(ex Exchange, store *state.Store, p Protect, transfers bool) *Live {
	return &Live{ex: ex, store: store, P: p, transfers: transfers, topupAt: map[string]float64{}}
}

// ------------------------------------------------------------------ helpers

func (l *Live) event(level, msg string) { l.store.Event(level, "live: "+msg, true) }

func (l *Live) try(what string, fn func() error) bool {
	if err := fn(); err != nil { // every failure is reported
		l.event("error", fmt.Sprintf("%s failed: %s", what, pyx.Describe(err)))
		return false
	}
	return true
}

func (l *Live) held() state.HeldMap { return l.store.Snapshot().Held }

func (l *Live) savePair(sym string, h *state.Held, cool bool) {
	_ = l.store.Update(func(d *state.Data) {
		if h == nil {
			d.Held.Delete(sym)
			if cool {
				d.Cool[sym] = (carry.NowMS()/market.DayMS)*market.DayMS + CooldownMS
			}
		} else {
			c := *h
			d.Held.Set(sym, &c)
		}
	})
}

// ------------------------------------------------------------------ pair operations

// OpenPair opens a pair; (nil, nil) when it was refused or undone (the reason is an event).
func (l *Live) OpenPair(sym string, n float64, why string) (*state.Held, error) {
	perpPx, spotPx, err := l.ex.Prices(sym)
	if err != nil {
		return nil, err
	}
	ss, smin, ps, pmin, err := l.ex.Filters(sym)
	if err != nil {
		return nil, err
	}
	q := minF(FloorStep(n/spotPx, ss), FloorStep(n/perpPx, ps))
	if q <= 0 || q*spotPx < smin || q*perpPx < pmin {
		l.event("warn", fmt.Sprintf("%s: %.2f USDT per leg is below the exchange minimum, not opened", sym, n))
		return nil, nil
	}
	pos, err := l.ex.Positions()
	if err != nil {
		return nil, err
	}
	if _, ok := pos[sym]; ok {
		// one-way mode would net the short against the owner's own position on this symbol
		l.event("warn", fmt.Sprintf("%s: the account already has a futures position on it, not opened", sym))
		return nil, nil
	}
	qGross := CeilStep(q/(1-SpotFee), ss)
	margin := strategy.P.Buffer * q * perpPx
	moved := pyx.Round(margin*1.01, 2)
	need := qGross * spotPx * 1.003
	if l.transfers {
		need += moved
	} else {
		need += 0.0
	}
	free, err := l.ex.SpotFree("USDT")
	if err != nil {
		return nil, err
	}
	if free < need {
		l.event("warn", fmt.Sprintf("%s: needs %.2f USDT on spot, %.2f free - not opened", sym, need, free))
		return nil, nil
	}
	if !l.transfers {
		fa, err := l.ex.FuturesAvailable()
		if err != nil {
			return nil, err
		}
		if fa < moved {
			fa2, err := l.ex.FuturesAvailable()
			if err != nil {
				return nil, err
			}
			l.event("warn", fmt.Sprintf("%s: needs %.2f USDT margin on futures, %.2f available (no transfers) - not opened", sym, moved, fa2))
			return nil, nil
		}
	}

	if l.transfers {
		if err := l.ex.Transfer(moved, true); err != nil {
			return nil, err
		}
	}
	perp, err := func() (Fill, error) {
		if err := l.ex.PrepareShort(sym); err != nil {
			return Fill{}, err
		}
		return l.ex.PerpOrder(sym, "SELL", q, false)
	}()
	if err != nil {
		if l.transfers {
			l.try(fmt.Sprintf("%s: return %.2f USDT to spot", sym, moved), func() error { return l.ex.Transfer(moved, false) })
		}
		l.event("error", fmt.Sprintf("%s: perp short failed (%s), pair not opened", sym, pyx.Describe(err)))
		return nil, nil
	}
	spot, err := l.ex.SpotOrder(sym, "BUY", qGross)
	if err != nil {
		l.try(fmt.Sprintf("%s: unwind the perp short", sym), func() error {
			_, e := l.ex.PerpOrder(sym, "BUY", perp.Qty, true)
			return e
		})
		if l.transfers {
			l.try(fmt.Sprintf("%s: return the margin to spot", sym), func() error {
				fa, e := l.ex.FuturesAvailable()
				if e != nil {
					return e
				}
				return l.ex.Transfer(minF(moved, fa), false)
			})
		}
		l.event("error", fmt.Sprintf("%s: spot buy failed (%s), the short was unwound", sym, pyx.Describe(err)))
		return nil, nil
	}

	fa, err := l.ex.FuturesAvailable()
	if err != nil {
		return nil, err
	}
	extra := minF(maxF(0.0, (strategy.P.Buffer-1)*perp.Qty*perp.Price), fa)
	if extra > 0 && !l.try(fmt.Sprintf("%s: add %.2f USDT isolated margin", sym, extra),
		func() error { return l.ex.AddMargin(sym, pyx.Round(extra, 2)) }) {
		extra = 0.0
	}
	trigger := perp.Price * (1 + l.P.StopRise)
	l.try(fmt.Sprintf("%s: exchange stop at %.6g", sym, trigger), func() error {
		_, e := l.ex.PlaceStop(sym, trigger)
		return e
	})

	now := carry.NowMS()
	h := &state.Held{N: perp.Qty * perp.Price, Q: perp.Qty, QSpot: spot.Qty, HasLive: true, Ref: perp.Price,
		Perp0: perp.Price, Spot0: spot.Price, SpotCost: qGross * spot.Price, Margin: moved, Opened: now, Marked: now,
		Fees: perp.FeeUSDT + spot.FeeUSDT, Funding: 0.0, Basis: 0.0, PLast: perp.Price, SLast: spot.Price}
	l.savePair(sym, h, false)
	l.event("info", fmt.Sprintf("opened %s: short %.6g @ %.6g, spot %.6g @ %.6g, %.2f USDT per leg, margin %.2f (%s)",
		sym, perp.Qty, perp.Price, spot.Qty, spot.Price, h.N, perp.Qty*perp.Price+extra, why))
	return h, nil
}

// ClosePair closes both legs. Returns the closed record, or nil if the perp could not be closed (pair kept).
func (l *Live) ClosePair(sym string, h *state.Held, reason string, cool bool) (*state.Closed, error) {
	l.try(fmt.Sprintf("%s: cancel the exchange stop", sym), func() error { return l.ex.CancelStops(sym) })
	positions, err := l.ex.Positions()
	if err != nil {
		return nil, err
	}
	var perpFill *Fill
	if pos, ok := positions[sym]; ok && pos.Amt < 0 {
		f, err := l.ex.PerpOrder(sym, "BUY", math.Abs(pos.Amt), true)
		if err != nil {
			l.event("error", fmt.Sprintf("%s: closing the short failed (%s); the pair is kept and the next check retries",
				sym, pyx.Describe(err)))
			return nil, nil
		}
		perpFill = &f
	}
	perpPx, spotPx, err := l.ex.Prices(sym)
	if err != nil {
		return nil, err
	}
	ss, smin, _, _, err := l.ex.Filters(sym)
	if err != nil {
		return nil, err
	}
	free, err := l.ex.SpotFree(BaseAsset(sym))
	if err != nil {
		return nil, err
	}
	qs := FloorStep(minF(free, h.QSpot), ss)
	var spotFill *Fill
	if qs > 0 && qs*spotPx >= smin {
		f, err := l.ex.SpotOrder(sym, "SELL", qs)
		if err != nil {
			l.event("error", fmt.Sprintf("%s: selling %.6g on spot failed (%s); sell it by hand", sym, qs, pyx.Describe(err)))
		} else {
			spotFill = &f
		}
	} else if qs > 0 {
		l.event("warn", fmt.Sprintf("%s: %.6g left on spot is below the minimum order, kept as dust", sym, qs))
	}

	exitPx := perpPx
	if perpFill != nil {
		exitPx = perpFill.Price
	}
	perpPnl := 0.0
	if perpFill != nil {
		perpPnl = (h.Ref - exitPx) * h.Q
	}
	fa, err := l.ex.FuturesAvailable()
	if err != nil {
		return nil, err
	}
	back := minF(fa, maxF(0.0, h.Margin+perpPnl))
	if back >= 1 && l.transfers { // without transfers the margin stays for the next pair
		l.try(fmt.Sprintf("%s: return %.2f USDT to spot", sym, back), func() error { return l.ex.Transfer(pyx.Round(back, 2), false) })
	}
	funding := l.safeFunding(sym, h.Opened)
	fees := h.Fees
	if perpFill != nil {
		fees += perpFill.FeeUSDT
	} else {
		fees += 0.0
	}
	if spotFill != nil {
		fees += spotFill.FeeUSDT
	}
	proceeds := 0.0
	if spotFill != nil {
		proceeds = spotFill.Qty * spotFill.Price
	}
	realized := 0.0
	if h.HasRealized {
		realized = h.Realized
	}
	pnl := proceeds - h.SpotCost + perpPnl + realized + funding - fees
	rec := state.Closed{Symbol: sym, Opened: carry.Day(h.Opened), Closed: carry.Day(carry.NowMS()), N: h.N,
		Funding: funding, Basis: pnl - funding + fees, Fees: fees, Pnl: pnl, Reason: reason}
	booked := 0.0
	if h.HasBooked {
		booked = h.Booked
	}
	_ = l.store.Update(func(d *state.Data) {
		d.Capital = d.Capital + pnl - booked
		d.Closed = append(d.Closed, rec)
		if len(d.Closed) > 200 {
			d.Closed = append([]state.Closed{}, d.Closed[len(d.Closed)-200:]...)
		}
		d.Stats.PairsClosed++
		if strings.Contains(reason, "stop") {
			d.Stats.PairStops++
		}
	})
	l.savePair(sym, nil, cool)
	level := "info"
	if cool {
		level = "warn"
	}
	l.event(level, fmt.Sprintf("closed %s (%s), pnl %+.2f USDT", sym, reason, pnl))
	return &rec, nil
}

// ResizePair grows or shrinks a held pair to newN per leg (unchanged when below the exchange minimum).
func (l *Live) ResizePair(sym string, h *state.Held, newN float64) (*state.Held, error) {
	perpPx, spotPx, err := l.ex.Prices(sym)
	if err != nil {
		return nil, err
	}
	ss, smin, ps, pmin, err := l.ex.Filters(sym)
	if err != nil {
		return nil, err
	}
	dq := minF(FloorStep(math.Abs(newN-h.N)/spotPx, ss), FloorStep(math.Abs(newN-h.N)/perpPx, ps))
	if dq <= 0 || dq*spotPx < smin || dq*perpPx < pmin {
		return h, nil
	}
	c := *h
	h = &c
	if newN > h.N {
		dqGross := CeilStep(dq/(1-SpotFee), ss)
		add := strategy.P.Buffer * dq * perpPx
		free, err := l.ex.SpotFree("USDT")
		if err != nil {
			return nil, err
		}
		need := dqGross * spotPx * 1.003
		if l.transfers {
			need += add
		} else {
			need += 0.0
		}
		if free < need {
			l.event("warn", fmt.Sprintf("%s: not enough spot USDT to grow the pair, kept at %.2f", sym, h.N))
			return h, nil
		}
		if !l.transfers {
			fa, err := l.ex.FuturesAvailable()
			if err != nil {
				return nil, err
			}
			if fa < add {
				l.event("warn", fmt.Sprintf("%s: not enough futures USDT for the margin to grow the pair, kept at %.2f", sym, h.N))
				return h, nil
			}
		}
		if l.transfers {
			if err := l.ex.Transfer(pyx.Round(add, 2), true); err != nil {
				return nil, err
			}
		}
		perp, err := l.ex.PerpOrder(sym, "SELL", dq, false)
		if err != nil {
			return nil, err
		}
		spot, err := l.ex.SpotOrder(sym, "BUY", dqGross)
		if err != nil {
			l.try(fmt.Sprintf("%s: unwind the added short", sym), func() error {
				_, e := l.ex.PerpOrder(sym, "BUY", perp.Qty, true)
				return e
			})
			l.event("error", fmt.Sprintf("%s: resize spot buy failed (%s), unwound", sym, err.Error()))
			return h, nil
		}
		l.try(fmt.Sprintf("%s: add margin", sym), func() error {
			return l.ex.AddMargin(sym, pyx.Round(maxF(0.0, add-perp.Qty*perp.Price), 2))
		})
		h.Ref = (h.Ref*h.Q + perp.Price*perp.Qty) / (h.Q + perp.Qty)
		h.Q += perp.Qty
		h.QSpot += spot.Qty
		h.SpotCost += dqGross * spot.Price
		h.Margin += pyx.Round(add, 2)
		h.Fees += perp.FeeUSDT + spot.FeeUSDT
	} else {
		perp, err := l.ex.PerpOrder(sym, "BUY", dq, true)
		if err != nil {
			return nil, err
		}
		spot, err := l.ex.SpotOrder(sym, "SELL", FloorStep(minF(dq, h.QSpot), ss))
		if err != nil {
			return nil, err
		}
		share := dq / h.Q
		h.Realized = h.Realized + (h.Ref-perp.Price)*dq + spot.Qty*spot.Price - h.SpotCost*share
		h.HasRealized = true
		h.SpotCost *= 1 - share
		h.Q -= dq
		h.QSpot -= spot.Qty
		h.Margin *= 1 - share
		h.Fees += perp.FeeUSDT + spot.FeeUSDT
	}
	h.N = h.Q * h.Ref
	l.savePair(sym, h, false)
	l.event("info", fmt.Sprintf("resized %s to %.2f USDT per leg", sym, h.N))
	return h, nil
}

// ------------------------------------------------------------------ the daily plan

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// Execute carries out an accepted plan: closes, re-hedges, resizes, then opens.
func (l *Live) Execute(plan *strategy.Plan) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.held()
	for _, s := range plan.Close {
		if h, ok := held.Get(s); ok {
			stopped := contains(plan.PairStopped, s)
			reason := "pair stop (basis -2 %)"
			if !stopped {
				var ok bool
				if reason, ok = plan.Skipped[s]; !ok {
					reason = "funding below 0.01 %/8h or out of the book"
				}
			}
			if _, err := l.ClosePair(s, h, reason, stopped); err != nil {
				return err
			}
		}
	}
	held = l.held()
	for _, s := range plan.Rehedge {
		if h, ok := held.Get(s); ok {
			rec, err := l.ClosePair(s, h, "re-hedge (perp +100 % since entry)", false)
			if err != nil {
				return err
			}
			if rec != nil {
				if _, err := l.OpenPair(s, plan.Notional[s], "re-hedge"); err != nil {
					return err
				}
			}
		}
	}
	held = l.held()
	for _, r := range plan.Resize {
		if held.Has(r.Symbol) && !contains(plan.Rehedge, r.Symbol) {
			s, newN := r.Symbol, r.New
			l.try(fmt.Sprintf("%s: resize", s), func() error {
				cur := l.held()
				h, ok := cur.Get(s)
				if !ok {
					return &keyError{s}
				}
				_, err := l.ResizePair(s, h, newN)
				return err
			})
		}
	}
	for _, s := range plan.Open {
		cur := l.held()
		if !cur.Has(s) {
			s := s
			l.try(fmt.Sprintf("%s: open", s), func() error {
				_, err := l.OpenPair(s, plan.Notional[s], fmt.Sprintf("funding %.3f %%/8h", plan.Score[s]*100))
				return err
			})
		}
	}
	return nil
}

type keyError struct{ k string }

func (e *keyError) Error() string  { return pyx.Repr(e.k) }
func (e *keyError) PyName() string { return "KeyError" }

// Mark marks every held pair to market; returns the day row (equity = capital booked + open PnL).
func (l *Live) Mark(day int64) (state.DailyRow, error) {
	held := l.held()
	pos, err := l.ex.Positions()
	if err != nil {
		return state.DailyRow{}, err
	}
	fund, basis := 0.0, 0.0
	for _, s := range held.Keys() {
		h, _ := held.Get(s)
		perpPx, spotPx, err := l.ex.Prices(s)
		if err != nil {
			return state.DailyRow{}, err
		}
		markPx := perpPx
		if p, ok := pos[s]; ok {
			markPx = p.Mark
		}
		funding := l.safeFunding(s, h.Opened)
		perpPnl := (h.Ref-markPx)*h.Q + h.Realized
		spotPnl := h.QSpot*spotPx - h.SpotCost
		fund += funding - h.Funding
		basis += perpPnl + spotPnl - h.Basis
		h.Funding, h.Basis, h.PLast, h.SLast, h.Marked = funding, perpPnl+spotPnl, perpPx, spotPx, day
	}
	var row state.DailyRow
	err = l.store.Update(func(d *state.Data) {
		for _, s := range held.Keys() {
			h, _ := held.Get(s)
			if cur, ok := d.Held.Get(s); ok {
				cur.Funding, cur.Basis, cur.PLast, cur.SLast, cur.Marked = h.Funding, h.Basis, h.PLast, h.SLast, h.Marked
			}
		}
		var v []float64
		for _, s := range d.Held.Keys() {
			h, _ := d.Held.Get(s)
			v = append(v, h.Funding+h.Basis-h.Fees)
		}
		openPnl := pyx.PySum(v)
		for _, s := range d.Held.Keys() {
			h, _ := d.Held.Get(s)
			h.Booked, h.HasBooked = 0.0, true // open PnL is not booked into capital until the pair closes
		}
		keys := d.Held.Keys()
		sort.Strings(keys)
		row = state.DailyRow{Day: carry.Day(day), Equity: d.Capital + openPnl, Funding: fund, Basis: basis, Fees: 0.0, Book: keys}
		d.Daily = append(d.Daily, row)
		if len(d.Daily) > 400 {
			d.Daily = append([]state.DailyRow{}, d.Daily[len(d.Daily)-400:]...)
		}
	})
	return row, err
}

func (l *Live) safeFunding(sym string, since int64) float64 {
	f, err := l.ex.Funding(sym, since)
	if err != nil { // funding is reporting, not safety
		return 0.0
	}
	return f
}

// ------------------------------------------------------------------ the watchdog

// WatchOnce checks every held pair once (skipped while the daily cycle or a close-all is trading).
func (l *Live) WatchOnce() error {
	if !l.mu.TryLock() {
		return nil
	}
	defer l.mu.Unlock()
	held := l.held()
	if held.Len() == 0 {
		return nil
	}
	pos, err := l.ex.Positions()
	if err != nil {
		return err
	}
	keys := held.Keys()
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	stops, err := l.ex.Stops(sorted)
	if err != nil {
		return err
	}
	for _, s := range keys {
		h, _ := held.Get(s)
		var p *Perp
		if v, ok := pos[s]; ok {
			p = &v
		}
		if err := l.watchPair(s, h, p, stops[s]); err != nil {
			return err
		}
	}
	return nil
}

func (l *Live) watchPair(s string, h *state.Held, p *Perp, stops []string) error {
	// L4: legs
	if p == nil || p.Amt > -(1-l.P.LegTol)*h.Q {
		_, err := l.ClosePair(s, h, "the perp leg is gone (exchange stop, liquidation or closed by hand)", true)
		return err
	}
	free, err := l.ex.SpotFree(BaseAsset(s))
	if err != nil {
		return err
	}
	if free < (1-l.P.LegTol)*h.QSpot {
		_, err := l.ClosePair(s, h, "the spot leg is gone (sold or moved by hand)", true)
		return err
	}
	rise := p.Mark/h.Ref - 1
	dist := math.Inf(1)
	if p.Liq > 0 && p.Mark > 0 {
		dist = p.Liq/p.Mark - 1
	}
	// L2: close before the liquidation
	if rise >= l.P.CloseRise || dist < l.P.CloseDist {
		_, err := l.ClosePair(s, h, fmt.Sprintf("liquidation protection: perp %s since entry, liquidation %s away",
			pyx.Pct0(rise, true), pyx.Pct0(dist, true)), true)
		return err
	}
	// L1: more margin
	if dist < l.P.TopupDist && nowS()-l.topupAt[s] >= l.P.TopupEveryS {
		if err := l.topup(s, h, p, dist); err != nil {
			return err
		}
	}
	// L3: the exchange stop
	if len(stops) == 0 {
		trigger := h.Ref * (1 + l.P.StopRise)
		if p.Mark >= trigger {
			_, err := l.ClosePair(s, h, "the exchange stop is missing and the price is past it", true)
			return err
		}
		if l.try(fmt.Sprintf("%s: re-place the exchange stop at %.6g", s, trigger), func() error {
			_, e := l.ex.PlaceStop(s, trigger)
			return e
		}) {
			l.event("warn", fmt.Sprintf("%s: the exchange stop was missing, placed again at %.6g", s, trigger))
		}
	}
	return nil
}

func (l *Live) topup(s string, h *state.Held, p *Perp, dist float64) error {
	want := pyx.Round(0.5*h.Q*p.Mark, 2) // half the current notional
	avail, err := l.ex.FuturesAvailable()
	if err != nil {
		return err
	}
	if avail < want && l.transfers {
		spotUSDT, err := l.ex.SpotFree("USDT")
		if err != nil {
			return err
		}
		move := pyx.Round(minF(want-avail, spotUSDT), 2)
		if move >= 1 && l.try(fmt.Sprintf("%s: move %.2f USDT to futures", s, move), func() error { return l.ex.Transfer(move, true) }) {
			avail += move
		}
	}
	amount := pyx.Round(minF(want, avail), 2)
	l.topupAt[s] = nowS()
	if amount < 1 {
		l.event("warn", fmt.Sprintf("%s: liquidation %s away and no USDT to add as margin", s, pyx.Pct0(dist, true)))
		return nil
	}
	if l.try(fmt.Sprintf("%s: add %.2f USDT margin", s, amount), func() error { return l.ex.AddMargin(s, amount) }) {
		c := *h
		c.Margin += amount
		l.savePair(s, &c, false)
		l.event("warn", fmt.Sprintf("%s: liquidation was %s away (perp %s), added %.2f USDT isolated margin",
			s, pyx.Pct0(dist, true), pyx.Pct0(p.Mark/h.Ref-1, true), amount))
	}
	return nil
}

// ------------------------------------------------------------------ manual

// Flatten closes every pair now (kill switch). Returns the number closed.
func (l *Live) Flatten(by any) (int, error) {
	n := 0
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.held()
	for _, s := range held.Keys() {
		h, _ := held.Get(s)
		reason := "close all"
		if pyx.Truthy(by) {
			reason = "close all by " + pyx.Str(by)
		}
		rec, err := l.ClosePair(s, h, reason, false)
		if err != nil {
			return n, err
		}
		if rec != nil {
			n++
		}
	}
	return n, nil
}

// Reconcile reports, at start, the pairs whose legs are not on the exchange (the watchdog then closes the other leg).
func (l *Live) Reconcile() error {
	held := l.held()
	if held.Len() == 0 {
		return nil
	}
	pos, err := l.ex.Positions()
	if err != nil {
		return err
	}
	var missing []string
	for _, s := range held.Keys() {
		if _, ok := pos[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		l.event("warn", fmt.Sprintf("at start the perp leg is missing for %s; the watchdog closes the spot leg", strings.Join(missing, ", ")))
	}
	return nil
}
