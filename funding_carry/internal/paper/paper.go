// Package paper is the paper execution: the plan's book becomes the held state (lab carry_live `accept`), and
// every day the held pairs are marked like lab/carry.run_carry does:
//
//	daily PnL per pair = funding settled on the short since the last mark (+rate x N)
//	                     + basis change on N (spot leg minus perp leg, open to open)
//	                     - fees when a pair is opened, closed or resized (FeeSide per unit of N traded)
//
// Fills are assumed at the daily open used by the decision; no order is ever sent.
package paper

import (
	"fmt"
	"math"

	"fundingcarry/internal/carry"
	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
)

// FundingIncome is the funding received by a short of notional n for settlements in [since, until).
func FundingIncome(rates []market.FundingRow, since, until int64, n float64) float64 {
	var v []float64
	for _, r := range rates {
		if since <= r.Time && r.Time < until {
			v = append(v, r.Rate*n)
		}
	}
	return pyx.PySum(v)
}

// Mark is the PnL of one held pair from its last mark to the day.
type Mark struct {
	Symbol  string  `json:"symbol"`
	Funding float64 `json:"funding"`
	Basis   float64 `json:"basis"`
	Perp    float64 `json:"perp"`
	Spot    float64 `json:"spot"`
	Priced  bool    `json:"priced"`
}

// Marks keeps the held order (the sums are added in that order).
type Marks []Mark

func (ms Marks) Get(sym string) (Mark, bool) {
	for _, m := range ms {
		if m.Symbol == sym {
			return m, true
		}
	}
	return Mark{}, false
}

// MarkAll is paper.mark: PnL of every held pair from its last mark to `day` (prices = today's (perp, spot) open).
func MarkAll(held state.HeldMap, prices map[string]strategy.Pair, rates map[string][]market.FundingRow, day int64) Marks {
	out := Marks{}
	for _, s := range held.Keys() {
		h, _ := held.Get(s)
		n := h.N
		since := h.Marked
		if since == 0 {
			since = day
		}
		fund := FundingIncome(rates[s], since, day, n)
		pr, priced := prices[s]
		var perp, spot, basis float64
		if priced {
			perp, spot = pr[0], pr[1]
			basis = n*(spot/h.SLast-1) - n*(perp/h.PLast-1)
		} else {
			perp, spot, basis = h.PLast, h.SLast, 0.0 // no price today: carried, flagged by the caller
		}
		out = append(out, Mark{Symbol: s, Funding: fund, Basis: basis, Perp: perp, Spot: spot, Priced: priced})
	}
	return out
}

// Ev is an event the day produced (level, message, notify).
type Ev struct {
	Level  string
	Msg    string
	Notify bool
}

// Result is the day's summary.
type Result struct {
	Row    state.DailyRow
	Events []Ev
}

// Apply books the marks, fees and the new book into d (mutated). Returns the day's summary.
func Apply(d *state.Data, plan *strategy.Plan, marks Marks) Result {
	day := plan.Day
	held := &d.Held
	stats := &d.Stats
	fv, bv := make([]float64, 0, len(marks)), make([]float64, 0, len(marks))
	for _, m := range marks {
		fv = append(fv, m.Funding)
		bv = append(bv, m.Basis)
	}
	fund, basis := pyx.PySum(fv), pyx.PySum(bv)
	for _, m := range marks {
		h, _ := held.Get(m.Symbol)
		h.Funding += m.Funding
		h.Basis += m.Basis
		h.PLast, h.SLast, h.Marked = m.Perp, m.Spot, day
	}

	fees := 0.0
	var events []Ev
	newHeld := state.HeldMap{}
	for _, s := range plan.Book {
		old, had := held.Get(s)
		pr := plan.Prices[s]
		perp, spot := pr[0], pr[1]
		n := plan.Notional[s]
		var fee float64
		if !had {
			fee = strategy.FeeSide * n
			newHeld.Set(s, &state.Held{N: n, Ref: perp, Perp0: perp, Spot0: spot, PLast: perp, SLast: spot,
				Marked: day, Opened: day, Funding: 0.0, Basis: 0.0, Fees: fee})
			stats.PairsOpened++
			events = append(events, Ev{"info", fmt.Sprintf("paper: opened %s pair %.2f USDT per leg (funding %.3f %%/8h)",
				s, n, plan.Score[s]*100), true})
		} else {
			fee = strategy.FeeSide * math.Abs(n-old.N)
			h := *old
			h.N = n
			for _, r := range plan.Rehedge {
				if r == s {
					h.Ref = perp
					fee += strategy.FeeSide * n
					events = append(events, Ev{"warn", fmt.Sprintf("paper: re-hedged %s (perp up >= +100 %% since entry)", s), true})
					break
				}
			}
			h.Fees = h.Fees + fee
			newHeld.Set(s, &h)
			if math.Abs(n-old.N) > 1e-9 {
				events = append(events, Ev{"info", fmt.Sprintf("paper: resized %s %.2f -> %.2f USDT", s, old.N, n), false})
			}
		}
		fees += fee
	}
	for _, s := range held.Keys() {
		if newHeld.Has(s) {
			continue
		}
		h, _ := held.Get(s)
		fee := strategy.FeeSide * h.N
		fees += fee
		h.Fees = h.Fees + fee
		stopped := false
		for _, p := range plan.PairStopped {
			if p == s {
				stopped = true
			}
		}
		reason := "pair stop (basis -2 %)"
		if !stopped {
			var ok bool
			if reason, ok = plan.Skipped[s]; !ok {
				reason = "funding below 0.01 %/8h or out of the book"
			}
		}
		pnl := h.Funding + h.Basis - h.Fees
		opened := h.Opened
		if opened == 0 {
			opened = day
		}
		d.Closed = append(d.Closed, state.Closed{Symbol: s, Opened: carry.Day(opened), Closed: carry.Day(day), N: h.N,
			Funding: h.Funding, Basis: h.Basis, Fees: h.Fees, Pnl: pnl, Reason: reason})
		if len(d.Closed) > 200 {
			d.Closed = append([]state.Closed{}, d.Closed[len(d.Closed)-200:]...)
		}
		stats.PairsClosed++
		if stopped {
			stats.PairStops++
		}
		priced := true
		if m, ok := marks.Get(s); ok {
			priced = m.Priced
		}
		level := "info"
		if stopped || !priced {
			level = "warn"
		}
		msg := fmt.Sprintf("paper: closed %s (%s), pnl %+.2f USDT", s, reason, pnl)
		if !priced {
			msg += " - no price today, closed at the last mark"
		}
		events = append(events, Ev{level, msg, true})
	}

	cool := map[string]int64{}
	for s, c := range d.Cool {
		if c > day {
			cool[s] = c
		}
	}
	for _, s := range plan.PairStopped {
		cool[s] = day + 7*market.DayMS
	}
	equity := d.Capital + fund + basis - fees
	d.Held, d.Capital, d.Cool = newHeld, equity, cool
	row := state.DailyRow{Day: carry.Day(day), Equity: equity, Funding: fund, Basis: basis, Fees: -fees,
		Book: append([]string{}, plan.Book...)}
	d.Daily = append(d.Daily, row)
	if len(d.Daily) > 400 {
		d.Daily = append([]state.DailyRow{}, d.Daily[len(d.Daily)-400:]...)
	}
	d.Days = append(d.Days, carry.Day(day))
	if len(d.Days) > 400 {
		d.Days = append([]string{}, d.Days[len(d.Days)-400:]...)
	}
	return Result{Row: row, Events: events}
}
