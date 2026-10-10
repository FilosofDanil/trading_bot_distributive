package runners

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"control/internal/guard"
	"control/internal/i18n"
)

// Notifier delivers an alert to every allowed Telegram user.
type Notifier interface{ NotifyAll(text string) }

// RunnerSnap is the last known state of one runner.
type RunnerSnap struct {
	Name       string
	URL        string
	OK         bool // the last poll succeeded
	LastOK     time.Time
	LastErr    string
	Latency    time.Duration // of the last health call
	FailStreak int
	Health     *Health    // from the last successful poll
	Strategies []Strategy // from the last successful poll
}

// runnerState is the poller's per-runner bookkeeping.
type runnerState struct {
	snap     RunnerSnap
	lastErr  map[string]string // strategy id -> last_error already seen
	seqs     map[string]int64  // strategy id -> last event seq seen
	polledOK bool              // at least one successful poll (strategy errors are a baseline before that)
}

// Registry owns the clients and polls them.
type Registry struct {
	mu       sync.RWMutex
	order    []string
	clients  map[string]*Client
	state    map[string]*runnerState
	interval time.Duration
	lang     string
	notifier Notifier
	pollMu   sync.Mutex // serializes polls (ticker vs /ping)
}

// NewRegistry builds clients for the given runners. lang is the language of the alerts.
func NewRegistry(clients []*Client, interval time.Duration, lang string, n Notifier) *Registry {
	r := &Registry{clients: map[string]*Client{}, state: map[string]*runnerState{},
		interval: interval, lang: lang, notifier: n}
	for _, c := range clients {
		r.order = append(r.order, c.Name)
		r.clients[c.Name] = c
		r.state[c.Name] = &runnerState{
			snap:    RunnerSnap{Name: c.Name, URL: c.URL},
			lastErr: map[string]string{},
			seqs:    map[string]int64{},
		}
	}
	return r
}

// Empty reports whether no runners are configured.
func (r *Registry) Empty() bool { return r == nil || len(r.order) == 0 }

// Run polls until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	r.PollNow(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.PollNow(ctx)
		}
	}
}

// PollNow polls every runner once (concurrently) and returns when all are done.
func (r *Registry) PollNow(ctx context.Context) {
	r.pollMu.Lock()
	defer r.pollMu.Unlock()
	var wg sync.WaitGroup
	for _, name := range r.order {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					log.Printf("runner %s: poll panic: %v", c.Name, p)
				}
			}()
			r.pollOne(ctx, c)
		}(r.clients[name])
	}
	wg.Wait()
}

func (r *Registry) pollOne(ctx context.Context, c *Client) {
	st := r.state[c.Name]
	start := time.Now()
	h, err := c.Health(ctx)
	latency := time.Since(start)
	var strategies []Strategy
	if err == nil {
		strategies, err = c.Strategies(ctx)
	}

	r.mu.Lock()
	prev := st.snap.FailStreak
	st.snap.Latency = latency
	if err != nil {
		st.snap.OK = false
		st.snap.FailStreak++
		st.snap.LastErr = err.Error()
	} else {
		st.snap.OK = true
		st.snap.FailStreak = 0
		st.snap.LastErr = ""
		st.snap.LastOK = time.Now()
		st.snap.Health = &h
		st.snap.Strategies = strategies
	}
	cur := st.snap.FailStreak
	r.mu.Unlock()

	switch FailTransition(prev, cur) {
	case FailDown:
		r.alert(i18n.T(r.lang, "alert_runner_down", guard.Escape(c.Name), cur, guard.Escape(st.snap.LastErr)))
	case FailRecovered:
		r.alert(i18n.T(r.lang, "alert_runner_up", guard.Escape(c.Name)))
	}
	if err != nil {
		return
	}
	for _, s := range strategies {
		r.checkStrategy(ctx, c, st, s)
	}
	st.polledOK = true
}

// checkStrategy raises the last_error and event alerts of one strategy.
func (r *Registry) checkStrategy(ctx context.Context, c *Client, st *runnerState, s Strategy) {
	if msg, changed := NewStrategyError(st.lastErr[s.ID], s.LastError); changed {
		if st.polledOK { // the first poll only records the baseline
			r.alert(i18n.T(r.lang, "alert_strategy_error", guard.Escape(s.ID), guard.Escape(msg)))
		}
		st.lastErr[s.ID] = msg
	} else if s.LastError == nil {
		delete(st.lastErr, s.ID)
	}

	last, seeded := st.seqs[s.ID]
	if !seeded { // do not replay history: only learn the current max seq
		evs, err := c.Events(ctx, s.ID, -1, 1)
		if err != nil {
			log.Printf("runner %s: events %s: %v", c.Name, s.ID, err)
			return
		}
		_, st.seqs[s.ID] = ForwardEvents(evs, 0)
		return
	}
	evs, err := c.Events(ctx, s.ID, last, 200)
	if err != nil {
		log.Printf("runner %s: events %s: %v", c.Name, s.ID, err)
		return
	}
	fwd, maxSeq := ForwardEvents(evs, last)
	for _, e := range fwd {
		r.alert(i18n.T(r.lang, "alert_event", LevelIcon(e.Level), guard.Escape(s.ID), guard.Escape(e.Msg)))
	}
	st.seqs[s.ID] = maxSeq
}

func (r *Registry) alert(text string) {
	if r.notifier != nil {
		r.notifier.NotifyAll(text)
	}
}

// LevelIcon maps an event level to an emoji.
func LevelIcon(level string) string {
	switch level {
	case "error":
		return "🔴"
	case "warn":
		return "🟡"
	}
	return "ℹ️"
}

// Snapshot returns the last known state of every runner, in configuration order.
func (r *Registry) Snapshot() []RunnerSnap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RunnerSnap, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.state[n].snap)
	}
	return out
}

// Client returns the client of a runner by name.
func (r *Registry) Client(name string) *Client { return r.clients[name] }

// Find resolves a strategy from the last snapshot: exact id, else a unique id prefix, else a runner
// name that has exactly one strategy; an empty query matches when there is exactly one strategy overall.
func (r *Registry) Find(query string) (*Client, string, error) {
	type ref struct{ runner, id string }
	var all []ref
	counts := map[string]int{}
	for _, s := range r.Snapshot() {
		for _, st := range s.Strategies {
			all = append(all, ref{s.Name, st.ID})
			counts[s.Name]++
		}
	}
	ids := make([]string, len(all))
	for i, a := range all {
		ids[i] = a.id
	}
	sort.Strings(ids)
	known := strings.Join(ids, ", ")
	query = strings.TrimSpace(query)

	if query == "" {
		if len(all) == 1 {
			return r.clients[all[0].runner], all[0].id, nil
		}
		return nil, "", unknownErr(query, known, len(all))
	}
	for _, a := range all {
		if a.id == query {
			return r.clients[a.runner], a.id, nil
		}
	}
	var pre []ref
	for _, a := range all {
		if strings.HasPrefix(a.id, query) {
			pre = append(pre, a)
		}
	}
	if len(pre) == 1 {
		return r.clients[pre[0].runner], pre[0].id, nil
	}
	if len(pre) == 0 && counts[query] == 1 {
		for _, a := range all {
			if a.runner == query {
				return r.clients[a.runner], a.id, nil
			}
		}
	}
	return nil, "", unknownErr(query, known, len(all))
}

func unknownErr(query, known string, n int) error {
	if n == 0 {
		return fmt.Errorf("no strategies known yet (runners unreachable?)")
	}
	if query == "" {
		return fmt.Errorf("strategy id required; known: %s", known)
	}
	return fmt.Errorf("no unique strategy matches %q; known: %s", query, known)
}
