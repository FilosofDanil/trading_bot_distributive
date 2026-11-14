package trader

import (
	"fmt"
	"strconv"
	"strings"

	"densitybounce/internal/shadow"
)

// Risk is the trader's RISK dict: limits and sizes, overridable with the environment (LoadRisk).
type Risk struct {
	Notional           float64 // USDT per trade at most
	RiskUSD            float64 // USDT lost at the stop; 0 = 0.2 % of the notional
	MaxOpen            int     // positions + working entries at once
	DayLoss            float64 // no new entries for the UTC day after -1 USDT
	DayLossR           float64 // ... or after -6 R; 0 = off
	TotalLoss          float64 // halt for good after -5 USDT since the record started
	MaxConsec          int     // this many losses in a row -> no new entries until the next UTC day
	MaxTradesDay       int
	SymCooldown        float64 // s after a closed trade or a cancelled entry before the same symbol trades again
	WallCool           float64 // s without entries at a density whose entry was cancelled because the density failed
	AttemptsPerDensity int
	Leverage           int
	FeeRebate          float64 // share of the commission the account gets back (0.4 = 40 %)
	GuardN             int     // live only: after this many trades of the running version ...
	GuardR             float64 // ... a mean R below this halts the strategy (guard_n 0 = off)
}

// DefaultRisk is Python's RISK.
func DefaultRisk() Risk {
	return Risk{Notional: 10.0, RiskUSD: 0.0, MaxOpen: 2, DayLoss: 1.0, DayLossR: 6.0, TotalLoss: 5.0, MaxConsec: 6,
		MaxTradesDay: 60, SymCooldown: 60.0, WallCool: 1800.0, AttemptsPerDensity: 2, Leverage: 2, FeeRebate: 0.0,
		GuardN: 30, GuardR: -0.3}
}

// RISK is the live risk configuration (tests save and restore it by value).
var RISK = DefaultRisk()

// FEE are the commission rates: maker 0.02 %, taker 0.05 %.
var FEE = shadow.Fee{Maker: 0.0002, Taker: 0.0005}

// LoadRisk applies the environment (the Python variable names) on top of RISK. env returns "" for an unset
// variable. A value that is not a number is an error.
func LoadRisk(env func(string) string) error {
	f := func(name string, x *float64) error {
		v := strings.TrimSpace(env(name))
		if v == "" {
			return nil
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%s: could not convert string to float: %q", name, v)
		}
		*x = n
		return nil
	}
	i := func(name string, x *int) error {
		var n float64
		if err := f(name, &n); err != nil {
			return err
		}
		if strings.TrimSpace(env(name)) != "" {
			*x = int(n)
		}
		return nil
	}
	r := &RISK
	for _, err := range []error{
		f("NOTIONAL", &r.Notional), f("RISK_USD", &r.RiskUSD), i("MAX_OPEN", &r.MaxOpen), f("DAY_LOSS", &r.DayLoss),
		f("TOTAL_LOSS", &r.TotalLoss), i("MAX_CONSEC_LOSSES", &r.MaxConsec), i("MAX_TRADES_DAY", &r.MaxTradesDay),
		i("LEVERAGE", &r.Leverage), f("FEE_REBATE", &r.FeeRebate), f("DAY_LOSS_R", &r.DayLossR),
		i("GUARD_N", &r.GuardN), f("GUARD_R", &r.GuardR),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// RiskUSD is the USDT a trade loses at its stop (before slippage and fees).
func RiskUSD() float64 {
	if RISK.RiskUSD != 0 {
		return RISK.RiskUSD
	}
	return RISK.Notional * 0.002
}
