package i18n

import (
	"strings"
	"testing"
)

func TestEveryMessageHasBothLanguagesWithSameVerbs(t *testing.T) {
	for _, k := range Keys() {
		for _, l := range []string{EN, RU} {
			if !Has(l, k) {
				t.Errorf("%s: missing %s", k, l)
			}
		}
		if en, ru := verbs(messages[k][EN]), verbs(messages[k][RU]); en != ru {
			t.Errorf("%s: format verbs differ: en %q ru %q", k, en, ru)
		}
	}
}

func verbs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		j := i + 1
		for j < len(s) && strings.IndexByte("0123456789.+-", s[j]) >= 0 {
			j++
		}
		if j < len(s) {
			b.WriteString(s[i : j+1])
			b.WriteByte(' ')
		}
		i = j
	}
	return b.String()
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{"ru": RU, "ru-RU": RU, "uk": RU, "en-US": EN, "de": EN, "": EN}
	for in, want := range cases {
		if got := Normalize(in, EN); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if Normalize("de", RU) != RU {
		t.Error("unknown code must fall back to the default")
	}
}
