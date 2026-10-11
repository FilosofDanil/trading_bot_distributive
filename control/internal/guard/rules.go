// Package guard runs the periodic loss check and enforces the trading block.
package guard

import (
	"time"

	"control/internal/store"
)

type Rules struct {
	AlertPct        float64       // alert when the day's loss reaches this % of the baseline
	BlockPct        float64       // close everything and block at this %
	BlockFor        time.Duration // how long the block lasts
	Period          time.Duration // length of the loss window ("day")
	AlertResetAfter time.Duration // minimum time before an alert flag may be cleared
}

// Decision tells the caller which side effects the latest snapshot requires.
type Decision struct {
	Alert   bool // send the loss alert
	Block   bool // the block has just started: flatten the account and notify
	Unblock bool // the block has just ended
	Blocked bool // the block is in force (enforce: nothing may stay open)
}

// Loss returns the loss against baseline (positive = money lost) and its percentage.
// equity = wallet balance + unrealized PnL, so open losing positions count.
func Loss(baseline, equity float64) (amount, pct float64) {
	amount = baseline - equity
	if baseline > 0 {
		pct = amount / baseline * 100
	}
	return amount, pct
}

// StartPeriod opens a new loss window with the current wallet balance as baseline.
func StartPeriod(u *store.User, wallet float64, now time.Time) {
	u.PeriodStart = now
	u.Baseline = wallet
	u.Alerted = false
	u.AlertedAt = time.Time{}
	u.CarrySymbols = nil // the caller re-adds the symbols still held
}

// Evaluate applies a fresh snapshot to the user's state and returns what to do.
// It mutates u (flags, window, last snapshot) and has no other side effects.
func Evaluate(u *store.User, snap store.Snapshot, now time.Time, r Rules) Decision {
	var d Decision

	if u.Blocked && !now.Before(u.BlockedUntil) {
		u.Blocked = false
		d.Unblock = true
		StartPeriod(u, snap.WalletBalance, now)
	}
	if !u.Blocked && (u.PeriodStart.IsZero() || u.Baseline <= 0 || !now.Before(u.PeriodStart.Add(r.Period))) {
		StartPeriod(u, snap.WalletBalance, now)
	}
	if u.PeriodStart.Equal(now) && snap.Adjust != 0 {
		// a fresh window: its baseline is the wallet as it is now, nothing booked since
		snap.Equity += snap.Adjust
		snap.Adjust = 0
	}

	snap.LossAmount, snap.LossPct = Loss(u.Baseline, snap.Equity)
	u.Last = snap

	switch {
	case u.Blocked:
		d.Blocked = true

	case snap.LossPct >= r.BlockPct:
		u.Blocked = true
		u.BlockedAt = now
		u.BlockedUntil = now.Add(r.BlockFor)
		u.Alerted = true
		u.AlertedAt = now
		d.Block = true
		d.Blocked = true

	default:
		// The alert flag is cleared only after the cooldown AND once the status has
		// changed (loss back under the alert level); a new breach then alerts again.
		if u.Alerted && now.Sub(u.AlertedAt) >= r.AlertResetAfter && snap.LossPct < r.AlertPct {
			u.Alerted = false
			u.AlertedAt = time.Time{}
		}
		if !u.Alerted && snap.LossPct >= r.AlertPct {
			u.Alerted = true
			u.AlertedAt = now
			d.Alert = true
		}
	}
	return d
}
