package guard

import (
	"testing"
	"time"

	"control/internal/store"
)

var testRules = Rules{AlertPct: 10, BlockPct: 20, BlockFor: 24 * time.Hour, Period: 24 * time.Hour, AlertResetAfter: time.Hour}

func snap(wallet, upnl float64) store.Snapshot {
	return store.Snapshot{WalletBalance: wallet, UnrealizedPnL: upnl, Equity: wallet + upnl}
}

func TestLossIncludesUnrealizedPnL(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	u := &store.User{}
	StartPeriod(u, 100, t0)

	// The example from the spec: balance 100, PnL -10 -> alert.
	d := Evaluate(u, snap(100, -10), t0.Add(10*time.Second), testRules)
	if !d.Alert || d.Block {
		t.Fatalf("want alert at -10%%, got %+v", d)
	}
	if u.Last.LossPct != 10 || u.Last.LossAmount != 10 {
		t.Fatalf("loss = %v (%v%%)", u.Last.LossAmount, u.Last.LossPct)
	}
	// PnL -20 -> block for 24h.
	d = Evaluate(u, snap(100, -20), t0.Add(20*time.Second), testRules)
	if !d.Block || !u.Blocked || !u.BlockedUntil.Equal(t0.Add(20*time.Second+24*time.Hour)) {
		t.Fatalf("want block at -20%%, got %+v until %v", d, u.BlockedUntil)
	}
}

func TestRealizedLossCounts(t *testing.T) {
	t0 := time.Now()
	u := &store.User{}
	StartPeriod(u, 1000, t0)
	// Closed trades lowered the wallet by 120, open position is -30 more: 15%.
	d := Evaluate(u, snap(880, -30), t0.Add(time.Minute), testRules)
	if !d.Alert || d.Block {
		t.Fatalf("want alert at 15%%, got %+v (%.2f%%)", d, u.Last.LossPct)
	}
}

func TestAlertFlagOnceThenResetAfterHourAndRecovery(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	u := &store.User{}
	StartPeriod(u, 100, t0)

	if d := Evaluate(u, snap(100, -11), t0, testRules); !d.Alert {
		t.Fatal("first breach must alert")
	}
	if d := Evaluate(u, snap(100, -12), t0.Add(10*time.Second), testRules); d.Alert {
		t.Fatal("already flagged: must not alert again")
	}
	// Recovered after 30 min: flag stays (cooldown is 1h).
	Evaluate(u, snap(100, -5), t0.Add(30*time.Minute), testRules)
	if !u.Alerted {
		t.Fatal("flag must stay during the 1h cooldown")
	}
	if d := Evaluate(u, snap(100, -11), t0.Add(40*time.Minute), testRules); d.Alert {
		t.Fatal("re-breach inside the cooldown must not alert")
	}
	// Still in loss after 1h: status unchanged, flag stays.
	if d := Evaluate(u, snap(100, -11), t0.Add(70*time.Minute), testRules); d.Alert || !u.Alerted {
		t.Fatal("unchanged status after 1h must not re-alert")
	}
	// Status changed (recovered) after 1h: unflag.
	Evaluate(u, snap(100, -4), t0.Add(80*time.Minute), testRules)
	if u.Alerted {
		t.Fatal("flag must clear after 1h once the loss is back under 10%")
	}
	if d := Evaluate(u, snap(100, -10.5), t0.Add(90*time.Minute), testRules); !d.Alert {
		t.Fatal("new breach after reset must alert")
	}
}

func TestBlockLastsAndEndsWithNewBaseline(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	u := &store.User{}
	StartPeriod(u, 100, t0)

	if d := Evaluate(u, snap(100, -25), t0, testRules); !d.Block {
		t.Fatal("jump straight to -25% must block")
	}
	// After the forced close the loss is realized; still blocked, no second block.
	d := Evaluate(u, snap(75, 0), t0.Add(time.Hour), testRules)
	if d.Block || !d.Blocked || d.Alert {
		t.Fatalf("during block: %+v", d)
	}
	// Period would have rolled over at +24h but the block keeps the window frozen.
	d = Evaluate(u, snap(75, 0), t0.Add(24*time.Hour-time.Second), testRules)
	if !d.Blocked || d.Unblock {
		t.Fatalf("one second before end: %+v", d)
	}
	d = Evaluate(u, snap(75, 0), t0.Add(24*time.Hour), testRules)
	if !d.Unblock || d.Blocked || u.Blocked {
		t.Fatalf("at 24h: %+v", d)
	}
	if u.Baseline != 75 || u.Alerted || u.Last.LossPct != 0 {
		t.Fatalf("new window: baseline %v alerted %v loss %v", u.Baseline, u.Alerted, u.Last.LossPct)
	}
}

func TestPeriodRollover(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	u := &store.User{}
	StartPeriod(u, 100, t0)
	Evaluate(u, snap(88, 0), t0.Add(time.Hour), testRules) // alert at 12%
	if !u.Alerted {
		t.Fatal("expected alert")
	}
	d := Evaluate(u, snap(88, 0), t0.Add(24*time.Hour), testRules)
	if d.Alert || u.Alerted || u.Baseline != 88 || !u.PeriodStart.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("rollover: %+v baseline %v alerted %v", d, u.Baseline, u.Alerted)
	}
}

func TestProfitIsNotLoss(t *testing.T) {
	u := &store.User{}
	t0 := time.Now()
	StartPeriod(u, 100, t0)
	d := Evaluate(u, snap(130, 5), t0.Add(time.Minute), testRules)
	if d.Alert || d.Block || u.Last.LossPct >= 0 {
		t.Fatalf("profit: %+v loss %v", d, u.Last.LossPct)
	}
}
