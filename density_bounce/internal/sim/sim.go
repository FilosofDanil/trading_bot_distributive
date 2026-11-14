// Package sim is the paper broker: live market data, simulated fills, no orders sent (bounce/sim.py, from the
// lab's lab/book/sim.py).
//
// Fill model (conservative for a small order):
//   - post-only entry limit: rejected if it would cross; filled when a trade prints THROUGH the price, or when
//     the volume traded AT the price after placement exceeds the queue that was resting there when we joined;
//   - stop (last price): triggered by the first trade at / through it, filled at that trade price - 0.02 %;
//   - reduce-only take limits (one per part): the same queue rule as the entry, each with its own queue;
//   - market close: best opposite price - 0.01 %.
//
// Fees: maker 0.02 %, taker 0.05 %. Latency: SIM_LAG (0.3 s) on placing and cancelling an entry (v0.4.5); the
// stop and the takes have none, so paper results are still an upper bound.
//
// Concurrency: every method runs under the runner's engine lock (see package trader); the delayed cancel runs
// through the trader's Sched.After.
package sim

import (
	"errors"
	"time"

	"densitybounce/internal/book"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/trader"
)

type take struct {
	name   string
	qty    float64
	tick   int32
	queue  float64
	traded float64
}

// Order is the working order state of one symbol.
type Order struct {
	Kind   string // entry | open
	Tick   int32  // entry: the limit tick
	Queue  float64
	Traded float64
	LiveAt float64 // entry: the order reaches the exchange at this time
	Stop   int32   // open: the stop tick
	Takes  []*take
	Left   float64 // position still open
	Gross  float64 // gross / fees / xn (exit notional) of the parts already taken
	Fees   float64
	XN     float64
}

// TakeCount is the number of takes still resting.
func (o *Order) TakeCount() int { return len(o.Takes) }

// Broker is the paper broker.
type Broker struct {
	t   *trader.Trader
	o   map[string]*Order
	Lag float64 // seconds: a new entry starts to count the tape that late, and a cancel takes effect that late
}

// New makes a paper broker. Lag (s) is the round trip to the exchange: a new entry starts to count the tape that
// late, and a cancel takes effect that late, so the order can still fill meanwhile, as a live cancel can.
func New(lag float64) *Broker { return &Broker{o: map[string]*Order{}, Lag: lag} }

// SimBroker is the paper broker (the Python class name).
type SimBroker = Broker

// NewSimBroker is New.
func NewSimBroker(lag float64) *SimBroker { return New(lag) }

// Attach is called by trader.New.
func (s *Broker) Attach(t *trader.Trader) { s.t = t }

// Order is the working order of a symbol (nil when there is none).
func (s *Broker) Order(sym string) *Order { return s.o[sym] }

func (s *Broker) book(sym string) *book.Book { return s.t.Book(sym) }

// PlaceEntry places the post-only entry limit.
func (s *Broker) PlaceEntry(sl *trader.Slot) error {
	b := s.book(sl.Sym)
	long := sl.Plan.Side == "long"
	e := b.T(sl.Plan.Entry)
	bb, okB := b.BestBid()
	ba, okA := b.BestAsk()
	if !okB || !okA || (long && e >= ba) || (!long && e <= bb) {
		return errors.New("post-only would cross")
	}
	side := book.Ask
	if long {
		side = book.Bid
	}
	s.o[sl.Sym] = &Order{Kind: "entry", Tick: e, Queue: b.QtyAt(side, e), LiveAt: b.LastTs + s.Lag}
	return nil
}

func (s *Broker) cancelDone(sl *trader.Slot) {
	if o := s.o[sl.Sym]; o != nil && o.Kind == "entry" {
		delete(s.o, sl.Sym)
	}
	s.t.OnEntryResult(sl.Sym, 0, 0, 0) // ignored when the entry filled meanwhile
}

// CancelEntry cancels the entry, Lag seconds from now.
func (s *Broker) CancelEntry(sl *trader.Slot) error {
	if s.Lag > 0 {
		s.t.Sched.After(time.Duration(s.Lag*float64(time.Second)), func() { s.cancelDone(sl) })
	} else {
		s.cancelDone(sl)
	}
	return nil
}

// Protect puts the stop and the takes of a filled position.
func (s *Broker) Protect(sl *trader.Slot) error {
	b := s.book(sl.Sym)
	side := book.Bid
	if sl.Plan.Side == "long" {
		side = book.Ask
	}
	var takes []*take
	for _, p := range sl.Parts {
		tk := b.T(p.Px)
		takes = append(takes, &take{name: p.Name, qty: p.Qty, tick: tk, queue: b.QtyAt(side, tk)})
	}
	s.o[sl.Sym] = &Order{Kind: "open", Stop: b.T(sl.Plan.Stop), Takes: takes, Left: sl.FilledQty}
	return nil
}

// MoveStop moves the resting stop.
func (s *Broker) MoveStop(sl *trader.Slot, price float64) error {
	if o := s.o[sl.Sym]; o != nil && o.Kind == "open" {
		o.Stop = s.book(sl.Sym).T(price)
	}
	return nil
}

// Close closes what is left at market.
func (s *Broker) Close(sl *trader.Slot, why string) error {
	b := s.book(sl.Sym)
	long := sl.Plan.Side == "long"
	var best int32
	var ok bool
	if long {
		best, ok = b.BestBid()
	} else {
		best, ok = b.BestAsk()
	}
	px := sl.EntryPx
	if ok {
		px = b.P(best)
	}
	if long {
		px *= 1 - 0.0001
	} else {
		px *= 1 + 0.0001
	}
	s.finish(sl, px, trader.FEE.Taker, why)
	return nil
}

// finish closes what is left at px; the trade's exit is the average over the parts taken before.
func (s *Broker) finish(sl *trader.Slot, px, feeRate float64, why string) {
	o := s.o[sl.Sym]
	delete(s.o, sl.Sym)
	if o == nil {
		o = &Order{Left: sl.FilledQty}
	}
	sgn := float64(sl.Plan.Sgn())
	q := o.Left
	fee := px * q * feeRate
	gross := o.Gross + sgn*(px-sl.EntryPx)*q
	fees := sl.EntryFee + o.Fees + fee
	exitPx := (o.XN + px*q) / sl.FilledQty
	s.t.OnClosed(sl.Sym, exitPx, gross-fees, fees, why)
}

func (s *Broker) take(sl *trader.Slot, o *Order, t *take, px float64) {
	sgn := float64(sl.Plan.Sgn())
	q := min(t.qty, o.Left)
	o.Left = pyfmt.Round(o.Left-q, 10)
	o.Gross += sgn * (px - sl.EntryPx) * q
	o.Fees += px * q * trader.FEE.Maker
	o.XN += px * q
	for i, x := range o.Takes {
		if x == t {
			o.Takes = append(o.Takes[:i], o.Takes[i+1:]...)
			break
		}
	}
	s.t.OnPart(sl.Sym, t.name, q, px)
}

// OnTrade feeds one print of a symbol (aggTrade) to the working order.
func (s *Broker) OnTrade(sym string, tr book.Trade) {
	o := s.o[sym]
	var sl *trader.Slot
	if s.t != nil {
		sl = s.t.Slot(sym)
	}
	if o == nil || sl == nil {
		return
	}
	long := sl.Plan.Side == "long"
	b := s.book(sym)
	if o.Kind == "entry" && sl.State == "entry" {
		if tr.TS() < o.LiveAt { // the order has not reached the exchange yet
			return
		}
		e := o.Tick
		var through bool
		if long {
			through = tr.Tick < e
		} else {
			through = tr.Tick > e
		}
		if tr.Tick == e && tr.BuyerMaker() == long { // the aggressor hit our side at our price
			o.Traded += tr.Qty()
		}
		if through || o.Traded > o.Queue+sl.Qty {
			delete(s.o, sym)
			px := b.P(e)
			s.t.OnEntryResult(sym, sl.Qty, px, px*sl.Qty*trader.FEE.Maker)
		}
		return
	}
	if o.Kind == "open" && (sl.State == "open" || sl.State == "closing") {
		var hit bool
		if long {
			hit = tr.Tick <= o.Stop
		} else {
			hit = tr.Tick >= o.Stop
		}
		if hit {
			px := b.P(tr.Tick)
			if long {
				px *= 1 - 0.0002
			} else {
				px *= 1 + 0.0002
			}
			s.finish(sl, px, trader.FEE.Taker, trader.StopReason(sl))
			return
		}
		for _, t := range append([]*take(nil), o.Takes...) {
			var through bool
			if long {
				through = tr.Tick > t.tick
			} else {
				through = tr.Tick < t.tick
			}
			if tr.Tick == t.tick && tr.BuyerMaker() != long {
				t.traded += tr.Qty()
			}
			if through || t.traded > t.queue+t.qty {
				s.take(sl, o, t, b.P(t.tick))
			}
		}
		if o.Left <= 1e-12 {
			delete(s.o, sym)
			fees := sl.EntryFee + o.Fees
			s.t.OnClosed(sym, o.XN/sl.FilledQty, o.Gross-fees, fees, "take")
		}
	}
}
