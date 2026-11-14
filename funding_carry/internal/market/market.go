// Package market is Binance public market data over REST (carry/market.py).
//
// The lab caches everything on disk; a runner only needs today's data, so exchange info is cached in memory
// for one UTC day (only the fields the runner reads are kept) and everything else is fetched when asked.
package market

import (
	"encoding/json"
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
)

const DayMS int64 = 86_400_000

// TfMS is the bar length of the timeframes the runner uses.
var TfMS = map[string]int64{"1h": 3_600_000, "1d": DayMS}

var (
	FAPI = strings.TrimRight(envOr("BINANCE_FAPI_URL", "https://fapi.binance.com"), "/")
	SPOT = strings.TrimRight(envOr("BINANCE_SPOT_URL", "https://api.binance.com"), "/")

	// HTTP is the shared client (timeout 30 s, as httpx.Client(timeout=30)).
	HTTP = &http.Client{Timeout: 30 * time.Second}
	// Sleep is replaced in tests so that retries do not wait.
	Sleep = time.Sleep
)

func envOr(k, d string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return d
}

// RestrictedLocation is HTTP 451: Binance refuses this server's region (US IPs). Retrying does not help.
type RestrictedLocation struct{ Msg string }

func (e *RestrictedLocation) Error() string  { return e.Msg }
func (e *RestrictedLocation) PyName() string { return "RestrictedLocation" }

// StatusError is a 4xx answer that is not retried (httpx raise_for_status).
type StatusError struct {
	Code int
	URL  string
}

func (e *StatusError) Error() string {
	kind := "Client error"
	if e.Code >= 500 {
		kind = "Server error"
	}
	return fmt.Sprintf("%s '%d %s' for url '%s'", kind, e.Code, http.StatusText(e.Code), e.URL)
}
func (e *StatusError) PyName() string { return "HTTPStatusError" }

// RuntimeError is Python's RuntimeError.
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string  { return e.Msg }
func (e *RuntimeError) PyName() string { return "RuntimeError" }

// doGet is GET with retry and a gentle brake on the 1-minute request weight. The caller closes the body.
func doGet(path string, params url.Values, base string) (*http.Response, error) {
	full := base + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	for attempt := 0; attempt < 6; attempt++ {
		r, err := HTTP.Get(full)
		if err != nil {
			Sleep(time.Duration(2*(attempt+1)) * time.Second)
			continue
		}
		if r.StatusCode == 451 {
			r.Body.Close()
			return nil, &RestrictedLocation{fmt.Sprintf("GET %s: 451 restricted location (deploy the service in an EU region)", path)}
		}
		if r.StatusCode == 418 || r.StatusCode == 429 || r.StatusCode >= 500 {
			r.Body.Close()
			Sleep(time.Duration(10*(attempt+1)) * time.Second)
			continue
		}
		if r.StatusCode >= 400 {
			r.Body.Close()
			return nil, &StatusError{Code: r.StatusCode, URL: full}
		}
		return r, nil
	}
	return nil, &RuntimeError{fmt.Sprintf("GET %s failed after retries", path)}
}

// brake sleeps 15 s when the used 1-minute weight is above 1800.
func brake(r *http.Response) {
	w := r.Header.Get("X-MBX-USED-WEIGHT-1M")
	if n, err := strconv.Atoi(w); err == nil && w != "" && n > 1800 {
		Sleep(15 * time.Second)
	}
}

// Get decodes the JSON answer of a GET into out.
func Get(path string, params url.Values, base string, out any) error {
	r, err := doGet(path, params, base)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return err
	}
	brake(r)
	return nil
}

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ------------------------------------------------------------------ exchange info (cached for one UTC day)

// Flex is a filter value that Binance sends as a string (sometimes as a number).
type Flex struct {
	S   string
	Set bool
}

func (f *Flex) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	f.Set = true
	f.S = strings.Trim(string(b), `"`)
	return nil
}

func (f Flex) MarshalJSON() ([]byte, error) { return json.Marshal(f.S) }

// Float is float(value); ok is false when the value is absent or not a number.
func (f Flex) Float() (float64, bool) {
	if !f.Set {
		return 0, false
	}
	v, err := strconv.ParseFloat(f.S, 64)
	return v, err == nil
}

// Filter keeps the filter fields the runner reads.
type Filter struct {
	Type        string `json:"filterType"`
	StepSize    Flex   `json:"stepSize"`
	MinNotional Flex   `json:"minNotional"`
	Notional    Flex   `json:"notional"`
	TickSize    Flex   `json:"tickSize"`
}

// Symbol keeps the exchangeInfo symbol fields the runner reads. DeliveryDate is 1<<62 when Binance sends none.
type Symbol struct {
	Symbol       string   `json:"symbol"`
	Status       string   `json:"status"`
	ContractType string   `json:"contractType"`
	QuoteAsset   string   `json:"quoteAsset"`
	DeliveryDate *int64   `json:"deliveryDate"`
	Filters      []Filter `json:"filters"`
}

// Delivery is int(x.get("deliveryDate", 1 << 62)).
func (s Symbol) Delivery() int64 {
	if s.DeliveryDate == nil {
		return 1 << 62
	}
	return *s.DeliveryDate
}

var keptFilters = map[string]bool{"LOT_SIZE": true, "MARKET_LOT_SIZE": true, "MIN_NOTIONAL": true, "NOTIONAL": true, "PRICE_FILTER": true}

// streamSymbols walks {"symbols": [...]} one symbol at a time, so the whole document is never in memory.
func streamSymbols(r io.Reader, keep func(*Symbol) bool) ([]Symbol, error) {
	dec := json.NewDecoder(r)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("exchangeInfo: not a JSON object")
	}
	var out []Symbol
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key, _ := kt.(string); key == "symbols" {
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				return nil, fmt.Errorf("exchangeInfo: symbols is not a list")
			}
			for dec.More() {
				var s Symbol
				if err := dec.Decode(&s); err != nil {
					return nil, err
				}
				if !keep(&s) {
					continue
				}
				fs := s.Filters[:0]
				for _, f := range s.Filters {
					if keptFilters[f.Type] {
						fs = append(fs, f)
					}
				}
				s.Filters = append([]Filter(nil), fs...)
				out = append(out, s)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
		} else {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

type cacheEntry struct {
	day   int64
	value any
}

var (
	cacheMu sync.Mutex
	daily   = map[string]cacheEntry{}
)

func today[T any](name string, fetch func() (T, error)) (T, error) {
	day := time.Now().UnixMilli() / DayMS
	cacheMu.Lock()
	hit, ok := daily[name]
	cacheMu.Unlock()
	if ok && hit.day == day {
		return hit.value.(T), nil
	}
	v, err := fetch()
	if err != nil {
		var zero T
		return zero, err
	}
	cacheMu.Lock()
	daily[name] = cacheEntry{day, v}
	cacheMu.Unlock()
	return v, nil
}

// ResetCache drops the cached exchange info (tests).
func ResetCache() {
	cacheMu.Lock()
	daily = map[string]cacheEntry{}
	cacheMu.Unlock()
}

func fetchInfo(path, base string, keep func(*Symbol) bool) ([]Symbol, error) {
	r, err := doGet(path, nil, base)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	syms, err := streamSymbols(r.Body, keep)
	if err != nil {
		return nil, err
	}
	brake(r)
	return syms, nil
}

// Perpetuals are the USDT-margined perpetuals currently trading (with deliveryDate). Replaced in tests.
var Perpetuals = func() ([]Symbol, error) {
	return today("fapi_exchangeInfo", func() ([]Symbol, error) {
		return fetchInfo("/fapi/v1/exchangeInfo", FAPI, func(s *Symbol) bool {
			return s.ContractType == "PERPETUAL" && s.QuoteAsset == "USDT" && s.Status == "TRADING"
		})
	})
}

// SpotInfo are the spot symbols quoted in USDT (any status; the filters of the pairs the runner trades).
var SpotInfo = func() ([]Symbol, error) {
	return today("spot_exchangeInfo", func() ([]Symbol, error) {
		return fetchInfo("/api/v3/exchangeInfo", SPOT, func(s *Symbol) bool { return s.QuoteAsset == "USDT" })
	})
}

// SpotSymbols are the USDT spot pairs currently trading. Replaced in tests.
var SpotSymbols = func() (map[string]bool, error) {
	info, err := SpotInfo()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range info {
		if s.QuoteAsset == "USDT" && s.Status == "TRADING" {
			out[s.Symbol] = true
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ klines, funding, open interest

// Bar is one kline.
type Bar struct {
	OpenTime  int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	QuoteVol  float64
	CloseTime int64
}

func rawFloat(b json.RawMessage) (float64, error) {
	return strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
}

func rawInt(b json.RawMessage) (int64, error) {
	s := strings.Trim(string(b), `"`)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	return int64(f), err
}

func parseBars(rows [][10]json.RawMessage) ([]Bar, error) {
	out := make([]Bar, 0, len(rows))
	for _, r := range rows {
		var b Bar
		var err error
		if b.OpenTime, err = rawInt(r[0]); err != nil {
			return nil, fmt.Errorf("kline row: %w", err)
		}
		for i, dst := range []*float64{&b.Open, &b.High, &b.Low, &b.Close} {
			if *dst, err = rawFloat(r[i+1]); err != nil {
				return nil, fmt.Errorf("kline row: %w", err)
			}
		}
		if b.CloseTime, err = rawInt(r[6]); err != nil {
			return nil, fmt.Errorf("kline row: %w", err)
		}
		if b.QuoteVol, err = rawFloat(r[7]); err != nil {
			return nil, fmt.Errorf("kline row: %w", err)
		}
		out = append(out, b)
	}
	return out, nil
}

// FetchKlines returns all closed perp klines with open_time in [start, end).
func FetchKlines(symbol, tf string, start, end int64) ([]Bar, error) {
	var all []Bar
	cursor := start
	const page = 1000 // weight 5 per call (1500 costs 10): 1/3 more bars per weight unit
	for cursor < end {
		var rows [][10]json.RawMessage
		err := Get("/fapi/v1/klines", q("symbol", symbol, "interval", tf, "startTime", itoa(cursor),
			"endTime", itoa(end-1), "limit", strconv.Itoa(page)), FAPI, &rows)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		part, err := parseBars(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, part...)
		cursor = part[len(part)-1].OpenTime + TfMS[tf]
		if len(rows) < page {
			break
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	now := time.Now().UnixMilli()
	seen := make(map[int64]struct{}, len(all))
	out := all[:0]
	for _, b := range all {
		if _, dup := seen[b.OpenTime]; dup {
			continue
		}
		seen[b.OpenTime] = struct{}{}
		if b.OpenTime < end && b.CloseTime < now {
			out = append(out, b)
		}
	}
	return out, nil
}

// SpotDailyOpen are the spot 1d bars in [start, end): open_time -> open (= price at 00:00 UTC).
func SpotDailyOpen(symbol string, start, end int64) (map[int64]float64, error) {
	var rows [][2]json.RawMessage
	err := Get("/api/v3/klines", q("symbol", symbol, "interval", "1d", "startTime", itoa(start),
		"endTime", itoa(end-1), "limit", "1000"), SPOT, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]float64, len(rows))
	for _, r := range rows {
		t, err := rawInt(r[0])
		if err != nil {
			return nil, err
		}
		o, err := rawFloat(r[1])
		if err != nil {
			return nil, err
		}
		out[t] = o
	}
	return out, nil
}

// FundingRow is one funding settlement.
type FundingRow struct {
	Time int64   `json:"fundingTime"`
	Rate float64 `json:"fundingRate"`
}

// Funding returns fundingTime, fundingRate (the lab's plain call: at most 1000 settlements from start - 1 day).
// Replaced in tests.
var Funding = func(symbol string, start, end int64) ([]FundingRow, error) {
	var rows []struct {
		Time json.RawMessage `json:"fundingTime"`
		Rate json.RawMessage `json:"fundingRate"`
	}
	err := Get("/fapi/v1/fundingRate", q("symbol", symbol, "startTime", itoa(start-DayMS), "endTime", itoa(end),
		"limit", "1000"), FAPI, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]FundingRow, 0, len(rows))
	for _, r := range rows {
		t, err := rawInt(r.Time)
		if err != nil {
			return nil, err
		}
		v, err := rawFloat(r.Rate)
		if err != nil {
			return nil, err
		}
		out = append(out, FundingRow{t, v})
	}
	return out, nil
}

// OIRow is one open-interest history row.
type OIRow struct {
	TS  int64
	Val float64 // sumOpenInterestValue (USDT), NaN when not a number
}

// OpenInterest is timestamp, sumOpenInterestValue; Binance keeps only the last 30 days, <= 500 rows per call.
func OpenInterest(symbol string, start, end int64, period string) ([]OIRow, error) {
	var out []OIRow
	cursor := start
	if floor := time.Now().UnixMilli() - 29*DayMS; floor > cursor {
		cursor = floor
	}
	for cursor < end {
		var rows []struct {
			TS  json.RawMessage `json:"timestamp"`
			Val json.RawMessage `json:"sumOpenInterestValue"`
		}
		err := Get("/futures/data/openInterestHist", q("symbol", symbol, "period", period, "startTime", itoa(cursor),
			"endTime", itoa(end), "limit", "500"), FAPI, &rows)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			ts, err := rawInt(r.TS)
			if err != nil {
				return nil, err
			}
			v, err := rawFloat(r.Val)
			if err != nil {
				v = math.NaN()
			}
			out = append(out, OIRow{ts, v})
		}
		cursor = out[len(out)-1].TS + 1
		if len(rows) < 500 {
			break
		}
	}
	return out, nil
}

// Ping is GET /fapi/v1/time: (ok, latency ms, error).
func Ping() (bool, int, *string) {
	t0 := time.Now()
	c := &http.Client{Timeout: 10 * time.Second}
	r, err := c.Get(FAPI + "/fapi/v1/time")
	ms := int(time.Since(t0).Milliseconds())
	if err != nil {
		s := err.Error()
		return false, ms, &s
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		s := fmt.Sprintf("HTTP %d", r.StatusCode)
		return false, ms, &s
	}
	return true, ms, nil
}
