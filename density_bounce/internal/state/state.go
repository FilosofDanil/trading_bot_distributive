// Package state is the runner state: one JSON file on the volume, written atomically (tmp file + rename) after
// every change. It holds the desired state (pause / resume), per-day and total aggregates (kept forever, a few
// hundred bytes a day), the events control polls, the free counters, and in live mode the open slots.
//
// The file layout is the one the Python runner wrote (bounce/state.py): this package loads it, NaN / Infinity
// tokens included, and never overwrites a file it cannot read (Open returns an error).
//
// Memory change against Python: the closed-trade journal is no longer in the state file. Every closed trade is
// appended to <state stem>_trades.jsonl (rotated to .old at 20 MB, like the plan journal <stem>_plans.jsonl).
// The Store keeps in memory only the last 50 full trade records and a compact ring of the last 1000 (ver, r)
// pairs for the live guard. A Python "trades" array found in the state file is migrated into the journal once
// and dropped from the state. Trades(n) and Plans(n) read the tail of the file, never the whole of it.
//
// One mutex guards the store; the exported methods lock, the unexported *Locked helpers do not and are never
// called with the lock held by a caller (no re-entrant locking).
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"densitybounce/internal/pyfmt"
	"densitybounce/internal/setup"
)

const (
	MaxEvents   = 300
	MaxTrades   = 1000 // the live guard's window (Python's MAX_TRADES) and the most Trades(n) returns
	KeepTrades  = 50   // full trade records kept in memory
	rotateBytes = 20e6
)

// NowISO is the ISO time of an epoch second (now when ts < 0), "2006-01-02T15:04:05Z".
func NowISO(ts float64) string {
	if ts < 0 {
		return time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	return time.Unix(int64(math.Floor(ts)), 0).UTC().Format("2006-01-02T15:04:05Z")
}

// UTCDay is the UTC day of an epoch second.
func UTCDay(ts float64) string {
	return time.Unix(int64(math.Floor(ts)), 0).UTC().Format("2006-01-02")
}

// Agg is a per-day or total aggregate.
type Agg struct {
	N     int     `json:"n"`
	Wins  int     `json:"wins"`
	PNL   float64 `json:"pnl"`
	Gross float64 `json:"gross"`
	Fees  float64 `json:"fees"`
	R     float64 `json:"r"`
}

// Group is the per-reason / per-side tally of the totals.
type Group struct {
	N    int     `json:"n"`
	PNL  float64 `json:"pnl"`
	Wins int     `json:"wins"`
}

// Totals are the aggregates since the record started.
type Totals struct {
	Agg
	Reasons map[string]Group `json:"reasons"`
	Sides   map[string]Group `json:"sides"`
}

// Event is one entry of the event log control polls.
type Event struct {
	Seq    int    `json:"seq"`
	TS     string `json:"ts"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Notify bool   `json:"notify"`
}

// ErrRec is one of the last 5 errors.
type ErrRec struct {
	TS  string `json:"ts"`
	Msg string `json:"msg"`
}

// SlotRec is what live mode persists about an open slot so leftovers can be closed after a restart.
type SlotRec struct {
	Side     string   `json:"side"`
	State    string   `json:"state"`
	Qty      float64  `json:"qty"`
	Entry    float64  `json:"entry"`
	OID      string   `json:"oid"`
	StopID   *string  `json:"stop_id"`
	TPIDs    []string `json:"tp_ids"`
	PlacedAt float64  `json:"placed_at"`
}

// Data is the state file (without the trade journal).
type Data struct {
	Version          int                `json:"version"`
	Desired          string             `json:"desired"`
	Capital0         float64            `json:"capital0"`
	StartDay         *string            `json:"start_day"`
	Days             map[string]Agg     `json:"days"`
	Totals           Totals             `json:"totals"`
	ConsecLosses     int                `json:"consec_losses"`
	Slots            map[string]SlotRec `json:"slots"`
	Stats            map[string]int     `json:"stats"`
	Events           []Event            `json:"events"`
	Seq              int                `json:"seq"`
	Errors           []ErrRec           `json:"errors"`
	LastError        *string            `json:"last_error"`
	ConsecBlockDay   *string            `json:"consec_block_day"`
	ConsecBlockLimit int                `json:"consec_block_limit"`
}

var knownKeys = map[string]bool{"version": true, "desired": true, "capital0": true, "start_day": true, "trades": true,
	"days": true, "totals": true, "consec_losses": true, "slots": true, "stats": true, "events": true, "seq": true,
	"errors": true, "last_error": true, "consec_block_day": true, "consec_block_limit": true}

// Fresh is Python's fresh(capital).
func Fresh(capital float64) Data {
	return Data{Version: 1, Desired: "running", Capital0: capital, Days: map[string]Agg{},
		Totals: Totals{Reasons: map[string]Group{}, Sides: map[string]Group{}},
		Slots:  map[string]SlotRec{},
		Stats:  map[string]int{"plans": 0, "placed": 0, "filled": 0, "cancelled": 0, "rejected": 0},
		Events: []Event{}, Errors: []ErrRec{}}
}

// TradePart is one take of a closed trade.
type TradePart struct {
	Name   string  `json:"name"`
	Px     float64 `json:"px"`
	Qty    float64 `json:"qty"`
	Done   bool    `json:"done"`
	FillPx float64 `json:"fill_px"`
}

// Trade is one closed trade of the journal (the Python dict, same keys and order).
type Trade struct {
	Ver      string         `json:"ver"`
	PID      string         `json:"pid"`
	TsOpen   float64        `json:"ts_open"`
	TsClose  float64        `json:"ts_close"`
	Sym      string         `json:"sym"`
	Side     string         `json:"side"`
	Entry    float64        `json:"entry"`
	Exit     float64        `json:"exit"`
	Qty      float64        `json:"qty"`
	Notional float64        `json:"notional"`
	Stop     float64        `json:"stop"`
	TP       float64        `json:"tp"`
	Reason   string         `json:"reason"`
	Gross    float64        `json:"gross"`
	Fees     float64        `json:"fees"`
	Rebate   float64        `json:"rebate"`
	PNL      float64        `json:"pnl"`
	R        *float64       `json:"r"`
	Slip     *float64       `json:"slip"`
	HoldS    float64        `json:"hold_s"`
	Features setup.Features `json:"features"`
	Parts    []TradePart    `json:"parts"`
}

type guardRec struct {
	ver string
	r   float64
	has bool
}

// Store is the thread-safe holder of the state.
type Store struct {
	mu     sync.Mutex
	path   string
	d      Data
	extra  map[string]json.RawMessage
	recent []Trade    // the last KeepTrades closed trades, oldest first
	ring   []guardRec // the last MaxTrades (ver, r), oldest first

	// Out receives the "[level] msg" echo of every event (stdout by default).
	Out io.Writer
	// Now is the clock of the event timestamps (time.Now by default).
	Now func() time.Time
}

// Open loads the state file, or creates a fresh one. A file that exists but cannot be read or parsed is an
// error: it is never overwritten.
func Open(path string, capital float64) (*Store, error) {
	s := &Store{path: path, Out: os.Stdout, Now: time.Now, extra: map[string]json.RawMessage{}}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := s.load(raw); err != nil {
			return nil, fmt.Errorf("state file %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		s.d = Fresh(capital)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return s, nil
}

// sanitizeJSON turns Python's NaN / Infinity / -Infinity tokens (outside strings) into null.
func sanitizeJSON(b []byte) []byte {
	if !bytes.Contains(b, []byte("NaN")) && !bytes.Contains(b, []byte("Infinity")) {
		return b
	}
	out := make([]byte, 0, len(b))
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(b) {
				i++
				out = append(out, b[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case bytes.HasPrefix(b[i:], []byte("NaN")):
			out = append(out, "null"...)
			i += 2
		case bytes.HasPrefix(b[i:], []byte("-Infinity")):
			out = append(out, "null"...)
			i += 8
		case bytes.HasPrefix(b[i:], []byte("Infinity")):
			out = append(out, "null"...)
			i += 7
		default:
			out = append(out, c)
		}
	}
	return out
}

func (s *Store) load(raw []byte) error {
	raw = sanitizeJSON(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	d := Fresh(0)
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if d.Days == nil {
		d.Days = map[string]Agg{}
	}
	if d.Totals.Reasons == nil {
		d.Totals.Reasons = map[string]Group{}
	}
	if d.Totals.Sides == nil {
		d.Totals.Sides = map[string]Group{}
	}
	if d.Slots == nil {
		d.Slots = map[string]SlotRec{}
	}
	if d.Stats == nil {
		d.Stats = map[string]int{}
	}
	if d.Events == nil {
		d.Events = []Event{}
	}
	if d.Errors == nil {
		d.Errors = []ErrRec{}
	}
	s.d = d
	for k, v := range top {
		if !knownKeys[k] {
			s.extra[k] = v
		}
	}
	trades, hadTrades := top["trades"]
	if hadTrades {
		if err := s.migrateTrades(trades); err != nil {
			return fmt.Errorf("migrating the trades to %s: %w", s.TradesPath(), err)
		}
	}
	s.loadTail()
	if hadTrades {
		if err := s.saveLocked(); err != nil { // the state without "trades"
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ journals

// TradesPath is <state stem>_trades.jsonl.
func (s *Store) TradesPath() string { return s.journalPath("_trades.jsonl") }

// PlansPath is <state stem>_plans.jsonl.
func (s *Store) PlansPath() string { return s.journalPath("_plans.jsonl") }

func (s *Store) journalPath(suffix string) string {
	ext := filepath.Ext(s.path)
	return strings.TrimSuffix(s.path, ext) + suffix
}

func oldPath(p string) string { return strings.TrimSuffix(p, filepath.Ext(p)) + ".old" }

// appendLine appends one line to a journal, rotating it to *.old first when it is over 20 MB.
func appendLine(p string, line []byte) error {
	if st, err := os.Stat(p); err == nil && float64(st.Size()) > rotateBytes {
		if err := os.Rename(p, oldPath(p)); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// tailLines returns the last n non-empty lines of a file, reading only its end.
func tailLines(p string, n int) ([][]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 << 10
	size := st.Size()
	pos := size
	var buf []byte
	for pos > 0 && bytes.Count(buf, []byte{'\n'}) < n+2 {
		step := int64(chunk)
		if pos < step {
			step = pos
		}
		pos -= step
		part := make([]byte, step)
		if _, err := f.ReadAt(part, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(part, buf...)
	}
	segs := bytes.Split(buf, []byte{'\n'})
	if pos > 0 && len(segs) > 0 {
		segs = segs[1:] // the first segment may be cut
	}
	var lines [][]byte
	for _, l := range segs {
		l = bytes.TrimSuffix(l, []byte{'\r'})
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func validLines(lines [][]byte) []json.RawMessage {
	out := []json.RawMessage{}
	for _, l := range lines {
		if json.Valid(l) {
			out = append(out, json.RawMessage(append([]byte(nil), l...)))
		}
	}
	return out
}

// Plan appends one plan record (traded, cancelled or skipped, with what the plain entry would have done) to the
// plan journal next to the state file: one JSON object a line, never rewritten; at 20 MB the file becomes *.old
// and a new one starts.
func (s *Store) Plan(rec any) {
	line, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(s.Out, "[warn] plan journal: %v\n", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendLine(s.PlansPath(), line); err != nil {
		fmt.Fprintf(s.Out, "[warn] plan journal: %v\n", err)
	}
}

// Plans are the last n plan records (the tail of the file is read, a corrupt line is skipped).
func (s *Store) Plans(n int) []json.RawMessage {
	if n <= 0 {
		return []json.RawMessage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := tailLines(s.PlansPath(), n)
	if err != nil {
		return []json.RawMessage{}
	}
	return validLines(lines)
}

// Trades are the last n closed trades of the journal (n up to MaxTrades), oldest first.
func (s *Store) Trades(n int) []json.RawMessage {
	n = min(n, MaxTrades)
	if n <= 0 {
		return []json.RawMessage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := tailLines(s.TradesPath(), n)
	if err != nil {
		return []json.RawMessage{}
	}
	return validLines(lines)
}

// migrateTrades writes a Python "trades" array into the journal. A journal that does not exist is created from
// a tmp file + rename; one that exists (an earlier, interrupted migration or a Go run) only receives the trades
// newer than its last record, so nothing is duplicated.
func (s *Store) migrateTrades(raw json.RawMessage) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	var lines [][]byte
	var closes []float64
	for _, it := range items {
		var c bytes.Buffer
		if json.Compact(&c, it) != nil {
			continue
		}
		var probe struct {
			TsClose float64 `json:"ts_close"`
		}
		if json.Unmarshal(it, &probe) != nil {
			continue
		}
		lines = append(lines, c.Bytes())
		closes = append(closes, probe.TsClose)
	}
	p := s.TradesPath()
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if len(lines) == 0 {
			return nil
		}
		tmp := p + ".tmp"
		var all bytes.Buffer
		for _, l := range lines {
			all.Write(l)
			all.WriteByte('\n')
		}
		if err := os.WriteFile(tmp, all.Bytes(), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, p)
	}
	last := math.Inf(-1)
	if tl, err := tailLines(p, 1); err == nil && len(tl) == 1 {
		var probe struct {
			TsClose float64 `json:"ts_close"`
		}
		if json.Unmarshal(tl[0], &probe) == nil {
			last = probe.TsClose
		}
	}
	for i, l := range lines {
		if closes[i] > last {
			if err := appendLine(p, l); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadTail fills the in-memory trade records from the end of the journal.
func (s *Store) loadTail() {
	s.recent, s.ring = nil, nil
	lines, err := tailLines(s.TradesPath(), MaxTrades)
	if err != nil {
		return
	}
	for _, l := range lines {
		var tr Trade
		if json.Unmarshal(l, &tr) != nil {
			continue
		}
		s.pushTradeLocked(tr)
	}
}

func (s *Store) pushTradeLocked(tr Trade) {
	if len(s.recent) == KeepTrades {
		copy(s.recent, s.recent[1:])
		s.recent = s.recent[:KeepTrades-1]
	}
	s.recent = append(s.recent, tr)
	g := guardRec{ver: tr.Ver}
	if tr.R != nil {
		g.r, g.has = *tr.R, true
	}
	if len(s.ring) == MaxTrades {
		copy(s.ring, s.ring[1:])
		s.ring = s.ring[:MaxTrades-1]
	}
	s.ring = append(s.ring, g)
}

// ------------------------------------------------------------------ save / snapshot

func (s *Store) marshalLocked() ([]byte, error) {
	b, err := json.Marshal(s.d)
	if err != nil || len(s.extra) == 0 {
		return b, err
	}
	b = bytes.TrimSuffix(b, []byte("}"))
	for k, v := range s.extra {
		kb, _ := json.Marshal(k)
		b = append(b, ',')
		b = append(b, kb...)
		b = append(b, ':')
		b = append(b, v...)
	}
	return append(b, '}'), nil
}

func (s *Store) saveLocked() error {
	b, err := s.marshalLocked()
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", strings.TrimSuffix(s.path, filepath.Ext(s.path)), os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Save persists the state (tmp file + rename).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveWarnLocked() {
	if err := s.saveLocked(); err != nil {
		fmt.Fprintf(s.Out, "[warn] state save: %v\n", err)
	}
}

// Path is the state file.
func (s *Store) Path() string { return s.path }

// Size is the size of the state file in bytes (0 when it cannot be read).
func (s *Store) Size() int64 {
	st, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func cloneData(d Data) Data {
	c := d
	c.Days = make(map[string]Agg, len(d.Days))
	for k, v := range d.Days {
		c.Days[k] = v
	}
	c.Totals.Reasons = make(map[string]Group, len(d.Totals.Reasons))
	for k, v := range d.Totals.Reasons {
		c.Totals.Reasons[k] = v
	}
	c.Totals.Sides = make(map[string]Group, len(d.Totals.Sides))
	for k, v := range d.Totals.Sides {
		c.Totals.Sides[k] = v
	}
	c.Slots = make(map[string]SlotRec, len(d.Slots))
	for k, v := range d.Slots {
		c.Slots[k] = v
	}
	c.Stats = make(map[string]int, len(d.Stats))
	for k, v := range d.Stats {
		c.Stats[k] = v
	}
	c.Events = append([]Event{}, d.Events...)
	c.Errors = append([]ErrRec{}, d.Errors...)
	return c
}

// Snapshot is a deep copy of the state (without the trade journal).
func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneData(s.d)
}

// Mutate applies fn to the state under the lock and saves it (tests and rare runner changes).
func (s *Store) Mutate(fn func(d *Data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
	s.saveWarnLocked()
}

// ------------------------------------------------------------------ events

// Event logs one event (the lists keep the newest MaxEvents / 5 errors), saves, and echoes "[level] msg".
func (s *Store) Event(level, msg string, notify bool) Event {
	s.mu.Lock()
	s.d.Seq++
	ev := Event{Seq: s.d.Seq, TS: s.Now().UTC().Format("2006-01-02T15:04:05Z"), Level: level, Msg: msg, Notify: notify}
	s.d.Events = append(s.d.Events, ev)
	if len(s.d.Events) > MaxEvents {
		s.d.Events = append([]Event{}, s.d.Events[len(s.d.Events)-MaxEvents:]...)
	}
	if level == "error" {
		s.d.Errors = append(s.d.Errors, ErrRec{TS: ev.TS, Msg: msg})
		if len(s.d.Errors) > 5 {
			s.d.Errors = append([]ErrRec{}, s.d.Errors[len(s.d.Errors)-5:]...)
		}
		m := msg
		s.d.LastError = &m
	}
	s.saveWarnLocked()
	out := s.Out
	s.mu.Unlock()
	if out != nil {
		fmt.Fprintf(out, "[%s] %s\n", level, msg)
	}
	return ev
}

// Events are the logged events, oldest first.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event{}, s.d.Events...)
}

// Errors are the last 5 errors.
func (s *Store) Errors() []ErrRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ErrRec{}, s.d.Errors...)
}

// LastError is the last error message ("" = none).
func (s *Store) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.LastError == nil {
		return ""
	}
	return *s.d.LastError
}

// ------------------------------------------------------------------ trades

// RecordTrade appends a closed trade to the journal, updates the day / total aggregates and saves.
func (s *Store) RecordTrade(tr Trade) {
	line, merr := json.Marshal(tr)
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &s.d
	day := UTCDay(tr.TsClose)
	if d.StartDay == nil || *d.StartDay == "" {
		d0 := day
		d.StartDay = &d0
	}
	if merr != nil {
		fmt.Fprintf(s.Out, "[warn] trade journal: %v\n", merr)
	} else if err := appendLine(s.TradesPath(), line); err != nil {
		fmt.Fprintf(s.Out, "[warn] trade journal: %v\n", err)
	}
	s.pushTradeLocked(tr)
	r := 0.0
	if tr.R != nil {
		r = *tr.R
	}
	add := func(a *Agg) {
		a.N++
		if tr.PNL > 0 {
			a.Wins++
		}
		a.PNL = pyfmt.Round(a.PNL+tr.PNL, 6)
		a.Gross = pyfmt.Round(a.Gross+tr.Gross, 6)
		a.Fees = pyfmt.Round(a.Fees+tr.Fees, 6)
		a.R = pyfmt.Round(a.R+r, 4)
	}
	da := d.Days[day]
	add(&da)
	d.Days[day] = da
	add(&d.Totals.Agg)
	for _, kv := range []struct {
		m   map[string]Group
		key string
	}{{d.Totals.Reasons, tr.Reason}, {d.Totals.Sides, tr.Side}} {
		g := kv.m[kv.key]
		g.N++
		if tr.PNL > 0 {
			g.Wins++
		}
		g.PNL = pyfmt.Round(g.PNL+tr.PNL, 6)
		kv.m[kv.key] = g
	}
	if tr.PNL < 0 {
		d.ConsecLosses++
	} else {
		d.ConsecLosses = 0
	}
	s.saveWarnLocked()
}

// LastTrades are the last n (at most KeepTrades) closed trades, oldest first.
func (s *Store) LastTrades(n int) []Trade {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.recent) {
		n = len(s.recent)
	}
	if n < 0 {
		n = 0
	}
	return append([]Trade{}, s.recent[len(s.recent)-n:]...)
}

// LastTradeClose is the ts_close of the newest closed trade (false when there is none).
func (s *Store) LastTradeClose() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recent) == 0 {
		return 0, false
	}
	return s.recent[len(s.recent)-1].TsClose, true
}

// GuardStats counts the trades of version ver with an R among the last MaxTrades, and their mean R.
func (s *Store) GuardStats(ver string) (n int, mean float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := 0.0
	for _, g := range s.ring {
		if g.ver == ver && g.has {
			sum += g.r
			n++
		}
	}
	if n > 0 {
		mean = sum / float64(n)
	}
	return
}

// SetGuardRing replaces the guard window (tests): the (ver, r) pairs oldest first; a nil r has no R.
func (s *Store) SetGuardRing(vers []string, rs []*float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = nil
	for i, v := range vers {
		g := guardRec{ver: v}
		if rs[i] != nil {
			g.r, g.has = *rs[i], true
		}
		s.ring = append(s.ring, g)
	}
	if len(s.ring) > MaxTrades {
		s.ring = s.ring[len(s.ring)-MaxTrades:]
	}
}

// ------------------------------------------------------------------ small accessors

// Desired is "running" or "paused".
func (s *Store) Desired() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Desired
}

// SetDesired persists the desired state.
func (s *Store) SetDesired(v string) {
	s.Mutate(func(d *Data) { d.Desired = v })
}

// Stat is a free counter.
func (s *Store) Stat(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Stats[key]
}

// AddStat increments a free counter (not saved until the next save, like Python).
func (s *Store) AddStat(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Stats[key]++
}

// Stats is a copy of the free counters.
func (s *Store) Stats() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.d.Stats))
	for k, v := range s.d.Stats {
		out[k] = v
	}
	return out
}

// TotalPNL is the total pnl since the record started.
func (s *Store) TotalPNL() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Totals.PNL
}

// Day is the aggregate of one UTC day (zero when there is none).
func (s *Store) Day(day string) (Agg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.d.Days[day]
	return a, ok
}

// ConsecLosses is the current streak of losses.
func (s *Store) ConsecLosses() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.ConsecLosses
}

// ConsecBlock is the day the streak limit blocked entries on and the limit that was in force ("" / 0 = none).
func (s *Store) ConsecBlock() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := ""
	if s.d.ConsecBlockDay != nil {
		day = *s.d.ConsecBlockDay
	}
	return day, s.d.ConsecBlockLimit
}

// BlockConsec records the block of a day after a streak of losses and resets the streak.
func (s *Store) BlockConsec(day string, limit int) {
	s.Mutate(func(d *Data) {
		d.ConsecBlockDay, d.ConsecBlockLimit, d.ConsecLosses = &day, limit, 0
	})
}

// SetLastError sets (or with "" clears) last_error.
func (s *Store) SetLastError(msg string) {
	s.Mutate(func(d *Data) {
		if msg == "" {
			d.LastError = nil
		} else {
			d.LastError = &msg
		}
	})
}

// Slots is a copy of the persisted live slots.
func (s *Store) Slots() map[string]SlotRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]SlotRec, len(s.d.Slots))
	for k, v := range s.d.Slots {
		out[k] = v
	}
	return out
}

// SetSlots replaces the persisted live slots.
func (s *Store) SetSlots(m map[string]SlotRec) {
	if m == nil {
		m = map[string]SlotRec{}
	}
	s.Mutate(func(d *Data) { d.Slots = m })
}

// Set stores one top-level key, like Python's store.set(key, value): known keys are typed, anything else is kept
// as an extra key of the file.
func (s *Store) Set(key string, value any) error {
	var err error
	s.Mutate(func(d *Data) {
		str := func() *string {
			switch v := value.(type) {
			case nil:
				return nil
			case string:
				return &v
			case *string:
				return v
			}
			err = fmt.Errorf("state.Set(%q): want a string, got %T", key, value)
			return nil
		}
		switch key {
		case "desired":
			if p := str(); p != nil {
				d.Desired = *p
			}
		case "capital0":
			if v, ok := value.(float64); ok {
				d.Capital0 = v
			} else {
				err = fmt.Errorf("state.Set(%q): want float64, got %T", key, value)
			}
		case "start_day":
			d.StartDay = str()
		case "last_error":
			d.LastError = str()
		case "consec_block_day":
			d.ConsecBlockDay = str()
		case "consec_block_limit":
			if v, ok := value.(int); ok {
				d.ConsecBlockLimit = v
			} else {
				err = fmt.Errorf("state.Set(%q): want int, got %T", key, value)
			}
		case "consec_losses":
			if v, ok := value.(int); ok {
				d.ConsecLosses = v
			} else {
				err = fmt.Errorf("state.Set(%q): want int, got %T", key, value)
			}
		case "slots":
			if v, ok := value.(map[string]SlotRec); ok {
				d.Slots = v
			} else {
				err = fmt.Errorf("state.Set(%q): want map[string]SlotRec, got %T", key, value)
			}
		default:
			b, merr := json.Marshal(value)
			if merr != nil {
				err = merr
				return
			}
			s.extra[key] = b
		}
	})
	return err
}
