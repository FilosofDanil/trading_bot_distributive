package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"densitybounce/internal/book"
	"densitybounce/internal/pyfmt"
)

// Params is setup.P of the Python runner. Every field is overridable with the PARAMS env var (LoadParams).
// The PARAMS names are the Python keys (see fieldTable).
type Params struct {
	MinAge        float64 // s a density must live before we trust it (20 until v0.4.5)
	ShadowAge     float64 // younger than MinAge but at least this old: not traded, journaled as a shadow plan
	HoldMin       float64 // reality criterion: the density is worth >= this many s of the aggressive flow toward it; 0 = off
	HoldCancel    float64 // a working entry is cancelled when the density is worth < this many s (0 = off)
	MktSecs       float64 // market filter: the median change of the other watched coins over this long ...
	MktMax        float64 // ... is >= this toward the density: no entry (0 = off)
	ConfirmSecs   float64 // place the entry this long after price reached the entry level (0 = at once)
	Intact        float64 // still >= 70 % of its peak size
	MaxTouches    int     // 0..2 previous approaches
	PlaceDist     float64 // mid within 0.35 % of the density
	ApproachSecs  float64 // sharp approach: in the last 3 min ...
	ApproachMin   float64 // ... mid moved >= 0.80 % toward the density
	ImpulseSecs   float64 // still moving: in the last 60 s ...
	ImpulseMin    float64 // ... mid moved >= 0.15 % toward the density ...
	ImpulseFrac   float64 // ... and covered >= half of the distance it had 60 s ago
	MoveSecs      float64 // the previous move starts at the extreme of the last 30 min
	MoveMin       float64 // and is >= 2.50 % long (start -> density)
	TakeFrac      float64 // take at 50 % of the move, measured from the density
	MinuteMin     float64 // skip coins trading < 100k USDT a minute
	Front         float64 // entry 0.02 % (>= 1 tick) in front of the density
	StopBuf       float64 // stop at least 0.15 % (>= 2 ticks) behind the density ...
	StopATR       float64 // ... and at least 2 x ATR of the 1m bars (0 = StopBuf only)
	ATRN          int
	RiskMin       float64
	RiskMax       float64
	MinRR         float64 // reward / risk at entry
	TickMax       float64 // skip coins whose tick is > 0.02 % of price
	SpreadMax     float64
	TrendEMA      int     // the 1h trend: last price vs the EMA of this many hour closes
	CounterAccel  float64 // against the 1h trend only when the 3 min speed >= this x the 30 min speed (0 = off)
	EatSecs       float64 // skip / cancel when the density lost >= EatCancel of its size in the last EatSecs
	EatCancel     float64 // (0 = off)
	StopDepthK    float64 // the book within StopDepthBand beyond the stop must hold >= this x the position (0 = off)
	StopDepthBand float64
	SlipMax       float64 // a stop filled > this beyond its price slipped; SlipBanN of them within
	SlipBanN      int     // SlipBanS -> no trades on that symbol for SlipBanS
	SlipBanS      float64
	CancelDist    float64
	CancelAfter   float64
	CancelShrink  float64
	MaxHold       float64 // close at market after 60 min
	ExitOnGone    bool    // close when the density disappears (off: the stop behind it is the exit)
	BeAtR         float64 // > 0: at +BeAtR R move the stop to entry + fees
	BeOnTP1       bool    // move the stop to entry + fees when tp1 fills
	BeOff         float64 // break-even stop = entry + 0.06 %
	TP1R          float64 // > 0: first partial take at +1R from the entry ...
	TP1Frac       float64 // (with TP1R 0 and this > 0: at this fraction of the move, measured from the density)
	TP1Part       float64 // ... for half of the position
	TP2Part       float64 // with a runner: the share taken at the take
	RunFrac       float64 // > 0: the rest runs to this fraction of the move (1.0 = back at the move's start)
}

// Default is the P of bounce/setup.py.
func Default() Params {
	return Params{
		MinAge: 60.0, ShadowAge: 20.0, HoldMin: 60.0, HoldCancel: 30.0, MktSecs: 1800.0, MktMax: 0.010,
		ConfirmSecs: 15.0, Intact: 0.7, MaxTouches: 2, PlaceDist: 0.0035, ApproachSecs: 180.0, ApproachMin: 0.0080,
		ImpulseSecs: 60.0, ImpulseMin: 0.0015, ImpulseFrac: 0.5, MoveSecs: 1800.0, MoveMin: 0.0250, TakeFrac: 0.5,
		MinuteMin: 100_000.0, Front: 0.0002, StopBuf: 0.0015, StopATR: 2.0, ATRN: 14, RiskMin: 0.0010,
		RiskMax: 0.0400, MinRR: 1.0, TickMax: 0.0002, SpreadMax: 0.0006, TrendEMA: 20, CounterAccel: 1.0,
		EatSecs: 10.0, EatCancel: 0.2, StopDepthK: 3.0, StopDepthBand: 0.003, SlipMax: 0.002, SlipBanN: 2,
		SlipBanS: 86400.0, CancelDist: 0.008, CancelAfter: 180.0, CancelShrink: 0.5, MaxHold: 3600.0,
		ExitOnGone: false, BeAtR: 0.0, BeOnTP1: false, BeOff: 0.0006, TP1R: 1.0, TP1Frac: 0.0, TP1Part: 0.5,
		TP2Part: 7.0 / 12.0, RunFrac: 0.0,
	}
}

// P is the live parameter set (setup.P). Tests save and restore it by value.
var P = Default()

type fieldKind int

const (
	kFloat fieldKind = iota
	kInt
	kBool
)

type field struct {
	name string
	kind fieldKind
	f    *float64
	i    *int
	b    *bool
}

// fieldTable maps every PARAMS key to its field in p, in the order of the Python dict.
func fieldTable(p *Params) []field {
	f := func(n string, x *float64) field { return field{name: n, kind: kFloat, f: x} }
	i := func(n string, x *int) field { return field{name: n, kind: kInt, i: x} }
	b := func(n string, x *bool) field { return field{name: n, kind: kBool, b: x} }
	return []field{
		f("min_age", &p.MinAge), f("shadow_age", &p.ShadowAge), f("hold_min", &p.HoldMin),
		f("hold_cancel", &p.HoldCancel), f("mkt_secs", &p.MktSecs), f("mkt_max", &p.MktMax),
		f("confirm_secs", &p.ConfirmSecs), f("intact", &p.Intact), i("max_touches", &p.MaxTouches),
		f("place_dist", &p.PlaceDist), f("approach_secs", &p.ApproachSecs), f("approach_min", &p.ApproachMin),
		f("impulse_secs", &p.ImpulseSecs), f("impulse_min", &p.ImpulseMin), f("impulse_frac", &p.ImpulseFrac),
		f("move_secs", &p.MoveSecs), f("move_min", &p.MoveMin), f("take_frac", &p.TakeFrac),
		f("minute_min", &p.MinuteMin), f("front", &p.Front), f("stop_buf", &p.StopBuf), f("stop_atr", &p.StopATR),
		i("atr_n", &p.ATRN), f("risk_min", &p.RiskMin), f("risk_max", &p.RiskMax), f("min_rr", &p.MinRR),
		f("tick_max", &p.TickMax), f("spread_max", &p.SpreadMax), i("trend_ema", &p.TrendEMA),
		f("counter_accel", &p.CounterAccel), f("eat_secs", &p.EatSecs), f("eat_cancel", &p.EatCancel),
		f("stop_depth_k", &p.StopDepthK), f("stop_depth_band", &p.StopDepthBand), f("slip_max", &p.SlipMax),
		i("slip_ban_n", &p.SlipBanN), f("slip_ban_s", &p.SlipBanS), f("cancel_dist", &p.CancelDist),
		f("cancel_after", &p.CancelAfter), f("cancel_shrink", &p.CancelShrink), f("max_hold", &p.MaxHold),
		b("exit_on_gone", &p.ExitOnGone), f("be_at_r", &p.BeAtR), b("be_on_tp1", &p.BeOnTP1),
		f("be_off", &p.BeOff), f("tp1_r", &p.TP1R), f("tp1_frac", &p.TP1Frac), f("tp1_part", &p.TP1Part),
		f("tp2_part", &p.TP2Part), f("run_frac", &p.RunFrac),
	}
}

// Keys are the sorted PARAMS keys of P.
func Keys() []string {
	var ks []string
	for _, f := range fieldTable(&Params{}) {
		ks = append(ks, f.name)
	}
	sort.Strings(ks)
	return ks
}

// toFloat is Python's float(v) for a decoded JSON value.
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(x, "_", ""))
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return f, nil
			}
			return 0, fmt.Errorf("could not convert string to float: %q", x)
		}
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not %T", v)
}

// toInt is Python's int(v): numbers truncate toward zero, strings must be integer literals.
func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, fmt.Errorf("cannot convert %v to integer", x)
		}
		return int(math.Trunc(x)), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(x, "_", "")))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int(): %q", x)
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string or a real number, not %T", v)
}

// truthy is Python's bool(v).
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// LoadParams applies a PARAMS JSON object on top of P and book.DENS (setup.load_params). An unknown key is an
// error that lists every known key, so a typo never passes. Values are coerced like Python's type(d[k])(v): int
// fields truncate, float fields become floats. Afterwards book.TapeKeep is set to cover the longest flow window.
// An empty raw string changes nothing.
func LoadParams(raw string) error {
	defer func() {
		book.TapeKeep = math.Max(math.Max(P.ApproachSecs, P.ImpulseSecs), 300) + 10
	}()
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var upd map[string]any
	if err := dec.Decode(&upd); err != nil {
		return fmt.Errorf("PARAMS: %v", err)
	}
	if dec.More() {
		return fmt.Errorf("PARAMS: extra data after the JSON object")
	}
	tbl := fieldTable(&P)
	byName := make(map[string]field, len(tbl))
	for _, f := range tbl {
		byName[f.name] = f
	}
	dens := map[string]bool{}
	for _, k := range book.DensKeys {
		dens[k] = true
	}
	var bad []string
	for k := range upd {
		if _, ok := byName[k]; !ok && !dens[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		dk := append([]string(nil), book.DensKeys...)
		sort.Strings(dk)
		return fmt.Errorf("PARAMS: unknown keys %s; known: %s", pyfmt.ReprStrs(bad),
			pyfmt.ReprStrs(append(Keys(), dk...)))
	}
	keys := make([]string, 0, len(upd))
	for k := range upd {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := upd[k]
		if f, ok := byName[k]; ok {
			switch f.kind {
			case kFloat:
				x, err := toFloat(v)
				if err != nil {
					return fmt.Errorf("PARAMS %s: %v", k, err)
				}
				*f.f = x
			case kInt:
				x, err := toInt(v)
				if err != nil {
					return fmt.Errorf("PARAMS %s: %v", k, err)
				}
				*f.i = x
			case kBool:
				*f.b = truthy(v)
			}
			continue
		}
		x, err := toFloat(v)
		if err != nil {
			return fmt.Errorf("PARAMS %s: %v", k, err)
		}
		book.SetDens(k, x)
	}
	return nil
}
