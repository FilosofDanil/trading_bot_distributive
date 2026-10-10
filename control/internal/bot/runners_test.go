package bot

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
