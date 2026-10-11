package guard

import (
	"context"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"control/internal/binance"
	"control/internal/store"
)

func TestAdjustment(t *testing.T) {
	recs := []binance.Income{
		{IncomeType: "TRANSFER", Asset: "USDT", Income: "300"},       // carry moved margin in
		{IncomeType: "TRANSFER", Asset: "BNB", Income: "1"},          // not a stable asset
		{Symbol: "XUSDT", IncomeType: "REALIZED_PNL", Income: "-40"}, // carry closed a pumped short
		{Symbol: "XUSDT", IncomeType: "FUNDING_FEE", Income: "0.5"},
		{Symbol: "XUSDT", IncomeType: "COMMISSION", Income: "-0.1"},
		{Symbol: "BTCUSDT", IncomeType: "REALIZED_PNL", Income: "-7"}, // the owner's own trade: counts
	}
	got := Adjustment(recs, map[string]bool{"XUSDT": true})
	if math.Abs(got-(300-40+0.5-0.1)) > 1e-9 {
		t.Fatalf("adjustment = %v", got)
	}
}

func TestSnapshotLeavesCarryOut(t *testing.T) {
	acc := &binance.Account{TotalWalletBalance: "1000", TotalUnrealizedProfit: "-95", Positions: []binance.Position{
		{Symbol: "XUSDT", PositionAmt: "-10", UnrealizedProfit: "-90"},
		{Symbol: "BTCUSDT", PositionAmt: "0.01", UnrealizedProfit: "-5"},
	}}
	s := snapshotOf(acc, time.Now(), map[string]bool{"XUSDT": true}, 200)
	if s.UnrealizedPnL != -5 || s.CarryUPnL != -90 || s.Equity != 1000-200-5 || s.OpenPositions != 1 || s.CarryCount != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
}

type blockHooks struct {
	mu              sync.Mutex
	blocks, unblock int
}

func TestCarryPairIsNotALossAndSurvivesTheBlock(t *testing.T) {
	fake := newFake(100)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	rec := &recorder{}
	hooks := &blockHooks{}
	var exemptMu sync.Mutex
	exempt := map[string]bool{}
	g := New(Options{FapiURL: srv.URL, SapiURL: srv.URL, Interval: 10 * time.Second, Rules: testRules,
		Location: time.UTC, Now: func() time.Time { return clock },
		Exempt: func() map[string]bool {
			exemptMu.Lock()
			defer exemptMu.Unlock()
			out := map[string]bool{}
			for s := range exempt {
				out[s] = true
			}
			return out
		},
		OnBlock:   func(store.User) { hooks.mu.Lock(); hooks.blocks++; hooks.mu.Unlock() },
		OnUnblock: func(store.User) { hooks.mu.Lock(); hooks.unblock++; hooks.mu.Unlock() },
	}, st, rec)
	ctx := context.Background()
	const chat = 7
	if _, err := g.Activate(ctx, chat, testKey, testSecret); err != nil {
		t.Fatal(err)
	}

	// The carry bot moves 300 USDT of margin in and opens a short that pumps: -60 unrealized.
	exemptMu.Lock()
	exempt["XUSDT"] = true
	exemptMu.Unlock()
	fake.set(func(f *fakeBinance) {
		f.wallet += 300
		f.income = append(f.income, map[string]any{"incomeType": "TRANSFER", "asset": "USDT", "income": "300",
			"time": clock.UnixMilli(), "tranId": 1})
		f.positions["XUSDT"] = -10
		f.upnl["XUSDT"] = -60
		f.algo["XUSDT"] = 1 // its exchange-side stop
	})
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 0 {
		t.Fatalf("a hedged carry short must not alert: %v", m)
	}
	u, _ := st.Get(chat)
	if u.Last.Equity != 100 || u.Last.CarryUPnL != -60 || u.Last.Adjust != 300 || len(u.CarrySymbols) != 1 {
		t.Fatalf("snapshot: %+v carry %v", u.Last, u.CarrySymbols)
	}

	// The carry closes the pair: the short's loss is realized, the margin goes back to spot.
	exemptMu.Lock()
	delete(exempt, "XUSDT")
	exemptMu.Unlock()
	fake.set(func(f *fakeBinance) {
		f.wallet += -60 - 240
		delete(f.positions, "XUSDT")
		delete(f.upnl, "XUSDT")
		delete(f.algo, "XUSDT")
		f.income = append(f.income,
			map[string]any{"symbol": "XUSDT", "incomeType": "REALIZED_PNL", "income": "-60", "time": clock.UnixMilli(), "tranId": 2},
			map[string]any{"incomeType": "TRANSFER", "asset": "USDT", "income": "-240", "time": clock.UnixMilli(), "tranId": 3})
	})
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 0 {
		t.Fatalf("closing a carry pair must not alert: %v", m)
	}
	if u, _ := st.Get(chat); math.Abs(u.Last.Equity-100) > 1e-9 {
		t.Fatalf("equity after the carry close: %+v", u.Last)
	}

	// A new carry pair, then the owner's own trade loses 20 %: block, but the carry pair stays.
	exemptMu.Lock()
	exempt["YUSDT"] = true
	exemptMu.Unlock()
	fake.set(func(f *fakeBinance) {
		f.positions["YUSDT"] = -5
		f.upnl["YUSDT"] = -1
		f.algo["YUSDT"] = 1
		f.positions["BTCUSDT"] = 0.01
		f.upnl["BTCUSDT"] = -20
	})
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 1 || !strings.Contains(m[0], "Trading blocked") {
		t.Fatalf("want block message, got %v", m)
	}
	if fake.count("POST /fapi/v1/order BTCUSDT") != 1 || fake.count("POST /fapi/v1/order YUSDT") != 0 ||
		fake.count("DELETE /fapi/v1/algoOpenOrders YUSDT") != 0 {
		t.Fatalf("enforcement must spare the carry pair: %v", fake.calls)
	}
	hooks.mu.Lock()
	if hooks.blocks != 1 {
		t.Fatalf("OnBlock calls = %d", hooks.blocks)
	}
	hooks.mu.Unlock()

	// During the block the carry pair is not "something open": no enforcement pass for it.
	before := fake.count("POST /fapi/v1/order")
	clock = clock.Add(time.Hour)
	g.Check(ctx, chat)
	if fake.count("POST /fapi/v1/order") != before {
		t.Fatalf("the carry pair was closed during the block: %v", fake.calls)
	}
	g.stopAllStreams()

	clock = clock.Add(24 * time.Hour)
	g.Check(ctx, chat)
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.unblock != 1 {
		t.Fatalf("OnUnblock calls = %d", hooks.unblock)
	}
}
