package runners

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner is a minimal runner: one strategy "funding.carry".
type fakeRunner struct {
	mu        sync.Mutex
	down      bool
	lastError *string
	events    []Event
	tokens    []string
	srv       *httptest.Server
}

func newFake(t *testing.T) *fakeRunner {
	f := &fakeRunner{}
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.tokens = append(f.tokens, r.Header.Get("X-Control-Token"))
			if r.Header.Get("X-Control-Token") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			if f.down {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v1/health", wrap(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"runner":"funding","version":"1","mode":"paper","binance":{"ok":true,"latency_ms":40,"error":null},"errors":[]}`))
	}))
	mux.HandleFunc("/v1/strategies", wrap(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Strategy{{ID: "funding.carry", Mode: "paper", State: "running",
			Desired: "running", LastError: f.lastError}})
	}))
	mux.HandleFunc("/v1/strategies/funding.carry/events", wrap(func(w http.ResponseWriter, r *http.Request) {
		out := []Event{}
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ := strconv.ParseInt(s, 10, 64)
			for _, e := range f.events {
				if e.Seq > since {
					out = append(out, e)
				}
			}
		} else if len(f.events) > 0 {
			out = f.events[len(f.events)-1:]
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("/v1/strategies/funding.carry/pause", wrap(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			By int64 `json:"by"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "strategy": Strategy{ID: "funding.carry", State: "paused", Desired: "paused", OpenPositions: int(in.By)}})
	}))
	mux.HandleFunc("/v1/strategies/funding.carry/plan", wrap(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"day":"2026-10-10","resize":{"YUSDT":[40.1,80.3]},"top":[["XUSDT",0.00031]],
"orders":[{"symbol":"XUSDT","spot":["BUY",12.0],"perp":["SELL",12.0],"notional":83.4,"ok":true}]}`))
	}))
	mux.HandleFunc("/v1/strategies/funding.carry/run", wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRunner) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

type sink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *sink) NotifyAll(text string) { s.mu.Lock(); s.msgs = append(s.msgs, text); s.mu.Unlock() }
func (s *sink) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.msgs
	s.msgs = nil
	return out
}

func TestClientSendsTokenAndDecodes(t *testing.T) {
	f := newFake(t)
	c := NewClient("funding", f.srv.URL+"/", "secret")
	ctx := context.Background()
	if _, err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.tokens) != 1 || f.tokens[0] != "secret" {
		t.Fatalf("tokens: %v", f.tokens)
	}
	st, err := c.Pause(ctx, "funding.carry", 77)
	if err != nil || st.State != "paused" || st.OpenPositions != 77 {
		t.Fatalf("pause: %+v %v", st, err)
	}
	p, err := c.Plan(ctx, "funding.carry")
	if err != nil {
		t.Fatal(err)
	}
	if p.Resize["YUSDT"] != [2]float64{40.1, 80.3} || len(p.Top) != 1 || p.Top[0].Symbol != "XUSDT" ||
		p.Orders[0].Spot.Side != "BUY" || p.Orders[0].Perp.Qty != 12 {
		t.Fatalf("plan: %+v", p)
	}
}

func TestClientErrors(t *testing.T) {
	f := newFake(t)
	bad := NewClient("funding", f.srv.URL, "wrong")
	_, err := bad.Health(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("401: %v", err)
	}
	good := NewClient("funding", f.srv.URL, "secret")
	_, err = good.Run(context.Background(), "funding.carry")
	if !errors.As(err, &he) || he.Status != 409 || he.Msg != "busy" {
		t.Fatalf("409: %v", err)
	}
	_, err = good.Plan(context.Background(), "nope")
	if !errors.As(err, &he) || he.Status != 404 {
		t.Fatalf("404: %v", err)
	}
}

func TestFind(t *testing.T) {
	r := &Registry{clients: map[string]*Client{"a": {Name: "a"}, "b": {Name: "b"}}, state: map[string]*runnerState{
		"a": {snap: RunnerSnap{Name: "a", Strategies: []Strategy{{ID: "a.carry"}, {ID: "a.carry2"}}}},
		"b": {snap: RunnerSnap{Name: "b", Strategies: []Strategy{{ID: "b.mom"}}}},
	}, order: []string{"a", "b"}}
	cases := []struct {
		q    string
		want string // "" = error
	}{
		{"a.carry", "a.carry"}, // exact beats the prefix of a.carry2
		{"a.carry2", "a.carry2"},
		{"b.m", "b.mom"},
		{"b", "b.mom"}, // runner with one strategy (also a prefix of b.mom: unique prefix)
		{"a", ""},      // two strategies, ambiguous
		{"", ""},       // three strategies overall
		{"zzz", ""},
	}
	for _, c := range cases {
		_, id, err := r.Find(c.q)
		if (err == nil) != (c.want != "") || id != c.want {
			t.Errorf("Find(%q) = %q, %v; want %q", c.q, id, err, c.want)
		}
	}
	_, _, err := r.Find("zzz")
	if err == nil || !strings.Contains(err.Error(), "a.carry") {
		t.Errorf("error must list known ids: %v", err)
	}
	r.state["a"].snap.Strategies = nil
	if _, id, err := r.Find(""); err != nil || id != "b.mom" {
		t.Errorf("single strategy overall: %q %v", id, err)
	}
	// a runner name that is not an id prefix
	r.clients["bots"] = &Client{Name: "bots"}
	r.state["bots"] = &runnerState{snap: RunnerSnap{Name: "bots", Strategies: []Strategy{{ID: "x.one"}}}}
	r.order = append(r.order, "bots")
	if _, id, err := r.Find("bots"); err != nil || id != "x.one" {
		t.Errorf("runner name: %q %v", id, err)
	}
}

func TestFailTransition(t *testing.T) {
	cases := []struct {
		prev, cur int
		want      FailEvent
	}{
		{0, 1, FailNone}, {1, 2, FailNone}, {2, 3, FailDown}, {3, 4, FailNone},
		{3, 0, FailRecovered}, {2, 0, FailNone}, {0, 0, FailNone},
	}
	for _, c := range cases {
		if got := FailTransition(c.prev, c.cur); got != c.want {
			t.Errorf("FailTransition(%d,%d) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func TestNewStrategyError(t *testing.T) {
	s := func(v string) *string { return &v }
	if _, ok := NewStrategyError("", nil); ok {
		t.Error("nil is not an error")
	}
	if _, ok := NewStrategyError("", s("")); ok {
		t.Error("empty is not an error")
	}
	if m, ok := NewStrategyError("", s("x")); !ok || m != "x" {
		t.Error("new error must alert")
	}
	if _, ok := NewStrategyError("x", s("x")); ok {
		t.Error("same error must not repeat")
	}
	if _, ok := NewStrategyError("x", s("y")); !ok {
		t.Error("changed error must alert")
	}
}

func TestForwardEvents(t *testing.T) {
	evs := []Event{
		{Seq: 1, Level: "info"},
		{Seq: 2, Level: "info", Notify: true},
		{Seq: 3, Level: "warn"},
		{Seq: 4, Level: "error"},
		{Seq: 5, Level: "info"},
	}
	out, max := ForwardEvents(evs, 1)
	if len(out) != 3 || out[0].Seq != 2 || out[2].Seq != 4 || max != 5 {
		t.Fatalf("out %+v max %d", out, max)
	}
	if out, max := ForwardEvents(nil, 9); len(out) != 0 || max != 9 {
		t.Fatalf("empty: %v %d", out, max)
	}
}

func TestPollerAlerts(t *testing.T) {
	f := newFake(t)
	f.events = []Event{{Seq: 10, Level: "error", Msg: "old"}}
	n := &sink{}
	r := NewRegistry([]*Client{NewClient("funding", f.srv.URL, "secret")}, time.Minute, "en", n)
	ctx := context.Background()

	r.PollNow(ctx) // first poll: baseline only, no replay of seq 10
	if got := n.take(); len(got) != 0 {
		t.Fatalf("first poll alerted: %v", got)
	}
	if s := r.Snapshot(); !s[0].OK || len(s[0].Strategies) != 1 || s[0].Health == nil {
		t.Fatalf("snapshot: %+v", s)
	}

	f.set(func() {
		f.events = append(f.events, Event{Seq: 11, Level: "info", Msg: "quiet"}, Event{Seq: 12, Level: "info", Notify: true, Msg: "opened <X>"})
	})
	r.PollNow(ctx)
	got := n.take()
	if len(got) != 1 || !strings.Contains(got[0], "opened &lt;X&gt;") {
		t.Fatalf("event alert: %v", got)
	}
	r.PollNow(ctx)
	if got := n.take(); len(got) != 0 {
		t.Fatalf("event replayed: %v", got)
	}

	e := "api down"
	f.set(func() { f.lastError = &e })
	r.PollNow(ctx)
	r.PollNow(ctx)
	if got := n.take(); len(got) != 1 || !strings.Contains(got[0], "api down") {
		t.Fatalf("error alert (once): %v", got)
	}

	f.set(func() { f.down = true })
	r.PollNow(ctx)
	r.PollNow(ctx)
	if got := n.take(); len(got) != 0 {
		t.Fatalf("alerted before 3 failures: %v", got)
	}
	r.PollNow(ctx)
	r.PollNow(ctx)
	if got := n.take(); len(got) != 1 || !strings.Contains(got[0], "funding") {
		t.Fatalf("down alert (once): %v", got)
	}
	if s := r.Snapshot()[0]; s.OK || s.FailStreak != 4 || s.LastErr == "" || len(s.Strategies) != 1 {
		t.Fatalf("snapshot while down: %+v", s)
	}
	f.set(func() { f.down = false })
	r.PollNow(ctx)
	if got := n.take(); len(got) != 1 {
		t.Fatalf("recovered alert: %v", got)
	}
	r.PollNow(ctx)
	if got := n.take(); len(got) != 0 {
		t.Fatalf("extra alerts: %v", got)
	}
}
