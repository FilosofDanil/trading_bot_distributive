package setup

import (
	"encoding/json"

	"densitybounce/internal/book"
)

// Features are the numbers a plan carries into the journals. The JSON keys and their order are those of the
// Python dict; nullable values are pointers (null). depth_x and armed_s are added by the trader later and are
// present only once set. A zero Features (a plan built by hand) marshals as {} like the empty Python dict.
type Features struct {
	DensityUSD   int64    `json:"density_usd"`
	PeakUSD      int64    `json:"peak_usd"`
	AgeS         float64  `json:"age_s"`
	Touches      int      `json:"touches"`
	Dist         float64  `json:"dist"`
	Spread       float64  `json:"spread"`
	MinuteUSD    int64    `json:"minute_usd"`
	DensVsMinute *float64 `json:"dens_vs_minute"`
	Moved60      float64  `json:"moved60"`
	Approach     float64  `json:"approach"`
	Move         float64  `json:"move"`
	MoveStart    float64  `json:"move_start"`
	MoveTS       float64  `json:"move_ts"`
	MoveS        float64  `json:"move_s"`
	Risk         float64  `json:"risk"`
	Reward       float64  `json:"reward"`
	RR           float64  `json:"rr"`
	ATR          *float64 `json:"atr"`
	StopATR      *float64 `json:"stop_atr"`
	Trend        *int     `json:"trend"`
	Counter      bool     `json:"counter"`
	Accel        *float64 `json:"accel"`
	HoldS        *float64 `json:"hold_s"`
	FlowS        int64    `json:"flow_s"`
	Mkt          *float64 `json:"mkt"`
	VolRatio     *float64 `json:"vol_ratio"`
	Ch24         *float64 `json:"ch24"`
	Drop10       float64  `json:"drop10"`
	DepthX       *float64 `json:"depth_x,omitempty"`
	ArmedS       *float64 `json:"armed_s,omitempty"`
}

type featuresAlias Features

// MarshalJSON writes {} for the zero value, else the full dict in the Python key order.
func (f Features) MarshalJSON() ([]byte, error) {
	if f == (Features{}) {
		return []byte("{}"), nil
	}
	return json.Marshal(featuresAlias(f))
}

// F64 returns a pointer to x (nullable feature values).
func F64(x float64) *float64 { return &x }

// IntP returns a pointer to x.
func IntP(x int) *int { return &x }

// Plan is one entry plan: the entry, the stop and the takes, plus the features it was made with.
type Plan struct {
	Side     string // long | short
	Entry    float64
	Stop     float64
	TP       float64
	DKey     book.DKey // (side, tick) of the density
	Features Features
	TP1      float64 // first partial take (0 = none)
	TPRun    float64 // the runner's take (0 = none)
	Skip     string  // soft gates that failed ("young+weak"): a shadow plan, journaled but not traded
	PID      string  // journal id, set by the trader
}

// Risk is |entry - stop|.
func (p *Plan) Risk() float64 {
	d := p.Entry - p.Stop
	if d < 0 {
		d = -d
	}
	return d
}

// Sgn is +1 for a long, -1 for a short.
func (p *Plan) Sgn() int {
	if p.Side == "long" {
		return 1
	}
	return -1
}
