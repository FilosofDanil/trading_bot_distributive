// Package pyx reproduces the few Python / numpy behaviours the carry code depends on, so that the Go port
// computes the same numbers and prints the same texts: Python 3.12 sum() (Neumaier), numpy pairwise sum,
// float repr, "%.0%" style formatting, round(x, n) and the "Type: message" text of an exception.
package pyx

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// PySum is Python 3.12's built-in sum() over floats (compensated, Neumaier). An empty slice gives 0.
func PySum(xs []float64) float64 {
	var f, c float64
	for _, x := range xs {
		t := f + x
		if math.Abs(f) >= math.Abs(x) {
			c += (f - t) + x
		} else {
			c += (x - t) + f
		}
		f = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		f += c
	}
	return f
}

// NPSum is numpy's float64 add.reduce (pairwise summation), as used by pandas Series.sum() / mean().
func NPSum(a []float64) float64 {
	return 0.0 + pairwise(a)
}

func pairwise(a []float64) float64 {
	n := len(a)
	if n < 8 {
		r := 0.0
		for i := 0; i < n; i++ {
			r += a[i]
		}
		return r
	}
	if n <= 128 {
		var r [8]float64
		copy(r[:], a[:8])
		i := 8
		for ; i < n-(n%8); i += 8 {
			for j := 0; j < 8; j++ {
				r[j] += a[i+j]
			}
		}
		res := ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]))
		for ; i < n; i++ {
			res += a[i]
		}
		return res
	}
	n2 := n / 2
	n2 -= n2 % 8
	return pairwise(a[:n2]) + pairwise(a[n2:])
}

// Float is Python's repr(float): 100.0, 0.5, 1e-05, 1.5e+16.
func Float(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	if x == 0 {
		if math.Signbit(x) {
			return "-0.0"
		}
		return "0.0"
	}
	ax := math.Abs(x)
	if ax >= 1e-4 && ax < 1e16 {
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	return strconv.FormatFloat(x, 'e', -1, 64)
}

// Pct0 is Python's format(x, "+.0%") (sign=true) or format(x, ".0%").
func Pct0(x float64, sign bool) string {
	switch {
	case math.IsNaN(x):
		if sign {
			return "+nan%"
		}
		return "nan%"
	case math.IsInf(x, 1):
		if sign {
			return "+inf%"
		}
		return "inf%"
	case math.IsInf(x, -1):
		return "-inf%"
	}
	s := strconv.FormatFloat(x*100, 'f', 0, 64)
	if sign && !strings.HasPrefix(s, "-") {
		s = "+" + s
	}
	return s + "%"
}

// Round is Python's round(x, nd) for a float (correctly rounded decimal, ties to even).
func Round(x float64, nd int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', nd, 64), 64)
	return v
}

// Repr is Python's repr(str) for the texts used in error messages.
func Repr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case string(r) == q:
			b.WriteString(`\` + q)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

// Namer is implemented by errors that stand for a named Python exception class.
type Namer interface{ PyName() string }

// Describe is f"{type(exc).__name__}: {exc}".
func Describe(err error) string {
	if err == nil {
		return ""
	}
	name := "Error"
	var n Namer
	if errors.As(err, &n) {
		name = n.PyName()
	}
	return name + ": " + err.Error()
}

// Cut truncates to at most n characters (Python's s[:n]).
func Cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Truthy is Python truthiness for a decoded JSON value (json.Number / string / bool / nil / containers).
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case interface{ Float64() (float64, error) }: // json.Number
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Str is str(v) for a decoded JSON value (numbers decoded with UseNumber keep their text).
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case float64:
		return Float(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case interface{ String() string }:
		return x.String()
	}
	return ""
}
