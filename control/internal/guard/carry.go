package guard

import (
	"context"
	"log"
	"math"
	"slices"
	"sort"
	"time"

	"control/internal/binance"
	"control/internal/store"
)

// A live carry bot may trade on the same futures account. Its pairs are hedged by a spot leg that this
// guard cannot see, so they are kept out of the loss measure and out of the block enforcement:
//   - positions and orders on carry symbols are ignored (a pumping short is not a loss, only half a hedge);
//   - income booked on carry symbols since the window start (realized PnL, funding, commission) and net
//     transfers between spot and futures (the carry moves its margin) are taken out of the wallet balance.
// The guard then measures what the rest of the account did, as it did before.

// stableAssets are the margin assets whose transfers move the USD wallet balance one to one.
var stableAssets = map[string]bool{"USDT": true, "USDC": true, "FDUSD": true, "BFUSD": true, "USD": true}

// transferTypes are income types that move money in or out of the futures wallet without trading.
var transferTypes = map[string]bool{"TRANSFER": true, "INTERNAL_TRANSFER": true, "STRATEGY_UMFUTURES_TRANSFER": true,
	"CROSS_COLLATERAL_TRANSFER": true, "COIN_SWAP_DEPOSIT": true, "COIN_SWAP_WITHDRAW": true}

// incomeCacheTTL bounds how stale the adjustment may get while the wallet balance does not move.
const incomeCacheTTL = 5 * time.Minute

type incomeCache struct {
	periodStart time.Time
	wallet      float64
	carry       string // sorted carry symbols the value was computed for
	at          time.Time
	value       float64
}

func (g *Guard) exempt() map[string]bool {
	if g.opt.Exempt == nil {
		return nil
	}
	return g.opt.Exempt()
}

// carrySet is the current carry symbols plus those held earlier in this window.
func carrySet(u store.User, current map[string]bool) map[string]bool {
	out := map[string]bool{}
	for s := range current {
		out[s] = true
	}
	for _, s := range u.CarrySymbols {
		out[s] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// adjustment returns net transfers + carry income since the window start, cached between wallet moves.
func (g *Guard) adjustment(ctx context.Context, c *binance.Client, u store.User, carry map[string]bool, wallet float64) float64 {
	if u.PeriodStart.IsZero() {
		return 0
	}
	key := ""
	for _, s := range sortedKeys(carry) {
		key += s + ","
	}
	now := g.opt.Now()
	g.mu.Lock()
	hit, ok := g.income[u.ChatID]
	g.mu.Unlock()
	if ok && hit.periodStart.Equal(u.PeriodStart) && hit.carry == key && math.Abs(hit.wallet-wallet) < 1e-9 &&
		now.Sub(hit.at) < incomeCacheTTL {
		return hit.value
	}
	recs, err := c.IncomeSince(ctx, u.PeriodStart)
	if err != nil {
		log.Printf("chat %d: income history: %v", u.ChatID, err)
		if ok && hit.periodStart.Equal(u.PeriodStart) {
			return hit.value // keep the last known adjustment for this window
		}
		return 0
	}
	v := Adjustment(recs, carry)
	g.mu.Lock()
	g.income[u.ChatID] = incomeCache{periodStart: u.PeriodStart, wallet: wallet, carry: key, at: now, value: v}
	g.mu.Unlock()
	return v
}

// Adjustment sums the income that is not this guard's business: transfers of stable assets and every
// record on a carry symbol. Positive = money that came in without the rest of the account earning it.
func Adjustment(recs []binance.Income, carry map[string]bool) float64 {
	var v float64
	for _, in := range recs {
		switch {
		case transferTypes[in.IncomeType] && in.Symbol == "":
			if stableAssets[in.Asset] {
				v += in.Income.Float()
			}
		case in.Symbol != "" && carry[in.Symbol]:
			v += in.Income.Float()
		}
	}
	return v
}

// snapshotOf builds the check snapshot with carry symbols left out. adjust is subtracted from the wallet.
func snapshotOf(acc *binance.Account, now time.Time, carry map[string]bool, adjust float64) store.Snapshot {
	wallet := acc.TotalWalletBalance.Float()
	upnl := acc.TotalUnrealizedProfit.Float()
	var carryUPnL float64
	var open, carryOpen int
	for _, p := range acc.OpenPositions() {
		if carry[p.Symbol] {
			carryUPnL += p.UnrealizedProfit.Float()
			carryOpen++
			continue
		}
		open++
	}
	return store.Snapshot{
		CheckedAt:     now,
		WalletBalance: wallet,
		UnrealizedPnL: upnl - carryUPnL,
		Equity:        wallet - adjust + upnl - carryUPnL,
		OpenPositions: open,
		Adjust:        adjust,
		CarryUPnL:     carryUPnL,
		CarryCount:    carryOpen,
	}
}

// rememberCarry adds the current carry symbols to the window's list (kept until the next window).
func rememberCarry(u *store.User, current map[string]bool) {
	for _, s := range sortedKeys(current) {
		if !slices.Contains(u.CarrySymbols, s) {
			u.CarrySymbols = append(u.CarrySymbols, s)
		}
	}
}

// nonCarryOpen counts open positions outside the carry symbols.
func nonCarryOpen(acc *binance.Account, carry map[string]bool) int {
	n := 0
	for _, p := range acc.OpenPositions() {
		if !carry[p.Symbol] {
			n++
		}
	}
	return n
}
