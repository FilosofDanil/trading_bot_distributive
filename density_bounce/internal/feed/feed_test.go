package feed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"densitybounce/internal/book"
	"densitybounce/internal/state"
	"densitybounce/internal/testkit"
	"densitybounce/internal/trader"
)

const infoJSON = `{"symbols":[
 {"symbol":"AUSDT","contractType":"PERPETUAL","quoteAsset":"USDT","status":"TRADING","filters":[
   {"filterType":"PRICE_FILTER","tickSize":"0.00010"},{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1"},
   {"filterType":"MIN_NOTIONAL","notional":"5"}]},
 {"symbol":"BUSDT","contractType":"PERPETUAL","quoteAsset":"USDT","status":"TRADING","filters":[
   {"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001"}]},
 {"symbol":"CUSDT_240628","contractType":"CURRENT_QUARTER","quoteAsset":"USDT","status":"TRADING","filters":[]},
 {"symbol":"DBUSD","contractType":"PERPETUAL","quoteAsset":"BUSD","status":"TRADING","filters":[]},
 {"symbol":"EUSDT","contractType":"PERPETUAL","quoteAsset":"USDT","status":"SETTLING","filters":[]}]}`

func TestParseInfo(t *testing.T) {
	info, err := ParseInfo([]byte(infoJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 {
		t.Fatalf("%v", info)
	}
	if a := info["AUSDT"]; a != (trader.SymInfo{Tick: 0.0001, Step: 1, MinQty: 1, MinNotional: 5}) {
		t.Fatalf("A %+v", a)
	}
	if b := info["BUSDT"]; b != (trader.SymInfo{Tick: 0.01, Step: 0.001, MinQty: 0.001, MinNotional: 5}) { // default notional 5
		t.Fatalf("B %+v", b)
	}
	if _, err := ParseInfo([]byte(`{"symbols":[{"symbol":"X","contractType":"PERPETUAL","quoteAsset":"USDT","status":"TRADING","filters":[]}]}`)); err == nil {
		t.Fatal("a missing PRICE_FILTER is an error (Python KeyError)")
	}
}

func tk(sym string, qv, last, ch string) Ticker {
	return Ticker{Symbol: sym, QuoteVolume: qv, LastPrice: last, PriceChangePercent: ch}
}

func TestPickRanksByVolumeAndMoveStably(t *testing.T) {
	testkit.PinP(t)
	info := map[string]trader.SymInfo{}
	for _, s := range []string{"A", "B", "C", "D", "E", "F", "G", "BTCUSDT", "THIN", "GIANT", "COARSE", "ZZ"} {
		info[s] = trader.SymInfo{Tick: 0.0001}
	}
	info["COARSE"] = trader.SymInfo{Tick: 0.01} // tick / price = 1 % > 0.02 %
	tickers := []Ticker{
		tk("A", "300e6", "1", "1.0"), tk("B", "200e6", "1", "-5.0"), tk("C", "200e6", "1", "5.0"), // B, C tie on both keys
		tk("D", "150e6", "1", "9"), tk("E", "900e6", "1", "0.5"), tk("F", "120e6", "1", "-9"),
		tk("G", "250e6", "1", "2"), tk("BTCUSDT", "5e9", "1", "1"), tk("THIN", "50e6", "1", "9"),
		tk("GIANT", "4e9", "1", "9"), tk("COARSE", "500e6", "1", "9"), tk("UNKNOWN", "500e6", "1", "9"), tk("ZZ", "100e6", "0", "9"),
	}
	// rows in order A B C D E F G; by volume: F D B C G A E -> 0..6 (B before C: stable), by |change|: E A G B C D F -> 0..6
	// score = by_v + 1.5 * by_c: A 5+1.5=6.5  B 2+4.5=6.5  C 3+6=9  D 1+7.5=8.5  E 6+0=6  F 0+9=9  G 4+3=7
	// ranked by -score (stable): C, F (9), D (8.5), G (7), A, B (6.5), E (6)
	got, err := Pick(tickers, info, map[string]bool{"KEEP": true}, map[string]bool{"E": true}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "C D F KEEP" {
		t.Fatalf("pick %v", got)
	}
	// without the exclusion E ranks again (score 6 is the lowest), n = 6 takes everything but E; ties A, B keep row order
	got, _ = Pick(tickers, info, nil, nil, 6)
	if strings.Join(got, " ") != "A B C D F G" {
		t.Fatalf("pick %v", got)
	}
	if _, err := Pick([]Ticker{tk("A", "x", "1", "1")}, info, nil, nil, 3); err == nil {
		t.Fatal("a bad number is an error (Python ValueError)")
	}
}

func TestHealthEmpty(t *testing.T) {
	f := New(trader.New(nil, nil, nil, "closes", &testkit.Sched{}), &sync.Mutex{}, nil, 0)
	if h := f.Health(); h != (Health{}) {
		t.Fatalf("%+v", h)
	}
}

// TestStreamsSyncABook runs a fake Binance (REST + both websocket routes) and checks that the book syncs, the tape
// fills and that depth and trades are read from the routes Binance uses since 2026-04-23.
func TestStreamsSyncABook(t *testing.T) {
	testkit.Silence()
	testkit.PinP(t)
	var paths sync.Map
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.Store(r.URL.Path, true)
		switch r.URL.Path {
		case "/fapi/v1/klines":
			if r.URL.Query().Get("interval") == "1m" {
				w.Write([]byte(`[[1699999800000,"1.0","1.002","0.998","1.0","1"],[1699999860000,"1.0","1.003","0.999","1.001","1"]]`))
			} else {
				w.Write([]byte(`[[1699996800000,"1.0","1.002","0.998","1.0005","1"]]`))
			}
		case "/fapi/v1/depth":
			w.Write([]byte(`{"lastUpdateId":100,"bids":[["1.0000","10"]],"asks":[["1.0010","10"]]}`))
		case "/public/ws/xusdt@depth@100ms":
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			c.WriteMessage(websocket.TextMessage, []byte(`{"e":"depthUpdate","E":1700000000000,"T":1700000000000,"s":"XUSDT","U":99,"u":101,"pu":98,"b":[["0.9999","5"]],"a":[]}`))
			c.WriteMessage(websocket.TextMessage, []byte(`{"e":"depthUpdate","E":1700000000100,"T":1700000000100,"s":"XUSDT","U":102,"u":103,"pu":101,"b":[],"a":[["1.0010","0"],["1.0011","7"]]}`))
			time.Sleep(3 * time.Second)
		case "/market/ws/xusdt@aggTrade":
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			c.WriteMessage(websocket.TextMessage, []byte(`{"e":"aggTrade","E":1700000000200,"a":1,"s":"XUSDT","p":"1.0001","q":"3","f":1,"l":1,"T":1700000000150,"m":true}`))
			time.Sleep(3 * time.Second)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store, err := state.Open(t.TempDir()+"/s.json", 0)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var trades atomic.Int32
	tr := trader.New(nil, store, map[string]trader.SymInfo{"XUSDT": {Tick: 0.0001, Step: 1, MinQty: 1, MinNotional: 5}}, "closes", &testkit.Sched{})
	f := New(tr, &mu, func(string, book.Trade) { trades.Add(1) }, 4)
	f.Rest, f.WSBase = srv.URL, "ws"+strings.TrimPrefix(srv.URL, "http")
	f.SetContext(t.Context())
	mu.Lock()
	f.SetSymbols([]string{"XUSDT"})
	mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		h := f.Health()
		mu.Unlock()
		if h.Synced == 1 && h.Msgs >= 3 && trades.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not synced: %+v trades %d", h, trades.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	b := tr.Book("XUSDT")
	if q := b.QtyAt(book.Bid, 9999); q != 5 {
		t.Errorf("bid at 0.9999 = %v", q)
	}
	if q := b.QtyAt(book.Ask, 10011); q != 7 || b.QtyAt(book.Ask, 10010) != 0 {
		t.Errorf("asks %v %v", q, b.QtyAt(book.Ask, 10010))
	}
	if len(b.Tape()) != 1 || b.Tape()[0].Ms != 1700000000150 || !b.Tape()[0].BuyerMaker() {
		t.Errorf("tape %+v", b.Tape())
	}
	if len(b.Bars()) == 0 || len(b.H1()) == 0 {
		t.Errorf("seeds: %d bars, %d hours", len(b.Bars()), len(b.H1()))
	}
	for _, p := range []string{"/public/ws/xusdt@depth@100ms", "/market/ws/xusdt@aggTrade"} {
		if _, ok := paths.Load(p); !ok {
			t.Errorf("route %s was never used", p)
		}
	}
	if w := f.Watching(); len(w) != 1 || w[0] != "XUSDT" {
		t.Errorf("watching %v", w)
	}
	f.SetSymbols(nil) // dropped: the book goes
	if tr.Book("XUSDT") != nil || len(f.Watching()) != 0 {
		t.Error("symbol not removed")
	}
}
