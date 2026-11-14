package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"densitybounce/internal/runner"
	"densitybounce/internal/state"
	"densitybounce/internal/testkit"
)

const token = "t0ken"

type fakePing struct{}

func (fakePing) Get() (bool, int, *string) { return true, 7, nil }

type apiEnv struct {
	r    *runner.Runner
	call func(method, path string, body any, tok string) (int, string)
}

func newAPI(t *testing.T, tok string) *apiEnv {
	t.Helper()
	testkit.Silence()
	testkit.PinP(t)
	testkit.PinRisk(t)
	store, err := state.Open(t.TempDir()+"/s.json", 100)
	if err != nil {
		t.Fatal(err)
	}
	store.Out = io.Discard
	r := runner.New(store, "paper", "closes", "", nil) // the loop is not started: views run inline
	srv := httptest.NewServer(New(r, tok, fakePing{}))
	t.Cleanup(srv.Close)
	call := func(method, path string, body any, send string) (int, string) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		if send != "" {
			req.Header.Set("X-Control-Token", send)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return &apiEnv{r, call}
}

func decode(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v in %s", err, s)
	}
}

const sid = "/v1/strategies/bounce.density"

func TestAuthAndHealth(t *testing.T) {
	e := newAPI(t, token)
	if c, b := e.call("GET", "/healthz", nil, ""); c != 200 || b != `{"ok":true}` {
		t.Fatal(c, b)
	}
	if c, b := e.call("GET", "/v1/health", nil, "wrong"); c != 401 || b != `{"error":"unauthorized"}` {
		t.Fatal(c, b)
	}
	if c, _ := e.call("GET", "/v1/health", nil, ""); c != 401 {
		t.Fatal(c)
	}
	c, b := e.call("GET", "/v1/health", nil, token)
	var h struct {
		Runner  string `json:"runner"`
		Version string `json:"version"`
		Mode    string `json:"mode"`
		Binance struct {
			OK        bool `json:"ok"`
			LatencyMS int  `json:"latency_ms"`
		} `json:"binance"`
		Feed struct {
			Watching int `json:"watching"`
		} `json:"feed"`
		Universe int           `json:"universe_days"`
		Errors   []interface{} `json:"errors"`
		Busy     bool          `json:"busy"`
	}
	decode(t, b, &h)
	if c != 200 || h.Runner != "bounce" || h.Mode != "paper" || !h.Binance.OK || h.Binance.LatencyMS != 7 || h.Feed.Watching != 0 ||
		h.Errors == nil || h.Version != "0.5.0" {
		t.Fatal(c, b)
	}
	for _, k := range []string{`"uptime_s"`, `"now"`, `"rss_mb"`, `"state_bytes"`, `"busy":false`, `"universe_days":0`, `"max_lag_s"`} {
		if !strings.Contains(b, k) {
			t.Errorf("health lacks %s: %s", k, b)
		}
	}
	if c, b := e.call("GET", "/nothing", nil, token); c != 404 || b != `{"error":"not found"}` {
		t.Fatal(c, b)
	}
	if c, _ := e.call("POST", "/healthz", nil, ""); c != 404 {
		t.Fatal(c)
	}
}

func TestNoTokenAnswers503(t *testing.T) {
	e := newAPI(t, "")
	if c, b := e.call("GET", "/v1/strategies", nil, "anything"); c != 503 || !strings.Contains(b, "CONTROL_TOKEN is not set") {
		t.Fatal(c, b)
	}
	if c, _ := e.call("GET", "/healthz", nil, ""); c != 200 {
		t.Fatal(c)
	}
}

func TestStrategyBookIsEmptyForLossGuard(t *testing.T) {
	e := newAPI(t, token)
	c, b := e.call("GET", "/v1/strategies", nil, token)
	var ss []map[string]any
	decode(t, b, &ss)
	if c != 200 || ss[0]["id"] != "bounce.density" || fmt.Sprint(ss[0]["book"]) != "[]" || ss[0]["equity"] != 100.0 ||
		ss[0]["state"] != "running" || ss[0]["last_error"] != nil || ss[0]["next_run_at"] != nil {
		t.Fatal(c, b)
	}
	if !strings.Contains(b, `"book":[]`) || !strings.Contains(b, `"symbols":[]`) || !strings.Contains(b, `"working":[]`) {
		t.Fatalf("empty lists must be [] not null: %s", b)
	}
}

func TestPauseResumeEvents(t *testing.T) {
	e := newAPI(t, token)
	c, b := e.call("POST", sid+"/pause", map[string]any{"by": 1, "reason": "tg"}, token)
	var r struct {
		OK       bool           `json:"ok"`
		Strategy map[string]any `json:"strategy"`
	}
	decode(t, b, &r)
	if c != 200 || r.Strategy["desired"] != "paused" || e.r.Store.Desired() != "paused" {
		t.Fatal(c, b)
	}
	c, b = e.call("GET", sid+"/events?since=0", nil, token)
	var evs []state.Event
	decode(t, b, &evs)
	if c != 200 || !evs[len(evs)-1].Notify || !strings.Contains(evs[len(evs)-1].Msg, "paused") ||
		!strings.Contains(evs[len(evs)-1].Msg, "by 1 (tg)") {
		t.Fatal(c, b)
	}
	_, b = e.call("POST", sid+"/resume", map[string]any{"by": 1}, token)
	decode(t, b, &r)
	if r.Strategy["desired"] != "running" {
		t.Fatal(b)
	}
	// events: since filters, n takes the tail, oldest first
	_, b = e.call("GET", sid+"/events?n=1", nil, token)
	decode(t, b, &evs)
	if len(evs) != 1 || !strings.Contains(evs[0].Msg, "resumed") {
		t.Fatal(b)
	}
	_, b = e.call("GET", fmt.Sprintf("%s/events?since=%d", sid, evs[0].Seq), nil, token)
	if b != "[]" {
		t.Fatal(b)
	}
}

func TestPlanReportPositionsHaveText(t *testing.T) {
	e := newAPI(t, token)
	rr := 1.8
	e.r.Store.RecordTrade(state.Trade{TsClose: 1000, Side: "long", Sym: "XUSDT", Reason: "take", Gross: 0.05, Fees: 0.004,
		PNL: 0.046, R: &rr, Parts: []state.TradePart{}})
	c, b := e.call("GET", sid+"/plan", nil, token)
	var plan struct {
		Book []string `json:"book"`
		Text []string `json:"text"`
	}
	decode(t, b, &plan)
	has := false
	for _, l := range plan.Text {
		has = has || strings.Contains(l, "rules:")
	}
	if c != 200 || len(plan.Book) != 0 || !has {
		t.Fatal(c, b)
	}
	c, b = e.call("GET", sid+"/report", nil, token)
	var rep struct {
		Trades int      `json:"trades"`
		Equity float64  `json:"equity"`
		Text   []string `json:"text"`
	}
	decode(t, b, &rep)
	if c != 200 || rep.Trades != 1 || rep.Equity != 100.046 || !strings.HasPrefix(rep.Text[0], "trades 1") {
		t.Fatal(c, b)
	}
	if c, b := e.call("GET", sid+"/positions", nil, token); c != 200 || b != "[]" {
		t.Fatal(c, b)
	}
	c, b = e.call("GET", sid+"/trades?n=5", nil, token)
	var trades []map[string]any
	decode(t, b, &trades)
	if c != 200 || trades[0]["reason"] != "take" {
		t.Fatal(c, b)
	}
	if c, b := e.call("GET", sid+"/trades?n=abc", nil, token); c != 200 || !strings.Contains(b, "take") {
		t.Fatal(c, b)
	}
}

func TestPlansEndpointReturnsTheTailOfThePlanJournal(t *testing.T) {
	e := newAPI(t, token)
	if c, b := e.call("GET", sid+"/plans", nil, token); c != 200 || b != "[]" { // no journal yet
		t.Fatal(c, b)
	}
	for i := 0; i < 4; i++ {
		e.r.Store.Plan(map[string]any{"pid": fmt.Sprintf("XUSDT-%d", i), "ver": "0.4.5", "act": "skip:weak", "skip": "weak",
			"shadow": map[string]any{"hit": "nofill", "r": nil}})
	}
	pids := func(b string) string {
		var recs []map[string]any
		decode(t, b, &recs)
		var out []string
		for _, r := range recs {
			out = append(out, r["pid"].(string))
		}
		return strings.Join(out, " ")
	}
	c, b := e.call("GET", sid+"/plans", nil, token)
	if c != 200 || pids(b) != "XUSDT-0 XUSDT-1 XUSDT-2 XUSDT-3" || !strings.Contains(b, `"shadow":{"hit":"nofill","r":null}`) {
		t.Fatal(c, b)
	}
	if c, b = e.call("GET", sid+"/plans?n=2", nil, token); c != 200 || pids(b) != "XUSDT-2 XUSDT-3" {
		t.Fatal(c, b)
	}
	if c, _ := e.call("GET", sid+"/plans", nil, "wrong"); c != 401 {
		t.Fatal(c)
	}
	if c, b := e.call("GET", "/v1/strategies/funding.carry/plans", nil, token); c != 404 ||
		b != `{"error":"unknown strategy 'funding.carry'; this runner has bounce.density"}` {
		t.Fatal(c, b)
	}
}

func TestFlattenNeedsConfirmAndUnknownStrategy(t *testing.T) {
	e := newAPI(t, token)
	if c, b := e.call("POST", sid+"/flatten", map[string]any{}, token); c != 400 || b != `{"error":"flatten needs {\"confirm\": true}"}` {
		t.Fatal(c, b)
	}
	if c, _ := e.call("POST", sid+"/flatten", nil, token); c != 400 {
		t.Fatal(c)
	}
	if c, _ := e.call("POST", sid+"/flatten", map[string]any{"confirm": "yes"}, token); c != 400 {
		t.Fatal("confirm must be the boolean true")
	}
	if c, b := e.call("POST", sid+"/flatten", map[string]any{"confirm": true}, token); c != 409 || !strings.Contains(b, "runner loop is not running") { // loop not running
		t.Fatal(c, b)
	}
	if c, _ := e.call("GET", "/v1/strategies/funding.carry/plan", nil, token); c != 404 {
		t.Fatal(c)
	}
	if c, _ := e.call("GET", sid+"/nothing", nil, token); c != 404 {
		t.Fatal(c)
	}
	if c, b := e.call("POST", sid+"/run", nil, token); c != 200 || b != `{"ok":true,"started":true,"preview":true}` {
		t.Fatal(c, b)
	}
}

func TestRSSIsNullWithoutProc(t *testing.T) {
	if v := RSSMB(); v != nil && *v <= 0 {
		t.Fatalf("rss %v", *v)
	}
}
