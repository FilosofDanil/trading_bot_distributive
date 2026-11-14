package book

import (
	"math"
	"runtime"
	"testing"
)

func TestTradePacksSideIntoQty(t *testing.T) {
	for _, bm := range []bool{true, false} {
		tr := NewTradeAt(1000.25, 10001, 12.5, bm)
		if tr.BuyerMaker() != bm || tr.Qty() != 12.5 || tr.TS() != 1000.25 || tr.Tick != 10001 {
			t.Fatalf("%+v: bm %v qty %v ts %v", tr, tr.BuyerMaker(), tr.Qty(), tr.TS())
		}
	}
	if !NewTrade(1, 1, 0, true).BuyerMaker() {
		t.Fatal("a zero-qty sell keeps its side")
	}
}

func TestTapeKeepsOnlyTheWindow(t *testing.T) {
	b := New("XUSDT", 0.0001)
	for i := 0; i < 20_000; i++ { // 20 prints a second for 1000 s
		b.OnTrade(int64(1_000_000+i*50), 1.0, 1, i%2 == 0)
	}
	tape := b.Tape()
	span := tape[len(tape)-1].TS() - tape[0].TS()
	if span > TapeKeep || span < TapeKeep-1 {
		t.Fatalf("tape spans %.1f s, want ~%.0f", span, TapeKeep)
	}
	if cap(b.tape) > 3*len(tape)+2048 {
		t.Fatalf("tape backing array %d for %d prints: not compacted", cap(b.tape), len(tape))
	}
	// the flow window is inside the tape: the same sum as counting by hand
	now := tape[len(tape)-1].TS()
	want := 0.0
	for _, tr := range tape {
		if tr.TS() >= now-180 && tr.BuyerMaker() {
			want += tr.Qty() * float64(tr.Tick)
		}
	}
	if got := b.Flow(now, 180, true); math.Abs(got-want*b.TickSize) > 1e-9 {
		t.Fatalf("flow %v, want %v", got, want*b.TickSize)
	}
}

// Sub-second prints: the 1-second buckets may leave out at most the part of one second at the window's start.
func TestTurnoverBucketsBound(t *testing.T) {
	b := New("XUSDT", 0.0001)
	exact := map[float64]float64{}
	for i := 0; i < 2000; i++ {
		ts := 1000 + float64(i)*0.37
		b.AddTurnover(ts, 100)
		exact[ts] = 100
	}
	now := 1000 + 1999*0.37
	sum := 0.0
	for ts, u := range exact {
		if ts >= now-180 {
			sum += u
		}
	}
	got := b.Turnover(now, 180)
	if got > sum || sum-got > 300 { // at most one second of prints (<= 3 here) missing
		t.Fatalf("turnover %v, exact %v", got, sum)
	}
}

func TestDensityCountsItsOwnPriceOnly(t *testing.T) {
	b := New("XUSDT", 0.0001)
	d := NewDensity(Bid, 10000, 1.0, 0, 50_000, 50_000, 0)
	b.Densities[d.Key()] = d
	b.OnTrade(1000, 1.0000, 5, true)  // an aggressive sell at the wall
	b.OnTrade(1001, 1.0000, 2, false) // a buy at the wall
	b.OnTrade(1002, 1.0001, 7, true)  // elsewhere
	if d.SellQty != 5 || d.BuyQty != 2 || b.Eaten(d) != 7 {
		t.Fatalf("sell %v buy %v eaten %v", d.SellQty, d.BuyQty, b.Eaten(d))
	}
}

// Python's cum_at kept one entry for every price ever traded and the tape 50 000 objects per symbol; here a
// day of prints at ever-new prices must not grow the book beyond the windows.
func TestMemoryStaysBounded(t *testing.T) {
	b := New("XUSDT", 0.0001)
	trade := func(n int) {
		for i := 0; i < n; i++ {
			ms := int64(1_700_000_000_000 + i*100)
			price := 1.0 + float64(i%500_000)*0.0001 // a new price nearly every print
			b.OnTrade(ms, price, 1, i%3 == 0)
		}
	}
	trade(50_000)
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	trade(400_000) // 11 hours at 10 prints a second
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	grow := int64(m2.HeapAlloc) - int64(m1.HeapAlloc)
	if grow > 4<<20 { // Python: ~250 B per print (tape) + one cum_at entry per price, ~100 MB here
		t.Fatalf("heap grew %d bytes over 400k prints", grow)
	}
	if n := len(b.Tape()); n > int(TapeKeep*10)+1 {
		t.Fatalf("tape holds %d prints", n)
	}
	t.Logf("tape %d prints, %d bars, heap growth %d bytes", len(b.Tape()), len(b.Bars()), grow)
}

func TestBestQuotesFollowUpdates(t *testing.T) {
	b := New("XUSDT", 0.0001)
	b.SetSide(Bid, map[int32]float64{9998: 1, 9999: 1})
	b.SetSide(Ask, map[int32]float64{10001: 1, 10002: 1})
	if bb, _ := b.BestBid(); bb != 9999 {
		t.Fatal(bb)
	}
	b.SetLevel(Bid, 10000, 3)
	b.SetLevel(Ask, 10001, 0)
	bb, _ := b.BestBid()
	ba, _ := b.BestAsk()
	if bb != 10000 || ba != 10002 {
		t.Fatalf("best %d / %d", bb, ba)
	}
	if m, ok := b.Mid(); !ok || math.Abs(m-1.0001) > 1e-12 {
		t.Fatalf("mid %v", m)
	}
	b.SetLevel(Bid, 10000, 0)
	b.SetLevel(Bid, 9999, 0)
	b.SetLevel(Bid, 9998, 0)
	if _, ok := b.BestBid(); ok {
		t.Fatal("empty side has no best")
	}
}

func TestPendingIsCapped(t *testing.T) {
	b := New("XUSDT", 0.0001)
	for i := 0; i < 3*maxPending; i++ {
		b.OnDepth(DepthEvent{FirstU: int64(i), LastU: int64(i), E: int64(i)})
	}
	if len(b.Pending) > maxPending || b.Pending[len(b.Pending)-1].LastU != int64(3*maxPending-1) {
		t.Fatalf("pending %d, last %d", len(b.Pending), b.Pending[len(b.Pending)-1].LastU)
	}
}
