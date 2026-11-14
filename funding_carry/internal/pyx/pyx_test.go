package pyx

import (
	"errors"
	"math"
	"testing"
)

type named struct{}

func (named) Error() string  { return "boom" }
func (named) PyName() string { return "ExchangeError" }

func TestHelpers(t *testing.T) {
	ten := make([]float64, 10)
	for i := range ten {
		ten[i] = 0.1
	}
	if PySum(ten) != 1.0 { // Python 3.12: sum([0.1] * 10) == 1.0
		t.Fatal(PySum(ten))
	}
	if Float(100) != "100.0" || Float(1e-5) != "1e-05" || Float(0.5) != "0.5" || Float(1.5e16) != "1.5e+16" {
		t.Fatal(Float(100), Float(1e-5), Float(1.5e16))
	}
	if Pct0(0.754, true) != "+75%" || Pct0(-0.2, true) != "-20%" || Pct0(math.Inf(1), true) != "+inf%" || Pct0(0.35, false) != "35%" {
		t.Fatal(Pct0(0.754, true), Pct0(0.35, false))
	}
	if Round(2.675, 2) != 2.67 || Round(1.005, 2) != 1.0 {
		t.Fatal(Round(2.675, 2), Round(1.005, 2))
	}
	if Repr("a'b") != `"a'b"` || Repr("ab") != "'ab'" {
		t.Fatal(Repr("a'b"))
	}
	if Describe(named{}) != "ExchangeError: boom" || Describe(errors.New("x")) != "Error: x" {
		t.Fatal(Describe(named{}))
	}
	if NPSum([]float64{1, 2, 3}) != 6 || Cut("héllo", 2) != "hé" {
		t.Fatal()
	}
	if !Truthy("a") || Truthy("") || Truthy(nil) || Str(nil) != "None" || Str(true) != "True" {
		t.Fatal()
	}
}
