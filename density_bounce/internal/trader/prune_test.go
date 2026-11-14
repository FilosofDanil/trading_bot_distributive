package trader_test

import (
	"testing"

	"densitybounce/internal/trader"
)

// ShadowSeen and Failed are pruned once their entries no longer change a decision (Python kept them forever).
func TestPruneDropsOnlyDeadEntries(t *testing.T) {
	e := newDefault(t)
	now := 10 * trader.ShadowEvery
	e.tr.ShadowSeen = map[trader.ShadowKey]float64{
		{Sym: "XUSDT", DKey: bidKey, Skip: "old"}:  now - trader.ShadowEvery,     // passes the check: dead
		{Sym: "XUSDT", DKey: bidKey, Skip: "live"}: now - trader.ShadowEvery + 1, // still suppresses: kept
	}
	e.tr.SetFailed("XUSDT", bidKey, now)   // expired at now: dead
	e.tr.SetFailed("YUSDT", bidKey, now+1) // still blocks: kept

	e.tr.StepShadows(now)

	if len(e.tr.ShadowSeen) != 1 {
		t.Fatalf("ShadowSeen = %v, want only the live entry", e.tr.ShadowSeen)
	}
	if _, ok := e.tr.ShadowSeen[trader.ShadowKey{Sym: "XUSDT", DKey: bidKey, Skip: "live"}]; !ok {
		t.Error("the live shadow entry was pruned")
	}
	if _, ok := e.tr.FailedUntil("XUSDT", bidKey); ok {
		t.Error("the expired block was kept")
	}
	if until, ok := e.tr.FailedUntil("YUSDT", bidKey); !ok || until != now+1 {
		t.Error("the live block was pruned")
	}

	// at most once per ShadowEvery
	e.tr.SetFailed("XUSDT", bidKey, now)
	e.tr.StepShadows(now + trader.ShadowEvery - 1)
	if _, ok := e.tr.FailedUntil("XUSDT", bidKey); !ok {
		t.Error("pruned again before ShadowEvery passed")
	}
	e.tr.StepShadows(now + trader.ShadowEvery)
	if _, ok := e.tr.FailedUntil("XUSDT", bidKey); ok {
		t.Error("not pruned after ShadowEvery")
	}
}
