// density-bounce runner (bounce/__main__.py).
//
//	bounce [serve]   control API on :PORT + the live market loop (Railway entry point)
//	bounce check     live mode: read-only check of the key / account, sends no order
//
// Environment: CONTROL_TOKEN (required to serve /v1/*), PORT (8080), MODE (paper | live), STATE_FILE
// (/data/bounce_state.json, paper), LIVE_STATE_FILE (/data/bounce_live_state.json), CAPITAL (100; PAPER_CAPITAL also
// works: the record's starting equity, used only when the state file is created), NOTIFY_TRADES (closes | all |
// none), WATCH (16 symbols), NOTIONAL (10 USDT per trade), LEVERAGE (2), MAX_OPEN (2), DAY_LOSS (1), TOTAL_LOSS (5),
// MAX_CONSEC_LOSSES (6), MAX_TRADES_DAY (60), PARAMS (JSON object over setup.P). Live also reads BINANCE_API_KEY +
// BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM), or KEY_FROM_CONTROL.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"densitybounce/internal/api"
	"densitybounce/internal/keys"
	"densitybounce/internal/live"
	"densitybounce/internal/meta"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/runner"
	"densitybounce/internal/setup"
	"densitybounce/internal/state"
	"densitybounce/internal/trader"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func env(k string) string { return os.Getenv(k) }

func firstEnv(def string, keys ...string) string {
	for _, k := range keys {
		if v := env(k); v != "" {
			return v
		}
	}
	return def
}

func build() *runner.Runner {
	mode := strings.ToLower(strings.TrimSpace(firstEnv("paper", "MODE")))
	if mode != "paper" && mode != "live" {
		die("MODE='%s': use paper or live", mode)
	}
	if err := setup.LoadParams(env("PARAMS")); err != nil {
		die("PARAMS: %v", err)
	}
	if err := trader.LoadRisk(env); err != nil {
		die("%v", err)
	}
	capital, err := strconv.ParseFloat(strings.TrimSpace(firstEnv("100", "CAPITAL", "PAPER_CAPITAL")), 64)
	if err != nil {
		die("CAPITAL: could not convert string to float: %q", firstEnv("100", "CAPITAL", "PAPER_CAPITAL"))
	}
	notify := strings.ToLower(strings.TrimSpace(firstEnv("closes", "NOTIFY_TRADES")))
	if mode == "paper" {
		store, err := state.Open(firstEnv("/data/bounce_state.json", "STATE_FILE"), capital)
		if err != nil {
			die("%v", err)
		}
		store.SetLastError("") // an earlier run's error (another mode) is not this run's state
		return runner.New(store, "paper", notify, "", nil)
	}

	// live: its own state file (the paper record stays as it was); not STATE_FILE: the image sets it to the paper record
	store, err := state.Open(firstEnv("/data/bounce_live_state.json", "LIVE_STATE_FILE"), capital)
	if err != nil {
		die("%v", err)
	}
	d := store.Snapshot()
	if d.Totals.N == 0 && len(d.Slots) == 0 && d.Capital0 != capital {
		_ = store.Set("capital0", capital) // nothing traded yet: CAPITAL may still change
	}
	store.SetLastError("") // the checks below set it again if live stays off
	if why := keys.FromControl(12, 10*time.Second); why != "" {
		// not notify: the reason is the strategy's last_error, which control already alerts once
		store.Event("error", fmt.Sprintf("%s: live trading is OFF - no Binance key from control: %s", meta.StrategyID, why), false)
		return runner.New(store, "live-blocked", notify, why, nil)
	}
	rest, err := live.RestFromEnv()
	if err != nil {
		die("%v", err)
	}
	problems, cerr := live.Check(rest)
	if cerr != nil {
		problems = []string{fmt.Sprintf("account check failed (%v)", cerr)}
	}
	if len(problems) > 0 {
		joined := strings.Join(problems, "; ")
		store.Event("error", fmt.Sprintf("%s: live trading is OFF - %s", meta.StrategyID, joined), false)
		return runner.New(store, "live-blocked", notify, joined, nil)
	}
	return runner.New(store, "live", notify, "", rest)
}

func listen(port string) net.Listener {
	ln, err := net.Listen("tcp", ":"+port) // dual-stack: IPv6 for the Railway private network, IPv4 for local runs
	if err != nil {
		fmt.Printf("WARNING: cannot bind [::]:%s (%v), falling back to 0.0.0.0\n", port, err)
		if ln, err = net.Listen("tcp4", "0.0.0.0:"+port); err != nil {
			die("%v", err)
		}
	}
	return ln
}

func serve() {
	r := build()
	token := api.EnvToken()
	port := firstEnv("8080", "PORT")
	if token == "" {
		fmt.Println("WARNING: CONTROL_TOKEN is empty - /v1/* answers 503 until it is set")
	}
	ln := listen(port)
	srv := &http.Server{Handler: api.New(r, token, nil), ReadHeaderTimeout: 10 * time.Second}
	d := r.Store.Snapshot()
	p := setup.P
	r.Store.Event("info", fmt.Sprintf("%s v%s started: mode %s, %s, %d trades so far (%s USDT); %s USDT a stop, <= %s USDT/trade, "+
		"approach >= %s/%ss, move >= %s/%smin, density >= %s s of the flow, entry %s s after the touch, market < %s, "+
		"stop max(%s, %s x ATR), %s at +%sR, take %s of the move", meta.StrategyID, meta.Version, r.Mode, d.Desired,
		d.Totals.N, pyfmt.FS(d.Totals.PNL, 4), pyfmt.G(trader.RiskUSD()), pyfmt.G(trader.RISK.Notional),
		pyfmt.Pct(p.ApproachMin, 2), pyfmt.F(p.ApproachSecs, 0), pyfmt.Pct(p.MoveMin, 2), pyfmt.F(p.MoveSecs/60, 0),
		pyfmt.F(p.HoldMin, 0), pyfmt.F(p.ConfirmSecs, 0), pyfmt.Pct(p.MktMax, 1), pyfmt.Pct(p.StopBuf, 2), pyfmt.G(p.StopATR),
		pyfmt.Pct(p.TP1Part, 0), pyfmt.G(p.TP1R), pyfmt.Pct(p.TakeFrac, 0)), true)
	r.Start()
	fmt.Printf("density-bounce: API on [::]:%s, state %s\n", port, r.Store.Path())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			die("%v", err)
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(sctx)
		cancel()
	}
	r.Stop()
	// open live trades keep their exchange-side stop and take; the next start reconciles them
	if err := r.Store.Save(); err != nil {
		fmt.Printf("WARNING: state save: %v\n", err)
	}
	fmt.Println("density-bounce: stopped")
}

func check() {
	rest, err := live.RestFromEnv()
	if err != nil {
		die("%v", err)
	}
	problems, err := live.Check(rest)
	if err != nil {
		problems = []string{fmt.Sprintf("account check failed (%v)", err)}
	}
	if len(problems) == 0 {
		fmt.Println("live check: OK")
		return
	}
	fmt.Println("live check:\n- " + strings.Join(problems, "\n- "))
}

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve()
	case "check":
		check()
	default:
		die("usage: bounce [serve | check]")
	}
}
