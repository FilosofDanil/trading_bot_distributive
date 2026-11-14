package live

import (
	"math"
	"strconv"
	"sync"
)

// StopOrder is a fake exchange stop.
type StopOrder struct {
	ID      string
	Trigger float64
}

// FakeExchange is the exchange the tests run the executor against: balances, an isolated short per symbol,
// exchange stops; Fail names methods that raise once. (It is plain code, so the runner tests can use it too.)
type FakeExchange struct {
	mu         sync.Mutex
	Px         map[string][2]float64 // perp mark, spot last
	Spot       map[string]float64
	Fut        float64 // futures available balance
	Pos        map[string]*Perp
	StopOrders map[string][]StopOrder
	Fail       map[string]bool
	Calls      []string
	N          int
}

// NewFake is FakeExchange(usdt, price) with the symbols XUSDT (at price) and YUSDT (at 2).
func NewFake(usdt, price float64) *FakeExchange {
	return &FakeExchange{Px: map[string][2]float64{"XUSDT": {price, price}, "YUSDT": {2, 2}},
		Spot: map[string]float64{"USDT": usdt}, Pos: map[string]*Perp{}, StopOrders: map[string][]StopOrder{},
		Fail: map[string]bool{}}
}

// Called counts how many times a method was called.
func (f *FakeExchange) Called(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *FakeExchange) check(name string) error {
	f.Calls = append(f.Calls, name)
	if f.Fail[name] {
		delete(f.Fail, name)
		return Errorf("%s failed (test)", name)
	}
	return nil
}

func (f *FakeExchange) Prices(sym string) (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.Px[sym]
	return p[0], p[1], nil
}

func (f *FakeExchange) Filters(sym string) (float64, float64, float64, float64, error) {
	return 0.001, 5.0, 0.001, 5.0, nil
}

func (f *FakeExchange) SpotFree(asset string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Spot[asset], nil
}

func (f *FakeExchange) FuturesAvailable() (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Fut, nil
}

func (f *FakeExchange) Transfer(amount float64, toFutures bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("transfer"); err != nil {
		return err
	}
	src := f.Fut
	if toFutures {
		src = f.Spot["USDT"]
	}
	if src < amount-1e-9 {
		return Errorf("insufficient balance")
	}
	if toFutures {
		f.Spot["USDT"] -= amount
		f.Fut += amount
	} else {
		f.Fut -= amount
		f.Spot["USDT"] += amount
	}
	return nil
}

func (f *FakeExchange) PrepareShort(sym string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.check("prepare_short")
}

func (f *FakeExchange) PerpOrder(sym, side string, qty float64, reduce bool) (Fill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("perp_order"); err != nil {
		return Fill{}, err
	}
	px := f.Px[sym][0]
	p := f.Pos[sym]
	if side == "SELL" {
		if f.Fut < qty*px {
			return Fill{}, Errorf("margin insufficient")
		}
		f.Fut -= qty * px * 1.0005
		if p == nil {
			f.Pos[sym] = &Perp{Amt: -qty, Entry: px, Mark: px, Liq: 0.0, Margin: qty * px}
		} else {
			p.Amt -= qty
			p.Margin += qty * px
		}
	} else {
		if p == nil || !reduce {
			panic("fake exchange: a BUY must reduce an open short")
		}
		share := qty / math.Abs(p.Amt)
		f.Fut += p.Margin*share + (p.Entry-px)*qty - qty*px*0.0005
		p.Margin *= 1 - share
		p.Amt += qty
		if math.Abs(p.Amt) < 1e-12 {
			delete(f.Pos, sym)
		}
	}
	f.liq(sym)
	return Fill{Qty: qty, Price: px, FeeUSDT: qty * px * 0.0005}, nil
}

func (f *FakeExchange) liq(sym string) {
	if p := f.Pos[sym]; p != nil { // short: liquidated when the loss eats the margin
		p.Liq = p.Entry + p.Margin/math.Abs(p.Amt)
		p.Mark = f.Px[sym][0]
	}
}

func (f *FakeExchange) SpotOrder(sym, side string, qty float64) (Fill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("spot_order"); err != nil {
		return Fill{}, err
	}
	px, base := f.Px[sym][1], BaseAsset(sym)
	if side == "BUY" {
		if f.Spot["USDT"] < qty*px {
			return Fill{}, Errorf("insufficient balance")
		}
		f.Spot["USDT"] -= qty * px
		f.Spot[base] += qty * 0.999
		return Fill{Qty: qty * 0.999, Price: px, FeeUSDT: qty * px * 0.001}, nil
	}
	f.Spot[base] -= qty
	f.Spot["USDT"] += qty * px * 0.999
	return Fill{Qty: qty, Price: px, FeeUSDT: qty * px * 0.001}, nil
}

func (f *FakeExchange) AddMargin(sym string, amount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("add_margin"); err != nil {
		return err
	}
	if f.Fut < amount-1e-9 {
		return Errorf("-4050")
	}
	f.Fut -= amount
	f.Pos[sym].Margin += amount
	f.liq(sym)
	return nil
}

func (f *FakeExchange) Positions() (map[string]Perp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]Perp{}
	for s, p := range f.Pos {
		f.liq(s)
		out[s] = *p
	}
	return out, nil
}

func (f *FakeExchange) Stops(syms []string) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for _, s := range syms {
		for _, o := range f.StopOrders[s] {
			out[s] = append(out[s], o.ID)
		}
	}
	return out, nil
}

func (f *FakeExchange) PlaceStop(sym string, trigger float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("place_stop"); err != nil {
		return "", err
	}
	f.N++
	id := strconv.Itoa(f.N)
	f.StopOrders[sym] = append(f.StopOrders[sym], StopOrder{id, trigger})
	return id, nil
}

func (f *FakeExchange) CancelStops(sym string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.StopOrders, sym)
	return nil
}

func (f *FakeExchange) Funding(sym string, sinceMS int64) (float64, error) { return 0.0, nil }
