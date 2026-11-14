package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fundingcarry/internal/testutil"
)

// The file below was written by the Python runner (Store.save after paper.apply over days, plus live-style held entries).
func TestLoadsPythonStateAndRoundTripKeepsEveryKey(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/state_py.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "carry_live_state.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	d := s.Snapshot()
	if d.Desired != "paused" || d.Seq != 7 || d.Held.Len() < 2 {
		t.Fatalf("%+v", d)
	}
	lv, _ := d.Held.Get("LIVEUSDT")
	l2, _ := d.Held.Get("LIVE2USDT")
	if !lv.HasLive || !lv.HasBooked || lv.HasRealized || !l2.HasRealized || l2.Realized != 0.3317 || lv.Q != 49.9 {
		t.Fatalf("%+v %+v", lv, l2)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	var got any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if df := testutil.Diff(want, got, 0); df != "" {
		t.Fatal(df)
	}
	// the order of the held pairs survives too
	var a, b struct{ Held json.RawMessage }
	json.Unmarshal(raw, &a)
	json.Unmarshal(out, &b)
	ka, kb := keyOrder(t, a.Held), keyOrder(t, b.Held)
	if strings.Join(ka, ",") != strings.Join(kb, ",") {
		t.Fatalf("%v vs %v", ka, kb)
	}
}

func keyOrder(t *testing.T, raw json.RawMessage) []string {
	var h HeldMap
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h.Keys()
}

func TestNonFiniteTokensAndUnknownKeys(t *testing.T) {
	src := `{"version":1,"desired":"running","capital0":500.0,"capital":NaN,"held":{"A":{"n":Infinity,"funding":-Infinity}},` +
		`"msg":"NaN stays","future":{"a":[1,2]}}`
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := d.Held.Get("A")
	if d.Capital != 0 || h.N != 0 || h.Funding != 0 || d.Capital0 != 500 {
		t.Fatalf("%+v %+v", d, h)
	}
	b, err := json.Marshal(d)
	if err != nil || !strings.Contains(string(b), `"future":{"a":[1,2]}`) || !strings.Contains(string(b), `"events":[]`) ||
		!strings.Contains(string(b), `"plan":null`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestStoreEventsAndSafety(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "s.json")
	s, err := New(p, 500)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 310; i++ {
		lvl := "info"
		if i >= 300 {
			lvl = "error"
		}
		s.Event(lvl, "m", i%2 == 0)
	}
	d := s.Snapshot()
	if len(d.Events) != MaxEvents || d.Seq != 310 || len(d.Errors) != 5 || d.Events[0].Seq != 11 {
		t.Fatal(len(d.Events), d.Seq, len(d.Errors))
	}
	if s.Size() == 0 {
		t.Fatal("size")
	}
	if _, err := New(p, 1); err != nil { // reload works
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := New(bad, 1); err == nil {
		t.Fatal("an unreadable file must be an error")
	}
	if b, _ := os.ReadFile(bad); string(b) != "{not json" {
		t.Fatal("an unreadable file must never be overwritten")
	}
}
