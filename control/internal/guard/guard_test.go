package guard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"control/internal/store"
)

const (
	testKey    = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	testSecret = "SeCrEtSeCrEtSeCrEtSeCrEt0123456789"
)

// fakeBinance simulates the futures endpoints LossGuard uses and checks every signature.
type fakeBinance struct {
	mu        sync.Mutex
	wallet    float64
	positions map[string]float64 // symbol -> signed amount
	upnl      map[string]float64
	orders    map[string]int // symbol -> open order count
	algo      map[string]int
	calls     []string
	badSig    int
	push      chan []byte
}

func newFake(wallet float64) *fakeBinance {
	return &fakeBinance{wallet: wallet, positions: map[string]float64{}, upnl: map[string]float64{},
		orders: map[string]int{}, algo: map[string]int{}, push: make(chan []byte, 4)}
}

func (f *fakeBinance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/private/ws/") {
		f.serveStream(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	call := r.Method + " " + r.URL.Path
	if s := q.Get("symbol"); s != "" {
		call += " " + s
	}
	f.calls = append(f.calls, call)

	if r.URL.Path != "/fapi/v1/time" && !strings.Contains(r.URL.Path, "listenKey") {
		raw := r.URL.RawQuery
		i := strings.LastIndex(raw, "&signature=")
		m := hmac.New(sha256.New, []byte(testSecret))
		m.Write([]byte(raw[:max(i, 0)]))
		if i < 0 || raw[i+len("&signature="):] != hex.EncodeToString(m.Sum(nil)) || r.Header.Get("X-MBX-APIKEY") != testKey {
			f.badSig++
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":-1022,"msg":"Signature for this request is not valid."}`)
			return
		}
	}

	out := any(map[string]any{})
	switch call := r.Method + " " + r.URL.Path; call {
	case "GET /fapi/v1/time":
		out = map[string]any{"serverTime": time.Now().UnixMilli()}
	case "GET /sapi/v1/account/apiRestrictions":
		out = map[string]any{"enableFutures": true}
	case "GET /fapi/v3/account":
		var total float64
		var ps []map[string]any
		for s, amt := range f.positions {
			total += f.upnl[s]
			ps = append(ps, map[string]any{"symbol": s, "positionSide": "BOTH",
				"positionAmt": fmt.Sprint(amt), "unrealizedProfit": fmt.Sprint(f.upnl[s])})
		}
		out = map[string]any{
			"totalWalletBalance":    fmt.Sprintf("%.8f", f.wallet),
			"totalUnrealizedProfit": fmt.Sprintf("%.8f", total),
			"positions":             ps,
		}
	case "GET /fapi/v1/openOrders":
		var os []map[string]any
		for s, n := range f.orders {
			for range n {
				os = append(os, map[string]any{"symbol": s, "status": "NEW"})
			}
		}
		out = os
	case "GET /fapi/v1/openAlgoOrders":
		var os []map[string]any
		for s, n := range f.algo {
			for range n {
				os = append(os, map[string]any{"symbol": s, "algoId": 1})
			}
		}
		out = os
	case "DELETE /fapi/v1/allOpenOrders":
		delete(f.orders, q.Get("symbol"))
	case "DELETE /fapi/v1/algoOpenOrders":
		delete(f.algo, q.Get("symbol"))
	case "POST /fapi/v1/listenKey":
		out = map[string]any{"listenKey": "lk1"}
	case "PUT /fapi/v1/listenKey", "DELETE /fapi/v1/listenKey":
	case "GET /fapi/v1/positionSide/dual":
		out = map[string]any{"dualSidePosition": false}
	case "POST /fapi/v1/order":
		s := q.Get("symbol")
		amt := f.positions[s]
		wantSide := "SELL"
		if amt < 0 {
			wantSide, amt = "BUY", -amt
		}
		if q.Get("type") != "MARKET" || q.Get("side") != wantSide || q.Get("reduceOnly") != "true" || q.Get("quantity") != fmt.Sprint(amt) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"code":-1102,"msg":"bad close order %s"}`, r.URL.RawQuery)
			return
		}
		f.wallet += f.upnl[s] // position closed: PnL realized
		delete(f.positions, s)
		delete(f.upnl, s)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":-5000,"msg":"not found"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

var upgrader = websocket.Upgrader{}

// serveStream is the user data stream: it forwards whatever the test pushes.
func (f *fakeBinance) serveStream(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.URL.Path, "/private/ws/") != "lk1" {
		http.NotFound(w, r)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	f.calls = append(f.calls, "WS connect")
	f.mu.Unlock()
	for {
		select {
		case msg := <-f.push:
			if conn.WriteMessage(websocket.TextMessage, msg) != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeBinance) set(fn func(f *fakeBinance)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeBinance) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Notify(_ int64, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, text)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.msgs
	r.msgs = nil
	return m
}

func TestEndToEndAlertBlockEnforceUnblock(t *testing.T) {
	fake := newFake(100)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	rec := &recorder{}
	g := New(Options{FapiURL: srv.URL, SapiURL: srv.URL, Interval: 10 * time.Second, Rules: testRules,
		Location: time.UTC, Now: func() time.Time { return clock }}, st, rec)
	ctx := context.Background()
	const chat = 7

	res, err := g.Activate(ctx, chat, testKey, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if res.User.Baseline != 100 || !res.User.Active || res.NoFuturesPerm {
		t.Fatalf("activate: %+v", res)
	}

	// -5%: nothing.
	fake.set(func(f *fakeBinance) { f.positions["BTCUSDT"] = 0.01; f.upnl["BTCUSDT"] = -5 })
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 0 {
		t.Fatalf("unexpected messages at -5%%: %v", m)
	}
	if u, _ := st.Get(chat); u.Last.Equity != 95 || u.Last.OpenPositions != 1 {
		t.Fatalf("snapshot not saved: %+v", u.Last)
	}

	// -10%: one alert, then silence.
	fake.set(func(f *fakeBinance) { f.upnl["BTCUSDT"] = -10 })
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 1 || !strings.Contains(m[0], "Daily loss alert") {
		t.Fatalf("want exactly one alert, got %v", m)
	}

	// -20% with an open order and a stop (algo) order: close everything and block.
	fake.set(func(f *fakeBinance) { f.upnl["BTCUSDT"] = -20; f.orders["ETHUSDT"] = 2; f.algo["BTCUSDT"] = 1 })
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	m := rec.take()
	if len(m) != 1 || !strings.Contains(m[0], "Trading blocked") || !strings.Contains(m[0], "Closed positions: 1") {
		t.Fatalf("want block message, got %v", m)
	}
	if fake.count("DELETE /fapi/v1/allOpenOrders ETHUSDT") != 1 || fake.count("DELETE /fapi/v1/algoOpenOrders BTCUSDT") != 1 ||
		fake.count("POST /fapi/v1/order BTCUSDT") != 1 {
		t.Fatalf("enforcement calls: %v", fake.calls)
	}
	u, _ := st.Get(chat)
	if !u.Blocked || !u.BlockedUntil.Equal(clock.Add(24*time.Hour)) {
		t.Fatalf("not blocked: %+v", u)
	}

	// While blocked, a new trade is closed on the next check.
	fake.set(func(f *fakeBinance) { f.positions["SOLUSDT"] = -3; f.upnl["SOLUSDT"] = 0.5 })
	clock = clock.Add(time.Hour)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 1 || !strings.Contains(m[0], "Closed 1 position") {
		t.Fatalf("want enforcement message, got %v", m)
	}
	if fake.count("POST /fapi/v1/order SOLUSDT") != 1 {
		t.Fatalf("SOL short not closed: %v", fake.calls)
	}
	// Nothing open: no messages, no orders.
	clock = clock.Add(10 * time.Second)
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 0 {
		t.Fatalf("idle block produced %v", m)
	}

	// Stop and reconnect are refused during the block.
	if _, err := g.Deactivate(chat); err != ErrBlocked {
		t.Fatalf("deactivate during block: %v", err)
	}

	// 24h later: unblocked with the new wallet balance as baseline.
	clock = u.BlockedUntil
	g.Check(ctx, chat)
	if m := rec.take(); len(m) != 1 || !strings.Contains(m[0], "block is over") {
		t.Fatalf("want unblock message, got %v", m)
	}
	u, _ = st.Get(chat)
	if u.Blocked || u.Baseline != 80.5 {
		t.Fatalf("after unblock: blocked %v baseline %v", u.Blocked, u.Baseline)
	}
	if fake.badSig != 0 {
		t.Fatalf("%d requests had bad signatures", fake.badSig)
	}
}

func TestReconnectSameKeyKeepsLossWindow(t *testing.T) {
	fake := newFake(100)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32))
	clock := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	g := New(Options{FapiURL: srv.URL, SapiURL: srv.URL, Rules: testRules, Now: func() time.Time { return clock }}, st, &recorder{})
	ctx := context.Background()

	if _, err := g.Activate(ctx, 1, testKey, testSecret); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeBinance) { f.wallet = 85 })
	clock = clock.Add(time.Hour)
	if _, err := g.Deactivate(1); err != nil {
		t.Fatal(err)
	}
	res, err := g.Activate(ctx, 1, testKey, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if res.User.Baseline != 100 || res.User.Last.LossPct != 15 {
		t.Fatalf("reconnect reset the window: baseline %v loss %v", res.User.Baseline, res.User.Last.LossPct)
	}
}

func TestBadKeysRejected(t *testing.T) {
	srv := httptest.NewServer(newFake(100))
	defer srv.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32))
	g := New(Options{FapiURL: srv.URL, SapiURL: srv.URL, Rules: testRules}, st, &recorder{})
	if _, err := g.Activate(context.Background(), 1, testKey, "WrongSecretWrongSecret123"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if u, _ := st.Get(1); u.Active {
		t.Fatal("user activated with bad keys")
	}
}

func TestStreamClosesNewTradeDuringBlockWithoutPolling(t *testing.T) {
	fake := newFake(100)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32))
	rec := &recorder{}
	g := New(Options{FapiURL: srv.URL, SapiURL: srv.URL, WsURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		Rules: testRules}, st, rec)
	defer g.stopAllStreams()
	ctx := context.Background()
	if _, err := g.Activate(ctx, 1, testKey, testSecret); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeBinance) { f.wallet = 79 })
	g.Check(ctx, 1) // -21%: block, stream opens
	rec.take()
	waitFor(t, func() bool { return fake.count("WS connect") == 1 })

	// The user opens a trade; Binance pushes the fill. No Check() is called.
	fake.set(func(f *fakeBinance) { f.positions["DOGEUSDT"] = 1000 })
	fake.push <- []byte(`{"e":"ORDER_TRADE_UPDATE","o":{"s":"DOGEUSDT","X":"FILLED"}}`)
	waitFor(t, func() bool { return fake.count("POST /fapi/v1/order DOGEUSDT") == 1 })
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.msgs) == 1 && strings.Contains(rec.msgs[0], "Closed 1 position")
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
