package exchange

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
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"fundingcarry/internal/market"
)

const secret = "s3cr3t"

type rec struct {
	mu     sync.Mutex
	routes map[string]func(r *http.Request) (int, any)
	reqs   []*http.Request
}

func (c *rec) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r)
	c.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/time") {
		json.NewEncoder(w).Encode(map[string]any{"serverTime": 1_700_000_000_000})
		return
	}
	code, body := 200, any(map[string]any{})
	if f := c.routes[r.Method+" "+r.URL.Path]; f != nil {
		code, body = f(r)
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func (c *rec) params() url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, _ := url.ParseQuery(c.reqs[len(c.reqs)-1].URL.RawQuery)
	return v
}

func ok(body any) func(*http.Request) (int, any) {
	return func(*http.Request) (int, any) { return 200, body }
}

func make_(t *testing.T, routes map[string]func(*http.Request) (int, any), sec, pemKey string) (*Binance, *rec) {
	t.Helper()
	c := &rec{routes: routes}
	ts := httptest.NewServer(c)
	t.Cleanup(ts.Close)
	b, err := New("KEY", sec, pemKey, ts.URL, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.sleep = func(time.Duration) {}
	return b, c
}

func TestHMACSignatureAndKeyHeader(t *testing.T) {
	b, c := make_(t, map[string]func(*http.Request) (int, any){"GET /fapi/v1/positionSide/dual": ok(map[string]any{"dualSidePosition": false})}, secret, "")
	if h, err := b.hedge(); err != nil || h {
		t.Fatal(h, err)
	}
	req := c.reqs[len(c.reqs)-1]
	payload, sig, _ := strings.Cut(req.URL.RawQuery, "&signature=")
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	if hex.EncodeToString(m.Sum(nil)) != sig {
		t.Fatal("bad signature")
	}
	if req.Header.Get("X-MBX-APIKEY") != "KEY" || !strings.Contains(payload, "timestamp=") || !strings.Contains(payload, "recvWindow=10000") {
		t.Fatal(payload)
	}
}

func TestEd25519SignatureIsBase64AndEscaped(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for _, k := range []string{pemKey, strings.ReplaceAll(pemKey, "\n", `\n`)} { // newlines may be written as \n
		b, c := make_(t, map[string]func(*http.Request) (int, any){"GET /fapi/v1/positionSide/dual": ok(map[string]any{"dualSidePosition": true})}, "", k)
		if h, err := b.hedge(); err != nil || !h {
			t.Fatal(h, err)
		}
		payload, sig, _ := strings.Cut(c.reqs[len(c.reqs)-1].URL.RawQuery, "&signature=")
		if strings.ContainsAny(sig, "+/") {
			t.Fatal("not escaped", sig)
		}
		un, _ := url.QueryUnescape(sig)
		raw, err := base64.StdEncoding.DecodeString(un)
		if err != nil || !ed25519.Verify(pub, []byte(payload), raw) {
			t.Fatal("signature does not verify", err)
		}
	}
}

func TestKeyErrors(t *testing.T) {
	if _, err := New("", "s", "", "", "", nil); err == nil || err.Error() != "live mode needs BINANCE_API_KEY" {
		t.Fatal(err)
	}
	if _, err := New("K", "", "", "", "", nil); err == nil || !strings.Contains(err.Error(), "BINANCE_API_SECRET") {
		t.Fatal(err)
	}
}

func TestPermissionsCheckNamesWhatIsMissing(t *testing.T) {
	b, _ := make_(t, map[string]func(*http.Request) (int, any){"GET /sapi/v1/account/apiRestrictions": ok(map[string]any{
		"enableReading": true, "enableSpotAndMarginTrading": false, "enableFutures": true,
		"permitsUniversalTransfer": false, "enableWithdrawals": true})}, secret, "")
	got, err := b.CheckPermissions()
	want := []string{"spot trading", "universal transfer", "withdrawals must be OFF"}
	if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatal(got, err)
	}
}

func TestStopGoesToTheAlgoServiceWithClosePosition(t *testing.T) {
	b, c := make_(t, map[string]func(*http.Request) (int, any){
		"GET /fapi/v1/positionSide/dual": ok(map[string]any{"dualSidePosition": true}),
		"POST /fapi/v1/algoOrder":        ok(map[string]any{"algoId": 77})}, secret, "")
	var f market.Filter
	f.Type, f.TickSize = "PRICE_FILTER", market.Flex{S: "0.0001", Set: true}
	b.perps = func() ([]market.Symbol, error) {
		return []market.Symbol{{Symbol: "XUSDT", Filters: []market.Filter{f}}}, nil
	}
	id, err := b.PlaceStop("XUSDT", 2.123456)
	if err != nil || id != "77" {
		t.Fatal(id, err)
	}
	p := c.params()
	if p.Get("algoType") != "CONDITIONAL" || p.Get("type") != "STOP_MARKET" || p.Get("side") != "BUY" || p.Get("closePosition") != "true" ||
		p.Get("workingType") != "MARK_PRICE" || p.Get("positionSide") != "SHORT" {
		t.Fatal(p)
	}
	if p.Get("triggerPrice") != "2.1234" || p.Has("quantity") || p.Has("reduceOnly") {
		t.Fatal(p)
	}
	if cid := p.Get("clientAlgoId"); !strings.HasPrefix(cid, "fcs-") || len(cid) > 36 {
		t.Fatal(cid)
	}
}

func TestCancelStopsOnlyTouchesThisBotsOrders(t *testing.T) {
	b, c := make_(t, map[string]func(*http.Request) (int, any){
		"GET /fapi/v1/openAlgoOrders": ok([]any{map[string]any{"algoId": 1, "clientAlgoId": "fcs-1-1"}, map[string]any{"algoId": 2, "clientAlgoId": "owner_sl"}}),
		"DELETE /fapi/v1/algoOrder":   ok(map[string]any{"code": "200"})}, secret, "")
	if err := b.CancelStops("XUSDT"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range c.reqs {
		if r.Method == "DELETE" {
			n++
			if r.URL.Query().Get("algoId") != "1" {
				t.Fatal(r.URL.RawQuery)
			}
		}
	}
	if n != 1 {
		t.Fatal(n)
	}
}

func TestSpotBuyNetQuantityAfterTheCoinFee(t *testing.T) {
	b, _ := make_(t, map[string]func(*http.Request) (int, any){"POST /api/v3/order": ok(map[string]any{
		"executedQty": "100", "cummulativeQuoteQty": "50", "status": "FILLED",
		"fills": []any{map[string]any{"commission": "0.1", "commissionAsset": "X"}}})}, secret, "")
	f, err := b.SpotOrder("XUSDT", "BUY", 100)
	if err != nil || math.Abs(f.Qty-99.9) > 1e-9 || math.Abs(f.Price-0.5) > 1e-9 || math.Abs(f.FeeUSDT-0.05) > 1e-9 {
		t.Fatal(f, err)
	}
}

func TestOneWayCloseIsReduceOnlyHedgeUsesPositionSide(t *testing.T) {
	b, c := make_(t, map[string]func(*http.Request) (int, any){
		"GET /fapi/v1/positionSide/dual": ok(map[string]any{"dualSidePosition": false}),
		"POST /fapi/v1/order":            ok(map[string]any{"executedQty": "10", "avgPrice": "2", "cumQuote": "20"})}, secret, "")
	f, err := b.PerpOrder("XUSDT", "BUY", 10, true)
	if err != nil || math.Abs(f.FeeUSDT-20*0.0005) > 1e-12 {
		t.Fatal(f, err)
	}
	if p := c.params(); p.Get("reduceOnly") != "true" || p.Has("positionSide") {
		t.Fatal(p)
	}
	b.setHedge(true)
	b.PerpOrder("XUSDT", "BUY", 10, true)
	if p := c.params(); p.Get("positionSide") != "SHORT" || p.Has("reduceOnly") {
		t.Fatal(p)
	}
}

func TestMarginTypeAlreadyIsolatedIsFineOtherErrorsRaise(t *testing.T) {
	n := 0
	b, _ := make_(t, map[string]func(*http.Request) (int, any){
		"POST /fapi/v1/marginType": func(*http.Request) (int, any) {
			n++
			code := -4048
			if n == 1 {
				code = -4046
			}
			return 400, map[string]any{"code": code, "msg": "x"}
		},
		"POST /fapi/v1/leverage": ok(map[string]any{"leverage": 1})}, secret, "")
	if err := b.PrepareShort("XUSDT"); err != nil {
		t.Fatal(err)
	}
	err := b.PrepareShort("XUSDT")
	be, isBE := err.(*BinanceError)
	if !isBE || be.Code == nil || *be.Code != -4048 || !strings.Contains(err.Error(), "POST /fapi/v1/marginType: HTTP 400 code -4048: x") {
		t.Fatal(err)
	}
}

func TestClockDriftResyncsAndRetries(t *testing.T) {
	n := 0
	b, _ := make_(t, map[string]func(*http.Request) (int, any){
		"GET /fapi/v3/balance": func(*http.Request) (int, any) {
			n++
			if n == 1 {
				return 400, map[string]any{"code": -1021, "msg": "Timestamp outside of the recvWindow"}
			}
			return 200, []any{map[string]any{"asset": "USDT", "availableBalance": "12.5"}}
		}}, secret, "")
	v, err := b.FuturesAvailable()
	if err != nil || v != 12.5 || n != 2 {
		t.Fatal(v, err, n)
	}
}

func TestNumAndClientIDs(t *testing.T) {
	for in, want := range map[float64]string{100: "100", 0.5: "0.5", 2.1234000000000002: "2.1234", 0: "0", 1e-11: "0"} {
		if got := Num(in); got != want {
			t.Fatalf("Num(%v) = %q want %q", in, got, want)
		}
	}
	b, _ := make_(t, nil, secret, "")
	a, c := b.cid("fcs"), b.cid("fc")
	if !strings.HasPrefix(a, "fcs-") || !strings.HasPrefix(c, "fc-") || a == c || len(a) > 36 {
		t.Fatal(a, c)
	}
}
