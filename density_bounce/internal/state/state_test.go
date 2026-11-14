package state

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	s.Out = io.Discard
	return s
}

func readJSON(t *testing.T, p string) any {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func lines(t *testing.T, p string) []string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func copyFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/py_state.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "bounce_state.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sameJSON(t *testing.T, name string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	var gv any
	_ = json.Unmarshal(g, &gv)
	if !reflect.DeepEqual(gv, want) {
		t.Errorf("%s differs:\n go     %s\n python %v", name, g, want)
	}
}

// TestLoadsAndMigratesAPythonStateFile: the file the Python Store wrote (NaN token, "trades" array) loads, the
// trades move once into <stem>_trades.jsonl and leave the state, a second load does not duplicate them.
func TestLoadsAndMigratesAPythonStateFile(t *testing.T) {
	p := copyFixture(t)
	if raw, _ := os.ReadFile(p); !bytes.Contains(raw, []byte("NaN")) || !bytes.Contains(raw, []byte(`"trades":[`)) {
		t.Fatal("fixture lacks the NaN token or the trades array")
	}
	s := open(t, p)
	exp := readJSON(t, "testdata/py_expect.json").(map[string]any)
	d := s.Snapshot()
	sameJSON(t, "totals", d.Totals, exp["totals"])
	sameJSON(t, "days", d.Days, exp["days"])
	sameJSON(t, "events", d.Events, exp["events"])
	sameJSON(t, "stats", d.Stats, exp["stats"])
	if d.ConsecLosses != int(exp["consec_losses"].(float64)) || d.Seq != int(exp["seq"].(float64)) ||
		*d.StartDay != exp["start_day"].(string) {
		t.Errorf("consec %d seq %d start %v", d.ConsecLosses, d.Seq, d.StartDay)
	}
	if d.Desired != "paused" || d.Capital0 != 100 || d.Version != 1 {
		t.Errorf("desired %q capital %v", d.Desired, d.Capital0)
	}
	if day, lim := s.ConsecBlock(); day != "2023-11-14" || lim != 6 {
		t.Errorf("consec block %q %d", day, lim)
	}
	if sl := d.Slots["XUSDT"]; sl.State != "open" || sl.StopID != nil || len(sl.TPIDs) != 1 || sl.OID != "dbbe1" {
		t.Errorf("slot %+v", sl)
	}
	if s.LastError() != "boom" || len(s.Errors()) != 1 {
		t.Errorf("last error %q", s.LastError())
	}
	// the journal equals the Python list
	want := readJSON(t, "testdata/py_trades.json").([]any)
	var got []any
	for _, l := range lines(t, s.TradesPath()) {
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	// the NaN of the state file became null in the journal
	if len(got) != len(want) || len(got) != 8 {
		t.Fatalf("%d journal lines, python %d", len(got), len(want))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journal differs from the Python list")
	}
	// the state no longer has the trades; unknown keys survive
	if top := readJSON(t, p).(map[string]any); top["trades"] != nil {
		t.Error("state still has trades")
	} else if _, has := top["trades"]; has {
		t.Error("trades key present")
	}
	// in memory: the last 50 records and the guard ring
	lt := s.LastTrades(5)
	if len(lt) != 5 || lt[4].PID != "XUSDT-7" || lt[0].PID != "YUSDT-3" && lt[0].PID != "XUSDT-3" {
		t.Errorf("last trades %+v", lt)
	}
	if n, mean := s.GuardStats("0.4.5"); n != 8 || math.Abs(mean-(-0.05-0+0.12-0.03-0.04+0.2-0.01+0-0.07)/0.0153/8) > 0.01 {
		t.Errorf("guard %d %v", n, mean)
	}
	if n, _ := s.GuardStats("0.5.0"); n != 0 {
		t.Errorf("other version counted: %d", n)
	}
	if tl := s.Trades(3); len(tl) != 3 {
		t.Errorf("Trades(3) = %d", len(tl))
	}
	// a second load does not duplicate
	s2 := open(t, p)
	if n := len(lines(t, s2.TradesPath())); n != 8 {
		t.Errorf("second load: %d lines", n)
	}
	if len(s2.Trades(100)) != 8 {
		t.Error("Trades(100)")
	}
	// the Python trades newer than the journal's last record are appended, older ones are not
	raw, _ := os.ReadFile("testdata/py_state.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil { // rollback to Python's file: same 8 trades again
		t.Fatal(err)
	}
	s3 := open(t, p)
	if n := len(lines(t, s3.TradesPath())); n != 8 {
		t.Errorf("re-migration duplicated: %d lines", n)
	}
}

func TestAnUnreadableStateFileIsNeverOverwritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"version": 1, "desired": `), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p, 100); err == nil {
		t.Fatal("no error")
	}
	if raw, _ := os.ReadFile(p); string(raw) != `{"version": 1, "desired": ` {
		t.Error("the file was touched")
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p, 100); err == nil {
		t.Error("empty file accepted")
	}
}

func TestFreshStateLayoutAndRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "s.json")
	s := open(t, p)
	top := readJSON(t, p).(map[string]any)
	for _, k := range []string{"version", "desired", "capital0", "start_day", "days", "totals", "consec_losses", "slots",
		"stats", "events", "seq", "errors", "last_error", "consec_block_day", "consec_block_limit"} {
		if _, ok := top[k]; !ok {
			t.Errorf("key %q missing", k)
		}
	}
	if _, ok := top["trades"]; ok {
		t.Error("trades in the fresh state")
	}
	if top["events"] == nil || top["errors"] == nil || top["slots"] == nil || top["days"] == nil {
		t.Error("an empty collection is null")
	}
	s.Event("info", "a", false)
	s.AddStat("plans")
	s.SetDesired("paused")
	if err := s.Set("custom_key", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, p)
	if s2.Desired() != "paused" || len(s2.Events()) != 1 || s2.Stat("plans") != 1 { // AddStat is saved with the next save (SetDesired)
		t.Errorf("round trip: %q %d %d", s2.Desired(), len(s2.Events()), s2.Stat("plans"))
	}
	if top := readJSON(t, p).(map[string]any); top["custom_key"] == nil {
		t.Error("extra key lost")
	}
	if s.Size() == 0 {
		t.Error("size")
	}
}

func TestEventsKeepTheLast300AndErrorsTheLast5(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "s.json"))
	for i := 0; i < 310; i++ {
		s.Event("info", "m", false)
	}
	for i := 0; i < 7; i++ {
		s.Event("error", "e", true)
	}
	ev := s.Events()
	if len(ev) != MaxEvents || ev[len(ev)-1].Seq != 317 || len(s.Errors()) != 5 || s.LastError() != "e" {
		t.Errorf("events %d last seq %d errors %d", len(ev), ev[len(ev)-1].Seq, len(s.Errors()))
	}
}

func mkTrade(i int, pnl float64, reason string) Trade {
	r := pnl / 0.0153
	return Trade{Ver: "0.5.0", PID: "P", TsClose: 1_700_000_000 + float64(i)*86400/2, Sym: "XUSDT", Side: "long",
		PNL: pnl, Gross: pnl + 0.01, Fees: 0.01, Reason: reason, R: &r}
}

func TestRecordTradeAggregatesLikePython(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "s.json"))
	s.RecordTrade(mkTrade(0, -0.05, "stop"))
	s.RecordTrade(mkTrade(1, -0.02, "stop"))
	s.RecordTrade(mkTrade(2, 0.1, "take"))
	d := s.Snapshot()
	if d.Totals.N != 3 || d.Totals.Wins != 1 || d.Totals.PNL != 0.03 || d.ConsecLosses != 0 || *d.StartDay != "2023-11-14" {
		t.Errorf("totals %+v consec %d start %v", d.Totals, d.ConsecLosses, d.StartDay)
	}
	if g := d.Totals.Reasons["stop"]; g.N != 2 || g.Wins != 0 || g.PNL != -0.07 {
		t.Errorf("reason %+v", g)
	}
	if len(d.Days) != 2 { // trades 0 and 1 are 12 h apart, 2 is the next day
		t.Errorf("days %v", d.Days)
	}
	s.RecordTrade(mkTrade(3, -0.01, "stop"))
	if s.ConsecLosses() != 1 {
		t.Error("consec")
	}
	if n, _ := s.GuardStats("0.5.0"); n != 4 {
		t.Errorf("guard n %d", n)
	}
	var nilR = mkTrade(4, -0.01, "stop")
	nilR.R = nil
	s.RecordTrade(nilR)
	if n, _ := s.GuardStats("0.5.0"); n != 4 { // no R, no vote
		t.Errorf("guard n with a nil r: %d", n)
	}
	if lt := s.LastTrades(100); len(lt) != 5 {
		t.Errorf("last trades %d", len(lt))
	}
}

func TestMemoryKeepsOnly50FullTradesAnd1000ForTheGuard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	s := open(t, p)
	for i := 0; i < 1100; i++ {
		s.RecordTrade(mkTrade(i, -0.01, "stop"))
	}
	if len(s.recent) != KeepTrades || len(s.ring) != MaxTrades {
		t.Errorf("recent %d ring %d", len(s.recent), len(s.ring))
	}
	if got := len(s.Trades(5000)); got != 1000 { // the tail of the file, never more than 1000
		t.Errorf("Trades(5000) = %d", got)
	}
	last := s.Trades(2)
	var tr Trade
	_ = json.Unmarshal(last[1], &tr)
	if tr.TsClose != mkTrade(1099, 0, "").TsClose {
		t.Errorf("tail ends at %v", tr.TsClose)
	}
	if n := len(lines(t, s.TradesPath())); n != 1100 { // the file keeps everything
		t.Errorf("journal %d", n)
	}
	s2 := open(t, p) // a restart rebuilds the memory from the tail
	if len(s2.recent) != KeepTrades || len(s2.ring) != MaxTrades {
		t.Errorf("after restart: recent %d ring %d", len(s2.recent), len(s2.ring))
	}
}

func TestPlansTailCorruptLinesAndRotation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.json"))
	if len(s.Plans(10)) != 0 || filepath.Base(s.PlansPath()) != "state_plans.jsonl" {
		t.Fatal("fresh plans")
	}
	for i, id := range []string{"p0", "p1", "p2", "p3", "p4"} {
		s.Plan(map[string]any{"pid": id, "n": i})
	}
	f, _ := os.OpenFile(s.PlansPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("not json\n")
	f.Close()
	pids := func(rs []json.RawMessage) []string {
		var out []string
		for _, r := range rs {
			var m struct{ PID string }
			_ = json.Unmarshal(r, &m)
			out = append(out, m.PID)
		}
		return out
	}
	if got := pids(s.Plans(3)); !reflect.DeepEqual(got, []string{"p3", "p4"}) { // the last 3 lines, the broken one skipped
		t.Errorf("plans(3) = %v", got)
	}
	if len(s.Plans(100)) != 5 {
		t.Error("plans(100)")
	}
	if err := os.WriteFile(s.PlansPath(), bytes.Repeat([]byte("x"), 21_000_000), 0o644); err != nil { // over 20 MB: the file becomes *.old
		t.Fatal(err)
	}
	s.Plan(map[string]string{"pid": "new"})
	if _, err := os.Stat(oldPath(s.PlansPath())); err != nil {
		t.Error("no .old file")
	}
	if got := pids(s.Plans(200)); !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("after rotation %v", got)
	}
}

func TestTailReadsAreBoundedOnALargeFile(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.json"))
	var buf bytes.Buffer
	for i := 0; i < 200_000; i++ { // ~ 13 MB of plan records
		buf.WriteString(`{"pid":"p` + strings.Repeat("x", 50) + `","n":` + itoa(i) + "}\n")
	}
	if err := os.WriteFile(s.PlansPath(), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := s.Plans(200)
	if len(rs) != 200 || !strings.HasSuffix(string(rs[199]), `"n":199999}`) || !strings.HasSuffix(string(rs[0]), `"n":199800}`) {
		t.Errorf("tail: %d records, last %s", len(rs), rs[len(rs)-1])
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestJournalRotationOfTrades(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.json"))
	if err := os.WriteFile(s.TradesPath(), bytes.Repeat([]byte("x"), 21_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	s.RecordTrade(mkTrade(0, 0.1, "take"))
	if _, err := os.Stat(oldPath(s.TradesPath())); err != nil {
		t.Error("no .old file")
	}
	if len(s.Trades(10)) != 1 {
		t.Error("trades after rotation")
	}
}
