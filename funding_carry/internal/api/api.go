// Package api is the control API v1 (docs/runner_api.md) on the Railway private network (carry/api.py):
// the same routes, codes and error texts. The server listens on :PORT (dual-stack).
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"fundingcarry/internal/carry"
	"fundingcarry/internal/market"
	"fundingcarry/internal/pyx"
	"fundingcarry/internal/runner"
	"fundingcarry/internal/state"
)

// rssMB reads VmRSS from /proc/self/status (Linux); nil elsewhere.
func rssMB() *float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					v := float64(int64(float64(kb)/1024*10+0.5)) / 10
					return &v
				}
			}
		}
	}
	return nil
}

// pingCache measures Binance reachability at most once a minute.
type pingCache struct {
	mu    sync.Mutex
	at    time.Time
	ok    bool
	ms    int
	err   *string
	fetch func() (bool, int, *string)
}

func (p *pingCache) get() (bool, int, *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || time.Since(p.at) > 60*time.Second {
		p.ok, p.ms, p.err = p.fetch()
		p.at = time.Now()
	}
	return p.ok, p.ms, p.err
}

type handler struct {
	runner *runner.Runner
	token  string
	ping   *pingCache
}

// NewHandler is make_server without the listener: pass the result to http.Server.
func NewHandler(r *runner.Runner, token string) http.Handler {
	return &handler{runner: r, token: token, ping: &pingCache{fetch: market.Ping}}
}

// SetPing replaces the Binance reachability probe (tests).
func SetPing(h http.Handler, f func() (bool, int, *string)) {
	h.(*handler).ping.fetch = f
}

// EnvToken is CONTROL_TOKEN, trimmed.
func EnvToken() string { return strings.TrimSpace(os.Getenv("CONTROL_TOKEN")) }

func send(w http.ResponseWriter, code int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		buf.Reset()
		code = 500
		enc.Encode(map[string]string{"error": "RuntimeError: " + err.Error()})
	}
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(code)
	w.Write(raw)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func (h *handler) auth(w http.ResponseWriter, r *http.Request) bool {
	if h.token == "" {
		send(w, 503, errBody("CONTROL_TOKEN is not set on the runner"))
		return false
	}
	got := r.Header.Get("X-Control-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		send(w, 401, errBody("unauthorized"))
		return false
	}
	return true
}

func body(r *http.Request) map[string]any {
	n := r.ContentLength
	if n <= 0 {
		return map[string]any{}
	}
	if n > 65536 {
		n = 65536
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, n))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		send(w, 501, errBody("Unsupported method ("+pyx.Repr(r.Method)+")"))
		return
	}
	var parts []string
	for _, p := range strings.Split(r.URL.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if r.Method == "GET" && len(parts) == 1 && parts[0] == "healthz" {
		send(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(parts) == 0 || parts[0] != "v1" {
		send(w, 404, errBody("not found"))
		return
	}
	if !h.auth(w, r) {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			send(w, 500, errBody("RuntimeError: "+fmt.Sprint(p)))
		}
	}()
	q := map[string]string{}
	for k, v := range r.URL.Query() {
		q[k] = v[len(v)-1]
	}
	method := r.Method
	if method == "GET" && len(parts) == 2 && parts[1] == "health" {
		send(w, 200, h.health())
		return
	}
	if method == "GET" && len(parts) == 2 && parts[1] == "strategies" {
		send(w, 200, []runner.StrategyView{h.runner.Strategy()})
		return
	}
	if len(parts) < 4 || parts[1] != "strategies" {
		send(w, 404, errBody("not found"))
		return
	}
	if parts[2] != carry.StrategyID {
		send(w, 404, errBody(fmt.Sprintf("unknown strategy %s; this runner has %s", pyx.Repr(parts[2]), carry.StrategyID)))
		return
	}
	action := parts[3]
	switch {
	case method == "POST" && (action == "pause" || action == "resume"):
		b := body(r)
		desired := "running"
		if action == "pause" {
			desired = "paused"
		}
		reason := ""
		if v, ok := b["reason"]; ok {
			reason = pyx.Str(v)
		}
		s := h.runner.SetDesired(desired, b["by"], reason)
		send(w, 200, map[string]any{"ok": true, "strategy": s})
	case method == "POST" && action == "flatten":
		b := body(r)
		if c, ok := b["confirm"].(bool); !ok || !c {
			send(w, 400, errBody(`flatten needs {"confirm": true}`))
			return
		}
		res, err := h.runner.Flatten(b["by"])
		if err != nil {
			if _, ok := err.(*runner.ValueError); ok {
				send(w, 409, errBody(err.Error()))
				return
			}
			send(w, 500, errBody(pyx.Describe(err)))
			return
		}
		send(w, 200, res)
	case method == "POST" && action == "run":
		res, err := h.runner.Trigger()
		if err != nil {
			if _, ok := err.(runner.Busy); ok {
				send(w, 409, errBody("busy"))
				return
			}
			send(w, 500, errBody(pyx.Describe(err)))
			return
		}
		send(w, 200, res)
	case method == "GET" && action == "plan":
		plan := h.runner.Store.Snapshot().Plan
		if t := bytes.TrimSpace(plan); len(t) > 0 && string(t) != "null" && string(t) != "{}" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(t)))
			w.WriteHeader(200)
			w.Write(t)
		} else {
			send(w, 404, errBody("no plan yet"))
		}
	case method == "GET" && action == "positions":
		send(w, 200, h.runner.Positions())
	case method == "GET" && action == "report":
		send(w, 200, h.runner.Report())
	case method == "GET" && action == "events":
		send(w, 200, h.events(q))
	default:
		send(w, 404, errBody("not found"))
	}
}

type binanceHealth struct {
	OK        bool    `json:"ok"`
	LatencyMS int     `json:"latency_ms"`
	Error     *string `json:"error"`
}

type health struct {
	Runner       string         `json:"runner"`
	Version      string         `json:"version"`
	UptimeS      int64          `json:"uptime_s"`
	Mode         string         `json:"mode"`
	Now          string         `json:"now"`
	Binance      binanceHealth  `json:"binance"`
	RSSMB        *float64       `json:"rss_mb"`
	UniverseDays int            `json:"universe_days"`
	StateBytes   int64          `json:"state_bytes"`
	Busy         bool           `json:"busy"`
	Errors       []state.ErrRec `json:"errors"`
}

func (h *handler) health() health {
	st := h.runner.Store.Snapshot()
	ok, ms, err := h.ping.get()
	errs := st.Errors
	if len(errs) > 5 {
		errs = errs[len(errs)-5:]
	}
	return health{Runner: carry.Runner, Version: carry.Version, UptimeS: int64(time.Since(h.runner.Started).Seconds()),
		Mode: h.runner.Mode, Now: carry.NowISO(), Binance: binanceHealth{ok, ms, err}, RSSMB: rssMB(),
		UniverseDays: len(st.Tags), StateBytes: h.runner.Store.Size(), Busy: h.runner.IsBusy(), Errors: append([]state.ErrRec{}, errs...)}
}

// pyInt is int(s): Python strips whitespace and accepts a sign.
func pyInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

func (h *handler) events(q map[string]string) []state.Event {
	evs := h.runner.Store.Snapshot().Events
	n := 20
	if v, ok := q["n"]; ok {
		if x, ok := pyInt(v); ok {
			n = x
		}
		if n < 1 {
			n = 1
		}
		if n > 200 {
			n = 200
		}
	}
	if v, ok := q["since"]; ok {
		since, _ := pyInt(v)
		out := []state.Event{}
		for _, e := range evs {
			if e.Seq > int64(since) {
				out = append(out, e)
			}
		}
		if len(out) > 200 {
			out = out[:200]
		}
		return out
	}
	if len(evs) > n {
		evs = evs[len(evs)-n:]
	}
	return append([]state.Event{}, evs...)
}
