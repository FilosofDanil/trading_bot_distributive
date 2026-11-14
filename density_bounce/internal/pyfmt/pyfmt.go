// Package pyfmt reproduces the Python number formatting the event and log texts use, so a message that goes to
// Telegram reads the same as before the port: format(x, "g"), f"{x:+.4f}", f"{x:.2%}", f"{n:,}", round(x, n).
package pyfmt

import (
	"math"
	"strconv"
	"strings"
)

func special(x float64) (string, bool) {
	switch {
	case math.IsNaN(x):
		return "nan", true
	case math.IsInf(x, 1):
		return "inf", true
	case math.IsInf(x, -1):
		return "-inf", true
	}
	return "", false
}

// G is format(x, "g") / f"{x:g}": six significant digits, trailing zeros removed, exponent form "1e-05".
func G(x float64) string {
	if s, ok := special(x); ok {
		return s
	}
	return strconv.FormatFloat(x, 'g', 6, 64)
}

// F is f"{x:.{prec}f}".
func F(x float64, prec int) string {
	if s, ok := special(x); ok {
		return s
	}
	return strconv.FormatFloat(x, 'f', prec, 64)
}

// FS is f"{x:+.{prec}f}" (an explicit sign).
func FS(x float64, prec int) string {
	s := F(x, prec)
	if !strings.HasPrefix(s, "-") {
		s = "+" + s
	}
	return s
}

// Pct is f"{x:.{prec}%}".
func Pct(x float64, prec int) string {
	if s, ok := special(x); ok {
		return s + "%"
	}
	return F(x*100, prec) + "%"
}

// Comma is f"{n:,}".
func Comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// FComma is f"{x:,.{prec}f}".
func FComma(x float64, prec int) string {
	s := F(x, prec)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if hasFrac {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Round is Python's round(x, n): the decimal rounding of the exact binary value, half to even on an exact tie.
func Round(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	if err != nil {
		return x
	}
	return r
}

// Int is Python's round(x) (an int): half to even.
func Int(x float64) int64 { return int64(math.RoundToEven(x)) }

// ReprStrs is repr() of a list of strings: ['a', 'b'].
func ReprStrs(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = "'" + strings.ReplaceAll(x, "'", "\\'") + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
