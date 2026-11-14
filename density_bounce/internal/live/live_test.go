package live

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	"densitybounce/internal/state"
	"densitybounce/internal/testkit"
	"densitybounce/internal/trader"
)

// fakeEx is a fake Binance: it records every request and serves the endpoints the broker uses.
type fakeEx struct {
	mu      sync.Mutex
	calls   []call
	answers map[string]string // raw JSON answers by "METHOD path"
	pos     map[string]float64
	posSeq  []float64 // scripted positionAmt readings for XUSDT (then pos)
	orders  map[string]string
	stops   map[string]string
	errs    map[string][]*BinanceError // errors for the next requests of a key
	drop    map[string]int             // kill the connection for the next n requests of a key
	skew    int64                      // added to the server time
}

type call struct {
	method, path string
	q            url.Values
}

func newFake() *fakeEx {
	return &fakeEx{answers: map[string]string{}, pos: map[string]float64{}, orders: map[string]string{},
		stops: map[string]string{}, errs: map[string][]*BinanceError{}, drop: map[string]int{}}
}

func (f *fakeEx) of(method, path string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, c := range f.calls {
		if c.method == method && c.path == path {
			out = append(out, c.q)
		}
	}
	return out
}

func (f *fakeEx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call{r.Method, r.URL.Path, q})
	if f.drop[key] > 0 {
		f.drop[key]--
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return
	}
	if e := f.errs[key]; len(e) > 0 {
		f.errs[key] = e[1:]
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"code":%d,"msg":%q}`, e[0].C, e[0].Msg)
		return
	}
	reply := func(s string) { w.Header().Set("X-MBX-USED-WEIGHT-1M", "7"); io.WriteString(w, s) }
	switch key {
	case "GET /fapi/v1/time":
		reply(fmt.Sprintf(`{"serverTime":%d}`, time.Now().UnixMilli()+f.skew))
	case "GET /fapi/v2/positionRisk":
		if len(f.posSeq) > 0 && q.Get("symbol") == "XUSDT" {
			f.pos["XUSDT"], f.posSeq = f.posSeq[0], f.posSeq[1:]
		}
		var syms []string
		if s := q.Get("symbol"); s != "" {
			syms = []string{s}
		} else {
			for s := range f.pos {
				syms = append(syms, s)
			}
		}
		var rows []string
		for _, s := range syms {
			rows = append(rows, fmt.Sprintf(`{"symbol":%q,"positionAmt":"%v"}`, s, f.pos[s]))
		}
		reply("[" + strings.Join(rows, ",") + "]")
	case "GET /fapi/v1/order":
		if o, ok := f.orders[q.Get("origClientOrderId")]; ok {
			reply(o)
		} else {
			reply(`{"status":"NEW","executedQty":"0"}`)
		}
	case "POST /fapi/v1/algoOrder":
		if len(f.stops) > 0 { // like Binance: one closePosition stop per side
			w.WriteHeader(400)
			io.WriteString(w, `{"code":-4130,"msg":"An open stop or take profit order with GTE and closePosition is existing"}`)
			return
		}
		f.stops[q.Get("clientAlgoId")] = q.Get("triggerPrice")
		reply(`{"algoStatus":"NEW"}`)
	case "DELETE /fapi/v1/algoOrder":
		delete(f.stops, q.Get("clientAlgoId"))
		reply(`{}`)
	case "GET /fapi/v1/exchangeInfo":
		reply(`{"symbols":[{"symbol":"XUSDT","filters":[{"filterType":"LOT_SIZE","stepSize":"1"}]}]}`)
	default:
		if a, ok := f.answers[key]; ok {
			reply(a)
		} else if r.Method == "GET" {
			reply(`[]`)
		} else {
			reply(`{}`)
		}
	}
}

type env struct {
	f     *fakeEx
	b     *LiveBroker
	t     *trader.Trader
	sched *testkit.Sched
	mu    *sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	testkit.Silence()
	testkit.PinP(t)
	testkit.PinRisk(t)
	setup.P.TP1Part = 0.25 // the slots below have tp1 / tp2 / run (v0.2.0)
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	rest, err := NewRest("k", "s", "")
	if err != nil {
		t.Fatal(err)
	}
	rest.Fapi, rest.Sapi = srv.URL, srv.URL
	mu := &sync.Mutex{}
	b := NewBroker(rest, mu)
	b.Sleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	t.Cleanup(b.Stop)
	store, err := state.Open(t.TempDir()+"/s.json", 0)
	if err != nil {
		t.Fatal(err)
	}
	store.Out = io.Discard
	sched := &testkit.Sched{}
	tr := trader.New(b, store, map[string]trader.SymInfo{"XUSDT": {Tick: 0.0001, Step: 1, MinQty: 1, MinNotional: 5}},
		"closes", sched)
	tr.Logf = func(string, ...any) {}
	mu.Lock() // the broker works under the engine lock
	t.Cleanup(func() { b.Stop(); mu.Unlock() })
	return &env{f, b, tr, sched, mu}
}

var bidKey = book.DKey{Side: book.Bid, Tick: 10000}

func (e *env) openSlot(qty float64) *trader.Slot {
	s := &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.0050, DKey: bidKey,
		TP1: 1.0019, TPRun: 1.0100}, Qty: qty, State: "open", FilledQty: qty, EntryPx: 1.0002}
	s.Parts = e.t.Split(s)
	e.t.PutSlot(s)
	return s
}

func (e *env) stoppedSlot() *trader.Slot {
	s := e.openSlot(59)
	s.Stop = s.Plan.Stop
	s.StopID = "old"
	e.f.stops["old"] = "0.9985"
	return s
}

func TestTakesOneReduceOnlyOrderPerPart(t *testing.T) {
	e := newEnv(t)
	s := e.openSlot(59)
	e.f.pos["XUSDT"] = 59
	if err := e.b.Protect(s); err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, q := range e.f.of("POST", "/fapi/v1/order") {
		got = append(got, [2]string{q.Get("price"), q.Get("quantity")})
		if q.Get("reduceOnly") != "true" {
			t.Errorf("take without reduceOnly: %v", q)
		}
	}
	want := [][2]string{{"1.0019", "14"}, {"1.0050", "36"}, {"1.0100", "9"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("takes %v, want %v", got, want)
	}
	rec := e.t.Store.Slots()["XUSDT"]
	for i, p := range s.Parts {
		if rec.TPIDs[i] != p.ID || p.ID == "" {
			t.Fatalf("tp_ids %v, parts %v", rec.TPIDs, s.Parts)
		}
	}
}

func TestPollReportsPartsAndCancelsTheRest(t *testing.T) {
	e := newEnv(t)
	e.b.Sleep = func(time.Duration) {}
	s := e.openSlot(59)
	for i, id := range []string{"t1", "t2", "t3"} {
		s.Parts[i].ID = id
	}
	e.f.orders["t1"] = `{"status":"FILLED","executedQty":"14","avgPrice":"1.0019"}`
	e.f.posSeq = []float64{59, 45, 45, 0, 0} // tp1 fills, then the stop closes the rest
	e.b.pollOpen(s)
	tr := e.t.Store.LastTrades(1)
	if len(tr) != 1 || tr[0].Reason != "stop" {
		t.Fatalf("trade %+v", tr)
	}
	if d := []bool{tr[0].Parts[0].Done, tr[0].Parts[1].Done, tr[0].Parts[2].Done}; fmt.Sprint(d) != "[true false false]" {
		t.Fatalf("parts done %v", d)
	}
	cancelled := map[string]bool{}
	for _, q := range e.f.of("DELETE", "/fapi/v1/order") {
		cancelled[q.Get("origClientOrderId")] = true
	}
	if len(cancelled) != 2 || !cancelled["t2"] || !cancelled["t3"] {
		t.Fatalf("cancelled %v", cancelled)
	}
}

func TestMoveStopReplacesTheOnlyClosePositionStop(t *testing.T) {
	e := newEnv(t)
	s := e.stoppedSlot()
	e.t.MoveStop(s, 1.0008, "stop_be")
	if len(e.f.stops) != 1 || e.f.stops[s.StopID] != "1.0008" {
		t.Fatalf("stops %v, id %s", e.f.stops, s.StopID)
	}
	if s.Stop != 1.0008 || s.StopWhy != "stop_be" {
		t.Fatalf("stop %v why %q", s.Stop, s.StopWhy)
	}
	if got := e.t.Store.Slots()["XUSDT"].StopID; got == nil || *got != s.StopID {
		t.Fatalf("saved stop id %v", got)
	}
}

func TestMoveStopFailureRestoresTheOldStop(t *testing.T) {
	e := newEnv(t)
	s := e.stoppedSlot()
	e.f.errs["POST /fapi/v1/algoOrder"] = []*BinanceError{{C: -1008, Msg: "overloaded"}}
	e.t.MoveStop(s, 1.0008, "stop_be")
	if len(e.f.stops) != 1 || e.f.stops[s.StopID] != "0.9985" || s.CloseSent {
		t.Fatalf("stops %v closeSent %v", e.f.stops, s.CloseSent)
	}
	if s.Stop != s.Plan.Stop || s.StopWhy != "" { // a later stop-out is the initial stop
		t.Fatalf("stop %v why %q", s.Stop, s.StopWhy)
	}
	found := false
	for _, ev := range e.t.Store.Events() {
		found = found || strings.Contains(ev.Msg, "move stop")
	}
	if !found {
		t.Fatal("no move stop event")
	}
}

func TestMoveStopClosesWhenNoStopCanBePlaced(t *testing.T) {
	e := newEnv(t)
	s := e.stoppedSlot()
	e.f.pos["XUSDT"] = 59
	e.f.errs["POST /fapi/v1/algoOrder"] = []*BinanceError{{C: -1008, Msg: "overloaded"}, {C: -1008, Msg: "overloaded"}}
	e.t.MoveStop(s, 1.0008, "stop_be")
	if !s.CloseSent || s.ExitReason != "stop_lost" {
		t.Fatalf("closeSent %v reason %q", s.CloseSent, s.ExitReason)
	}
	var market url.Values
	for _, q := range e.f.of("POST", "/fapi/v1/order") {
		if q.Get("type") == "MARKET" {
			market = q
		}
	}
	if market.Get("side") != "SELL" || market.Get("reduceOnly") != "true" {
		t.Fatalf("market order %v", market)
	}
}

func TestMoveStopPastThePriceClosesAtMarket(t *testing.T) {
	e := newEnv(t)
	s := e.stoppedSlot()
	e.f.pos["XUSDT"] = 59
	e.f.errs["POST /fapi/v1/algoOrder"] = []*BinanceError{{C: -2021, Msg: "would immediately trigger"}}
	e.t.MoveStop(s, 1.0008, "stop_be")
	if !s.CloseSent || s.ExitReason != "stop_be" || len(e.f.stops) != 0 {
		t.Fatalf("closeSent %v reason %q stops %v", s.CloseSent, s.ExitReason, e.f.stops)
	}
}

func TestMoveStopKeepsTheOldStopOn4130(t *testing.T) {
	e := newEnv(t)
	s := e.stoppedSlot()
	e.f.errs["DELETE /fapi/v1/algoOrder"] = []*BinanceError{{C: -1000, Msg: "cancel did not happen"}}
	// the cancel is only logged (an exchange answer); the old stop still stands, so the new one answers -4130
	err := e.b.MoveStop(s, 1.0008)
	if c, _ := code(err); c != -4130 || e.f.stops["old"] != "0.9985" || s.CloseSent {
		t.Fatalf("err %v stops %v closeSent %v", err, e.f.stops, s.CloseSent)
	}
	if len(e.f.of("POST", "/fapi/v1/algoOrder")) != 1 {
		t.Fatal("the old stop must not be placed again after -4130")
	}
}

func TestProtectPlacesExchangeStopAndReduceOnlyTake(t *testing.T) {
	e := newEnv(t)
	s := e.openSlot(9)
	e.f.pos["XUSDT"] = 9
	if err := e.b.Protect(s); err != nil {
		t.Fatal(err)
	}
	algo := e.f.of("POST", "/fapi/v1/algoOrder")[0]
	if algo.Get("type") != "STOP_MARKET" || algo.Get("side") != "SELL" || algo.Get("closePosition") != "true" ||
		algo.Get("triggerPrice") != "0.9985" || !strings.HasPrefix(algo.Get("clientAlgoId"), PREFIX) {
		t.Fatalf("algo %v", algo)
	}
	take := e.f.of("POST", "/fapi/v1/order")[0]
	if take.Get("reduceOnly") != "true" || take.Get("price") != "1.0050" || take.Get("quantity") != "9" {
		t.Fatalf("take %v", take)
	}
	if got := e.t.Store.Slots()["XUSDT"].StopID; got == nil || *got != s.StopID {
		t.Fatalf("saved stop id %v", got)
	}
}

func TestProtectRetriesOnceThenGivesUp(t *testing.T) {
	e := newEnv(t)
	e.b.Sleep = func(time.Duration) {}
	s := e.openSlot(9)
	e.f.errs["POST /fapi/v1/algoOrder"] = []*BinanceError{{C: -1008, Msg: "x"}, {C: -1008, Msg: "x"}}
	if err := e.b.Protect(s); err == nil {
		t.Fatal("want an error after the second failure")
	}
	e.f.errs["POST /fapi/v1/algoOrder"] = []*BinanceError{{C: -2021, Msg: "trigger"}}
	n := len(e.f.of("POST", "/fapi/v1/algoOrder"))
	if err := e.b.Protect(s); err == nil || len(e.f.of("POST", "/fapi/v1/algoOrder")) != n+1 {
		t.Fatalf("-2021 must not be retried: %v", err)
	}
}

func TestForeignPositionsAndOrdersAreNeverTraded(t *testing.T) {
	e := newEnv(t)
	e.f.answers["GET /fapi/v1/openOrders"] = `[{"symbol":"AUSDT","clientOrderId":"fc-1-1"},{"symbol":"XUSDT","clientOrderId":"` + PREFIX + `e1"}]`
	e.f.answers["GET /fapi/v1/openAlgoOrders"] = `[{"symbol":"BUSDT","clientAlgoId":"manual"}]`
	e.f.pos = map[string]float64{"CUSDT": -3}
	if err := e.b.RefreshForeign(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"AUSDT", "BUSDT", "CUSDT", "BTCUSDT"} {
		if !e.t.BlockedSyms[s] {
			t.Errorf("%s not blocked: %v", s, e.t.BlockedSyms)
		}
	}
	if e.t.BlockedSyms["XUSDT"] {
		t.Error("XUSDT must not be blocked")
	}
	s := &trader.Slot{Sym: "AUSDT", Plan: &setup.Plan{Side: "long", Entry: 1, Stop: 0.99, TP: 1.02}, Qty: 10}
	if err := e.b.PlaceEntry(s); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("err %v", err)
	}
}

func TestLeftoverCloseNeverFlipsOrOverCloses(t *testing.T) {
	e := newEnv(t)
	e.f.pos["XUSDT"] = -5 // a short, but the bot recorded a long
	if err := CloseOwnPosition(e.b.Rest, "XUSDT", "long", 9); err != nil {
		t.Fatal(err)
	}
	if n := len(e.f.of("POST", "/fapi/v1/order")); n != 0 {
		t.Fatalf("%d orders sent", n)
	}
	e.f.pos["XUSDT"] = 20 // bigger than the bot's own 9
	if err := CloseOwnPosition(e.b.Rest, "XUSDT", "long", 9); err != nil {
		t.Fatal(err)
	}
	o := e.f.of("POST", "/fapi/v1/order")[0]
	if o.Get("side") != "SELL" || o.Get("quantity") != "9" || o.Get("reduceOnly") != "true" {
		t.Fatalf("order %v", o)
	}
}

func TestPlaceEntryRejectionAndExpiry(t *testing.T) {
	e := newEnv(t)
	e.b.foreign = map[string]bool{}
	s := &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.005}, Qty: 9, State: "entry"}
	e.t.PutSlot(s)
	e.f.errs["POST /fapi/v1/order"] = []*BinanceError{{C: -2010, Msg: "rejected"}}
	if err := e.b.PlaceEntry(s); err == nil || !strings.Contains(err.Error(), "-2010") {
		t.Fatalf("err %v", err)
	}
	e.f.answers["POST /fapi/v1/order"] = `{"status":"EXPIRED","executedQty":"0"}`
	if err := e.b.PlaceEntry(s); err == nil || !strings.Contains(err.Error(), "post-only expired") {
		t.Fatalf("err %v", err)
	}
	o := e.f.of("POST", "/fapi/v1/order")[0]
	if o.Get("timeInForce") != "GTX" || o.Get("price") != "1.0002" || o.Get("quantity") != "9" || !strings.HasPrefix(o.Get("newClientOrderId"), PREFIX+"e") {
		t.Fatalf("order %v", o)
	}
	if m := e.f.of("POST", "/fapi/v1/marginType"); len(m) != 1 || m[0].Get("marginType") != "ISOLATED" {
		t.Fatalf("margin type calls %v", m)
	}
	if len(e.f.of("POST", "/fapi/v1/leverage")) != 1 {
		t.Fatal("leverage set once per symbol")
	}
}

func TestPlaceEntryLostResponseLookup(t *testing.T) {
	e := newEnv(t)
	e.b.foreign = map[string]bool{}
	e.b.Sleep = func(time.Duration) {}
	mk := func() *trader.Slot {
		return &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "short", Entry: 1.0002, Stop: 1.002, TP: 0.99}, Qty: 9, State: "entry"}
	}
	// the order never reached the exchange: -2013 on the lookup
	s := mk()
	e.f.drop["POST /fapi/v1/order"] = 1
	e.f.errs["GET /fapi/v1/order"] = []*BinanceError{{C: -2013, Msg: "Order does not exist."}}
	if err := e.b.PlaceEntry(s); err == nil || err.Error() != "entry not placed" {
		t.Fatalf("err %v", err)
	}
	// the order exists: the lookup finds it and the entry goes on
	s = mk()
	e.t.PutSlot(s)
	e.f.drop["POST /fapi/v1/order"] = 1
	if err := e.b.PlaceEntry(s); err != nil {
		t.Fatalf("err %v", err)
	}
	// nothing can be found: unknown, left to the sweep
	s = mk()
	e.f.drop["POST /fapi/v1/order"] = 1
	e.f.drop["GET /fapi/v1/order"] = 50 // the transport retries an idempotent GET once
	if err := e.b.PlaceEntry(s); err == nil || !strings.Contains(err.Error(), "entry state unknown") {
		t.Fatalf("err %v", err)
	}
	if u, ok := e.b.unknown["XUSDT"]; !ok || u.side != "short" || u.qty != 9 {
		t.Fatalf("unknown %v", e.b.unknown)
	}
}

func TestPollEntryReportsTheFillAndProtects(t *testing.T) {
	e := newEnv(t)
	e.b.Sleep = func(time.Duration) {}
	s := &trader.Slot{Sym: "XUSDT", Plan: &setup.Plan{Side: "long", Entry: 1.0002, Stop: 0.9985, TP: 1.005, DKey: bidKey}, Qty: 9,
		State: "entry", OID: "dbbe1"}
	e.t.PutSlot(s)
	e.f.orders["dbbe1"] = `{"status":"FILLED","executedQty":"9","avgPrice":"1.0002"}`
	e.b.pollEntry(s)
	if s.State != "open" || s.FilledQty != 9 || s.EntryPx != 1.0002 {
		t.Fatalf("slot %+v", s)
	}
	e.f.pos["XUSDT"] = 9
	e.sched.Settle() // the trader's protect
	if len(e.f.stops) != 1 || s.StopID == "" {
		t.Fatalf("stops %v", e.f.stops)
	}
}

func TestCheckBlocksReadOnlyKeyAndHedgeMode(t *testing.T) {
	e := newEnv(t)
	e.f.answers["GET /sapi/v1/account/apiRestrictions"] = `{"enableFutures":false,"enableWithdrawals":true}`
	e.f.answers["GET /fapi/v1/positionSide/dual"] = `{"dualSidePosition":true}`
	e.f.answers["GET /fapi/v2/account"] = `{"availableBalance":"100"}`
	problems, err := Check(e.b.Rest)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(problems, "|")
	for _, w := range []string{"Ed25519", "withdrawals", "hedge"} {
		if !strings.Contains(all, w) {
			t.Errorf("no %q problem: %v", w, problems)
		}
	}
	e.f.answers["GET /sapi/v1/account/apiRestrictions"] = `{"enableFutures":true}`
	e.f.answers["GET /fapi/v1/positionSide/dual"] = `{"dualSidePosition":false}`
	if problems, err := Check(e.b.Rest); err != nil || len(problems) != 0 {
		t.Fatalf("problems %v err %v", problems, err)
	}
	e.f.errs["GET /sapi/v1/account/apiRestrictions"] = []*BinanceError{{C: -2015, Msg: "Invalid API-key"}}
	problems, _ = Check(e.b.Rest)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "cannot read the key's permissions (BinanceError: -2015 Invalid API-key)") {
		t.Fatalf("problems %v", problems)
	}
}

func TestCheckNeedsTheMarginOfAllTrades(t *testing.T) {
	e := newEnv(t)
	trader.RISK.Notional = 500 // 2 trades x 500 at x2 = 500 USDT margin
	e.f.answers["GET /sapi/v1/account/apiRestrictions"] = `{"enableFutures":true}`
	e.f.answers["GET /fapi/v1/positionSide/dual"] = `{"dualSidePosition":false}`
	e.f.answers["GET /fapi/v2/account"] = `{"availableBalance":"499"}`
	problems, _ := Check(e.b.Rest)
	if len(problems) != 1 || !strings.Contains(problems[0], "< 500 USDT (2 x 500 USDT at x2)") {
		t.Fatalf("problems %v", problems)
	}
	e.f.answers["GET /fapi/v2/account"] = `{"availableBalance":"600"}`
	if problems, _ := Check(e.b.Rest); len(problems) != 0 {
		t.Fatalf("problems %v", problems)
	}
}

func TestSignatures(t *testing.T) {
	r, err := NewRest("k", "s", "")
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte("s"))
	m.Write([]byte("a=1"))
	if r.Sign("a=1") != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("HMAC signature differs")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	r, err = NewRest("k", "", strings.ReplaceAll(pemText, "\n", `\n`)) // Railway variables carry \n literally
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.StdEncoding.DecodeString(r.Sign("a=1"))
	if !ed25519.Verify(pub, []byte("a=1"), sig) {
		t.Fatal("Ed25519 signature does not verify")
	}
	if _, err := NewRest("k", "", ""); err == nil {
		t.Fatal("a key without a secret must fail")
	}
	if _, err := NewRest("", "s", ""); err == nil {
		t.Fatal("no api key must fail")
	}
}

func TestSignedCallResyncsOn1021AndSignsTheSentQuery(t *testing.T) {
	e := newEnv(t)
	e.f.skew = 100000
	e.f.errs["GET /fapi/v2/account"] = []*BinanceError{{C: -1021, Msg: "Timestamp for this request is outside of the recvWindow."}}
	e.f.answers["GET /fapi/v2/account"] = `{"availableBalance":"5"}`
	if _, err := e.b.Rest.Call("GET", "/fapi/v2/account", true, "", Q{"x": "a b"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.f.of("GET", "/fapi/v1/time")); n != 1 {
		t.Fatalf("%d time syncs", n)
	}
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	var last call
	for _, c := range e.f.calls {
		if c.path == "/fapi/v2/account" {
			last = c
		}
	}
	if len(e.f.calls) < 3 {
		t.Fatalf("calls %v", e.f.calls)
	}
	ts, _ := strconv.ParseInt(last.q.Get("timestamp"), 10, 64)
	if d := ts - time.Now().UnixMilli(); d < 90000 {
		t.Fatalf("the retry did not use the server clock: offset %d", d)
	}
	sig := last.q.Get("signature")
	last.q.Del("signature")
	m := hmac.New(sha256.New, []byte("s"))
	m.Write([]byte(last.q.Encode()))
	if sig != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("the signature does not cover the query that was sent")
	}
	if e.b.Rest.Weight() != 7 {
		t.Fatalf("weight %d", e.b.Rest.Weight())
	}
}

func TestBinanceErrorCode(t *testing.T) {
	var err error = &BinanceError{C: -2021, Msg: "x"}
	if c, ok := code(err); !ok || c != -2021 || err.Error() != "-2021 x" {
		t.Fatalf("%v", err)
	}
	if _, ok := code(io.EOF); ok {
		t.Fatal("a network error has no code")
	}
	_ = json.Valid
}

func TestFmtAndDecimals(t *testing.T) {
	for _, c := range []struct {
		step float64
		dec  int
	}{{0.0001, 4}, {1e-05, 5}, {1, 0}, {10, 0}, {0.001, 3}, {0.5, 1}, {0.25, 2}, {1e-08, 8}, {100, 0}} {
		if d := Decimals(c.step); d != c.dec {
			t.Errorf("Decimals(%v) = %d, want %d", c.step, d, c.dec)
		}
	}
	for _, c := range []struct {
		x, step float64
		want    string
	}{{0.12345, 0.001, "0.123"}, {2.5, 1, "2"}, {3.5, 1, "4"}, {1.0002, 0.0001, "1.0002"}, {59, 1, "59"},
		{0.3 * 3, 0.1, "0.9"}, {1234.5678, 0.01, "1234.57"}, {0.00001234, 1e-05, "0.00001"}} {
		if got := Fmt(c.x, c.step); got != c.want {
			t.Errorf("Fmt(%v, %v) = %s, want %s", c.x, c.step, got, c.want)
		}
	}
}

func TestSweepCancelsOrphansAndClosesUnknown(t *testing.T) {
	e := newEnv(t)
	s := e.openSlot(9)
	s.OID, s.StopID = "dbbe-live", "dbbs-live"
	e.f.answers["GET /fapi/v1/openOrders"] = `[{"symbol":"XUSDT","clientOrderId":"dbbe-live"},{"symbol":"XUSDT","clientOrderId":"dbbe-orphan"},{"symbol":"AUSDT","clientOrderId":"fc-1"}]`
	e.f.answers["GET /fapi/v1/openAlgoOrders"] = `[{"symbol":"XUSDT","clientAlgoId":"dbbs-orphan"}]`
	e.b.unknown["XUSDT"] = unknownEntry{side: "long", qty: 9}
	e.t.ClearSlots() // the unknown entry has no slot
	e.f.pos["XUSDT"] = 9
	e.b.Now = func() float64 { return 1e10 } // nothing was just issued
	if err := e.b.Sweep(); err != nil {
		t.Fatal(err)
	}
	del := e.f.of("DELETE", "/fapi/v1/order")
	if len(del) != 2 { // the slot is gone, so both bot orders are orphans; the foreign one is untouched
		t.Fatalf("deleted %v", del)
	}
	if len(e.f.of("DELETE", "/fapi/v1/algoOrder")) != 1 || len(e.b.unknown) != 0 {
		t.Fatalf("algo deletes / unknown: %v", e.b.unknown)
	}
	if o := e.f.of("POST", "/fapi/v1/order"); len(o) != 1 || o[0].Get("side") != "SELL" {
		t.Fatalf("close orders %v", o)
	}
}

func TestReconcileClosesRecordedLeftovers(t *testing.T) {
	e := newEnv(t)
	e.t.Store.SetSlots(map[string]state.SlotRec{"XUSDT": {Side: "long", State: "open", Qty: 9}})
	e.f.answers["GET /fapi/v1/openOrders"] = `[{"symbol":"XUSDT","clientOrderId":"dbbt1"}]`
	e.f.pos["XUSDT"] = 9
	if err := e.b.reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.of("DELETE", "/fapi/v1/order")) != 1 || len(e.f.of("POST", "/fapi/v1/order")) != 1 {
		t.Fatal("leftover order not cancelled or position not closed")
	}
	if len(e.t.Store.Slots()) != 0 {
		t.Fatal("slots not cleared")
	}
	evs := e.t.Store.Events()
	if m := evs[len(evs)-1].Msg; m != "restart: cancelled 1 leftover bot order(s), closed leftovers on XUSDT" || !evs[len(evs)-1].Notify {
		t.Fatalf("event %q", m)
	}
}
