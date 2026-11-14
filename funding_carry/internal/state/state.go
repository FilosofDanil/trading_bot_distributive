// Package state is the runner state: one JSON file on the Railway volume, written atomically after every
// change (carry/state.py). The JSON layout is exactly the one the Python runner wrote, so a file left on
// /data by the Python runner loads here (a live runner may hold real pairs).
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"fundingcarry/internal/carry"
)

const MaxEvents = 300

// ------------------------------------------------------------------ held pairs

// Held is one held pair. The paper runner writes n, ref, perp0, spot0, p_last, s_last, marked, opened,
// funding, basis, fees; the live runner adds q, q_spot, spot_cost, margin (together), realized and booked.
// The Has* flags remember which of those optional keys are in the file, so a round trip keeps every key.
type Held struct {
	N, Ref, Perp0, Spot0, PLast, SLast float64
	Marked, Opened                     int64
	Funding, Basis, Fees               float64

	Q, QSpot, SpotCost, Margin float64
	HasLive                    bool
	Realized                   float64
	HasRealized                bool
	Booked                     float64
	HasBooked                  bool
}

type heldJSON struct {
	N        float64  `json:"n"`
	Q        *float64 `json:"q,omitempty"`
	QSpot    *float64 `json:"q_spot,omitempty"`
	Ref      float64  `json:"ref"`
	Perp0    float64  `json:"perp0"`
	Spot0    float64  `json:"spot0"`
	SpotCost *float64 `json:"spot_cost,omitempty"`
	Margin   *float64 `json:"margin,omitempty"`
	Opened   int64    `json:"opened"`
	Marked   int64    `json:"marked"`
	Fees     float64  `json:"fees"`
	Funding  float64  `json:"funding"`
	Basis    float64  `json:"basis"`
	PLast    float64  `json:"p_last"`
	SLast    float64  `json:"s_last"`
	Realized *float64 `json:"realized,omitempty"`
	Booked   *float64 `json:"booked,omitempty"`
}

func ptr(x float64) *float64 { return &x }

func (h Held) MarshalJSON() ([]byte, error) {
	j := heldJSON{N: h.N, Ref: h.Ref, Perp0: h.Perp0, Spot0: h.Spot0, Opened: h.Opened, Marked: h.Marked,
		Fees: h.Fees, Funding: h.Funding, Basis: h.Basis, PLast: h.PLast, SLast: h.SLast}
	if h.HasLive {
		j.Q, j.QSpot, j.SpotCost, j.Margin = ptr(h.Q), ptr(h.QSpot), ptr(h.SpotCost), ptr(h.Margin)
	}
	if h.HasRealized {
		j.Realized = ptr(h.Realized)
	}
	if h.HasBooked {
		j.Booked = ptr(h.Booked)
	}
	return json.Marshal(j)
}

func (h *Held) UnmarshalJSON(b []byte) error {
	var j heldJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*h = Held{N: j.N, Ref: j.Ref, Perp0: j.Perp0, Spot0: j.Spot0, Opened: j.Opened, Marked: j.Marked,
		Fees: j.Fees, Funding: j.Funding, Basis: j.Basis, PLast: j.PLast, SLast: j.SLast}
	if j.Q != nil || j.QSpot != nil || j.SpotCost != nil || j.Margin != nil {
		h.HasLive = true
		h.Q, h.QSpot, h.SpotCost, h.Margin = deref(j.Q), deref(j.QSpot), deref(j.SpotCost), deref(j.Margin)
	}
	if j.Realized != nil {
		h.HasRealized, h.Realized = true, *j.Realized
	}
	if j.Booked != nil {
		h.HasBooked, h.Booked = true, *j.Booked
	}
	return nil
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// HeldMap is a Python dict of held pairs: it keeps the insertion order (which decides in which order pairs are
// closed and reported), also across a save and a load.
type HeldMap struct {
	keys []string
	m    map[string]*Held
}

func (h *HeldMap) Get(sym string) (*Held, bool) {
	v, ok := h.m[sym]
	return v, ok
}

func (h *HeldMap) Has(sym string) bool { _, ok := h.m[sym]; return ok }

func (h *HeldMap) Set(sym string, v *Held) {
	if h.m == nil {
		h.m = map[string]*Held{}
	}
	if _, ok := h.m[sym]; !ok {
		h.keys = append(h.keys, sym)
	}
	h.m[sym] = v
}

func (h *HeldMap) Delete(sym string) {
	if _, ok := h.m[sym]; !ok {
		return
	}
	delete(h.m, sym)
	for i, k := range h.keys {
		if k == sym {
			h.keys = append(h.keys[:i:i], h.keys[i+1:]...)
			break
		}
	}
}

// Keys are the symbols in insertion order (a copy).
func (h *HeldMap) Keys() []string { return append([]string{}, h.keys...) }

func (h *HeldMap) Len() int { return len(h.keys) }

// Clone is a deep copy.
func (h HeldMap) Clone() HeldMap {
	out := HeldMap{keys: append([]string(nil), h.keys...), m: make(map[string]*Held, len(h.m))}
	for k, v := range h.m {
		c := *v
		out.m[k] = &c
	}
	return out
}

func (h HeldMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range h.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(h.m[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (h *HeldMap) UnmarshalJSON(b []byte) error {
	*h = HeldMap{}
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("held: not an object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var v Held
		if err := dec.Decode(&v); err != nil {
			return err
		}
		h.Set(kt.(string), &v)
	}
	return nil
}

// ------------------------------------------------------------------ the state document

type DailyRow struct {
	Day     string   `json:"day"`
	Equity  float64  `json:"equity"`
	Funding float64  `json:"funding"`
	Basis   float64  `json:"basis"`
	Fees    float64  `json:"fees"`
	Book    []string `json:"book"`
}

type Closed struct {
	Symbol  string  `json:"symbol"`
	Opened  string  `json:"opened"`
	Closed  string  `json:"closed"`
	N       float64 `json:"n"`
	Funding float64 `json:"funding"`
	Basis   float64 `json:"basis"`
	Fees    float64 `json:"fees"`
	Pnl     float64 `json:"pnl"`
	Reason  string  `json:"reason"`
}

type Stats struct {
	PairsOpened int `json:"pairs_opened"`
	PairsClosed int `json:"pairs_closed"`
	PairStops   int `json:"pair_stops"`
}

type Event struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Notify bool   `json:"notify"`
}

type ErrRec struct {
	TS  string `json:"ts"`
	Msg string `json:"msg"`
}

// Data is the whole state document (state.fresh()).
type Data struct {
	Version    int                 `json:"version"`
	Desired    string              `json:"desired"`
	Capital0   float64             `json:"capital0"`
	Capital    float64             `json:"capital"`
	StartDay   *string             `json:"start_day"`
	Held       HeldMap             `json:"held"`
	Cool       map[string]int64    `json:"cool"`
	Tags       map[string][]string `json:"tags"`
	Daily      []DailyRow          `json:"daily"`
	Days       []string            `json:"days"`
	Closed     []Closed            `json:"closed"`
	Stats      Stats               `json:"stats"`
	Plan       json.RawMessage     `json:"plan"`
	Events     []Event             `json:"events"`
	Seq        int64               `json:"seq"`
	Errors     []ErrRec            `json:"errors"`
	LastRunDay *string             `json:"last_run_day"`
	LastRunAt  *string             `json:"last_run_at"`
	LastError  *string             `json:"last_error"`

	// Extra keeps top-level keys this version does not know, so a newer file is never trimmed.
	Extra map[string]json.RawMessage `json:"-"`
}

var knownKeys = map[string]bool{"version": true, "desired": true, "capital0": true, "capital": true, "start_day": true,
	"held": true, "cool": true, "tags": true, "daily": true, "days": true, "closed": true, "stats": true, "plan": true,
	"events": true, "seq": true, "errors": true, "last_run_day": true, "last_run_at": true, "last_error": true}

// Fresh is state.fresh(capital).
func Fresh(capital float64) *Data {
	d := &Data{Version: 1, Desired: "running", Capital0: capital, Capital: capital}
	d.normalise()
	return d
}

// normalise makes the lists and maps non-nil, so they are written as [] and {} (never null).
func (d *Data) normalise() {
	if d.Cool == nil {
		d.Cool = map[string]int64{}
	}
	if d.Tags == nil {
		d.Tags = map[string][]string{}
	}
	if d.Daily == nil {
		d.Daily = []DailyRow{}
	}
	if d.Days == nil {
		d.Days = []string{}
	}
	if d.Closed == nil {
		d.Closed = []Closed{}
	}
	if d.Events == nil {
		d.Events = []Event{}
	}
	if d.Errors == nil {
		d.Errors = []ErrRec{}
	}
	if d.Held.m == nil {
		d.Held = HeldMap{m: map[string]*Held{}}
	}
}

type dataAlias Data

func (d *Data) UnmarshalJSON(b []byte) error {
	var a dataAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	*d = Data(a)
	for k, v := range all {
		if !knownKeys[k] {
			if d.Extra == nil {
				d.Extra = map[string]json.RawMessage{}
			}
			d.Extra[k] = v
		}
	}
	d.normalise()
	return nil
}

func (d Data) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(dataAlias(d))
	if err != nil || len(d.Extra) == 0 {
		return b, err
	}
	b = bytes.TrimSuffix(b, []byte("}"))
	for k, v := range d.Extra {
		kb, _ := json.Marshal(k)
		b = append(b, ',')
		b = append(b, kb...)
		b = append(b, ':')
		b = append(b, v...)
	}
	return append(b, '}'), nil
}

// Clone is a deep copy (Store.Snapshot = copy.deepcopy).
func (d *Data) Clone() *Data {
	c := *d
	c.Held = d.Held.Clone()
	c.Cool = make(map[string]int64, len(d.Cool))
	for k, v := range d.Cool {
		c.Cool[k] = v
	}
	c.Tags = make(map[string][]string, len(d.Tags))
	for k, v := range d.Tags {
		c.Tags[k] = append([]string(nil), v...)
	}
	c.Daily = make([]DailyRow, len(d.Daily))
	for i, r := range d.Daily {
		r.Book = append([]string{}, r.Book...)
		c.Daily[i] = r
	}
	c.Days = append([]string{}, d.Days...)
	c.Closed = append([]Closed{}, d.Closed...)
	c.Events = append([]Event{}, d.Events...)
	c.Errors = append([]ErrRec{}, d.Errors...)
	c.Plan = append(json.RawMessage(nil), d.Plan...)
	c.StartDay, c.LastRunDay, c.LastRunAt, c.LastError = cp(d.StartDay), cp(d.LastRunDay), cp(d.LastRunAt), cp(d.LastError)
	if d.Extra != nil {
		c.Extra = make(map[string]json.RawMessage, len(d.Extra))
		for k, v := range d.Extra {
			c.Extra[k] = v
		}
	}
	return &c
}

func cp(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// Str makes a *string.
func Str(s string) *string { return &s }

// Parse decodes a state file. Python wrote NaN / Infinity / -Infinity tokens for non-finite floats: they are read as null.
func Parse(b []byte) (*Data, error) {
	d := &Data{}
	if err := json.Unmarshal(cleanNonFinite(b), d); err != nil {
		return nil, err
	}
	return d, nil
}

// cleanNonFinite replaces the bare tokens NaN, Infinity and -Infinity (outside strings) by null.
func cleanNonFinite(b []byte) []byte {
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

// ------------------------------------------------------------------ the store

// Store is the thread-safe holder of the state; every Update persists it (tmp file + rename).
type Store struct {
	Path string
	mu   sync.Mutex
	d    *Data
}

// New loads the state file, or creates a fresh one. A file that exists but cannot be read is an error and is never overwritten.
func New(path string, capital float64) (*Store, error) {
	s := &Store{Path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		d, perr := Parse(b)
		if perr != nil {
			return nil, fmt.Errorf("state file %s is not readable (left untouched): %w", path, perr)
		}
		s.d = d
	case os.IsNotExist(err):
		s.d = Fresh(capital)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := s.Save(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.Marshal(s.d)
	if err != nil {
		return fmt.Errorf("state not saved: %w", err)
	}
	ext := filepath.Ext(s.Path)
	tmp := strings.TrimSuffix(s.Path, ext) + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Save persists the state.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// Update runs fn on the live state under the lock, then saves. fn must not call other Store methods.
func (s *Store) Update(fn func(d *Data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.d)
	return s.saveLocked()
}

// View runs fn on the live state under the lock without saving (read-only use).
func (s *Store) View(fn func(d *Data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.d)
}

// Snapshot is a deep copy of the state.
func (s *Store) Snapshot() *Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Clone()
}

// Event appends an event (and an error to the last-5 list), saves, and prints the log line.
func (s *Store) Event(level, msg string, notify bool) Event {
	s.mu.Lock()
	s.d.Seq++
	ev := Event{Seq: s.d.Seq, TS: carry.NowISO(), Level: level, Msg: msg, Notify: notify}
	evs := append(append([]Event{}, s.d.Events...), ev)
	if len(evs) > MaxEvents {
		evs = evs[len(evs)-MaxEvents:]
	}
	s.d.Events = evs
	if level == "error" {
		errs := append(append([]ErrRec{}, s.d.Errors...), ErrRec{TS: ev.TS, Msg: msg})
		if len(errs) > 5 {
			errs = errs[len(errs)-5:]
		}
		s.d.Errors = errs
	}
	if err := s.saveLocked(); err != nil {
		fmt.Printf("[state] save failed: %v\n", err)
	}
	s.mu.Unlock()
	fmt.Printf("[%s] %s\n", level, msg)
	return ev
}

// Size is the state file size in bytes (0 when unreadable).
func (s *Store) Size() int64 {
	st, err := os.Stat(s.Path)
	if err != nil {
		return 0
	}
	return st.Size()
}
