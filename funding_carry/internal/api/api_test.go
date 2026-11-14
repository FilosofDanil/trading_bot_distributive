package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"fundingcarry/internal/carry"
	"fundingcarry/internal/runner"
	"fundingcarry/internal/state"
)

const token = "t0ken"

func srv(t *testing.T) (string, *runner.Runner, string) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.New(path, 500)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(store, 5, "paper", nil)
	h := NewHandler(r, token)
	SetPing(h, func() (bool, int, *string) { return true, 5, nil })
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL, r, path
}

func call(t *testing.T, base, path, method, tok string, body any) (int, any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if tok != "" {
		req.Header.Set("X-Control-Token", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func TestHealthzIsOpenAndV1NeedsToken(t *testing.T) {
	base, _, _ := srv(t)
	if c, b := call(t, base, "/healthz", "GET", "", nil); c != 200 || obj(b)["ok"] != true {
		t.Fatal(c, b)
	}
	if c, _ := call(t, base, "/v1/health", "GET", "", nil); c != 401 {
		t.Fatal(c)
	}
	if c, _ := call(t, base, "/v1/health", "GET", "wrong", nil); c != 401 {
		t.Fatal(c)
	}
	c, h := call(t, base, "/v1/health", "GET", token, nil)
	if c != 200 || obj(h)["mode"] != "paper" || obj(obj(h)["binance"])["ok"] != true || obj(h)["runner"] != "funding" ||
		obj(h)["version"] != "0.2.0" || obj(h)["errors"] == nil {
		t.Fatal(c, h)
	}
}

func TestNoTokenConfiguredAnswers503(t *testing.T) {
	store, _ := state.New(filepath.Join(t.TempDir(), "s.json"), 500)
	ts := httptest.NewServer(NewHandler(runner.New(store, 5, "paper", nil), ""))
	defer ts.Close()
	if c, b := call(t, ts.URL, "/v1/health", "GET", "x", nil); c != 503 || obj(b)["error"] != "CONTROL_TOKEN is not set on the runner" {
		t.Fatal(c, b)
	}
}

func TestPauseResumePersistAndEmitEvents(t *testing.T) {
	base, _, path := srv(t)
	id := carry.StrategyID
	c, r := call(t, base, "/v1/strategies/"+id+"/pause", "POST", token, map[string]any{"by": 7, "reason": "tg"})
	s := obj(obj(r)["strategy"])
	if c != 200 || s["desired"] != "paused" || s["state"] != "paused" {
		t.Fatal(c, r)
	}
	raw, _ := os.ReadFile(path)
	var disk map[string]any
	json.Unmarshal(raw, &disk)
	if disk["desired"] != "paused" {
		t.Fatal(disk["desired"])
	}
	_, evs := call(t, base, "/v1/strategies/"+id+"/events?n=5", "GET", token, nil)
	last := obj(evs.([]any)[len(evs.([]any))-1])
	if last["notify"] != true || !bytes.Contains([]byte(last["msg"].(string)), []byte("paused")) || !bytes.Contains([]byte(last["msg"].(string)), []byte("by 7 (tg)")) {
		t.Fatal(last)
	}
	seq := last["seq"].(float64)
	call(t, base, "/v1/strategies/"+id+"/resume", "POST", token, map[string]any{"by": 7})
	_, newer := call(t, base, "/v1/strategies/"+id+"/events?since="+itoa(int(seq)), "GET", token, nil)
	n := newer.([]any)
	if len(n) != 1 || obj(n[0])["seq"].(float64) != seq+1 || !bytes.Contains([]byte(obj(n[0])["msg"].(string)), []byte("resumed")) {
		t.Fatal(newer)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestUnknownStrategyAndEmptyPlan(t *testing.T) {
	base, _, _ := srv(t)
	if c, b := call(t, base, "/v1/strategies/other.x/plan", "GET", token, nil); c != 404 || obj(b)["error"] != "unknown strategy 'other.x'; this runner has funding.carry" {
		t.Fatal(c, b)
	}
	if c, b := call(t, base, "/v1/strategies/"+carry.StrategyID+"/plan", "GET", token, nil); c != 404 || obj(b)["error"] != "no plan yet" {
		t.Fatal(c, b)
	}
	c, rep := call(t, base, "/v1/strategies/"+carry.StrategyID+"/report", "GET", token, nil)
	if c != 200 || obj(rep)["days"].(float64) != 0 || obj(rep)["equity"].(float64) != 500 || obj(rep)["curve"] == nil {
		t.Fatal(c, rep)
	}
	c, lst := call(t, base, "/v1/strategies", "GET", token, nil)
	l := lst.([]any)
	if c != 200 || obj(l[0])["id"] != carry.StrategyID || obj(l[0])["mode"] != "paper" || obj(l[0])["last_run_day"] != nil {
		t.Fatal(c, lst)
	}
	if c, _ := call(t, base, "/v1/strategies/"+carry.StrategyID+"/positions", "GET", token, nil); c != 200 {
		t.Fatal(c)
	}
	if c, b := call(t, base, "/v1/strategies/"+carry.StrategyID+"/flatten", "POST", token, map[string]any{}); c != 400 || obj(b)["error"] != `flatten needs {"confirm": true}` {
		t.Fatal(c, b)
	}
	if c, b := call(t, base, "/v1/strategies/"+carry.StrategyID+"/flatten", "POST", token, map[string]any{"confirm": true}); c != 409 {
		t.Fatal(c, b)
	}
	if c, _ := call(t, base, "/nothing", "GET", token, nil); c != 404 {
		t.Fatal(c)
	}
}
