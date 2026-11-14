// Package live is the live broker (bounce/live.py): a signed REST client for Binance USDⓈ-M, the account check
// and the LiveBroker that implements trader.Broker with real orders.
//
// Keys: BINANCE_API_KEY + BINANCE_API_SECRET (HMAC-SHA256, hex) or BINANCE_PRIVATE_KEY (Ed25519 PEM, base64
// signature). The signature covers exactly the query string that is sent.
package live

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"densitybounce/internal/pyfmt"
	"densitybounce/internal/trader"
)

// Endpoints and the client-id prefix of every order of this bot.
const (
	FAPI   = "https://fapi.binance.com"
	SAPI   = "https://api.binance.com"
	PREFIX = "dbb"
)

// BinanceError is an error answer of the exchange ({"code": -2013, "msg": "..."}).
type BinanceError struct {
	C      int
	Msg    string
	Status int
}

func (e *BinanceError) Error() string { return fmt.Sprintf("%d %s", e.C, e.Msg) }

// Code is the exchange error code (the trader reads it through an interface to catch -2021).
func (e *BinanceError) Code() int { return e.C }

// code is the code of an error that is a BinanceError (ok false for a network error).
func code(err error) (int, bool) {
	var be *BinanceError
	if errors.As(err, &be) {
		return be.C, true
	}
	return 0, false
}

// Q is the parameter list of a request.
type Q map[string]string

// num decodes a JSON number or a quoted number (Binance sends most numbers as strings).
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*n = num(f)
	return nil
}

// Decimals is the number of decimals of a step: Decimal(str(x)).normalize() exponent, at least 0.
func Decimals(x float64) int {
	s := strconv.FormatFloat(x, 'e', -1, 64) // shortest digits, "d.ddde-04"
	mant, exps, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(exps)
	digits := strings.TrimLeft(strings.ReplaceAll(mant, ".", ""), "-")
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return 0
	}
	// value = d.ddd x 10^exp -> the last kept digit sits at exp - (len(digits)-1)
	last := exp - (len(digits) - 1)
	return max(0, -last)
}

// Fmt rounds x to the grid of step (half to even) and formats it without float noise.
func Fmt(x, step float64) string {
	n := math.RoundToEven(x / step)
	return strconv.FormatFloat(n*step, 'f', Decimals(step), 64)
}

// Rest is the signed REST client (HMAC-SHA256 or Ed25519).
type Rest struct {
	key    string
	secret string
	ed     ed25519.PrivateKey
	HTTP   *http.Client
	Fapi   string
	Sapi   string
	Now    func() time.Time

	mu     sync.Mutex
	offset int64
	weight int
}

// NewRest builds a client. privatePEM may carry a literal "\n" (Railway variables).
func NewRest(apiKey, secret, privatePEM string) (*Rest, error) {
	if apiKey == "" {
		return nil, errors.New("live mode needs BINANCE_API_KEY")
	}
	r := &Rest{key: apiKey, secret: secret, HTTP: &http.Client{Timeout: 10 * time.Second}, Fapi: FAPI, Sapi: SAPI,
		Now: time.Now}
	if privatePEM != "" {
		block, _ := pem.Decode([]byte(strings.ReplaceAll(privatePEM, `\n`, "\n")))
		if block == nil {
			return nil, errors.New("BINANCE_PRIVATE_KEY must be an Ed25519 private key (PEM)")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("BINANCE_PRIVATE_KEY: %w", err)
		}
		ek, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("BINANCE_PRIVATE_KEY must be an Ed25519 private key (PEM)")
		}
		r.ed = ek
	}
	if r.secret == "" && r.ed == nil {
		return nil, errors.New("live mode needs BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM)")
	}
	return r, nil
}

// RestFromEnv reads BINANCE_API_KEY, BINANCE_API_SECRET and BINANCE_PRIVATE_KEY.
func RestFromEnv() (*Rest, error) {
	return NewRest(strings.TrimSpace(os.Getenv("BINANCE_API_KEY")), strings.TrimSpace(os.Getenv("BINANCE_API_SECRET")),
		strings.TrimSpace(os.Getenv("BINANCE_PRIVATE_KEY")))
}

// Sign signs a query string.
func (r *Rest) Sign(payload string) string {
	if r.ed != nil {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(r.ed, []byte(payload)))
	}
	m := hmac.New(sha256.New, []byte(r.secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// Weight is the last X-MBX-USED-WEIGHT-1M the exchange reported.
func (r *Rest) Weight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.weight
}

// SyncTime measures the offset between the exchange clock and ours.
func (r *Rest) SyncTime() error {
	resp, err := r.HTTP.Get(r.Fapi + "/fapi/v1/time")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	r.mu.Lock()
	r.offset = t.ServerTime - r.Now().UnixMilli()
	r.mu.Unlock()
	return nil
}

// Call sends one request and returns the body of a 200 answer. A -1021 (timestamp outside recvWindow) resyncs the
// clock and retries once. An error answer is a *BinanceError; anything else is a network error.
func (r *Rest) Call(method, path string, signed bool, base string, params Q) ([]byte, error) {
	if base == "" {
		base = r.Fapi
	}
	for attempt := 0; attempt < 2; attempt++ {
		v := url.Values{}
		for k, x := range params {
			v.Set(k, x)
		}
		var q string
		if signed {
			r.mu.Lock()
			off := r.offset
			r.mu.Unlock()
			v.Set("timestamp", strconv.FormatInt(r.Now().UnixMilli()+off, 10))
			v.Set("recvWindow", "5000")
			q = v.Encode()
			q += "&signature=" + url.QueryEscape(r.Sign(q))
		} else {
			q = v.Encode()
		}
		u := base + path
		if q != "" {
			u += "?" + q
		}
		req, err := http.NewRequest(method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-MBX-APIKEY", r.key)
		resp, err := r.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if w := resp.Header.Get("X-MBX-USED-WEIGHT-1M"); w != "" {
			if n, err := strconv.Atoi(w); err == nil {
				r.mu.Lock()
				r.weight = n
				r.mu.Unlock()
			}
		}
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode == 200 {
			return body, nil
		}
		var eb struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		c, msg := 0, string(body)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if json.Unmarshal(body, &eb) == nil {
			c, msg = eb.Code, eb.Msg
		}
		if c == -1021 && attempt == 0 {
			if err := r.SyncTime(); err != nil {
				return nil, err
			}
			continue
		}
		return nil, &BinanceError{C: c, Msg: msg, Status: resp.StatusCode}
	}
	return nil, &BinanceError{C: -1021, Msg: "timestamp outside recvWindow after a resync"}
}

// JSON is Call plus decoding of the answer into out.
func (r *Rest) JSON(out any, method, path string, signed bool, base string, params Q) error {
	b, err := r.Call(method, path, signed, base, params)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func errName(err error) string {
	if _, ok := code(err); ok {
		return "BinanceError"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
}

// Check says what prevents live trading ([] = ok): key permissions, position mode, balance. The error is a
// failure of the account calls after the permission read (the caller reports "account check failed").
func Check(r *Rest) ([]string, error) {
	problems := []string{}
	if err := r.SyncTime(); err != nil {
		return nil, err
	}
	var ar struct {
		EnableFutures     bool `json:"enableFutures"`
		EnableWithdrawals bool `json:"enableWithdrawals"`
	}
	if err := r.JSON(&ar, "GET", "/sapi/v1/account/apiRestrictions", true, r.Sapi, nil); err != nil {
		return append(problems, fmt.Sprintf("cannot read the key's permissions (%s: %v)", errName(err), err)), nil
	}
	if !ar.EnableFutures {
		problems = append(problems, "futures trading is not enabled on the key (an HMAC key without an IP whitelist is "+
			"read-only: use a self-generated Ed25519 key, BINANCE_PRIVATE_KEY)")
	}
	if ar.EnableWithdrawals {
		problems = append(problems, "withdrawals must be OFF on the key")
	}
	var dual struct {
		Dual bool `json:"dualSidePosition"`
	}
	if err := r.JSON(&dual, "GET", "/fapi/v1/positionSide/dual", true, "", nil); err != nil {
		return nil, err
	}
	if dual.Dual {
		problems = append(problems, "the account is in hedge mode; the bot needs one-way mode")
	}
	var acc struct {
		Available num `json:"availableBalance"`
	}
	if err := r.JSON(&acc, "GET", "/fapi/v2/account", true, "", nil); err != nil {
		return nil, err
	}
	rk := trader.RISK
	margin := float64(rk.MaxOpen) * rk.Notional / float64(rk.Leverage) // isolated margin of every trade at once
	if float64(acc.Available) < margin {
		problems = append(problems, fmt.Sprintf("futures available balance %s < %s USDT (%d x %s USDT at x%d)",
			pyfmt.F(float64(acc.Available), 2), pyfmt.G(margin), rk.MaxOpen, pyfmt.G(rk.Notional), rk.Leverage))
	}
	return problems, nil
}
