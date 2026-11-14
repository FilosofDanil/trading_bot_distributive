// funding-carry runner (Go port of the Python `python -m carry`).
//
//	funding_carry [serve]                              control API on :PORT + the daily paper / live cycle (Railway entry point)
//	funding_carry once [--day YYYY-MM-DD] [--preview]  one cycle now against STATE_FILE (local debugging)
//
// Environment: CONTROL_TOKEN (required to serve /v1/*), PORT (8080), STATE_FILE (/data/carry_state.json),
// CARRY_CAPITAL (capital in USDT, 500; used only when the state file is created), RUN_AFTER_MIN (5),
// MODE (paper | live). Live also reads BINANCE_API_KEY + BINANCE_API_SECRET or BINANCE_PRIVATE_KEY (Ed25519 PEM)
// or KEY_FROM_CONTROL (http://control.railway.internal:8090: the key stored in control with /start, internal/keys),
// MAX_COINS (3), LIVE_STATE_FILE (/data/carry_live_state.json; STATE_FILE is the paper record), LIQ_TOPUP_DIST /
// LIQ_CLOSE_DIST / LIQ_CLOSE_RISE / LIQ_STOP_RISE (internal/live Protect).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fundingcarry/internal/api"
	"fundingcarry/internal/carry"
	"fundingcarry/internal/exchange"
	"fundingcarry/internal/keys"
	"fundingcarry/internal/live"
	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/runner"
	"fundingcarry/internal/state"
	"fundingcarry/internal/strategy"
)

const usage = `funding_carry [serve]                              control API on :PORT + the daily cycle (Railway entry point)
funding_carry once [--day YYYY-MM-DD] [--preview]  one cycle now against STATE_FILE (local debugging)`

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func newRunner() (*runner.Runner, error) {
	mode := strings.ToLower(strings.TrimSpace(env("MODE", "paper")))
	if mode != "paper" && mode != "live" {
		return nil, fmt.Errorf("MODE=%s: use paper or live", pyx.Repr(mode))
	}
	capital, err := strconv.ParseFloat(strings.TrimSpace(env("CARRY_CAPITAL", "500")), 64)
	if err != nil {
		return nil, fmt.Errorf("CARRY_CAPITAL: %w", err)
	}
	runAfter, err := strconv.Atoi(strings.TrimSpace(env("RUN_AFTER_MIN", "5")))
	if err != nil {
		return nil, fmt.Errorf("RUN_AFTER_MIN: %w", err)
	}
	if mode == "paper" {
		store, err := state.New(env("STATE_FILE", "/data/carry_state.json"), capital)
		if err != nil {
			return nil, err
		}
		return runner.New(store, runAfter, mode, nil), nil
	}

	// live: its own state file (the paper record stays as it was), at most MAX_COINS pairs
	k, err := strconv.Atoi(strings.TrimSpace(env("MAX_COINS", "3")))
	if err != nil {
		return nil, fmt.Errorf("MAX_COINS: %w", err)
	}
	strategy.P.K = k
	// not STATE_FILE: the image sets it to the paper record
	store, err := state.New(env("LIVE_STATE_FILE", "/data/carry_live_state.json"), capital)
	if err != nil {
		return nil, err
	}
	if err := store.Update(func(d *state.Data) { // nothing traded yet: CARRY_CAPITAL may still change
		if d.Held.Len() == 0 && len(d.Closed) == 0 && len(d.Daily) == 0 && d.Capital0 != capital {
			d.Capital0, d.Capital = capital, capital
		}
	}); err != nil {
		return nil, err
	}
	if why := keys.FromControl(12, 10*time.Second); why != "" {
		store.Event("error", fmt.Sprintf("%s: live trading is OFF - no Binance key from control: %s", carry.StrategyID, why), true)
		return runner.New(store, runAfter, "live-blocked", nil), nil
	}
	ex, err := exchange.FromEnv()
	if err != nil {
		return nil, err
	}
	missing, err := ex.CheckPermissions()
	if err != nil {
		missing = []string{fmt.Sprintf("cannot read the key's permissions (%s)", pyx.Describe(err))}
	}
	transfers := true // optional: without it each wallet pays its own leg
	var rest []string
	for _, m := range missing {
		if m == "universal transfer" {
			transfers = false
		} else {
			rest = append(rest, m)
		}
	}
	if len(rest) > 0 {
		store.Event("error", fmt.Sprintf("%s: live trading is OFF - the Binance key lacks: %s. %s", carry.StrategyID,
			strings.Join(rest, ", "), permissionHint(rest)), true)
		return runner.New(store, runAfter, "live-blocked", nil), nil
	}
	if !transfers {
		store.Event("info", fmt.Sprintf("%s: the key has no Universal Transfer - no USDT is moved between wallets. "+
			"Keep about %.0f USDT on spot (spot legs) and %.0f USDT on futures (isolated margin 2x each short); "+
			"margin top-ups come from futures only.", carry.StrategyID, capital/3, capital*2/3), true)
	}
	lv := live.New(ex, store, live.ProtectFromEnv(), transfers)
	if err := lv.Reconcile(); err != nil {
		return nil, err
	}
	return runner.New(store, runAfter, "live", lv), nil
}

// permissionHint says what to do about the missing permissions (the read-only HMAC note only when trading itself is off).
func permissionHint(missing []string) string {
	for _, m := range missing {
		if m == "spot trading" || m == "futures" {
			return "An HMAC key without an IP whitelist is read-only; use a self-generated Ed25519 key " +
				"(BINANCE_PRIVATE_KEY) or an IP-restricted key on a static IP."
		}
	}
	return "Fix the key's restrictions in Binance API Management, then restart the runner."
}

func serve() error {
	rn, err := newRunner()
	if err != nil {
		return err
	}
	token := api.EnvToken()
	port := env("PORT", "8080")
	if token == "" {
		fmt.Println("WARNING: CONTROL_TOKEN is empty - /v1/* answers 503 until it is set")
	}
	ln, err := net.Listen("tcp", ":"+port) // dual-stack: Railway private network (IPv6) + IPv4
	if err != nil {
		fmt.Printf("WARNING: cannot bind [::]:%s (%v), falling back to 0.0.0.0\n", port, err)
		if ln, err = net.Listen("tcp4", "0.0.0.0:"+port); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: api.NewHandler(rn, token), ReadHeaderTimeout: 30 * time.Second}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	st := rn.Store.Snapshot()
	rn.Store.Event("info", fmt.Sprintf("%s v%s started: mode %s, equity %.2f USDT, %d pair(s), %s", carry.StrategyID,
		carry.Version, rn.Mode, st.Capital, st.Held.Len(), st.Desired), true)
	go rn.Loop(stop)
	if rn.Live != nil {
		go rn.Watch(stop, live.WatchEveryS*time.Second)
		p := rn.Live.P
		rn.Store.Event("info", fmt.Sprintf("%s: LIVE with liquidation guard every 30 s: add margin when the liquidation is < %s away, "+
			"close the pair at perp +%s or liquidation < %s away, exchange stop at +%s", carry.StrategyID,
			pyx.Pct0(p.TopupDist, false), pyx.Pct0(p.CloseRise, false), pyx.Pct0(p.CloseDist, false), pyx.Pct0(p.StopRise, false)), true)
	}
	fmt.Printf("funding-carry: API on [::]:%s, state %s\n", port, rn.Store.Path)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	// wait for a running cycle so the state is never left half-written (it is saved atomically anyway)
	deadline := time.Now().Add(25 * time.Second)
	for rn.IsBusy() && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	if err := rn.Store.Save(); err != nil {
		return err
	}
	fmt.Println("funding-carry: stopped")
	return nil
}

func once(a []string) error {
	rn, err := newRunner()
	if err != nil {
		return err
	}
	day := carry.NowMS() - carry.NowMS()%market.DayMS
	preview := false
	for i, x := range a {
		switch x {
		case "--preview":
			preview = true
		case "--day":
			if i+1 >= len(a) {
				return fmt.Errorf("--day needs YYYY-MM-DD")
			}
			t, err := time.Parse("2006-01-02", a[i+1])
			if err != nil {
				return err
			}
			day = t.UnixMilli()
		}
	}
	plan, err := rn.RunDay(day, preview)
	if err != nil {
		return err
	}
	out, err := json.Marshal(map[string]any{"day": plan.Day, "book": plan.Book, "open": plan.Open, "close": plan.Close,
		"pair_stopped": plan.PairStopped, "top": plan.Top, "candidates": plan.Candidates, "idle": plan.Idle})
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func main() {
	a := os.Args[1:]
	cmd := "serve"
	if len(a) > 0 {
		cmd = a[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "once":
		err = once(a[1:])
	default:
		fail("%s", usage)
	}
	if err != nil {
		fail("%v", err)
	}
}
