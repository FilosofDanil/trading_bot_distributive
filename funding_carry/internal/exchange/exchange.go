// Package exchange is the signed Binance client for live carry: spot, USD-M futures and the universal transfer
// (carry/exchange.py). It implements live.Exchange.
//
// Keys come from the environment only: BINANCE_API_KEY plus either BINANCE_API_SECRET (HMAC-SHA256) or
// BINANCE_PRIVATE_KEY (an Ed25519 private key in PEM, for a self-generated key). Binance gives an HMAC key without
// an IP whitelist read permission only, so on a host without a static IP the key has to be a self-generated one;
// CheckPermissions reads GET /sapi/v1/account/apiRestrictions and the runner refuses to trade without spot
// trading and futures; without universal transfer it trades with each wallet's own USDT (internal/live).
//
// Notes from the API (2025-12): conditional orders (STOP_MARKET) live in the Algo Service
// (POST /fapi/v1/algoOrder; /fapi/v1/order answers -4120); the margin type cannot change while a position or an
// order exists (-4048 / -4047; -4046 = already that type); a spot BUY pays its taker fee in the bought coin.
package exchange

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fundingcarry/internal/live"
	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
)

const (
	RecvWindow = 10_000
	OrderTag   = "fc" // newClientOrderId / clientAlgoId prefix: fc-<ms>-<n>, stops fcs-<ms>-<n>
	StopTag    = "fcs"
)

// BinanceError is an error answer of Binance.
type BinanceError struct {
	Status   int
	Code     *int64
	CodeText string // the code as Binance sent it ("None" when absent)
	Msg      string
	Path     string
}

func (e *BinanceError) Error() string {
	return fmt.Sprintf("%s: HTTP %d code %s: %s", e.Path, e.Status, e.CodeText, e.Msg)
}
func (e *BinanceError) PyName() string { return "BinanceError" }

type keyError struct{ k string }

func (e *keyError) Error() string  { return pyx.Repr(e.k) }
func (e *keyError) PyName() string { return "KeyError" }

type stopIteration struct{}

func (e *stopIteration) Error() string  { return "" }
func (e *stopIteration) PyName() string { return "StopIteration" }

// Param is one query parameter; the order is the order of the signed query string.
type Param struct{ K, V string }

type signer struct {
	secret string
	key    ed25519.PrivateKey
}

func newSigner(secret, privatePEM string) (*signer, error) {
	s := &signer{secret: secret}
	if privatePEM != "" {
		block, _ := pem.Decode([]byte(strings.ReplaceAll(privatePEM, `\n`, "\n")))
		if block == nil {
			return nil, fmt.Errorf("BINANCE_PRIVATE_KEY is not a PEM private key")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("BINANCE_PRIVATE_KEY: %w", err)
		}
		ek, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("BINANCE_PRIVATE_KEY must be an Ed25519 private key (PEM)")
		}
		s.key = ek
	}
	if s.secret == "" && s.key == nil {
		return nil, fmt.Errorf("live mode needs BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM)")
	}
	return s, nil
}

// sign signs exactly the query string that is sent: Ed25519 base64 when a private key is set, else HMAC-SHA256 hex.
func (s *signer) sign(payload string) string {
	if s.key != nil {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(payload)))
	}
	m := hmac.New(sha256.New, []byte(s.secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// Binance is the client.
type Binance struct {
	apiKey string
	signer *signer
	fapi   string
	spot   string
	http   *http.Client

	mu     sync.Mutex
	offset map[string]int64
	hedgeV *bool
	ids    atomic.Int64

	perps    func() ([]market.Symbol, error)
	spotInfo func() ([]market.Symbol, error)
	sleep    func(time.Duration)
}

// New makes the client and syncs the clock offset. hc may be nil (20 s timeout).
func New(apiKey, secret, privatePEM, fapi, spot string, hc *http.Client) (*Binance, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("live mode needs BINANCE_API_KEY")
	}
	sg, err := newSigner(secret, privatePEM)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	b := &Binance{apiKey: apiKey, signer: sg, fapi: fapi, spot: spot, http: hc,
		offset: map[string]int64{fapi: 0, spot: 0}, sleep: time.Sleep,
		perps:    func() ([]market.Symbol, error) { return market.Perpetuals() },
		spotInfo: func() ([]market.Symbol, error) { return market.SpotInfo() }}
	if err := b.SyncTime(); err != nil {
		return nil, err
	}
	return b, nil
}

// FromEnv reads BINANCE_API_KEY and BINANCE_API_SECRET / BINANCE_PRIVATE_KEY.
func FromEnv() (*Binance, error) {
	g := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	return New(g("BINANCE_API_KEY"), g("BINANCE_API_SECRET"), g("BINANCE_PRIVATE_KEY"), market.FAPI, market.SPOT, nil)
}

// ------------------------------------------------------------------ transport

// SyncTime reads the server clocks (futures and spot) and stores the offsets.
func (b *Binance) SyncTime() error {
	for _, bp := range [][2]string{{b.fapi, "/fapi/v1/time"}, {b.spot, "/api/v3/time"}} {
		t0 := float64(time.Now().UnixNano()) / 1e9
		r, err := b.http.Get(bp[0] + bp[1])
		if err != nil {
			return &live.ExchangeError{Msg: fmt.Sprintf("GET %s: %v", bp[1], err)}
		}
		var body map[string]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		err = dec.Decode(&body)
		r.Body.Close()
		if err != nil {
			return err
		}
		server, ok := num(body["serverTime"])
		if !ok {
			return &keyError{"serverTime"}
		}
		t1 := float64(time.Now().UnixNano()) / 1e9
		b.mu.Lock()
		b.offset[bp[0]] = int64(server - (t0+t1)/2*1000)
		b.mu.Unlock()
	}
	return nil
}

func (b *Binance) cid(tag string) string {
	s := fmt.Sprintf("%s-%d-%d", tag, time.Now().UnixMilli(), b.ids.Add(1))
	if len(s) > 36 {
		s = s[:36]
	}
	return s
}

func encode(ps []Param) string {
	var sb strings.Builder
	for i, p := range ps {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(p.K))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(p.V))
	}
	return sb.String()
}

// Call sends a (signed) request and returns the decoded JSON (numbers as json.Number).
func (b *Binance) Call(method, base, path string, params []Param, signed bool) (any, error) {
	for attempt := 0; attempt < 3; attempt++ {
		ps := append([]Param{}, params...)
		var q string
		if signed {
			b.mu.Lock()
			off := b.offset[base]
			b.mu.Unlock()
			ps = append(ps, Param{"recvWindow", strconv.Itoa(RecvWindow)},
				Param{"timestamp", strconv.FormatInt(time.Now().UnixMilli()+off, 10)})
			q = encode(ps)
			q += "&signature=" + url.QueryEscape(b.signer.sign(q)) // Ed25519 base64 needs escaping
		} else {
			q = encode(ps)
		}
		req, err := http.NewRequest(method, base+path+"?"+q, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-MBX-APIKEY", b.apiKey)
		r, err := b.http.Do(req)
		if err != nil {
			if attempt == 2 || method != "GET" {
				return nil, &live.ExchangeError{Msg: fmt.Sprintf("%s %s: %v", method, path, err)}
			}
			b.sleep(2 * time.Second)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		r.Body.Close()
		if rerr != nil {
			if attempt == 2 || method != "GET" {
				return nil, &live.ExchangeError{Msg: fmt.Sprintf("%s %s: %v", method, path, rerr)}
			}
			b.sleep(2 * time.Second)
			continue
		}
		if r.StatusCode < 400 {
			var v any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			return v, nil
		}
		var body map[string]any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&body) != nil {
			body = nil
		}
		code, codeText := (*int64)(nil), "None"
		if c, ok := body["code"]; ok && c != nil {
			codeText = pyx.Str(c)
			if n, ok := c.(json.Number); ok {
				if v, err := n.Int64(); err == nil {
					code = &v
				}
			}
		}
		if code != nil && *code == -1021 && attempt < 2 { // clock drift: resync and retry
			if err := b.SyncTime(); err != nil {
				return nil, err
			}
			continue
		}
		if r.StatusCode == 429 || r.StatusCode == 418 || r.StatusCode >= 500 {
			if attempt < 2 && method == "GET" {
				b.sleep(time.Duration(5*(attempt+1)) * time.Second)
				continue
			}
		}
		msg, ok := body["msg"]
		text := pyx.Cut(string(raw), 200)
		if ok {
			text = pyx.Str(msg)
		}
		return nil, &BinanceError{Status: r.StatusCode, Code: code, CodeText: codeText, Msg: text, Path: method + " " + path}
	}
	return nil, &live.ExchangeError{Msg: fmt.Sprintf("%s %s: retries exhausted", method, path)}
}

// ------------------------------------------------------------------ JSON helpers (float(x), str(x), dict.get)

type obj = map[string]any

func asObj(v any) obj {
	o, _ := v.(map[string]any)
	return o
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// fnum is float(o[key]).
func fnum(o obj, key string) (float64, error) {
	v, ok := o[key]
	if !ok {
		return 0, &keyError{key}
	}
	f, ok := num(v)
	if !ok {
		return 0, fmt.Errorf("could not convert %s to float: %s", pyx.Repr(key), pyx.Str(v))
	}
	return f, nil
}

// fnumOr0 is float(o.get(key) or 0).
func fnumOr0(o obj, key string) (float64, error) {
	v := o[key]
	if !pyx.Truthy(v) {
		return 0, nil
	}
	f, ok := num(v)
	if !ok {
		return 0, fmt.Errorf("could not convert %s to float: %s", pyx.Repr(key), pyx.Str(v))
	}
	return f, nil
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// ------------------------------------------------------------------ permissions / mode

// CheckPermissions returns the permissions missing for live carry ([] = ok).
func (b *Binance) CheckPermissions() ([]string, error) {
	v, err := b.Call("GET", b.spot, "/sapi/v1/account/apiRestrictions", nil, true)
	if err != nil {
		return nil, err
	}
	r := asObj(v)
	var missing []string
	for _, n := range [][2]string{{"enableReading", "reading"}, {"enableSpotAndMarginTrading", "spot trading"},
		{"enableFutures", "futures"}, {"permitsUniversalTransfer", "universal transfer"}} {
		if !pyx.Truthy(r[n[0]]) {
			missing = append(missing, n[1])
		}
	}
	if pyx.Truthy(r["enableWithdrawals"]) {
		missing = append(missing, "withdrawals must be OFF")
	}
	return missing, nil
}

func (b *Binance) hedge() (bool, error) {
	b.mu.Lock()
	h := b.hedgeV
	b.mu.Unlock()
	if h != nil {
		return *h, nil
	}
	v, err := b.Call("GET", b.fapi, "/fapi/v1/positionSide/dual", nil, true)
	if err != nil {
		return false, err
	}
	d, ok := asObj(v)["dualSidePosition"]
	if !ok {
		return false, &keyError{"dualSidePosition"}
	}
	t := pyx.Truthy(d)
	b.mu.Lock()
	b.hedgeV = &t
	b.mu.Unlock()
	return t, nil
}

func (b *Binance) setHedge(v bool) {
	b.mu.Lock()
	b.hedgeV = &v
	b.mu.Unlock()
}

// ------------------------------------------------------------------ market data

func (b *Binance) Prices(sym string) (float64, float64, error) {
	v, err := b.Call("GET", b.fapi, "/fapi/v1/premiumIndex", []Param{{"symbol", sym}}, false)
	if err != nil {
		return 0, 0, err
	}
	mark, err := fnum(asObj(v), "markPrice")
	if err != nil {
		return 0, 0, err
	}
	v, err = b.Call("GET", b.spot, "/api/v3/ticker/price", []Param{{"symbol", sym}}, false)
	if err != nil {
		return 0, 0, err
	}
	last, err := fnum(asObj(v), "price")
	if err != nil {
		return 0, 0, err
	}
	return mark, last, nil
}

func (b *Binance) Filters(sym string) (ss, smin, ps, pmin float64, err error) {
	perps, err := b.perps()
	if err != nil {
		return
	}
	var fut, spt *market.Symbol
	for i := range perps {
		if perps[i].Symbol == sym {
			fut = &perps[i]
			break
		}
	}
	info, err := b.spotInfo()
	if err != nil {
		return
	}
	for i := range info {
		if info[i].Symbol == sym {
			spt = &info[i]
			break
		}
	}
	if fut == nil || spt == nil {
		err = live.Errorf("%s: not trading on both spot and futures", sym)
		return
	}
	ss, smin = steps(*spt, true)
	ps, pmin = steps(*fut, false)
	return
}

func steps(info market.Symbol, spot bool) (step, mn float64) {
	mn = 5.0
	for _, f := range info.Filters {
		if f.Type == "LOT_SIZE" || f.Type == "MARKET_LOT_SIZE" {
			if v, ok := f.StepSize.Float(); ok && v > 0 && v > step {
				step = v
			}
		}
		if spot && (f.Type == "NOTIONAL" || f.Type == "MIN_NOTIONAL") {
			if v, ok := f.MinNotional.Float(); ok {
				mn = v
			}
		}
		if !spot && f.Type == "MIN_NOTIONAL" {
			if v, ok := f.Notional.Float(); ok {
				mn = v
			}
		}
	}
	return step, mn
}

func (b *Binance) tick(sym string) (float64, error) {
	perps, err := b.perps()
	if err != nil {
		return 0, err
	}
	for _, s := range perps {
		if s.Symbol == sym {
			for _, f := range s.Filters {
				if f.Type == "PRICE_FILTER" {
					v, ok := f.TickSize.Float()
					if !ok {
						return 0, &keyError{"tickSize"}
					}
					return v, nil
				}
			}
			return 0.0, nil
		}
	}
	return 0, &stopIteration{}
}

// ------------------------------------------------------------------ balances / transfer

func (b *Binance) SpotFree(asset string) (float64, error) {
	v, err := b.Call("GET", b.spot, "/api/v3/account", []Param{{"omitZeroBalances", "true"}}, true)
	if err != nil {
		return 0, err
	}
	for _, x := range list(asObj(v)["balances"]) {
		bal := asObj(x)
		if pyx.Str(bal["asset"]) == asset {
			return fnum(bal, "free")
		}
	}
	return 0.0, nil
}

func (b *Binance) FuturesAvailable() (float64, error) {
	v, err := b.Call("GET", b.fapi, "/fapi/v3/balance", nil, true)
	if err != nil {
		return 0, err
	}
	for _, x := range list(v) {
		bal := asObj(x)
		if pyx.Str(bal["asset"]) == "USDT" {
			return fnum(bal, "availableBalance")
		}
	}
	return 0.0, nil
}

func (b *Binance) Transfer(amount float64, toFutures bool) error {
	t := "UMFUTURE_MAIN"
	if toFutures {
		t = "MAIN_UMFUTURE"
	}
	_, err := b.Call("POST", b.spot, "/sapi/v1/asset/transfer",
		[]Param{{"type", t}, {"asset", "USDT"}, {"amount", fmt.Sprintf("%.2f", amount)}}, true)
	return err
}

// ------------------------------------------------------------------ futures

func (b *Binance) PrepareShort(sym string) error {
	_, err := b.Call("POST", b.fapi, "/fapi/v1/marginType", []Param{{"symbol", sym}, {"marginType", "ISOLATED"}}, true)
	if err != nil {
		be, ok := err.(*BinanceError)
		if !ok || be.Code == nil || *be.Code != -4046 { // -4046: already isolated
			return err
		}
	}
	_, err = b.Call("POST", b.fapi, "/fapi/v1/leverage", []Param{{"symbol", sym}, {"leverage", "1"}}, true)
	return err
}

// Num formats a quantity or price: %.10f with trailing zeros trimmed.
func Num(x float64) string {
	s := fmt.Sprintf("%.10f", x)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" {
		return "0"
	}
	return s
}

func (b *Binance) PerpOrder(sym, side string, qty float64, reduce bool) (live.Fill, error) {
	p := []Param{{"symbol", sym}, {"side", side}, {"type", "MARKET"}, {"quantity", Num(qty)},
		{"newOrderRespType", "RESULT"}, {"newClientOrderId", b.cid(OrderTag)}}
	h, err := b.hedge()
	if err != nil {
		return live.Fill{}, err
	}
	if h {
		p = append(p, Param{"positionSide", "SHORT"})
	} else if reduce {
		p = append(p, Param{"reduceOnly", "true"})
	}
	v, err := b.Call("POST", b.fapi, "/fapi/v1/order", p, true)
	if err != nil {
		return live.Fill{}, err
	}
	r := asObj(v)
	q, err := fnum(r, "executedQty")
	if err != nil {
		return live.Fill{}, err
	}
	px, err := fnum(r, "avgPrice")
	if err != nil {
		return live.Fill{}, err
	}
	if q <= 0 {
		return live.Fill{}, live.Errorf("%s: perp %s %s not filled (%s)", sym, side, pyx.Float(qty), pyx.Str(r["status"]))
	}
	cq, err := fnumOr0(r, "cumQuote")
	if err != nil {
		return live.Fill{}, err
	}
	if cq == 0 {
		cq = q * px
	}
	return live.Fill{Qty: q, Price: px, FeeUSDT: cq * live.PerpFee}, nil
}

func (b *Binance) AddMargin(sym string, amount float64) error {
	h, err := b.hedge()
	if err != nil {
		return err
	}
	side := "BOTH"
	if h {
		side = "SHORT"
	}
	_, err = b.Call("POST", b.fapi, "/fapi/v1/positionMargin",
		[]Param{{"symbol", sym}, {"amount", fmt.Sprintf("%.2f", amount)}, {"type", "1"}, {"positionSide", side}}, true)
	return err
}

func (b *Binance) Positions() (map[string]live.Perp, error) {
	v, err := b.Call("GET", b.fapi, "/fapi/v3/positionRisk", nil, true)
	if err != nil {
		return nil, err
	}
	out := map[string]live.Perp{}
	for _, x := range list(v) {
		r := asObj(x)
		amt, err := fnum(r, "positionAmt")
		if err != nil {
			return nil, err
		}
		if amt == 0 {
			continue
		}
		h, err := b.hedge()
		if err != nil {
			return nil, err
		}
		if h && pyx.Str(r["positionSide"]) != "SHORT" {
			continue
		}
		entry, err := fnum(r, "entryPrice")
		if err != nil {
			return nil, err
		}
		mark, err := fnum(r, "markPrice")
		if err != nil {
			return nil, err
		}
		liq, err := fnumOr0(r, "liquidationPrice")
		if err != nil {
			return nil, err
		}
		margin, err := fnumOr0(r, "isolatedMargin")
		if err != nil {
			return nil, err
		}
		out[pyx.Str(r["symbol"])] = live.Perp{Amt: amt, Entry: entry, Mark: mark, Liq: liq, Margin: margin}
	}
	return out, nil
}

func (b *Binance) Stops(syms []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, s := range syms { // per symbol: weight 1 (40 without a symbol)
		v, err := b.Call("GET", b.fapi, "/fapi/v1/openAlgoOrders", []Param{{"symbol", s}}, true)
		if err != nil {
			return nil, err
		}
		for _, x := range list(v) {
			o := asObj(x)
			if strings.HasPrefix(pyx.Str(o["clientAlgoId"]), StopTag+"-") {
				id, ok := o["algoId"]
				if !ok {
					return nil, &keyError{"algoId"}
				}
				out[s] = append(out[s], pyx.Str(id))
			}
		}
	}
	return out, nil
}

func (b *Binance) PlaceStop(sym string, trigger float64) (string, error) {
	tick, err := b.tick(sym)
	if err != nil {
		return "", err
	}
	trig := trigger
	if tick != 0 {
		trig = live.FloorStep(trigger, tick)
	}
	h, err := b.hedge()
	if err != nil {
		return "", err
	}
	side := "BOTH"
	if h {
		side = "SHORT"
	}
	p := []Param{{"algoType", "CONDITIONAL"}, {"symbol", sym}, {"side", "BUY"}, {"type", "STOP_MARKET"},
		{"triggerPrice", Num(trig)}, {"closePosition", "true"}, {"workingType", "MARK_PRICE"},
		{"positionSide", side}, {"clientAlgoId", b.cid(StopTag)}}
	v, err := b.Call("POST", b.fapi, "/fapi/v1/algoOrder", p, true)
	if err != nil {
		return "", err
	}
	id, ok := asObj(v)["algoId"]
	if !ok {
		return "", &keyError{"algoId"}
	}
	return pyx.Str(id), nil
}

func (b *Binance) CancelStops(sym string) error {
	ids, err := b.Stops([]string{sym}) // only this bot's stops, never the owner's own
	if err != nil {
		return err
	}
	for _, id := range ids[sym] {
		if _, err := b.Call("DELETE", b.fapi, "/fapi/v1/algoOrder", []Param{{"algoId", id}}, true); err != nil {
			return err
		}
	}
	return nil
}

func (b *Binance) Funding(sym string, sinceMS int64) (float64, error) {
	v, err := b.Call("GET", b.fapi, "/fapi/v1/income", []Param{{"symbol", sym}, {"incomeType", "FUNDING_FEE"},
		{"startTime", strconv.FormatInt(sinceMS, 10)}, {"limit", "1000"}}, true)
	if err != nil {
		return 0, err
	}
	var vals []float64
	for _, x := range list(v) {
		f, err := fnum(asObj(x), "income")
		if err != nil {
			return 0, err
		}
		vals = append(vals, f)
	}
	return pyx.PySum(vals), nil
}

// ------------------------------------------------------------------ spot

func (b *Binance) SpotOrder(sym, side string, qty float64) (live.Fill, error) {
	v, err := b.Call("POST", b.spot, "/api/v3/order", []Param{{"symbol", sym}, {"side", side}, {"type", "MARKET"},
		{"quantity", Num(qty)}, {"newOrderRespType", "FULL"}, {"newClientOrderId", b.cid(OrderTag)}}, true)
	if err != nil {
		return live.Fill{}, err
	}
	r := asObj(v)
	executed, err := fnum(r, "executedQty")
	if err != nil {
		return live.Fill{}, err
	}
	quote, err := fnum(r, "cummulativeQuoteQty")
	if err != nil {
		return live.Fill{}, err
	}
	if executed <= 0 {
		return live.Fill{}, live.Errorf("%s: spot %s %s not filled (%s)", sym, side, pyx.Float(qty), pyx.Str(r["status"]))
	}
	px := quote / executed
	base := live.BaseAsset(sym)
	var feeBase, feeUSDT []float64
	other := false
	for _, x := range list(r["fills"]) {
		f := asObj(x)
		asset := pyx.Str(f["commissionAsset"])
		switch asset {
		case base:
			c, err := fnum(f, "commission")
			if err != nil {
				return live.Fill{}, err
			}
			feeBase = append(feeBase, c)
		case "USDT":
			c, err := fnum(f, "commission")
			if err != nil {
				return live.Fill{}, err
			}
			feeUSDT = append(feeUSDT, c)
		default:
			other = true
		}
	}
	fb, fu := pyx.PySum(feeBase), pyx.PySum(feeUSDT)
	bnb := 0.0
	if other {
		bnb = quote * live.SpotFee * 0.75 // BNB: ~25 % off, estimated
	}
	fee := fb*px + fu + bnb
	net := executed
	if side == "BUY" {
		net = executed - fb
	}
	return live.Fill{Qty: net, Price: px, FeeUSDT: fee}, nil
}
