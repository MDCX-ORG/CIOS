package rules

import (
	"errors"
	"math"
	"testing"
)

// mkDerived builds a Derived from a formula string. Used by
// Compute tests only — LoadDerived is the production loader and
// has its own dedicated test path (cmd/cios-rules).
func mkDerived(t *testing.T, name, formula string) Derived {
	t.Helper()
	f, err := ParseFormula(formula)
	if err != nil {
		t.Fatalf("ParseFormula(%q): %v", formula, err)
	}
	return Derived{
		Name:    name,
		Hosts:   []string{"cdu", "chiller"},
		Unit:    "celsius",
		Formula: f,
	}
}

// eqf is the compute_test-local copy of approxEq. formula_test.go
// defines its own approxEq in the same package; both are file-
// private to the _test.go compilation unit, so a second copy
// doesn't collide. (We could factor to a test helper file, but
// each file is small enough that the duplication costs less than
// the extra include.)
func eqf(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCompute_AddressJoin_WithLocPrefix(t *testing.T) {
	// spec-002 §9 example: a deltat on a cdu's primary water loop.
	// assetPath="sgp01.pod002.cdu000", locPrefix="fws", name="deltat"
	// → "sgp01.pod002.cdu000.fws.deltat".
	d := mkDerived(t, "deltat", "return.temp - supply.temp")
	pointPath, v, err := Compute(d, "sgp01.pod002.cdu000", "fws",
		map[string]float64{"return.temp": 34, "supply.temp": 30})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pointPath != "sgp01.pod002.cdu000.fws.deltat" {
		t.Fatalf("pointPath = %q, want %q", pointPath, "sgp01.pod002.cdu000.fws.deltat")
	}
	if !eqf(v, 4) {
		t.Fatalf("value = %v, want 4", v)
	}
}

func TestCompute_AddressJoin_EmptyLocPrefix(t *testing.T) {
	// No fold dimension: the formula reads at asset level.
	// pointPath collapses to assetPath.d.Name.
	d := mkDerived(t, "deltat", "return.temp - supply.temp")
	pointPath, _, err := Compute(d, "sgp01.pod002.cdu000", "",
		map[string]float64{"return.temp": 34, "supply.temp": 30})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pointPath != "sgp01.pod002.cdu000.deltat" {
		t.Fatalf("pointPath = %q, want %q", pointPath, "sgp01.pod002.cdu000.deltat")
	}
}

func TestCompute_DeltatExample(t *testing.T) {
	// The spec-002 §9 worked example. cdu primary loop:
	//   return.temp = 34 celsius, supply.temp = 30 celsius
	//   deltat = 4 celsius
	d := mkDerived(t, "deltat", "return.temp - supply.temp")
	_, v, err := Compute(d, "sgp01.pod002.cdu000", "fws",
		map[string]float64{"return.temp": 34, "supply.temp": 30})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !eqf(v, 4) {
		t.Fatalf("value = %v, want 4", v)
	}
}

func TestCompute_MissingInput_PropagatesErrMissingInput(t *testing.T) {
	d := mkDerived(t, "deltat", "return.temp - supply.temp")
	_, _, err := Compute(d, "sgp01.pod002.cdu000", "fws",
		map[string]float64{"return.temp": 34}) // supply.temp missing
	if !errors.Is(err, ErrMissingInput) {
		t.Fatalf("want ErrMissingInput, got %v", err)
	}
	// And the point path is not returned when there's an error.
	// (Empty string is fine — caller doesn't use it.)
}

func TestCompute_EvalError_PropagatesUnchanged(t *testing.T) {
	// Division by zero is a Formula-level error, NOT a missing-input
	// error. The caller logs and skips the bucket. We just need to
	// confirm Compute doesn't swallow or wrap it.
	d := mkDerived(t, "weird", "a / b")
	_, _, err := Compute(d, "sgp01.pod002.cdu000", "fws",
		map[string]float64{"a": 1, "b": 0})
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if errors.Is(err, ErrMissingInput) {
		t.Fatalf("div-by-zero must not be classified as ErrMissingInput: %v", err)
	}
}

func TestCompute_PureFunction_Stateless(t *testing.T) {
	// Same inputs → same outputs. Compute must not retain state
	// across calls (the engine.Observe-style "Instance" pattern
	// does NOT apply here — derived quantities are stateless
	// reductions, not state machines).
	d := mkDerived(t, "deltat", "return.temp - supply.temp")
	inputs := map[string]float64{"return.temp": 34, "supply.temp": 30}
	pp1, v1, err := Compute(d, "sgp01.pod002.cdu000", "fws", inputs)
	if err != nil {
		t.Fatalf("Compute #1: %v", err)
	}
	pp2, v2, err := Compute(d, "sgp01.pod002.cdu000", "fws", inputs)
	if err != nil {
		t.Fatalf("Compute #2: %v", err)
	}
	if pp1 != pp2 || v1 != v2 {
		t.Fatalf("Compute not pure: (%q, %v) vs (%q, %v)", pp1, v1, pp2, v2)
	}
}
