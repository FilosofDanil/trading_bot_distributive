// Package feed is the market data (bounce/feed.py): a screener picks the symbols to watch, and each watched symbol
// gets two public websockets, diff depth (local order book) and aggTrade (tape).
//
// Locking: Feed works under the runner's engine lock (Mu). Network I/O, sleeping and JSON parsing happen outside
// it; the lock is taken only to touch the trader, the books and the feed's own state.
//
// Differences from the Python feed: (1) a book that stays unsynced with no snapshot in flight for more than 30 s
// gets a new snapshot task (Python gave up after 20 failed attempts until the next reconnect); (2) the JSON of the
// hot path is decoded into small typed structs outside the lock.
package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"densitybounce/internal/book"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/setup"
	"densitybounce/internal/trader"
)

// Endpoints and screener settings.
const (
	REST = "https://fapi.binance.com"
	WS   = "wss://fstream.binance.com"
)

// Screen is the screener's configuration.
var Screen = struct {
	N      int
	VolMin float64
	VolMax float64
	Every  time.Duration
	Never  map[string]bool
}{16, 100e6, 3e9, 300 * time.Second,
	map[string]bool{"BTCUSDT": true, "ETHUSDT": true, "USDCUSDT": true, "FDUSDUSDT": true, "BTCDOMUSDT": true}}

// ParseInfo reads exchangeInfo: the perpetual USDT contracts that are trading, with their filters.
func ParseInfo(raw []byte) (map[string]trader.SymInfo, error) {
	var in struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			ContractType string `json:"contractType"`
			QuoteAsset   string `json:"quoteAsset"`
			Status       string `json:"status"`
			Filters      []struct {
				FilterType string `json:"filterType"`
				TickSize   string `json:"tickSize"`
				StepSize   string `json:"stepSize"`
				MinQty     string `json:"minQty"`
				Notional   string `json:"notional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := map[string]trader.SymInfo{}
	for _, s := range in.Symbols {
		if s.ContractType != "PERPETUAL" || s.QuoteAsset != "USDT" || s.Status != "TRADING" {
			continue
		}
		f := map[string]int{} // the last filter of a type wins, like Python's dict
		for i, x := range s.Filters {
			f[x.FilterType] = i
		}
		pf, ok1 := f["PRICE_FILTER"]
		ls, ok2 := f["LOT_SIZE"]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%s: no PRICE_FILTER / LOT_SIZE", s.Symbol)
		}
		tick, err := strconv.ParseFloat(s.Filters[pf].TickSize, 64)
		if err != nil {
			return nil, fmt.Errorf("%s tickSize: %w", s.Symbol, err)
		}
		step, err := strconv.ParseFloat(s.Filters[ls].StepSize, 64)
		if err != nil {
			return nil, fmt.Errorf("%s stepSize: %w", s.Symbol, err)
		}
		minQty, err := strconv.ParseFloat(s.Filters[ls].MinQty, 64)
		if err != nil {
			return nil, fmt.Errorf("%s minQty: %w", s.Symbol, err)
		}
		minNot := 5.0
		if mn, ok := f["MIN_NOTIONAL"]; ok && s.Filters[mn].Notional != "" {
			if minNot, err = strconv.ParseFloat(s.Filters[mn].Notional, 64); err != nil {
				return nil, fmt.Errorf("%s notional: %w", s.Symbol, err)
			}
		}
		out[s.Symbol] = trader.SymInfo{Tick: tick, Step: step, MinQty: minQty, MinNotional: minNot}
	}
	return out, nil
}

// Ticker is one row of /fapi/v1/ticker/24hr.
type Ticker struct {
	Symbol             string `json:"symbol"`
	QuoteVolume        string `json:"quoteVolume"`
	LastPrice          string `json:"lastPrice"`
	PriceChangePercent string `json:"priceChangePercent"`
}

type row struct {
	sym    string
	qv, ch float64
}

// Pick are the liquid, fine-tick perps that are the most active AND moving coins of the day, plus keep. Python's
// sorted() is stable: the ranks and the order of equal keys are reproduced with sort.SliceStable.
func Pick(tickers []Ticker, info map[string]trader.SymInfo, keep, exclude map[string]bool, n int) ([]string, error) {
	var rows []row
	for _, t := range tickers {
		s := t.Symbol
		if _, ok := info[s]; !ok || Screen.Never[s] || exclude[s] {
			continue
		}
		qv, err := strconv.ParseFloat(t.QuoteVolume, 64)
		if err != nil {
			return nil, fmt.Errorf("%s quoteVolume: %w", s, err)
		}
		last, err := strconv.ParseFloat(t.LastPrice, 64)
		if err != nil {
			return nil, fmt.Errorf("%s lastPrice: %w", s, err)
		}
		if !(Screen.VolMin <= qv && qv <= Screen.VolMax) || last <= 0 {
			continue
		}
		if info[s].Tick/last > setup.P.TickMax {
			continue
		}
		ch, err := strconv.ParseFloat(t.PriceChangePercent, 64)
		if err != nil {
			return nil, fmt.Errorf("%s priceChangePercent: %w", s, err)
		}
		if ch < 0 {
			ch = -ch
		}
		rows = append(rows, row{s, qv, ch})
	}
	rank := func(key func(row) float64) map[string]int {
		r := append([]row(nil), rows...)
		sort.SliceStable(r, func(i, j int) bool { return key(r[i]) < key(r[j]) })
		m := make(map[string]int, len(r))
		for i, x := range r {
			m[x.sym] = i
		}
		return m
	}
	byV := rank(func(r row) float64 { return r.qv })
	byC := rank(func(r row) float64 { return r.ch })
	ranked := append([]row(nil), rows...)
	score := func(r row) float64 { return -(float64(byV[r.sym]) + 1.5*float64(byC[r.sym])) }
	sort.SliceStable(ranked, func(i, j int) bool { return score(ranked[i]) < score(ranked[j]) })
	set := map[string]bool{}
	for i, r := range ranked {
		if i >= n {
			break
		}
		set[r.sym] = true
	}
	for k := range keep {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Health is the feed's part of GET /v1/health.
type Health struct {
	Watching  int     `json:"watching"`
	Synced    int     `json:"synced"`
	MaxLagS   float64 `json:"max_lag_s"`
	Msgs      int64   `json:"msgs"`
	Resyncs   int64   `json:"resyncs"`
	Densities int     `json:"densities"`
}

// Feed watches the symbols the screener picks.
type Feed struct {
	T *trader.Trader
	// Mu is the engine lock.
	Mu sync.Locker
	// OnTrade is SimBroker.OnTrade in paper mode (called under the lock).
	OnTrade func(sym string, tr book.Trade)
	N       int
	Rest    string // REST base URL
	WSBase  string // websocket base URL
	HTTP    *http.Client
	Now     func() time.Time

	ctx context.Context

	// state under Mu
	tasks      map[string]context.CancelFunc
	msgs       int64
	resyncs    int64
	inflight   map[string]bool
	snapEnd    map[string]time.Time
	watching   []string
	screenedAt time.Time
}

// New makes a feed. n <= 0: WATCH env or 16.
func New(t *trader.Trader, mu sync.Locker, onTrade func(string, book.Trade), n int) *Feed {
	if n <= 0 {
		n, _ = strconv.Atoi(os.Getenv("WATCH"))
	}
	if n <= 0 {
		n = Screen.N
	}
	return &Feed{T: t, Mu: mu, OnTrade: onTrade, N: n, Rest: REST, WSBase: WS, HTTP: &http.Client{Timeout: 15 * time.Second},
		Now: time.Now, ctx: context.Background(), tasks: map[string]context.CancelFunc{}, inflight: map[string]bool{},
		snapEnd: map[string]time.Time{}}
}

// Watching is the symbols being watched (under the lock).
func (f *Feed) Watching() []string { return f.watching }

// SetContext sets the context all goroutines of the feed end with (call before Run / SetSymbols).
func (f *Feed) SetContext(ctx context.Context) { f.ctx = ctx }

// Get fetches a REST path and returns the body; a status >= 400 is an error.
func (f *Feed) Get(path string, params map[string]string) ([]byte, error) {
	u := f.Rest + path
	if len(params) > 0 {
		v := url.Values{}
		for k, x := range params {
			v.Set(k, x)
		}
		u += "?" + v.Encode()
	}
	req, err := http.NewRequestWithContext(f.ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, path)
	}
	return b, nil
}

// Screener runs the screen now and then every 300 s until the context ends. exclude is called under the lock.
func (f *Feed) Screener(exclude func() map[string]bool) {
	for {
		if err := f.ScreenOnce(exclude); err != nil {
			log.Printf("WARNING screener: %v", err)
			f.Mu.Lock()
			f.T.Store.Event("warn", fmt.Sprintf("screener: %v", err), false)
			f.Mu.Unlock()
		}
		select {
		case <-f.ctx.Done():
			return
		case <-time.After(Screen.Every):
		}
	}
}

// ScreenOnce fetches exchangeInfo and the 24h tickers and applies the pick. Called WITHOUT the lock.
func (f *Feed) ScreenOnce(exclude func() map[string]bool) error {
	raw, err := f.Get("/fapi/v1/exchangeInfo", nil)
	if err != nil {
		return err
	}
	info, err := ParseInfo(raw)
	if err != nil {
		return err
	}
	f.Mu.Lock()
	for s, i := range info {
		f.T.Info[s] = i
	}
	f.Mu.Unlock()
	tick, err := f.Get("/fapi/v1/ticker/24hr", nil)
	if err != nil {
		return err
	}
	var tickers []Ticker
	if err := json.Unmarshal(tick, &tickers); err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var ex map[string]bool
	if exclude != nil {
		ex = exclude()
	}
	want, err := Pick(tickers, info, f.T.Keep(), ex, f.N)
	if err != nil {
		return err
	}
	if !equalStrs(want, f.watching) {
		log.Printf("INFO watching %d: %v", len(want), want)
	}
	f.SetSymbols(want)
	chg := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		if v, err := strconv.ParseFloat(t.PriceChangePercent, 64); err == nil {
			chg[t.Symbol] = pyfmt.Round(v/100, 4)
		} else {
			return err
		}
	}
	for _, b := range f.T.Books() {
		if v, ok := chg[b.Symbol]; ok {
			x := v
			b.Ch24 = &x
		} else {
			b.Ch24 = nil
		}
	}
	f.screenedAt = f.Now()
	return nil
}

// Rescreen is POST /run's re-screen: the tickers only, with the info already known. Called WITHOUT the lock.
func (f *Feed) Rescreen() error {
	tick, err := f.Get("/fapi/v1/ticker/24hr", nil)
	if err != nil {
		return err
	}
	var tickers []Ticker
	if err := json.Unmarshal(tick, &tickers); err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	want, err := Pick(tickers, f.T.Info, f.T.Keep(), f.T.BlockedSyms, f.N)
	if err != nil {
		return err
	}
	f.SetSymbols(want)
	return nil
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SetSymbols starts the books and streams of new symbols and stops the ones no longer wanted. Under the lock.
func (f *Feed) SetSymbols(want []string) {
	keep := f.T.Keep()
	wanted := make(map[string]bool, len(want))
	for _, s := range want {
		wanted[s] = true
	}
	for _, s := range sortedTasks(f.tasks) {
		if !wanted[s] && !keep[s] {
			f.tasks[s]()
			delete(f.tasks, s)
			f.T.RemoveBook(s)
		}
	}
	for _, s := range want {
		if _, ok := f.tasks[s]; ok {
			continue
		}
		info, ok := f.T.Info[s]
		if !ok {
			log.Printf("WARNING %s has no exchange info, not watched", s)
			continue
		}
		b := book.New(s, info.Tick)
		f.T.AddBook(s, b)
		ctx, cancel := context.WithCancel(f.ctx)
		f.tasks[s] = cancel
		go f.seed(b)
		go f.runSymbol(ctx, s)
	}
	f.watching = sortedTasks(f.tasks)
}

func sortedTasks(m map[string]context.CancelFunc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seed: the last hour of 1m bars, so a move that began before the symbol was added is measured whole (and the ATR
// of the stop is known at once), and 48 hour closes for the 1h trend.
func (f *Feed) seed(b *book.Book) {
	for _, c := range []struct {
		interval string
		limit    int
		put      func([]book.Kline)
	}{{"1m", 60, b.SeedBars}, {"1h", 48, b.SeedH1}} {
		raw, err := f.Get("/fapi/v1/klines", map[string]string{"symbol": b.Symbol, "interval": c.interval,
			"limit": strconv.Itoa(c.limit)})
		var rows [][]json.Number
		if err == nil {
			err = json.Unmarshal(raw, &rows)
		}
		var ks []book.Kline
		for _, r := range rows {
			if err != nil {
				break
			}
			if len(r) < 5 {
				err = fmt.Errorf("short kline row")
				break
			}
			var k book.Kline
			var e1, e2, e3 error
			k.OpenMs, e1 = r[0].Int64()
			k.High, e2 = r[2].Float64()
			k.Low, e3 = r[3].Float64()
			k.Close, err = r[4].Float64()
			if e1 != nil || e2 != nil || e3 != nil {
				err = fmt.Errorf("bad kline row")
			}
			ks = append(ks, k)
		}
		if err != nil {
			log.Printf("WARNING %s klines %s: %v", b.Symbol, c.interval, err)
			continue
		}
		f.Mu.Lock()
		c.put(ks)
		f.Mu.Unlock()
	}
}

// spawnSnapshot starts a snapshot task unless one runs for the symbol. Under the lock.
func (f *Feed) spawnSnapshot(b *book.Book) {
	if !f.inflight[b.Symbol] {
		f.inflight[b.Symbol] = true
		go f.snapshot(b)
	}
}

type snapMsg struct {
	LastUpdateID int64       `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

func levels(b *book.Book, pq [][2]string) ([]book.Level, error) {
	out := make([]book.Level, 0, len(pq))
	for _, x := range pq {
		p, err := strconv.ParseFloat(x[0], 64)
		if err != nil {
			return nil, err
		}
		q, err := strconv.ParseFloat(x[1], 64)
		if err != nil {
			return nil, err
		}
		out = append(out, book.Level{Tick: b.T(p), Qty: float32(q)})
	}
	return out, nil
}

func (f *Feed) sleep(d time.Duration) bool {
	select {
	case <-f.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// snapshot fetches a REST snapshot and bridges it to the buffered stream; retries until it bridges.
func (f *Feed) snapshot(b *book.Book) {
	defer func() {
		f.Mu.Lock()
		delete(f.inflight, b.Symbol)
		f.snapEnd[b.Symbol] = f.Now()
		f.Mu.Unlock()
	}()
	for attempt := 0; attempt < 20; attempt++ {
		if !f.sleep(500*time.Millisecond + time.Duration(attempt)*time.Second) { // let events buffer first
			return
		}
		f.Mu.Lock()
		same := f.T.Book(b.Symbol) == b
		f.Mu.Unlock()
		if !same {
			return
		}
		raw, err := f.Get("/fapi/v1/depth", map[string]string{"symbol": b.Symbol, "limit": "1000"})
		var snap snapMsg
		var bids, asks []book.Level
		if err == nil {
			err = json.Unmarshal(raw, &snap)
		}
		if err == nil {
			if bids, err = levels(b, snap.Bids); err == nil {
				asks, err = levels(b, snap.Asks)
			}
		}
		if err != nil {
			log.Printf("WARNING %s snapshot: %v", b.Symbol, err)
			continue
		}
		f.Mu.Lock()
		if b.Synced { // a gap may have reset it meanwhile
			f.Mu.Unlock()
			return
		}
		ok := b.LoadSnapshot(snap.LastUpdateID, bids, asks)
		if !ok {
			f.resyncs++
		}
		f.Mu.Unlock()
		if ok {
			return
		}
	}
}

// Watchdog spawns a new snapshot for a book that is unsynced with no snapshot in flight for more than 30 s (the
// documented improvement over the Python feed). Runs until the context ends.
func (f *Feed) Watchdog() {
	for f.sleep(5 * time.Second) {
		f.Mu.Lock()
		for _, b := range f.T.Books() {
			if b.Synced || f.inflight[b.Symbol] {
				continue
			}
			if end, ok := f.snapEnd[b.Symbol]; ok && f.Now().Sub(end) > 30*time.Second {
				f.spawnSnapshot(b)
			}
		}
		f.Mu.Unlock()
	}
}

func (f *Feed) runSymbol(ctx context.Context, sym string) {
	low := lower(sym)
	// since 2026-04-23 Binance routes depth to /public and aggTrade to /market; a wrong route is SILENT
	go f.loop(ctx, sym, f.WSBase+"/public/ws/"+low+"@depth@100ms", true)
	go f.loop(ctx, sym, f.WSBase+"/market/ws/"+low+"@aggTrade", false)
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// encoding/json matches keys case-insensitively when there is no exact tag: "e" (a string) would land in the E field
// (a number). Both structs therefore declare both "e" and "E".
type depthMsg struct {
	Ev string      `json:"e"`
	E  int64       `json:"E"`
	U  int64       `json:"U"`
	Lu int64       `json:"u"`
	Pu *int64      `json:"pu"`
	B  [][2]string `json:"b"`
	A  [][2]string `json:"a"`
}

type tradeMsg struct {
	P  string `json:"p"`
	Q  string `json:"q"`
	T  int64  `json:"T"`
	M  bool   `json:"m"`
	Ev string `json:"e"`
	E  int64  `json:"E"`
}

func (f *Feed) loop(ctx context.Context, sym, wsURL string, depth bool) {
	backoff := time.Second
	quiet := 300 * time.Second // no message for this long = dead / silent stream
	if depth {
		quiet = 30 * time.Second
	}
	for ctx.Err() == nil {
		f.Mu.Lock()
		b := f.T.Book(sym)
		if b == nil {
			f.Mu.Unlock()
			return
		}
		if depth {
			b.Reset()
		}
		f.Mu.Unlock()
		err := f.session(ctx, b, sym, wsURL, depth, quiet, &backoff)
		if ctx.Err() != nil {
			return
		}
		kind := "trade"
		if depth {
			kind = "depth"
		}
		log.Printf("WARNING %s %s ws: %v (reconnect in %.0fs)", sym, kind, err, backoff.Seconds())
		if !f.sleep(backoff + time.Duration(rand.Float64()*float64(time.Second))) {
			return
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (f *Feed) session(ctx context.Context, b *book.Book, sym, wsURL string, depth bool, quiet time.Duration,
	backoff *time.Duration) error {
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, _, err := d.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 23)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // closing the connection ends the blocked read when the symbol is dropped
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())
	conn.SetPongHandler(func(string) error { lastPong.Store(time.Now().UnixNano()); return nil })
	go func() { // ping every 20 s; no pong within 20 s = dead connection
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastPong.Load())) > 40*time.Second {
					conn.Close()
					return
				}
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	started := false
	for {
		_ = conn.SetReadDeadline(time.Now().Add(quiet))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if depth {
			var m depthMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			ev := book.DepthEvent{FirstU: m.U, LastU: m.Lu, PrevU: -1, E: m.E}
			if m.Pu != nil {
				ev.PrevU = *m.Pu
			}
			if ev.Bids, err = levels(b, m.B); err != nil {
				return err
			}
			if ev.Asks, err = levels(b, m.A); err != nil {
				return err
			}
			f.Mu.Lock()
			f.msgs++
			if !started {
				started = true
				f.spawnSnapshot(b)
			}
			if !b.OnDepth(ev) {
				f.resyncs++
				log.Printf("INFO %s book gap -> resync", sym)
				b.Pending = append(b.Pending, ev)
				f.spawnSnapshot(b)
			}
			f.Mu.Unlock()
		} else {
			var m tradeMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			p, err := strconv.ParseFloat(m.P, 64)
			if err != nil {
				return err
			}
			q, err := strconv.ParseFloat(m.Q, 64)
			if err != nil {
				return err
			}
			f.Mu.Lock()
			f.msgs++
			tr := b.OnTrade(m.T, p, q, m.M)
			if f.OnTrade != nil {
				f.OnTrade(sym, tr)
			}
			f.Mu.Unlock()
		}
		*backoff = time.Second
	}
}

// Health is the feed's view (under the lock).
func (f *Feed) Health() Health {
	now := float64(f.Now().UnixNano()) / 1e9
	h := Health{Msgs: f.msgs, Resyncs: f.resyncs}
	lag := 0.0
	for _, b := range f.T.Books() {
		h.Watching++
		if b.Synced {
			h.Synced++
		}
		if d := now - b.LastTs; d > lag || h.Watching == 1 {
			lag = d
		}
		h.Densities += len(b.Alive())
	}
	h.MaxLagS = pyfmt.Round(lag, 1)
	return h
}
