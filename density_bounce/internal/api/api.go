// Package api is the control API v1 (docs/runner_api.md, bounce/api.py): the same routes, codes and error texts.
package api

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"densitybounce/internal/feed"
	"densitybounce/internal/meta"
	"densitybounce/internal/pyfmt"
	"densitybounce/internal/runner"
)

// Pinger tells whether Binance is reachable.
type Pinger interface {
	Get() (ok bool, latencyMS int, err *string)
}

// BinancePing measures Binance reachability, at most once a minute.
type BinancePing struct {
	Base string
	mu   sync.Mutex
	at   time.Time
	ok   bool
	ms   int
	err  *string
}

// NewPing makes the default pinger (REST base of the feed).
func NewPing() *BinancePing {
	s := "not checked yet"
	return &BinancePing{Base: feed.REST, err: &s}
}

// Get is the cached reachability.
func (p *BinancePing) Get() (bool, int, *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.at) > 60*time.Second {
		p.ok, p.ms, p.err = ping(p.Base)
		p.at = time.Now()
	}
	return p.ok, p.ms, p.err
}

func ping(base string) (bool, int, *string) {
	t0 := time.Now()
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(base + "/fapi/v1/ping")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	ms := int(time.Since(t0).Milliseconds())
	if err != nil {
		s := err.Error()
		return false, ms, &s
	}
	return true, ms, nil
}

// RSSMB is the resident memory from /proc/self/status (nil where there is none).
func RSSMB() *float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.Atoi(f[1]); err == nil {
					v := pyfmt.Round(float64(kb)/1024, 1)
					return &v
				}
			}
		}
	}
	return nil
}

// EnvToken is CONTROL_TOKEN.
func EnvToken() string { return strings.TrimSpace(os.Getenv("CONTROL_TOKEN")) }

type healthBody struct {
	Runner  string `json:"runner"`
	Version string `json:"version"`
	UptimeS int64  `json:"uptime_s"`
	Mode    string `json:"mode"`
	Now     string `json:"now"`
	Binance struct {
		OK        bool    `json:"ok"`
		LatencyMS int     `json:"latency_ms"`
		Error     *string `json:"error"`
	} `json:"binance"`
	RSSMB      *float64 `json:"rss_mb"`
	StateBytes int64    `json:"state_bytes"`
	Busy       bool     `json:"busy"`
	Errors     any      `json:"errors"`
	runner.HealthExtra
}

type server struct {
	r      *runner.Runner
	token  string
	pinger Pinger
}

// New is the handler of the control API.
func New(r *runner.Runner, token string, pinger Pinger) http.Handler {
	if pinger == nil {
		pinger = NewPing()
	}
	return &server{r: r, token: token, pinger: pinger}
}

func send(w http.ResponseWriter, code int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil { // NaN / Inf never reaches the wire
		code = 500
		buf.Reset()
		_ = json.NewEncoder(&buf).Encode(map[string]string{"error": "EncodeError: " + err.Error()})
	}
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

type errBody map[string]string

func (s *server) auth(w http.ResponseWriter, req *http.Request) bool {
	if s.token == "" {
		send(w, 503, errBody{"error": "CONTROL_TOKEN is not set on the runner"})
		return false
	}
	if !hmac.Equal([]byte(req.Header.Get("X-Control-Token")), []byte(s.token)) {
		send(w, 401, errBody{"error": "unauthorized"})
		return false
	}
	return true
}

// body is the JSON object of a request ({} when empty, not an object or not JSON).
func body(req *http.Request) map[string]any {
	if req.ContentLength == 0 {
		return map[string]any{}
	}
	raw, _ := io.ReadAll(io.LimitReader(req.Body, 65536))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func queryInt(v string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return max(lo, min(n, hi))
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			send(w, 500, errBody{"error": fmt.Sprintf("%T: %v", p, p)})
		}
	}()
	var parts []string
	for _, p := range strings.Split(req.URL.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	method := req.Method
	if method == "GET" && len(parts) == 1 && parts[0] == "healthz" {
		send(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(parts) == 0 || parts[0] != "v1" {
		send(w, 404, errBody{"error": "not found"})
		return
	}
	if !s.auth(w, req) {
		return
	}
	q := map[string]string{} // parse_qs: blank values are dropped, the last value wins
	vals, _ := url.ParseQuery(req.URL.RawQuery)
	for k, v := range vals {
		if last := v[len(v)-1]; last != "" {
			q[k] = last
		}
	}
	if method == "GET" && len(parts) == 2 && parts[1] == "health" {
		send(w, 200, s.health())
		return
	}
	if method == "GET" && len(parts) == 2 && parts[1] == "strategies" {
		send(w, 200, []runner.Strategy{s.r.Strategy()})
		return
	}
	if len(parts) < 4 || parts[1] != "strategies" {
		send(w, 404, errBody{"error": "not found"})
		return
	}
	if parts[2] != meta.StrategyID {
		send(w, 404, errBody{"error": fmt.Sprintf("unknown strategy '%s'; this runner has %s", parts[2], meta.StrategyID)})
		return
	}
	action := parts[3]
	switch {
	case method == "POST" && (action == "pause" || action == "resume"):
		b := body(req)
		desired := "running"
		if action == "pause" {
			desired = "paused"
		}
		reason := ""
		if v, ok := b["reason"]; ok {
			reason = pyStr(v)
		}
		send(w, 200, map[string]any{"ok": true, "strategy": s.r.SetDesired(desired, b["by"], reason)})
	case method == "POST" && action == "flatten":
		b := body(req)
		if c, ok := b["confirm"].(bool); !ok || !c {
			send(w, 400, errBody{"error": `flatten needs {"confirm": true}`})
			return
		}
		res, err := s.r.Flatten(b["by"])
		switch {
		case err == runner.ErrNotRunning:
			send(w, 409, errBody{"error": err.Error()})
		case err != nil:
			send(w, 500, errBody{"error": err.Error()})
		default:
			send(w, 200, res)
		}
	case method == "POST" && action == "run":
		send(w, 200, s.r.Trigger())
	case method == "GET" && action == "plan":
		send(w, 200, s.r.Plan())
	case method == "GET" && action == "positions":
		send(w, 200, s.r.Positions())
	case method == "GET" && action == "report":
		send(w, 200, s.r.Report())
	case method == "GET" && action == "trades":
		send(w, 200, s.r.Store.Trades(queryInt(q["n"], 50, 1, 1000)))
	case method == "GET" && action == "plans": // every plan: traded, cancelled, skipped (shadow)
		send(w, 200, s.r.Store.Plans(queryInt(q["n"], 200, 1, 5000)))
	case method == "GET" && action == "events":
		send(w, 200, s.events(q))
	default:
		send(w, 404, errBody{"error": "not found"})
	}
}

func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func (s *server) health() healthBody {
	h := healthBody{Runner: meta.Runner, Version: meta.Version, UptimeS: int64(time.Since(s.r.Started).Seconds()),
		Mode: s.r.Mode, Now: time.Now().UTC().Format("2006-01-02T15:04:05Z"), RSSMB: RSSMB(),
		StateBytes: s.r.Store.Size(), Errors: s.r.Store.Errors(), HealthExtra: s.r.HealthExtra()}
	h.Binance.OK, h.Binance.LatencyMS, h.Binance.Error = s.pinger.Get()
	return h
}

func (s *server) events(q map[string]string) any {
	evs := s.r.Store.Events()
	if since, ok := q["since"]; ok {
		sv := queryInt(since, 0, 0, 1<<62)
		out := evs[:0:0]
		for _, e := range evs {
			if e.Seq > sv {
				out = append(out, e)
			}
		}
		if len(out) > 200 {
			out = out[:200]
		}
		if out == nil {
			return evs[:0]
		}
		return out
	}
	n := queryInt(q["n"], 20, 1, 200)
	if n > len(evs) {
		n = len(evs)
	}
	return evs[len(evs)-n:]
}
