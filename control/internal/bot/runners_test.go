package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"control/internal/runners"
)

// A scalp runner (density_bounce) sends its own lines in `text`; they replace the carry layout, escaped.
func TestTextReport(t *testing.T) {
	var r runners.Report
	raw := `{"start":"2026-11-03","days":1,"capital0":100,"equity":100.5,"return_pct":0.5,"max_dd_pct":0.1,
		"text":["trades 3 · win 67%","  take  n 2 <x>"]}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	out := textReport("en", "bounce.density", r, "▁█")
	for _, want := range []string{"bounce.density", "100.50", "+0.50%", "trades 3 · win 67%", "&lt;x&gt;", "<code>▁█</code>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "Funding") || strings.Contains(out, "Pairs") {
		t.Errorf("carry fields must not appear: %q", out)
	}
}

func TestSpark(t *testing.T) {
	if got := spark([]float64{1, 2, 3, 4, 5, 6, 7, 8}, 30); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("spark = %q", got)
	}
	if got := spark([]float64{5, 5}, 30); utf8.RuneCountInString(got) != 2 {
		t.Errorf("flat spark = %q", got)
	}
	long := make([]float64, 100)
	if got := spark(long, 30); utf8.RuneCountInString(got) != 30 {
		t.Errorf("spark must keep the last 30 points, got %d", utf8.RuneCountInString(got))
	}
}

func TestFitPreKeepsTagsBalanced(t *testing.T) {
	rows := make([]string, 2000)
	for i := range rows {
		rows[i] = "XUSDT  <row> with & chars"
	}
	out := fitPre("<b>head</b>", rows, "\n<b>tail</b>")
	if utf8.RuneCountInString(out) > 4096 {
		t.Errorf("too long: %d", utf8.RuneCountInString(out))
	}
	if strings.Count(out, "<pre>") != 1 || strings.Count(out, "</pre>") != 1 || !strings.HasSuffix(out, "<b>tail</b>") {
		t.Errorf("unbalanced: ...%q", out[len(out)-60:])
	}
	if strings.Contains(out, "<row>") {
		t.Error("row text must be escaped")
	}
}
