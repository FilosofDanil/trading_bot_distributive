// Package binance is a minimal client for the Binance USDⓈ-M futures API: balance,
// positions, orders, position closing and the user data stream.
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const recvWindow = "10000"

// APIError is an error body returned by Binance ({"code":-2015,"msg":"..."}).
type APIError struct {
	HTTPStatus int
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("binance %d: %s (http %d)", e.Code, e.Msg, e.HTTPStatus)
}

// IsAuthError reports whether err means the API key/secret/permissions are wrong.
func IsAuthError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case -2014, -2015, -1022, -2008:
		return true
	}
	return ae.HTTPStatus == http.StatusUnauthorized
}

type Client struct {
	FapiURL string
	SapiURL string
	key     string
	secret  string
	http    *http.Client
	offset  atomic.Int64 // server time minus local time, ms
}

func New(fapiURL, sapiURL, key, secret string) *Client {
	return &Client{
		FapiURL: strings.TrimRight(fapiURL, "/"),
		SapiURL: strings.TrimRight(sapiURL, "/"),
		key:     key,
		secret:  secret,
		http:    &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Client) sign(query string) string {
	m := hmac.New(sha256.New, []byte(c.secret))
	m.Write([]byte(query))
	return hex.EncodeToString(m.Sum(nil))
}

// SyncTime measures the offset between local and Binance server clocks.
func (c *Client) SyncTime(ctx context.Context) error {
	var r struct {
		ServerTime int64 `json:"serverTime"`
	}
	before := time.Now().UnixMilli()
	if err := c.do(ctx, c.FapiURL, http.MethodGet, "/fapi/v1/time", nil, false, &r); err != nil {
		return err
	}
	after := time.Now().UnixMilli()
	c.offset.Store(r.ServerTime - (before+after)/2)
	return nil
}

func (c *Client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	err := c.do(ctx, c.FapiURL, method, path, params, true, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == -1021 { // timestamp outside recvWindow: resync and retry once
		if serr := c.SyncTime(ctx); serr == nil {
			err = c.do(ctx, c.FapiURL, method, path, params, true, out)
		}
	}
	return err
}

func (c *Client) do(ctx context.Context, base, method, path string, params url.Values, signed bool, out any) error {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	query := q.Encode()
	if signed {
		q.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli()+c.offset.Load(), 10))
		q.Set("recvWindow", recvWindow)
		query = q.Encode()
		query += "&signature=" + c.sign(query)
	}
	u := base + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("X-MBX-APIKEY", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{HTTPStatus: resp.StatusCode}
		if json.Unmarshal(body, ae) != nil || ae.Msg == "" {
			ae.Msg = strings.TrimSpace(string(body))
			if len(ae.Msg) > 200 {
				ae.Msg = ae.Msg[:200]
			}
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// Num is a Binance decimal string ("12.345") that also accepts plain JSON numbers.
type Num string

func (n *Num) UnmarshalJSON(b []byte) error {
	*n = Num(strings.Trim(string(b), `"`))
	return nil
}

func (n Num) Float() float64 {
	f, _ := strconv.ParseFloat(string(n), 64)
	return f
}

type Position struct {
	Symbol           string `json:"symbol"`
	PositionSide     string `json:"positionSide"`
	PositionAmt      Num    `json:"positionAmt"`
	UnrealizedProfit Num    `json:"unrealizedProfit"`
}

type Account struct {
	TotalWalletBalance    Num        `json:"totalWalletBalance"`
	TotalUnrealizedProfit Num        `json:"totalUnrealizedProfit"`
	TotalMarginBalance    Num        `json:"totalMarginBalance"`
	Positions             []Position `json:"positions"`
}

// OpenPositions returns the positions with a non-zero amount.
func (a *Account) OpenPositions() []Position {
	var out []Position
	for _, p := range a.Positions {
		if p.PositionAmt.Float() != 0 {
			out = append(out, p)
		}
	}
	return out
}

// Account returns balances and positions (GET /fapi/v3/account).
func (c *Client) Account(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.call(ctx, http.MethodGet, "/fapi/v3/account", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

type Order struct {
	Symbol       string `json:"symbol"`
	OrderID      int64  `json:"orderId"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	Side         string `json:"side"`
	PositionSide string `json:"positionSide"`
}

// OpenOrders lists open orders for all symbols.
func (c *Client) OpenOrders(ctx context.Context) ([]Order, error) {
	var o []Order
	return o, c.call(ctx, http.MethodGet, "/fapi/v1/openOrders", nil, &o)
}

// CancelAllOpenOrders cancels every open order on a symbol.
func (c *Client) CancelAllOpenOrders(ctx context.Context, symbol string) error {
	return c.call(ctx, http.MethodDelete, "/fapi/v1/allOpenOrders", url.Values{"symbol": {symbol}}, nil)
}

type AlgoOrder struct {
	Symbol string `json:"symbol"`
	AlgoID int64  `json:"algoId"`
}

// OpenAlgoOrders lists open conditional (algo service) orders: stops, take-profits, trailing.
func (c *Client) OpenAlgoOrders(ctx context.Context) ([]AlgoOrder, error) {
	var o []AlgoOrder
	return o, c.call(ctx, http.MethodGet, "/fapi/v1/openAlgoOrders", nil, &o)
}

// CancelAllAlgoOrders cancels every open conditional order on a symbol.
func (c *Client) CancelAllAlgoOrders(ctx context.Context, symbol string) error {
	return c.call(ctx, http.MethodDelete, "/fapi/v1/algoOpenOrders", url.Values{"symbol": {symbol}}, nil)
}

// HedgeMode reports whether the account uses dual-side (hedge) position mode.
func (c *Client) HedgeMode(ctx context.Context) (bool, error) {
	var r struct {
		DualSidePosition bool `json:"dualSidePosition"`
	}
	err := c.call(ctx, http.MethodGet, "/fapi/v1/positionSide/dual", nil, &r)
	return r.DualSidePosition, err
}

// ClosePosition sends a MARKET order that flattens p.
func (c *Client) ClosePosition(ctx context.Context, p Position, hedge bool) error {
	amt := string(p.PositionAmt)
	side := "SELL"
	if strings.HasPrefix(amt, "-") {
		side = "BUY"
		amt = strings.TrimPrefix(amt, "-")
	}
	params := url.Values{
		"symbol":   {p.Symbol},
		"side":     {side},
		"type":     {"MARKET"},
		"quantity": {amt},
	}
	if hedge && p.PositionSide != "" && p.PositionSide != "BOTH" {
		params.Set("positionSide", p.PositionSide) // reduceOnly is rejected in hedge mode
	} else {
		params.Set("reduceOnly", "true")
	}
	return c.call(ctx, http.MethodPost, "/fapi/v1/order", params, nil)
}

// Income is one record of the futures income history (GET /fapi/v1/income).
type Income struct {
	Symbol     string `json:"symbol"` // empty for transfers
	IncomeType string `json:"incomeType"`
	Income     Num    `json:"income"` // signed, in Asset
	Asset      string `json:"asset"`
	Time       int64  `json:"time"`
	TranID     Num    `json:"tranId"`
}

// IncomeSince returns every income record with time >= since, oldest first, following pages.
func (c *Client) IncomeSince(ctx context.Context, since time.Time) ([]Income, error) {
	const limit = 1000
	var all []Income
	seen := map[string]bool{}
	from := since.UnixMilli()
	for page := 0; page < 20; page++ {
		var part []Income
		p := url.Values{"startTime": {strconv.FormatInt(from, 10)}, "limit": {strconv.Itoa(limit)}}
		if err := c.call(ctx, http.MethodGet, "/fapi/v1/income", p, &part); err != nil {
			return nil, err
		}
		for _, in := range part {
			k := string(in.TranID) + "|" + in.IncomeType + "|" + in.Symbol + "|" + strconv.FormatInt(in.Time, 10)
			if !seen[k] {
				seen[k] = true
				all = append(all, in)
			}
		}
		if len(part) < limit {
			return all, nil
		}
		// the next page starts at the last timestamp (not +1): records sharing it are deduplicated
		if last := part[len(part)-1].Time; last > from {
			from = last
		} else {
			from++
		}
	}
	return nil, errors.New("income history: more than 20 pages")
}

// FuturesEnabled checks the key's permissions (spot API). ok=false with err=nil means
// the key exists but futures trading is not enabled on it.
func (c *Client) FuturesEnabled(ctx context.Context) (bool, error) {
	var r struct {
		EnableFutures bool `json:"enableFutures"`
	}
	err := c.do(ctx, c.SapiURL, http.MethodGet, "/sapi/v1/account/apiRestrictions", nil, true, &r)
	return r.EnableFutures, err
}

// NewListenKey starts (or extends) a user data stream session.
func (c *Client) NewListenKey(ctx context.Context) (string, error) {
	var r struct {
		ListenKey string `json:"listenKey"`
	}
	err := c.do(ctx, c.FapiURL, http.MethodPost, "/fapi/v1/listenKey", nil, false, &r)
	return r.ListenKey, err
}

// KeepAliveListenKey extends the listen key validity by 60 minutes.
func (c *Client) KeepAliveListenKey(ctx context.Context) error {
	return c.do(ctx, c.FapiURL, http.MethodPut, "/fapi/v1/listenKey", nil, false, nil)
}

// CloseListenKey ends the user data stream session.
func (c *Client) CloseListenKey(ctx context.Context) error {
	return c.do(ctx, c.FapiURL, http.MethodDelete, "/fapi/v1/listenKey", nil, false, nil)
}
