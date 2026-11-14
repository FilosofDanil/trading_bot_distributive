// Package setup is the density-bounce setup (pure functions of a book.Book): plan an entry, decide when to
// cancel or exit. It is the Go port of bounce/setup.py; the rules are in the README and in that file's docstring.
//
// Long off a bid density (support), short off an ask density (resistance): the density must be big, old and
// intact; price makes a sharp approach (>= 0.80 % in 3 min, still moving) after a move of >= 2.5 % from the
// extreme of the last 30 min; the stop is max(0.15 %, 2 x ATR) behind the density; the take is the middle of the
// previous move; half the position goes at +1R. Soft gates (young, weak, counter_slow, market) turn a plan into
// a shadow plan (the ghost returned by Scan) that is journaled but not traded.
//
// Not safe for concurrent use: every call happens with the runner's engine lock held.
package setup

import (
	"math"
	"strings"

	"densitybounce/internal/book"
	"densitybounce/internal/pyfmt"
)

// MoveStart is (price, ts) of the extreme in the last secs: the max for a drop (long setup), the min for a
// rally. The mid samples cover the last minutes, the 1m bars (seeded when a symbol is added) the rest.
func MoveStart(b *book.Book, now, secs float64, long bool) (price, ts float64, ok bool) {
	for t, m := range b.MidsBackward() {
		if t < now-secs {
			break
		}
		if !ok || (long && m > price) || (!long && m < price) {
			price, ts, ok = m, t, true
		}
	}
	bars := b.Bars()
	for i := len(bars) - 1; i >= 0; i-- {
		x := bars[i]
		if float64(x.Min)*60 < now-secs {
			break
		}
		px := b.Px(x.Lo)
		if long {
			px = b.Px(x.Hi)
		}
		if !ok || (long && px > price) || (!long && px < price) {
			price, ts, ok = px, float64(x.Min)*60.0, true
		}
	}
	return
}

// Accel is the speed toward the density over the last ApproachSecs against the average speed of the whole move
// (start at startTS -> now): > 1 = accelerating into the density, < 1 = slowing down, <= 0 = stalled or turned.
// ok is false without the mid history. A move shorter than ApproachSecs is compared over its own length.
func Accel(b *book.Book, now, mid float64, sgn int, start, startTS float64) (float64, bool) {
	dur := math.Max(now-startTS, 30.0)
	w := math.Min(P.ApproachSecs, dur)
	mApp, ok := b.MidAgo(now, w)
	if !ok || start <= 0 {
		return 0, false
	}
	tot := (start - mid) / start * float64(sgn)
	if tot <= 0 {
		return 0, false
	}
	return (mApp - mid) / mApp * float64(sgn) / w / (tot / dur), true
}

// HoldSecs is how many seconds the density can absorb the aggressive flow that is hitting its side now, and that
// flow in USDT / s. ok is false without tape (hold unknown).
func HoldSecs(b *book.Book, d *book.Density, now float64) (hold float64, ok bool, rate float64) {
	rate = b.FlowRate(now, d.Side == book.Bid, P.ImpulseSecs, P.ApproachSecs)
	if rate > 0 {
		return d.USD / rate, true, rate
	}
	return 0, false, rate
}

func optF(x float64, ok bool, n int) *float64 {
	if !ok {
		return nil
	}
	return F64(pyfmt.Round(x, n))
}

func features(b *book.Book, d *book.Density, now, mid, spread float64) Features {
	turn := b.MinuteTurnover(now)
	f := Features{
		DensityUSD: pyfmt.Int(d.USD), PeakUSD: pyfmt.Int(d.PeakUSD), AgeS: pyfmt.Round(d.Age(now), 1),
		Touches: d.Touches, Dist: pyfmt.Round(math.Abs(mid-d.Price)/mid, 5), Spread: pyfmt.Round(spread, 5),
		MinuteUSD: pyfmt.Int(turn),
	}
	if turn != 0 {
		f.DensVsMinute = F64(pyfmt.Round(d.PeakUSD/turn, 2))
	}
	return f
}

func floorTick(x, tick float64, long bool) int32 {
	if long {
		return int32(math.Floor(x/tick + 1e-9))
	}
	return int32(math.Ceil(x/tick - 1e-9))
}

// PlanBounce is the best (nearest) bounce entry on this book right now that passes every gate, or nil.
func PlanBounce(b *book.Book, now float64, skip map[book.DKey]bool, mkt *float64) *Plan {
	p, _ := Scan(b, now, skip, mkt)
	return p
}

// Scan returns the nearest entry that passes every gate, and the nearest one that fails only soft gates: a
// shadow plan with Skip = "young" / "weak" / "counter_slow" / "market" joined by "+". mkt is the median change of
// the other watched coins over MktSecs (nil = unknown: no market filter).
func Scan(b *book.Book, now float64, skip map[book.DKey]bool, mkt *float64) (plan, ghost *Plan) {
	mid, okMid := b.Mid()
	bb, okB := b.BestBid()
	ba, okA := b.BestAsk()
	if !okMid || !okB || !okA {
		return nil, nil
	}
	tick := b.TickSize
	spread := float64(ba-bb) * tick / mid
	if tick/mid > P.TickMax || spread > P.SpreadMax || b.MinuteTurnover(now) < P.MinuteMin {
		return nil, nil
	}
	mAgo, ok := b.MidAgo(now, P.ImpulseSecs)
	if !ok {
		return nil, nil
	}
	atr := 0.0
	if P.StopATR > 0 {
		atr, _ = b.ATR(now, P.ATRN)
		if atr == 0 {
			return nil, nil // no bars yet: the stop would be the bare stop_buf
		}
	}
	var best [2]*Plan // index 1 = ok (tradable), 0 = soft-gated
	var bestDist [2]float64
	for _, d := range b.Alive() {
		if skip[d.Key()] || d.Touches > P.MaxTouches {
			continue
		}
		if d.Age(now) < math.Min(P.MinAge, P.ShadowAge) {
			continue
		}
		if d.USD < P.Intact*d.PeakUSD {
			continue
		}
		if P.EatCancel > 0 && d.Drop(now, P.EatSecs) >= P.EatCancel {
			continue // being eaten right now: a breakout, not a wall
		}
		long := d.Side == book.Bid
		sgn := 1
		if !long {
			sgn = -1
		}
		fsgn := float64(sgn)
		dist := (mid - d.Price) / mid * fsgn
		if !(0 < dist && dist <= P.PlaceDist) {
			continue
		}
		moved := (mAgo - mid) / mAgo * fsgn // toward the density, as a fraction
		distAgo := (mAgo - d.Price) / mAgo * fsgn
		if moved < P.ImpulseMin || distAgo <= 0 || moved < P.ImpulseFrac*distAgo {
			continue
		}
		mApp, ok := b.MidAgo(now, P.ApproachSecs)
		if !ok {
			continue
		}
		approach := (mApp - mid) / mApp * fsgn
		if approach < P.ApproachMin {
			continue
		}
		start, startTS, ok := MoveStart(b, now, P.MoveSecs, long)
		if !ok {
			continue
		}
		move := (start - d.Price) / start * fsgn
		if move < P.MoveMin {
			continue
		}
		acc, accOK := Accel(b, now, mid, sgn, start, startTS)
		tr, trOK := b.TrendBefore(P.TrendEMA, startTS) // the 1h trend before this move began
		counter := trOK && tr == -sgn
		hold, holdOK, rate := HoldSecs(b, d, now)
		var mktTo float64
		if mkt != nil {
			mktTo = -*mkt * fsgn // the market's move toward the density
		}
		var why []string // soft gates: journaled as a shadow plan
		if d.Age(now) < P.MinAge {
			why = append(why, "young")
		}
		if P.HoldMin > 0 && holdOK && hold < P.HoldMin {
			why = append(why, "weak") // not worth a minute of the flow that is hitting it
		}
		if counter && P.CounterAccel > 0 && accOK && acc < P.CounterAccel {
			why = append(why, "counter_slow") // against the trend, slowing down: no reversal sign
		}
		if P.MktMax > 0 && mkt != nil && mktTo >= P.MktMax {
			why = append(why, "market") // every coin is moving this way: walls do not hold
		}
		front := int32(math.Max(1, math.Ceil(P.Front*d.Price/tick)))
		buf := int32(math.Max(2, math.Ceil(P.StopBuf*d.Price/tick)))
		if atr != 0 {
			buf = max(buf, int32(math.Ceil(P.StopATR*atr/tick-1e-9)))
		}
		eT := d.Tick + int32(sgn)*front
		sT := d.Tick - int32(sgn)*buf
		if (long && eT >= ba) || (!long && eT <= bb) {
			continue // a post-only order would cross
		}
		tpRaw := d.Price + fsgn*P.TakeFrac*math.Abs(start-d.Price)
		tpT := floorTick(tpRaw, tick, long)
		entry, stop, tp := b.P(eT), b.P(sT), b.P(tpT)
		risk := math.Abs(entry-stop) / entry
		reward := (tp - entry) * fsgn / entry
		if !(P.RiskMin <= risk && risk <= P.RiskMax) || reward <= 0 || reward < P.MinRR*risk {
			continue
		}
		okIdx := 0
		if len(why) == 0 {
			okIdx = 1
		}
		if best[okIdx] == nil || dist < bestDist[okIdx] {
			turn := b.MinuteTurnover(now)
			f := features(b, d, now, mid, spread)
			f.Moved60 = pyfmt.Round(moved, 5)
			f.Approach = pyfmt.Round(approach, 5)
			f.Move = pyfmt.Round(move, 5)
			f.MoveStart = start
			f.MoveTS = pyfmt.Round(startTS, 1)
			f.MoveS = pyfmt.Round(now-startTS, 1)
			f.Risk = pyfmt.Round(risk, 5)
			f.Reward = pyfmt.Round(reward, 5)
			f.RR = pyfmt.Round(reward/risk, 2)
			if atr != 0 {
				f.ATR = F64(pyfmt.Round(atr/mid, 5))
				f.StopATR = F64(pyfmt.Round(math.Abs(entry-stop)/atr, 2))
			}
			if trOK {
				f.Trend = IntP(tr)
			}
			f.Counter = counter
			f.Accel = optF(acc, accOK, 2)
			f.HoldS = optF(hold, holdOK, 1)
			f.FlowS = pyfmt.Int(rate)
			if mkt != nil {
				f.Mkt = F64(pyfmt.Round(*mkt, 5))
			}
			if turn != 0 {
				f.VolRatio = F64(pyfmt.Round(b.Turnover(now, 180)/3/turn, 2))
			}
			f.Ch24 = b.Ch24
			f.Drop10 = pyfmt.Round(d.Drop(now, P.EatSecs), 3)
			// partial takes, rounded toward the entry (a take that is touched but not traded through stays unfilled)
			var tp1Raw float64
			if P.TP1R > 0 {
				tp1Raw = entry + fsgn*P.TP1R*math.Abs(entry-stop)
			} else {
				tp1Raw = d.Price + fsgn*P.TP1Frac*math.Abs(start-d.Price)
			}
			tp1T := floorTick(tp1Raw, tick, long)
			tp1 := 0.0
			if 0 < int(tp1T-eT)*sgn && int(tp1T-eT)*sgn < int(tpT-eT)*sgn {
				tp1 = b.P(tp1T)
			}
			runRaw := d.Price + fsgn*P.RunFrac*math.Abs(start-d.Price)
			runT := floorTick(runRaw, tick, long)
			tpRun := 0.0
			if int(runT-tpT)*sgn > 0 {
				tpRun = b.P(runT)
			}
			side := "short"
			if long {
				side = "long"
			}
			best[okIdx] = &Plan{Side: side, Entry: entry, Stop: stop, TP: tp, DKey: d.Key(), Features: f,
				TP1: tp1, TPRun: tpRun, Skip: strings.Join(why, "+")}
			bestDist[okIdx] = dist
		}
	}
	return best[1], best[0]
}

// WallFailed are the cancel reasons that say the density itself let us down: the trader does not try that
// density again for a while.
var WallFailed = []string{"density_gone", "density_shrank", "density_eaten", "density_weak", "unconfirmed"}

// IsWallFailed reports whether why is one of WallFailed.
func IsWallFailed(why string) bool {
	for _, w := range WallFailed {
		if w == why {
			return true
		}
	}
	return false
}

// CancelReason is why an armed or working entry should be cancelled (” = keep it).
func CancelReason(b *book.Book, plan *Plan, placedAt, now float64) string {
	d := b.Densities[plan.DKey]
	mid, okMid := b.Mid()
	if d == nil || d.Gone != "" {
		return "density_gone"
	}
	if d.USD < P.CancelShrink*d.PeakUSD {
		return "density_shrank"
	}
	if P.EatCancel > 0 && d.Drop(now, P.EatSecs) >= P.EatCancel {
		return "density_eaten"
	}
	if P.HoldCancel > 0 {
		hold, ok, _ := HoldSecs(b, d, now)
		if ok && hold < P.HoldCancel {
			return "density_weak" // the flow has grown: the wall is seconds of it now
		}
	}
	if plan.Features.Counter && P.CounterAccel > 0 && okMid && mid != 0 && plan.Features.MoveTS != 0 {
		// against the 1h trend the entry needs the approach to stay sharp until the fill
		a, ok := Accel(b, now, mid, plan.Sgn(), plan.Features.MoveStart, plan.Features.MoveTS)
		if ok && a < P.CounterAccel {
			return "counter_stalled"
		}
	}
	if okMid && mid != 0 && math.Abs(mid-d.Price)/mid > P.CancelDist {
		return "price_away"
	}
	if now-placedAt > P.CancelAfter {
		return "entry_timeout"
	}
	return ""
}

// Touched: price has reached the entry level, no bid is left above it (long) / no ask below it (short), so an
// order resting there would be first in line.
func Touched(b *book.Book, plan *Plan) bool {
	eT := b.T(plan.Entry)
	bb, okB := b.BestBid()
	ba, okA := b.BestAsk()
	if !okB || !okA {
		return false
	}
	if plan.Side == "long" {
		return bb <= eT
	}
	return ba >= eT
}

// Confirm, after ConfirmSecs at the entry level: ("", tick, true) = place the post-only entry at tick because the
// density held; ("unconfirmed", 0, false) = it did not (under Intact of its peak, gone, or price is through it);
// ("", 0, false) = wait (no room between the density and the opposite quote yet).
func Confirm(b *book.Book, plan *Plan) (why string, tick int32, ok bool) {
	d := b.Densities[plan.DKey]
	bb, okB := b.BestBid()
	ba, okA := b.BestAsk()
	if d == nil || d.Gone != "" || !okB || !okA || d.USD < P.Intact*d.PeakUSD {
		return "unconfirmed", 0, false
	}
	eT := b.T(plan.Entry)
	if plan.Side == "long" {
		t := min(eT, ba-1) // post-only: below the best ask
		if t > d.Tick {
			return "", t, true
		}
		return "", 0, false
	}
	t := max(eT, bb+1)
	if t < d.Tick {
		return "", t, true
	}
	return "", 0, false
}

// StopDepth is the base qty resting from the stop to StopDepthBand beyond it on the side a market stop fills
// against (bids for a long), the density itself not counted: how much the stop can fill without sliding further.
func StopDepth(b *book.Book, plan *Plan) float64 {
	long := plan.Side == "long"
	sT := b.T(plan.Stop)
	band := int32(math.Max(1, float64(pyfmt.Int(P.StopDepthBand*plan.Stop/b.TickSize))))
	lo, hi := sT, sT+band
	side := book.Ask
	if long {
		lo, hi = sT-band, sT
		side = book.Bid
	}
	sum := 0.0
	for k := lo; k <= hi; k++ {
		if k != plan.DKey.Tick {
			sum += b.QtyAt(side, k)
		}
	}
	return sum
}

// Manage is the per-open-position memory for exit decisions.
type Manage struct {
	OpenedAt float64
	BeDone   bool
}

// ExitReason: "" = hold; "be" = move the stop to break-even; anything else = close at market with that reason.
// Stop and take are resting orders (on the exchange in live mode); this only adds the time exit and options.
func ExitReason(b *book.Book, plan *Plan, entryPx float64, m *Manage, now float64) string {
	mid, ok := b.Mid()
	if !ok {
		return ""
	}
	if now-m.OpenedAt > P.MaxHold {
		return "time"
	}
	sgn := float64(plan.Sgn())
	r := math.Abs(entryPx - plan.Stop)
	if r == 0 {
		r = plan.Risk()
	}
	prog := (mid - entryPx) * sgn / r
	if P.ExitOnGone {
		d := b.Densities[plan.DKey]
		if (d == nil || d.Gone != "") && prog < 0.5 {
			return "density_gone"
		}
	}
	if P.BeAtR > 0 && !m.BeDone && prog >= P.BeAtR {
		return "be"
	}
	return ""
}

// BEPrice is the break-even stop: entry + fees, but never closer to price than half the initial risk.
func BEPrice(plan *Plan, entryPx, tick float64) float64 {
	sgn := float64(plan.Sgn())
	r := math.Abs(entryPx - plan.Stop)
	if r == 0 {
		r = plan.Risk()
	}
	raw := entryPx + sgn*math.Min(P.BeOff*entryPx, 0.5*r)
	var t float64
	if sgn > 0 {
		t = math.Floor(raw/tick + 1e-9)
	} else {
		t = math.Ceil(raw/tick - 1e-9)
	}
	return pyfmt.Round(t*tick, 12)
}
