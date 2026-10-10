package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"control/internal/binance"
	"control/internal/i18n"
	"control/internal/store"
)

// Notifier delivers a ready (localized, HTML) message to a chat.
type Notifier interface {
	Notify(chatID int64, text string)
}

type Options struct {
	FapiURL  string
	SapiURL  string
	WsURL    string
	Interval time.Duration
	Rules    Rules
	Location *time.Location
	Now      func() time.Time // for tests; nil = time.Now
}

type Guard struct {
	opt    Options
	st     *store.Store
	notify Notifier

	mu        sync.Mutex
	clients   map[int64]*binance.Client
	inflight  map[int64]bool
	streams   map[int64]context.CancelFunc
	enforceMu map[int64]*sync.Mutex
	failures  map[int64]int // consecutive failed checks
	warned    map[int64]bool
}

func New(opt Options, st *store.Store, n Notifier) *Guard {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Location == nil {
		opt.Location = time.Local
	}
	return &Guard{
		opt: opt, st: st, notify: n,
		clients:   map[int64]*binance.Client{},
		inflight:  map[int64]bool{},
		streams:   map[int64]context.CancelFunc{},
		enforceMu: map[int64]*sync.Mutex{},
		failures:  map[int64]int{},
		warned:    map[int64]bool{},
	}
}

func (g *Guard) Rules() Rules             { return g.opt.Rules }
func (g *Guard) Location() *time.Location { return g.opt.Location }

// Run performs a check of every active account each Interval until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(g.opt.Interval)
	defer t.Stop()
	g.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			g.stopAllStreams()
			return
		case <-t.C:
			g.tick(ctx)
		}
	}
}

func (g *Guard) tick(ctx context.Context) {
	for _, id := range g.st.Active() {
		g.mu.Lock()
		busy := g.inflight[id]
		g.inflight[id] = true
		g.mu.Unlock()
		if busy { // previous check still running (slow API): skip this tick
			continue
		}
		go func(id int64) {
			defer func() {
				g.mu.Lock()
				delete(g.inflight, id)
				g.mu.Unlock()
			}()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			g.Check(cctx, id)
		}(id)
	}
}

func (g *Guard) client(u store.User) (*binance.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.clients[u.ChatID]; ok {
		return c, nil
	}
	key, secret, err := g.st.Credentials(u)
	if err != nil {
		return nil, err
	}
	c := binance.New(g.opt.FapiURL, g.opt.SapiURL, key, secret)
	g.clients[u.ChatID] = c
	return c, nil
}

func snapshotOf(acc *binance.Account, now time.Time) store.Snapshot {
	wallet := acc.TotalWalletBalance.Float()
	upnl := acc.TotalUnrealizedProfit.Float()
	return store.Snapshot{
		CheckedAt:     now,
		WalletBalance: wallet,
		UnrealizedPnL: upnl,
		Equity:        wallet + upnl,
		OpenPositions: len(acc.OpenPositions()),
	}
}

// Check is one cron run for one account: read the balance, save the snapshot, act.
func (g *Guard) Check(ctx context.Context, chatID int64) {
	u, ok := g.st.Get(chatID)
	if !ok || !u.Active {
		return
	}
	c, err := g.client(u)
	if err != nil {
		g.recordFailure(chatID, u.Lang, err)
		return
	}
	acc, err := c.Account(ctx)
	if err != nil {
		g.recordFailure(chatID, u.Lang, err)
		return
	}
	g.recordSuccess(chatID, u.Lang)

	now := g.opt.Now()
	snap := snapshotOf(acc, now)
	var d Decision
	u, err = g.st.Update(chatID, func(u *store.User) {
		if u.Active {
			d = Evaluate(u, snap, now, g.opt.Rules)
		}
	})
	if err != nil {
		log.Printf("chat %d: save state: %v", chatID, err)
	}
	g.act(ctx, u, d, acc)
}

func (g *Guard) act(ctx context.Context, u store.User, d Decision, acc *binance.Account) {
	lang, r, last := u.Lang, g.opt.Rules, u.Last
	switch {
	case d.Alert:
		g.notify.Notify(u.ChatID, i18n.T(lang, "alert",
			money(last.LossAmount), pct(last.LossPct), money(u.Baseline),
			money(last.Equity), signed(last.UnrealizedPnL),
			r.BlockPct, money(u.Baseline*(1-r.BlockPct/100)), human(r.BlockFor, lang)))

	case d.Block:
		res := g.Enforce(ctx, u.ChatID)
		g.notify.Notify(u.ChatID, i18n.T(lang, "blocked",
			money(last.LossAmount), pct(last.LossPct), res.Closed, res.CancelledSymbols,
			g.fmtTime(u.BlockedUntil)))
		g.reportErrors(u, res)
		g.ensureStream(u)

	case d.Unblock:
		g.stopStream(u.ChatID)
		g.notify.Notify(u.ChatID, i18n.T(lang, "unblocked", money(u.Baseline)))

	case d.Blocked:
		g.ensureStream(u) // e.g. after a restart during a block
		if len(acc.OpenPositions()) > 0 || g.hasOpenOrders(ctx, u) {
			g.enforceAndReport(ctx, u)
		}
	}
}

// EnforceResult summarizes one flattening pass.
type EnforceResult struct {
	Closed           int
	CancelledSymbols int
	Errors           []string
}

func (r EnforceResult) Did() bool { return r.Closed > 0 || r.CancelledSymbols > 0 }

func (g *Guard) lockEnforce(chatID int64) func() {
	g.mu.Lock()
	m, ok := g.enforceMu[chatID]
	if !ok {
		m = &sync.Mutex{}
		g.enforceMu[chatID] = m
	}
	g.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (g *Guard) hasOpenOrders(ctx context.Context, u store.User) bool {
	c, err := g.client(u)
	if err != nil {
		return false
	}
	if o, err := c.OpenOrders(ctx); err == nil && len(o) > 0 {
		return true
	}
	if o, err := c.OpenAlgoOrders(ctx); err == nil && len(o) > 0 {
		return true
	}
	return false
}

// Enforce cancels all open orders (regular and conditional) and market-closes every
// open position. Concurrent calls for the same account are serialized.
func (g *Guard) Enforce(ctx context.Context, chatID int64) EnforceResult {
	defer g.lockEnforce(chatID)()
	var res EnforceResult
	u, _ := g.st.Get(chatID)
	c, err := g.client(u)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}

	symbols := map[string]bool{}
	if orders, err := c.OpenOrders(ctx); err != nil {
		res.Errors = append(res.Errors, "open orders: "+err.Error())
	} else {
		for _, o := range orders {
			symbols[o.Symbol] = true
		}
	}
	algoSymbols := map[string]bool{}
	if orders, err := c.OpenAlgoOrders(ctx); err != nil {
		if !isNotFound(err) {
			res.Errors = append(res.Errors, "open algo orders: "+err.Error())
		}
	} else {
		for _, o := range orders {
			algoSymbols[o.Symbol] = true
		}
	}
	cancelled := map[string]bool{}
	for s := range symbols {
		if err := c.CancelAllOpenOrders(ctx, s); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("cancel %s: %v", s, err))
		} else {
			cancelled[s] = true
		}
	}
	for s := range algoSymbols {
		if err := c.CancelAllAlgoOrders(ctx, s); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("cancel algo %s: %v", s, err))
		} else {
			cancelled[s] = true
		}
	}
	res.CancelledSymbols = len(cancelled)

	acc, err := c.Account(ctx)
	if err != nil {
		res.Errors = append(res.Errors, "account: "+err.Error())
		return res
	}
	open := acc.OpenPositions()
	if len(open) == 0 {
		return res
	}
	hedge, err := c.HedgeMode(ctx)
	if err != nil {
		hedge = anyHedgeSide(open)
	}
	for _, p := range open {
		if err := c.ClosePosition(ctx, p, hedge); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("close %s %s: %v", p.Symbol, p.PositionAmt, err))
		} else {
			res.Closed++
		}
	}
	return res
}

func anyHedgeSide(ps []binance.Position) bool {
	for _, p := range ps {
		if p.PositionSide == "LONG" || p.PositionSide == "SHORT" {
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	var ae *binance.APIError
	return errors.As(err, &ae) && ae.HTTPStatus == http.StatusNotFound
}

func (g *Guard) enforceAndReport(ctx context.Context, u store.User) {
	res := g.Enforce(ctx, u.ChatID)
	if res.Did() {
		g.notify.Notify(u.ChatID, i18n.T(u.Lang, "enforced", g.fmtTime(u.BlockedUntil), res.Closed, res.CancelledSymbols))
	}
	g.reportErrors(u, res)
}

func (g *Guard) reportErrors(u store.User, res EnforceResult) {
	if len(res.Errors) > 0 {
		log.Printf("chat %d: enforce errors: %v", u.ChatID, res.Errors)
		g.notify.Notify(u.ChatID, i18n.T(u.Lang, "enforce_errors", escape(strings.Join(res.Errors, "\n"))))
	}
}

// ensureStream opens the user data stream while the block is in force, so new orders
// are closed as soon as Binance pushes them rather than on the next poll.
func (g *Guard) ensureStream(u store.User) {
	g.mu.Lock()
	if _, ok := g.streams[u.ChatID]; ok || g.opt.WsURL == "" {
		g.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.streams[u.ChatID] = cancel
	g.mu.Unlock()

	c, err := g.client(u)
	if err != nil {
		g.stopStream(u.ChatID)
		return
	}
	kick := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
				cur, _ := g.st.Get(u.ChatID)
				if !cur.Blocked || !cur.Active {
					continue
				}
				ectx, ecancel := context.WithTimeout(ctx, 30*time.Second)
				g.enforceAndReport(ectx, cur)
				ecancel()
			}
		}
	}()
	go c.RunUserStream(ctx, g.opt.WsURL, func(ev binance.StreamEvent) {
		switch ev.Type {
		case "ORDER_TRADE_UPDATE", "ACCOUNT_UPDATE", "ALGO_UPDATE":
			select {
			case kick <- struct{}{}:
			default: // an enforcement pass is already queued
			}
		}
	})
	log.Printf("chat %d: user stream started (block in force)", u.ChatID)
}

func (g *Guard) stopStream(chatID int64) {
	g.mu.Lock()
	cancel, ok := g.streams[chatID]
	delete(g.streams, chatID)
	g.mu.Unlock()
	if ok {
		cancel()
		log.Printf("chat %d: user stream stopped", chatID)
	}
}

func (g *Guard) stopAllStreams() {
	g.mu.Lock()
	ids := make([]int64, 0, len(g.streams))
	for id := range g.streams {
		ids = append(ids, id)
	}
	g.mu.Unlock()
	for _, id := range ids {
		g.stopStream(id)
	}
}

func (g *Guard) recordFailure(chatID int64, lang string, err error) {
	now := g.opt.Now()
	_, _ = g.st.Update(chatID, func(u *store.User) {
		u.Last.CheckedAt = now
		u.Last.Error = err.Error()
	})
	g.mu.Lock()
	g.failures[chatID]++
	n := g.failures[chatID]
	warn := !g.warned[chatID] && (n >= 3 || binance.IsAuthError(err))
	if warn {
		g.warned[chatID] = true
	}
	g.mu.Unlock()
	log.Printf("chat %d: check failed (%d in a row): %v", chatID, n, err)
	if warn {
		g.notify.Notify(chatID, i18n.T(lang, "check_failing", escape(err.Error())))
	}
}

func (g *Guard) recordSuccess(chatID int64, lang string) {
	g.mu.Lock()
	warned := g.warned[chatID]
	delete(g.failures, chatID)
	delete(g.warned, chatID)
	g.mu.Unlock()
	if warned {
		g.notify.Notify(chatID, i18n.T(lang, "check_recovered"))
	}
}

// KeyHash identifies an API key without storing it in clear text.
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// ActivateResult reports what the bot should tell the user after /start.
type ActivateResult struct {
	User          store.User
	NoFuturesPerm bool
}

// Activate validates the keys against Binance, stores them encrypted and starts
// monitoring. A running loss window is kept when the same key is connected again,
// so reconnecting cannot be used to reset the day's loss.
func (g *Guard) Activate(ctx context.Context, chatID int64, key, secret string) (ActivateResult, error) {
	var res ActivateResult
	c := binance.New(g.opt.FapiURL, g.opt.SapiURL, key, secret)
	if err := c.SyncTime(ctx); err != nil {
		return res, err
	}
	acc, err := c.Account(ctx)
	if err != nil {
		return res, err
	}
	if ok, err := c.FuturesEnabled(ctx); err == nil && !ok {
		res.NoFuturesPerm = true
	}

	keyEnc, err := g.st.Encrypt(key)
	if err != nil {
		return res, err
	}
	secretEnc, err := g.st.Encrypt(secret)
	if err != nil {
		return res, err
	}
	now := g.opt.Now()
	snap := snapshotOf(acc, now)
	hash := KeyHash(key)
	res.User, err = g.st.Update(chatID, func(u *store.User) {
		keep := u.KeyHash == hash && u.Baseline > 0 && now.Before(u.PeriodStart.Add(g.opt.Rules.Period))
		u.APIKeyEnc, u.APISecretEnc, u.PendingKey = keyEnc, secretEnc, ""
		u.KeyHash = hash
		u.Step = store.StepNone
		u.Active = true
		if !keep {
			StartPeriod(u, snap.WalletBalance, now)
		}
		snap.LossAmount, snap.LossPct = Loss(u.Baseline, snap.Equity)
		u.Last = snap
	})
	if err != nil {
		return res, err
	}
	g.mu.Lock()
	g.clients[chatID] = c
	delete(g.failures, chatID)
	delete(g.warned, chatID)
	g.mu.Unlock()
	return res, nil
}

// Deactivate stops monitoring and deletes the keys. Refused while blocked.
func (g *Guard) Deactivate(chatID int64) (store.User, error) {
	u, _ := g.st.Get(chatID)
	if u.Blocked {
		return u, ErrBlocked
	}
	g.stopStream(chatID)
	g.mu.Lock()
	delete(g.clients, chatID)
	g.mu.Unlock()
	return g.st.Update(chatID, func(u *store.User) {
		u.Active = false
		u.APIKeyEnc, u.APISecretEnc, u.PendingKey = "", "", ""
		u.Step = store.StepNone
	})
}

var ErrBlocked = errors.New("trading is blocked")

func (g *Guard) fmtTime(t time.Time) string {
	return t.In(g.opt.Location).Format("2006-01-02 15:04 MST")
}
