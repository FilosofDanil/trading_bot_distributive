// Package book is the local order book (Binance USD-M diff depth), the tape and the density detector of one
// symbol: the Go port of bounce/book.py with bounded, compact memory (docs/go_port.md).
//
// Prices are integer ticks (round(price / tickSize)) so map keys are exact. Everything here is pure (no I/O)
// and not safe for concurrent use: the runner serialises the feed, the trader and the API on one lock.
//
// What changed against the Python book, per watched symbol:
//   - the tape keeps only the trades of the last TapeKeep seconds (16 bytes each), enough for Flow (180 s) and
//     the shadow plans; Python kept 50 000 trade objects of ~200 bytes;
//   - turnover lives in 1-second buckets of the last 15 minutes (12 bytes each) instead of a tuple per trade;
//   - there is no cum_at map (it grew with every price ever traded): a density counts the aggressive qty at its
//     own price from the moment it is first seen, which is what cum(tick) - base computed;
//   - book levels are float32 qty, mids / 1m bars / 1h closes are int32 ticks.
package book

import (
	"iter"
	"math"
	"sort"
)

// Dens holds the density detector parameters (pre-registered, not tuned).
type Dens struct {
	Band     float64 // track densities within 3 % of mid
	BaseBand float64 // baseline = median level notional within 2 % of mid on that side
	Ratio    float64 // density >= 8 x the median level
	USDMin   float64 // and >= 25k USDT
	MinuteK  float64 // and >= half the average traded USDT per minute (last 15 min of tape)
	GoneFrac float64 // a density with < 30 % of its peak size left is gone
}

// DENS is the detector configuration (book.DENS in Python); setup.LoadParams may override it from PARAMS.
var DENS = Dens{Band: 0.03, BaseBand: 0.02, Ratio: 8.0, USDMin: 25_000.0, MinuteK: 0.5, GoneFrac: 0.30}

// DensKeys are the PARAMS names of the DENS fields.
var DensKeys = []string{"band", "base_band", "ratio", "usd_min", "minute_k", "gone_frac"}

// SetDens sets one DENS field by its PARAMS name; false for an unknown name.
func SetDens(key string, v float64) bool {
	switch key {
	case "band":
		DENS.Band = v
	case "base_band":
		DENS.BaseBand = v
	case "ratio":
		DENS.Ratio = v
	case "usd_min":
		DENS.USDMin = v
	case "minute_k":
		DENS.MinuteK = v
	case "gone_frac":
		DENS.GoneFrac = v
	default:
		return false
	}
	return true
}

// TapeKeep is how many seconds of trades the tape keeps. It must cover the longest Flow window
// (setup approach_secs) and the 300 s a shadow plan may wait for an unsynced book; setup sets it after PARAMS.
var TapeKeep = 310.0

const (
	midsMax    = 2400 // (ts, mid) every >= 0.25 s: 10 min
	barsMax    = 240  // [minute, high, low] of traded prices
	h1Max      = 48   // [hour, close] of traded prices
	histMax    = 60   // (ts, usd) of every density scan (~0.5 s)
	turnWindow = 900.0
	turnSlots  = 1024 // 1-second turnover buckets, > turnWindow
	maxPending = 5000 // depth events buffered before a snapshot bridges them; the oldest go first
	maxTick    = math.MaxInt32 / 4
)

// Side of the book.
type Side uint8

const (
	Bid Side = iota // support: long setups
	Ask             // resistance: short setups
)

func (s Side) String() string {
	if s == Bid {
		return "bid"
	}
	return "ask"
}

// DKey identifies a density: (side, tick).
type DKey struct {
	Side Side
	Tick int32
}

// Level is one price level of a depth update or snapshot.
type Level struct {
	Tick int32
	Qty  float32
}

// DepthEvent is one diff-depth stream event, already converted to ticks (Book.Levels).
type DepthEvent struct {
	FirstU int64 // "U"
	LastU  int64 // "u"
	PrevU  int64 // "pu" (-1 when absent)
	E      int64 // event time, ms
	Bids   []Level
	Asks   []Level
}

// Trade is one aggTrade print in 16 bytes. q < 0 marks buyer-maker (an aggressive seller hit the bid).
type Trade struct {
	Ms   int64
	Tick int32
	q    float32
}

// NewTrade builds a tape entry; ms is the trade time in epoch milliseconds.
func NewTrade(ms int64, tick int32, qty float64, buyerMaker bool) Trade {
	q := float32(math.Abs(qty))
	if buyerMaker {
		q = -q
		if q == 0 {
			q = float32(math.Copysign(0, -1))
		}
	}
	return Trade{Ms: ms, Tick: tick, q: q}
}

// NewTradeAt is NewTrade with the time in seconds (tests, fixtures).
func NewTradeAt(ts float64, tick int32, qty float64, buyerMaker bool) Trade {
	return NewTrade(int64(math.Round(ts*1000)), tick, qty, buyerMaker)
}

// TS is the trade time in seconds, the same float as Python's float(T) / 1000.
func (t Trade) TS() float64 { return float64(t.Ms) / 1000 }

// Qty is the base quantity.
func (t Trade) Qty() float64 { return math.Abs(float64(t.q)) }

// BuyerMaker is true when the aggressor sold into the bid.
func (t Trade) BuyerMaker() bool { return math.Signbit(float64(t.q)) }

type histPoint struct {
	ts  float64
	usd float64
}

// Density is a large resting order at one price.
type Density struct {
	Side      Side
	Tick      int32
	Price     float64
	FirstSeen float64
	PeakUSD   float64
	USD       float64
	LastSeen  float64
	Touches   int     // completed approaches: mid came within 0.15 % and went back out beyond 0.4 %
	Near      bool    //
	Gone      string  // "" alive, "eaten" (traded through), "pulled" (cancelled), "crossed"
	GoneAt    float64 //
	BuyQty    float64 // aggressive buy qty traded at Tick since the density was first seen
	SellQty   float64 // aggressive sell qty traded at Tick since the density was first seen
	hist      [histMax]histPoint
	histHead  int
	histN     int
}

// NewDensity is Python's Density(side, tick, price, first_seen, peak_usd, usd, last_seen).
func NewDensity(side Side, tick int32, price, firstSeen, peakUSD, usd, lastSeen float64) *Density {
	return &Density{Side: side, Tick: tick, Price: price, FirstSeen: firstSeen, PeakUSD: peakUSD, USD: usd, LastSeen: lastSeen}
}

// Key is the density's (side, tick).
func (d *Density) Key() DKey { return DKey{d.Side, d.Tick} }

// Age in seconds.
func (d *Density) Age(now float64) float64 { return now - d.FirstSeen }

// AddHist records one scan (ts, usd); the last 60 are kept.
func (d *Density) AddHist(ts, usd float64) {
	i := (d.histHead + d.histN) % histMax
	if d.histN == histMax {
		d.histHead = (d.histHead + 1) % histMax
	} else {
		d.histN++
	}
	d.hist[i] = histPoint{ts, usd}
}

// Drop is the share of its size the density lost against its largest size of the last secs (0 = none).
func (d *Density) Drop(now, secs float64) float64 {
	top := 0.0
	for i := 0; i < d.histN; i++ {
		p := d.hist[(d.histHead+i)%histMax]
		if p.ts >= now-secs && p.usd > top {
			top = p.usd
		}
	}
	if top > d.USD {
		return 1 - d.USD/top
	}
	return 0.0
}

// Bar is one 1m bar of traded prices, in ticks.
type Bar struct {
	Min int32
	Hi  int32
	Lo  int32
}

// HourClose is one 1h close of traded prices, in ticks.
type HourClose struct {
	Hour  int32
	Close int32
}

// Kline is the part of a Binance kline row the seeds read.
type Kline struct {
	OpenMs int64
	High   float64
	Low    float64
	Close  float64
}

// Book is the local order book, the recent tape and the densities of one symbol.
type Book struct {
	Symbol     string
	TickSize   float64
	Synced     bool
	Pending    []DepthEvent
	LastTs     float64
	Ch24       *float64 // 24h change from the screener's ticker (nil = unknown)
	Densities  map[DKey]*Density
	lastU      int64
	hasLastU   bool
	snapshotID int64

	bids, asks map[int32]float32
	best       [2]int32
	bestOK     [2]bool

	tape     []Trade
	tapeHead int

	midMs   [midsMax]int64
	midM2   [midsMax]int32 // best bid + best ask in ticks: mid = m2 / 2 * tick
	midHead int
	midN    int

	bars []Bar
	h1   []HourClose

	turnSec   [turnSlots]int64
	turnUSD   [turnSlots]float32
	turnMax   float64 // newest trade time seen
	turnAny   bool
	turnFloor float64 // the Python queue's prune floor: trades before it are gone
}

// New returns an empty, unsynced book.
func New(symbol string, tickSize float64) *Book {
	b := &Book{Symbol: symbol, TickSize: tickSize, Densities: map[DKey]*Density{},
		bids: map[int32]float32{}, asks: map[int32]float32{}, turnFloor: math.Inf(-1)}
	for i := range b.turnSec {
		b.turnSec[i] = math.MinInt64
	}
	return b
}

// ------------------------------------------------------------------ prices

// T is a price in ticks: int(round(price / tickSize)), half to even like Python's round.
func (b *Book) T(price float64) int32 {
	x := math.RoundToEven(price / b.TickSize)
	if x > maxTick {
		return maxTick
	}
	if x < -maxTick {
		return -maxTick
	}
	return int32(x)
}

// P is a tick as a price: round(tick * tickSize, 12).
func (b *Book) P(tick int32) float64 { return round12(float64(tick) * b.TickSize) }

// Px is a tick as the raw product tick * tickSize (Python's traded price in the bars).
func (b *Book) Px(tick int32) float64 { return float64(tick) * b.TickSize }

func round12(x float64) float64 { return math.Round(x*1e12) / 1e12 }

// Levels converts [price, qty] pairs to ticks.
func (b *Book) Levels(pq [][2]float64) []Level {
	out := make([]Level, 0, len(pq))
	for _, x := range pq {
		out = append(out, Level{b.T(x[0]), float32(x[1])})
	}
	return out
}

// ------------------------------------------------------------------ levels

func (b *Book) side(s Side) map[int32]float32 {
	if s == Bid {
		return b.bids
	}
	return b.asks
}

// QtyAt is the qty resting at tick k on side s (0 when none).
func (b *Book) QtyAt(s Side, k int32) float64 { return float64(b.side(s)[k]) }

// SetLevel sets one level; qty 0 removes it.
func (b *Book) SetLevel(s Side, k int32, qty float64) {
	m := b.side(s)
	if qty == 0 {
		delete(m, k)
		if b.bestOK[s] && b.best[s] == k {
			b.bestOK[s] = false
		}
		return
	}
	m[k] = float32(qty)
	if b.bestOK[s] {
		if len(m) == 1 || (s == Bid && k > b.best[s]) || (s == Ask && k < b.best[s]) {
			b.best[s] = k
		}
	}
}

// SetSide replaces a whole side (tests).
func (b *Book) SetSide(s Side, levels map[int32]float64) {
	m := make(map[int32]float32, len(levels))
	for k, q := range levels {
		if q != 0 {
			m[k] = float32(q)
		}
	}
	if s == Bid {
		b.bids = m
	} else {
		b.asks = m
	}
	b.bestOK[s] = false
}

// RangeLevels calls fn for every level of side s, in no particular order.
func (b *Book) RangeLevels(s Side, fn func(k int32, qty float64)) {
	for k, q := range b.side(s) {
		fn(k, float64(q))
	}
}

// LevelCount is the number of levels on side s.
func (b *Book) LevelCount(s Side) int { return len(b.side(s)) }

func (b *Book) bestOf(s Side) (int32, bool) {
	m := b.side(s)
	if len(m) == 0 {
		return 0, false
	}
	if !b.bestOK[s] {
		first := true
		var best int32
		for k := range m {
			if first || (s == Bid && k > best) || (s == Ask && k < best) {
				best, first = k, false
			}
		}
		b.best[s], b.bestOK[s] = best, true
	}
	return b.best[s], true
}

// BestBid is the highest bid tick.
func (b *Book) BestBid() (int32, bool) { return b.bestOf(Bid) }

// BestAsk is the lowest ask tick.
func (b *Book) BestAsk() (int32, bool) { return b.bestOf(Ask) }

func (b *Book) mid2() (int32, bool) {
	bb, ok1 := b.BestBid()
	ba, ok2 := b.BestAsk()
	if !ok1 || !ok2 || bb >= ba {
		return 0, false
	}
	return bb + ba, true
}

func (b *Book) fromM2(m2 int32) float64 { return float64(m2) / 2 * b.TickSize }

// Mid is (best bid + best ask) / 2 as a price; false without a valid, uncrossed book.
func (b *Book) Mid() (float64, bool) {
	m2, ok := b.mid2()
	if !ok {
		return 0, false
	}
	return b.fromM2(m2), true
}

// ------------------------------------------------------------------ sync

// Reset clears the book before a resync.
func (b *Book) Reset() {
	clear(b.bids)
	clear(b.asks)
	b.bestOK = [2]bool{}
	b.hasLastU = false
	b.Synced = false
	b.snapshotID = 0
	b.Pending = nil
}

// LoadSnapshot applies a REST snapshot, then replays the buffered events. False if the buffer does not bridge it.
func (b *Book) LoadSnapshot(lastUpdateID int64, bids, asks []Level) bool {
	b.bids = make(map[int32]float32, len(bids))
	b.asks = make(map[int32]float32, len(asks))
	for _, l := range bids {
		if l.Qty > 0 {
			b.bids[l.Tick] = l.Qty
		}
	}
	for _, l := range asks {
		if l.Qty > 0 {
			b.asks[l.Tick] = l.Qty
		}
	}
	b.bestOK = [2]bool{}
	b.snapshotID = lastUpdateID
	b.hasLastU = false
	buf := b.Pending
	b.Pending = nil
	b.Synced = true
	for _, ev := range buf {
		if !b.OnDepth(ev) {
			return false
		}
	}
	return true
}

// OnDepth applies one diff event. False when the book must be resynced (gap).
func (b *Book) OnDepth(ev DepthEvent) bool {
	if !b.Synced {
		if len(b.Pending) >= maxPending { // a snapshot that never comes: keep the newest events only
			n := copy(b.Pending, b.Pending[len(b.Pending)/2:])
			b.Pending = b.Pending[:n]
		}
		b.Pending = append(b.Pending, ev)
		return true
	}
	if !b.hasLastU { // first event after the snapshot
		if ev.LastU < b.snapshotID {
			return true // older than the snapshot: drop
		}
		if ev.FirstU > b.snapshotID {
			b.Reset() // gap between snapshot and stream
			return false
		}
	} else if ev.PrevU != b.lastU {
		b.Reset()
		return false
	}
	for _, l := range ev.Bids {
		b.SetLevel(Bid, l.Tick, float64(l.Qty))
	}
	for _, l := range ev.Asks {
		b.SetLevel(Ask, l.Tick, float64(l.Qty))
	}
	b.lastU, b.hasLastU = ev.LastU, true
	b.LastTs = float64(ev.E) / 1000
	b.sampleMid(b.LastTs)
	return true
}

// ------------------------------------------------------------------ mids

func (b *Book) sampleMid(ts float64) {
	if b.midN > 0 {
		last := b.midMs[(b.midHead+b.midN-1)%midsMax]
		if ts-float64(last)/1000 < 0.25 {
			return
		}
	}
	m2, ok := b.mid2()
	if ok && m2 != 0 {
		b.pushMid(int64(math.Round(ts*1000)), m2)
	}
}

func (b *Book) pushMid(ms int64, m2 int32) {
	i := (b.midHead + b.midN) % midsMax
	if b.midN == midsMax {
		b.midHead = (b.midHead + 1) % midsMax
	} else {
		b.midN++
	}
	b.midMs[i], b.midM2[i] = ms, m2
}

// PushMid appends a mid sample (tests, fixtures); mid is rounded to the half-tick grid.
func (b *Book) PushMid(ts, mid float64) {
	b.pushMid(int64(math.Round(ts*1000)), int32(math.RoundToEven(2*mid/b.TickSize)))
}

// MidsBackward yields (ts, mid) from the newest sample to the oldest.
func (b *Book) MidsBackward() iter.Seq2[float64, float64] {
	return func(yield func(float64, float64) bool) {
		for i := b.midN - 1; i >= 0; i-- {
			j := (b.midHead + i) % midsMax
			if !yield(float64(b.midMs[j])/1000, b.fromM2(b.midM2[j])) {
				return
			}
		}
	}
}

// MidCount is the number of mid samples kept.
func (b *Book) MidCount() int { return b.midN }

// MidAgo is the mid about seconds ago: the newest sample not newer than now - seconds.
func (b *Book) MidAgo(now, seconds float64) (float64, bool) {
	target := now - seconds
	for ts, m := range b.MidsBackward() {
		if ts <= target {
			return m, true
		}
	}
	return 0, false
}

// PriceAgo is MidAgo, or the middle of the 1m bar of that minute when the samples do not reach that far back.
func (b *Book) PriceAgo(now, seconds float64) (float64, bool) {
	if m, ok := b.MidAgo(now, seconds); ok {
		return m, true
	}
	return b.BarAgo(now, seconds)
}

// ------------------------------------------------------------------ bars

func floorDiv(a, n int64) int64 {
	q := a / n
	if (a%n != 0) && ((a < 0) != (n < 0)) {
		q--
	}
	return q
}

// Bars are the 1m bars, oldest first (a view: do not keep it).
func (b *Book) Bars() []Bar { return b.bars }

// H1 are the hour closes, oldest first (a view: do not keep it).
func (b *Book) H1() []HourClose { return b.h1 }

// AppendBar appends a bar (deque.append: the oldest goes when full).
func (b *Book) AppendBar(x Bar) {
	if len(b.bars) == barsMax {
		copy(b.bars, b.bars[1:])
		b.bars[len(b.bars)-1] = x
		return
	}
	b.bars = append(b.bars, x)
}

// PrependBars puts bars (oldest first) in front (deque.extendleft: the newest go when full).
func (b *Book) PrependBars(xs []Bar) {
	nb := make([]Bar, 0, min(len(xs)+len(b.bars), barsMax))
	nb = append(nb, xs...)
	nb = append(nb, b.bars...)
	if len(nb) > barsMax {
		nb = nb[:barsMax]
	}
	b.bars = nb
}

// AppendH1 appends an hour close (the oldest goes when full).
func (b *Book) AppendH1(x HourClose) {
	if len(b.h1) == h1Max {
		copy(b.h1, b.h1[1:])
		b.h1[len(b.h1)-1] = x
		return
	}
	b.h1 = append(b.h1, x)
}

// PrependH1 puts hour closes (oldest first) in front (the newest go when full).
func (b *Book) PrependH1(xs []HourClose) {
	nh := make([]HourClose, 0, min(len(xs)+len(b.h1), h1Max))
	nh = append(nh, xs...)
	nh = append(nh, b.h1...)
	if len(nh) > h1Max {
		nh = nh[:h1Max]
	}
	b.h1 = nh
}

// ClearH1 drops the hour closes (tests).
func (b *Book) ClearH1() { b.h1 = b.h1[:0] }

func (b *Book) bar(ts float64, tick int32) {
	m := int32(math.Floor(ts / 60))
	if n := len(b.bars); n > 0 && b.bars[n-1].Min == m {
		x := &b.bars[n-1]
		x.Hi, x.Lo = max(x.Hi, tick), min(x.Lo, tick)
	} else if n == 0 || m > b.bars[n-1].Min {
		b.AppendBar(Bar{m, tick, tick})
	}
	h := int32(math.Floor(ts / 3600))
	if n := len(b.h1); n > 0 && b.h1[n-1].Hour == h {
		b.h1[n-1].Close = tick
	} else if n == 0 || h > b.h1[n-1].Hour {
		b.AppendH1(HourClose{h, tick})
	}
}

// SeedBars puts 1m klines older than our own tape in front of the bars.
func (b *Book) SeedBars(rows []Kline) {
	var old []Bar
	for _, k := range rows {
		m := int32(floorDiv(k.OpenMs, 60_000))
		if len(b.bars) == 0 || m < b.bars[0].Min {
			old = append(old, Bar{m, b.T(k.High), b.T(k.Low)})
		}
	}
	b.PrependBars(old)
}

// SeedH1 puts 1h klines older than our own tape in front of the hour closes.
func (b *Book) SeedH1(rows []Kline) {
	var old []HourClose
	for _, k := range rows {
		h := int32(floorDiv(k.OpenMs, 3_600_000))
		if len(b.h1) == 0 || h < b.h1[0].Hour {
			old = append(old, HourClose{h, b.T(k.Close)})
		}
	}
	b.PrependH1(old)
}

// BarAgo is the middle of the 1m bar of the minute seconds ago (false when the bars do not reach back).
func (b *Book) BarAgo(now, seconds float64) (float64, bool) {
	target := int32(math.Floor((now - seconds) / 60))
	for i := len(b.bars) - 1; i >= 0; i-- {
		x := b.bars[i]
		if x.Min <= target {
			return (b.Px(x.Hi) + b.Px(x.Lo)) / 2, true
		}
	}
	return 0, false
}

// ATR is the mean high - low of the last n complete 1m bars (the range, not the true range).
func (b *Book) ATR(now float64, n int) (float64, bool) {
	cur := int32(math.Floor(now / 60))
	sum, k := 0.0, 0
	for i := len(b.bars) - 1; i >= 0 && k < n; i-- {
		x := b.bars[i]
		if x.Min < cur {
			sum += b.Px(x.Hi) - b.Px(x.Lo)
			k++
		}
	}
	if k != n || n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// Trend is +1 / -1 when the last hour close is above / below the EMA(n) of the hour closes; false without n hours.
func (b *Book) Trend(n int) (int, bool) { return b.trend(n, math.Inf(1)) }

// TrendBefore is Trend over the hours that closed by `before` (the trend BEFORE a move that started then).
func (b *Book) TrendBefore(n int, before float64) (int, bool) { return b.trend(n, before) }

func (b *Book) trend(n int, before float64) (int, bool) {
	var closes []float64
	for _, x := range b.h1 {
		if float64((int64(x.Hour)+1)*3600) <= before {
			closes = append(closes, b.Px(x.Close))
		}
	}
	if len(closes) < n || len(closes) == 0 {
		return 0, false
	}
	e := closes[0]
	for _, c := range closes[1:] {
		e += 2 / float64(n+1) * (c - e)
	}
	last := closes[len(closes)-1]
	switch {
	case last > e:
		return 1, true
	case last < e:
		return -1, true
	}
	return 0, true
}

// ------------------------------------------------------------------ tape

// PushTrade appends a print to the tape only (OnTrade does the rest); trades older than TapeKeep go.
func (b *Book) PushTrade(tr Trade) {
	b.tape = append(b.tape, tr)
	cut := tr.Ms - int64(TapeKeep*1000)
	for b.tapeHead < len(b.tape) && b.tape[b.tapeHead].Ms < cut {
		b.tapeHead++
	}
	if b.tapeHead > 1024 && b.tapeHead*2 > len(b.tape) {
		n := copy(b.tape, b.tape[b.tapeHead:])
		b.tape = b.tape[:n]
		b.tapeHead = 0
	}
}

// Tape is the kept prints, oldest first (a view, valid until the next push).
func (b *Book) Tape() []Trade { return b.tape[b.tapeHead:] }

// OnTrade books one aggTrade: tape, the densities at that price, turnover and the bars. Returns the print.
func (b *Book) OnTrade(ms int64, price, qty float64, buyerMaker bool) Trade {
	tick := b.T(price)
	tr := NewTrade(ms, tick, qty, buyerMaker)
	b.PushTrade(tr)
	ts := tr.TS()
	b.LastTs = max(b.LastTs, ts)
	for _, s := range [2]Side{Bid, Ask} {
		if d := b.Densities[DKey{s, tick}]; d != nil {
			if buyerMaker {
				d.SellQty += qty
			} else {
				d.BuyQty += qty
			}
		}
	}
	px := float64(tick) * b.TickSize
	b.AddTurnover(ts, qty*px)
	b.bar(ts, tick)
	return tr
}

// Flow is the USDT traded by one aggressor side (sell = hits bids, buy = lifts asks) in the last seconds.
func (b *Book) Flow(now, seconds float64, sell bool) float64 {
	s := 0.0
	t := b.Tape()
	for i := len(t) - 1; i >= 0; i-- {
		tr := t[i]
		if tr.TS() < now-seconds {
			break
		}
		if tr.BuyerMaker() == sell {
			s += tr.Qty() * float64(tr.Tick)
		}
	}
	return s * b.TickSize
}

// FlowRate is the USDT a second that side hits the book with: the faster of the short and the long window.
func (b *Book) FlowRate(now float64, sell bool, short, long float64) float64 {
	return max(b.Flow(now, short, sell)/short, b.Flow(now, long, sell)/long)
}

// ------------------------------------------------------------------ turnover

func turnIdx(sec int64) int { return int(((sec % turnSlots) + turnSlots) % turnSlots) }

// AddTurnover books usd traded at ts into its 1-second bucket.
func (b *Book) AddTurnover(ts, usd float64) {
	sec := int64(math.Floor(ts))
	i := turnIdx(sec)
	if b.turnSec[i] != sec {
		b.turnSec[i], b.turnUSD[i] = sec, 0
	}
	b.turnUSD[i] += float32(usd)
	if !b.turnAny || ts > b.turnMax {
		b.turnMax, b.turnAny = ts, true
	}
	b.turnFloor = max(b.turnFloor, ts-turnWindow)
}

// turnSum sums the buckets that start at or after floor: (sum, start of the oldest bucket, any).
func (b *Book) turnSum(floor float64) (float64, float64, bool) {
	if !b.turnAny {
		return 0, 0, false
	}
	hi := int64(math.Floor(b.turnMax))
	lo := int64(math.Ceil(floor))
	if lo < hi-turnSlots+1 {
		lo = hi - turnSlots + 1
	}
	sum, oldest, any := 0.0, 0.0, false
	for s := lo; s <= hi; s++ {
		i := turnIdx(s)
		if b.turnSec[i] != s {
			continue
		}
		if !any {
			oldest, any = float64(s), true
		}
		sum += float64(b.turnUSD[i])
	}
	return sum, oldest, any
}

// Turnover is the USDT traded in the last seconds (at most the 15 min the buckets keep).
func (b *Book) Turnover(now, seconds float64) float64 {
	s, _, _ := b.turnSum(max(b.turnFloor, now-seconds))
	return s
}

// MinuteTurnover is the average traded USDT per minute over the last 15 min (or over what we have).
func (b *Book) MinuteTurnover(now float64) float64 {
	b.turnFloor = max(b.turnFloor, now-turnWindow)
	s, oldest, any := b.turnSum(b.turnFloor)
	if !any {
		return 0
	}
	return s / max(60.0, now-oldest) * 60
}

// ------------------------------------------------------------------ densities

func median(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

type lvUSD struct {
	k   int32
	usd float64
}

// ScanDensities refreshes the density table (call every ~0.5 s).
func (b *Book) ScanDensities(now float64) {
	mid, ok := b.Mid()
	if !ok {
		return
	}
	turn := b.MinuteTurnover(now)
	midT := mid / b.TickSize
	wide := max(DENS.BaseBand, DENS.Band)
	var lv []lvUSD
	var base []float64
	for _, s := range [2]Side{Bid, Ask} {
		lo, hi := midT*(1-wide), midT
		baseLo, baseHi := midT*(1-DENS.BaseBand), midT
		bandLo, bandHi := midT*(1-DENS.Band), midT
		if s == Ask {
			lo, hi = midT, midT*(1+wide)
			baseLo, baseHi = midT, midT*(1+DENS.BaseBand)
			bandLo, bandHi = midT, midT*(1+DENS.Band)
		}
		lv, base = lv[:0], base[:0]
		for k, q := range b.side(s) {
			fk := float64(k)
			if lo <= fk && fk <= hi {
				u := float64(q) * fk * b.TickSize
				lv = append(lv, lvUSD{k, u})
				if baseLo <= fk && fk <= baseHi {
					base = append(base, u)
				}
			}
		}
		if len(base) < 15 {
			continue
		}
		thr := max(DENS.Ratio*median(base), DENS.USDMin, DENS.MinuteK*turn)
		for _, x := range lv {
			fk := float64(x.k)
			if x.usd >= thr && bandLo <= fk && fk <= bandHi {
				key := DKey{s, x.k}
				d := b.Densities[key]
				if d == nil || d.Gone != "" {
					b.Densities[key] = NewDensity(s, x.k, b.P(x.k), now, x.usd, x.usd, now)
				} else {
					d.USD, d.LastSeen = x.usd, now
					d.PeakUSD = max(d.PeakUSD, x.usd)
				}
			}
		}
	}
	// update every alive density: size, touches, death
	bb, okB := b.BestBid()
	ba, okA := b.BestAsk()
	for key, d := range b.Densities {
		if d.Gone != "" {
			if now-d.GoneAt > 600 {
				delete(b.Densities, key)
			}
			continue
		}
		d.USD = b.QtyAt(d.Side, d.Tick) * d.Price
		d.PeakUSD = max(d.PeakUSD, d.USD) // also while it is under the detection threshold
		d.AddHist(now, d.USD)
		crossed := (d.Side == Bid && okA && ba <= d.Tick) || (d.Side == Ask && okB && bb >= d.Tick)
		if crossed || d.USD < DENS.GoneFrac*d.PeakUSD {
			traded := b.Eaten(d) * d.Price
			switch {
			case crossed:
				d.Gone = "crossed"
			case traded >= 0.5*(d.PeakUSD-d.USD):
				d.Gone = "eaten"
			default:
				d.Gone = "pulled"
			}
			d.GoneAt = now
			continue
		}
		dist := math.Abs(mid-d.Price) / d.Price
		if dist <= 0.0015 {
			d.Near = true
		} else if d.Near && dist >= 0.004 {
			d.Near = false
			d.Touches++
		}
	}
}

// Eaten is the qty traded at the density's price since it appeared (both aggressor sides).
func (b *Book) Eaten(d *Density) float64 { return d.BuyQty + d.SellQty }

// Alive are the densities that are not gone, ordered by side then tick.
func (b *Book) Alive() []*Density { return b.alive(false, Bid) }

// AliveSide are the alive densities of one side, ordered by tick.
func (b *Book) AliveSide(s Side) []*Density { return b.alive(true, s) }

func (b *Book) alive(one bool, s Side) []*Density {
	var out []*Density
	for _, d := range b.Densities {
		if d.Gone == "" && (!one || d.Side == s) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Side != out[j].Side {
			return out[i].Side < out[j].Side
		}
		return out[i].Tick < out[j].Tick
	})
	return out
}
