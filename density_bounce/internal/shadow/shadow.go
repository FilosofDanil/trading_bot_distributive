// Package shadow follows what the PLAIN entry would have done for every plan the bot saw (bounce/shadow.py).
//
// The plain entry is the rule until v0.4.5: a limit at the plan's entry price placed at once, kept for
// cancel_after seconds whatever the density does, then the plan's stop, half at tp1, the rest at the take,
// max_hold. It is followed on the tape for every plan: the ones the bot traded, the ones it cancelled (and why)
// and the ones a soft gate skipped. The plan journal then answers "what did this gate or this cancel cost or
// save" on every candidate, not only on the filled trades.
//
// Conservative: an order fills only when a trade prints THROUGH its price; a stop fills at its price minus
// StopSlip. Fees are the journal's (maker entry and takes, taker stop and time exit). R is net, in units of the
// plan's risk. Not safe for concurrent use (the runner's engine lock).
package shadow

import (
	"math"

	"densitybounce/internal/book"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/setup"
)

// StopSlip is the slippage a shadow stop is filled with.
const StopSlip = 0.0002

// Fee are the commission rates (a share of the notional).
type Fee struct{ Maker, Taker float64 }

// Out is the result of a shadow plan: {"hit", "r"} when nothing was filled (r null), the full set otherwise.
type Out struct {
	Hit   string   `json:"hit"`
	R     *float64 `json:"r"`
	FillS *float64 `json:"fill_s,omitempty"`
	HoldS *float64 `json:"hold_s,omitempty"`
	MfeR  *float64 `json:"mfe_r,omitempty"`
	MaeR  *float64 `json:"mae_r,omitempty"`
}

// Shadow is one followed plan.
type Shadow struct {
	Sym    string
	Plan   *setup.Plan
	T0     float64
	Act    string // what the bot did: skip:<gates> | cancel:<why> | filled | open (not decided yet)
	Final  bool   // Act will not change any more
	Entry  float64
	State  string // wait | open | done
	LastTS float64
	FillTS float64
	TP1    bool // tp1 was traded through
	Best   float64
	Worst  float64
	Out    Out
}

// New starts following plan at t0.
func New(sym string, plan *setup.Plan, t0 float64, act string, final bool) *Shadow {
	return &Shadow{Sym: sym, Plan: plan, T0: t0, Act: act, Final: final, Entry: plan.Entry, State: "wait", LastTS: t0}
}

func (s *Shadow) finish(hit string, px, ts float64, fee Fee) {
	p := s.Plan
	sgn := float64(p.Sgn())
	risk := math.Abs(s.Entry - p.Stop)
	s.State = "done"
	if hit == "nofill" || risk <= 0 {
		s.Out = Out{Hit: hit}
		return
	}
	last := sgn * (px - s.Entry) / risk // the part closed now, in R
	half := 0.0
	if s.TP1 {
		half = 0.5 * sgn * (p.TP1 - s.Entry) / risk
	}
	gross := half
	if s.TP1 {
		gross += 0.5 * last
	} else {
		gross += 1.0 * last
	}
	taker := hit == "stop" || hit == "time"
	exitFee := 0.0
	if s.TP1 {
		exitFee = 0.5 * fee.Maker
	}
	part := 1.0
	if s.TP1 {
		part = 0.5
	}
	if taker {
		exitFee += part * fee.Taker
	} else {
		exitFee += part * fee.Maker
	}
	feeR := (fee.Maker + exitFee) * s.Entry / risk
	name := hit
	if s.TP1 && hit != "take" {
		name = "tp1+" + hit
	}
	f := func(x float64, n int) *float64 { v := pyfmt.Round(x, n); return &v }
	s.Out = Out{Hit: name, R: f(gross-feeR, 3), FillS: f(s.FillTS-s.T0, 1), HoldS: f(ts-s.FillTS, 1),
		MfeR: f(sgn*(s.Best-s.Entry)/risk, 2), MaeR: f(sgn*(s.Worst-s.Entry)/risk, 2)}
}

// Step advances over the trades printed since the last step.
func (s *Shadow) Step(b *book.Book, now float64, fee Fee) {
	if s.State == "done" {
		return
	}
	p := s.Plan
	sgn := int32(p.Sgn())
	tape := b.Tape()
	i := len(tape)
	for i > 0 && tape[i-1].TS() > s.LastTS {
		i--
	}
	eT, sT, tpT := b.T(s.Entry), b.T(p.Stop), b.T(p.TP)
	var tp1T int32
	hasTP1 := p.TP1 != 0
	if hasTP1 {
		tp1T = b.T(p.TP1)
	}
	for _, tr := range tape[i:] {
		ts := tr.TS()
		s.LastTS = ts
		px := b.P(tr.Tick)
		if s.State == "wait" {
			if ts-s.T0 > setup.P.CancelAfter {
				s.finish("nofill", 0, ts, fee)
				return
			}
			if (tr.Tick-eT)*sgn < 0 { // traded through the entry
				s.State, s.FillTS = "open", ts
				s.Best, s.Worst = s.Entry, s.Entry
			} else {
				continue
			}
		}
		if ts-s.FillTS > setup.P.MaxHold {
			s.finish("time", px, ts, fee)
			return
		}
		if (px-s.Best)*float64(sgn) > 0 {
			s.Best = px
		}
		if (px-s.Worst)*float64(sgn) < 0 {
			s.Worst = px
		}
		if (tr.Tick-sT)*sgn <= 0 {
			s.finish("stop", p.Stop*(1-float64(sgn)*StopSlip), ts, fee)
			return
		}
		if hasTP1 && !s.TP1 && (tr.Tick-tp1T)*sgn > 0 {
			s.TP1 = true
		}
		if (tr.Tick-tpT)*sgn > 0 {
			s.finish("take", p.TP, ts, fee)
			return
		}
	}
	if s.State == "wait" && now-s.T0 > setup.P.CancelAfter {
		s.finish("nofill", 0, now, fee)
	} else if s.State == "open" && now-s.FillTS > setup.P.MaxHold {
		px := s.Entry
		if m, ok := b.Mid(); ok && m != 0 {
			px = m
		}
		s.finish("time", px, now, fee)
	}
}

// Lost closes the record with what is known: the book is gone (the symbol left the watch list).
func (s *Shadow) Lost() {
	if s.State != "done" {
		s.State = "done"
		s.Out = Out{Hit: "lost"}
	}
}

// Record is one line of the plan journal.
type Record struct {
	PID      string         `json:"pid"`
	Ver      string         `json:"ver"`
	TS       float64        `json:"ts"`
	Sym      string         `json:"sym"`
	Side     string         `json:"side"`
	Entry    float64        `json:"entry"`
	Stop     float64        `json:"stop"`
	TP       float64        `json:"tp"`
	TP1      float64        `json:"tp1"`
	Skip     string         `json:"skip"`
	Act      string         `json:"act"`
	Shadow   Out            `json:"shadow"`
	Features setup.Features `json:"features"`
}

// Record is the journal line of this shadow plan.
func (s *Shadow) Record(ver string) Record {
	p := s.Plan
	return Record{PID: p.PID, Ver: ver, TS: pyfmt.Round(s.T0, 3), Sym: s.Sym, Side: p.Side, Entry: s.Entry,
		Stop: p.Stop, TP: p.TP, TP1: p.TP1, Skip: p.Skip, Act: s.Act, Shadow: s.Out, Features: p.Features}
}
