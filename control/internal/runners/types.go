package runners

import (
	"encoding/json"
	"fmt"
	"time"
)

// Health mirrors GET /v1/health.
type Health struct {
	Runner       string  `json:"runner"`
	Version      string  `json:"version"`
	UptimeS      int64   `json:"uptime_s"`
	Mode         string  `json:"mode"`
	Now          string  `json:"now"`
	Binance      Binance `json:"binance"`
	RSSMB        float64 `json:"rss_mb"`
	UniverseDays int     `json:"universe_days"`
	StateBytes   int64   `json:"state_bytes"`
	Busy         bool    `json:"busy"`
	Errors       []Error `json:"errors"`
}

type Binance struct {
	OK        bool    `json:"ok"`
	LatencyMS float64 `json:"latency_ms"`
	Error     *string `json:"error"`
}

type Error struct {
	TS  string `json:"ts"`
	Msg string `json:"msg"`
}

// Strategy mirrors one element of GET /v1/strategies.
type Strategy struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Mode          string   `json:"mode"`
	State         string   `json:"state"`   // running | paused | busy | error
	Desired       string   `json:"desired"` // running | paused
	OpenPositions int      `json:"open_positions"`
	Book          []string `json:"book"`
	Capital0      float64  `json:"capital0"`
	Equity        float64  `json:"equity"`
	PnlTotal      float64  `json:"pnl_total"`
	PnlToday      float64  `json:"pnl_today"`
	LastRunDay    *string  `json:"last_run_day"`
	LastRunAt     *string  `json:"last_run_at"`
	NextRunAt     *string  `json:"next_run_at"`
	LastError     *string  `json:"last_error"`
}

// RunResult is the answer of POST /run.
type RunResult struct {
	OK      bool `json:"ok"`
	Started bool `json:"started"`
	Preview bool `json:"preview"`
}

// FundingRate is one `top` entry: ["SYM", 0.0003] (fraction per 8 h).
type FundingRate struct {
	Symbol string
	F8     float64
}

func (f *FundingRate) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("funding entry: want [symbol, rate], got %d items", len(raw))
	}
	if err := json.Unmarshal(raw[0], &f.Symbol); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &f.F8)
}

// Leg is an order leg: ["BUY", 12.0]. The zero value means "no leg".
type Leg struct {
	Side string
	Qty  float64
}

func (l *Leg) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("order leg: want [side, qty], got %d items", len(raw))
	}
	if err := json.Unmarshal(raw[0], &l.Side); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &l.Qty)
}

type Order struct {
	Symbol          string  `json:"symbol"`
	Why             string  `json:"why"`
	Spot            Leg     `json:"spot"`
	Perp            Leg     `json:"perp"`
	Notional        float64 `json:"notional"`
	MarginToFutures float64 `json:"margin_to_futures"`
	OK              bool    `json:"ok"`
}

// Plan mirrors GET /plan.
type Plan struct {
	Day         string                `json:"day"`
	Preview     bool                  `json:"preview"`
	Accepted    bool                  `json:"accepted"`
	Paused      bool                  `json:"paused"`
	Equity      float64               `json:"equity"`
	Book        []string              `json:"book"`
	Open        []string              `json:"open"`
	Close       []string              `json:"close"`
	Resize      map[string][2]float64 `json:"resize"` // symbol -> [from, to] notional
	PairStopped []string              `json:"pair_stopped"`
	Rehedge     []string              `json:"rehedge"`
	Top         []FundingRate         `json:"top"`
	MinF8       float64               `json:"min_f8"`
	Skipped     map[string]string     `json:"skipped"`
	Orders      []Order               `json:"orders"`
	Idle        float64               `json:"idle"`
	ComputedAt  string                `json:"computed_at"`
}

type Position struct {
	Symbol  string  `json:"symbol"`
	N       float64 `json:"n"`
	Perp0   float64 `json:"perp0"`
	Spot0   float64 `json:"spot0"`
	Opened  string  `json:"opened"`
	Days    int     `json:"days"`
	Funding float64 `json:"funding"`
	Basis   float64 `json:"basis"`
	Fees    float64 `json:"fees"`
	Pnl     float64 `json:"pnl"`
}

// CurvePoint is one equity curve entry: ["2026-10-10", 500.0].
type CurvePoint struct {
	Day    string
	Equity float64
}

func (p *CurvePoint) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("curve point: want [day, equity], got %d items", len(raw))
	}
	if err := json.Unmarshal(raw[0], &p.Day); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &p.Equity)
}

type Report struct {
	Start       string       `json:"start"`
	Days        int          `json:"days"`
	Capital0    float64      `json:"capital0"`
	Equity      float64      `json:"equity"`
	ReturnPct   float64      `json:"return_pct"`
	AnnualPct   float64      `json:"annual_pct"`
	MaxDDPct    float64      `json:"max_dd_pct"`
	Funding     float64      `json:"funding"`
	Basis       float64      `json:"basis"`
	Fees        float64      `json:"fees"`
	PairsOpened int          `json:"pairs_opened"`
	PairsClosed int          `json:"pairs_closed"`
	PairStops   int          `json:"pair_stops"`
	ActiveDays  int          `json:"active_days"`
	Curve       []CurvePoint `json:"curve"`
}

type Event struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Level  string `json:"level"` // info | warn | error
	Msg    string `json:"msg"`
	Notify bool   `json:"notify"`
}

// Time parses the event timestamp (zero time when malformed).
func (e Event) Time() time.Time {
	t, _ := time.Parse(time.RFC3339, e.TS)
	return t
}
