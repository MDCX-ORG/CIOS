package alarm

import (
	"errors"
	"testing"
)

// ---- ParseExpr happy paths -------------------------------------------------

func TestParseExpr_SimpleCmp(t *testing.T) {
	cases := []struct {
		in   string
		refs []string
	}{
		{"fws.deltat < 4", []string{"fws.deltat"}},
		{"status == 3", []string{"status"}},
		{"fws.deltat >= 4.5", []string{"fws.deltat"}},
		{"a != b", []string{"a", "b"}},
		{"a==1", []string{"a"}},
	}
	for _, tc := range cases {
		e, err := ParseExpr(tc.in)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", tc.in, err)
		}
		got := e.Refs()
		if len(got) != len(tc.refs) {
			t.Fatalf("ParseExpr(%q): refs=%v want %v", tc.in, got, tc.refs)
		}
		for i := range got {
			if got[i] != tc.refs[i] {
				t.Fatalf("ParseExpr(%q): refs=%v want %v", tc.in, got, tc.refs)
			}
		}
	}
}

func TestParseExpr_Arith(t *testing.T) {
	e, err := ParseExpr("fws.supply.temp - fws.return.temp > 5")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	got, err := e.Eval(map[string]float64{
		"fws.supply.temp": 30,
		"fws.return.temp": 24,
	})
	if err != nil || !got {
		t.Fatalf("Eval = (%v,%v), want (true,nil)", got, err)
	}
	got, err = e.Eval(map[string]float64{
		"fws.supply.temp": 30,
		"fws.return.temp": 29,
	})
	if err != nil || got {
		t.Fatalf("Eval = (%v,%v), want (false,nil)", got, err)
	}
}

func TestParseExpr_UnaryAndParen(t *testing.T) {
	// -x + 1 < 2 ⇒ -(-5) + 1 = 6, not < 2 ⇒ false
	e, err := ParseExpr("-x + 1 < 2")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	got, err := e.Eval(map[string]float64{"x": -5})
	if err != nil || got {
		t.Fatalf("Eval(-5) = (%v,%v), want (false,nil)", got, err)
	}
	got, err = e.Eval(map[string]float64{"x": 1})
	if err != nil || !got {
		t.Fatalf("Eval(1) = (%v,%v), want (true,nil)", got, err)
	}

	// (a or b) and c
	e, err = ParseExpr("(a or b) and c")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	cases := []struct {
		snap map[string]float64
		want bool
	}{
		{map[string]float64{"a": 1, "b": 0, "c": 1}, true},
		{map[string]float64{"a": 0, "b": 1, "c": 1}, true},
		{map[string]float64{"a": 1, "b": 0, "c": 0}, false},
		{map[string]float64{"a": 0, "b": 0, "c": 1}, false},
	}
	for i, tc := range cases {
		got, err := e.Eval(tc.snap)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got != tc.want {
			t.Fatalf("case %d: got %v want %v", i, got, tc.want)
		}
	}
}

// ---- Eval truth-table coverage --------------------------------------------

func TestEval_AllOperators(t *testing.T) {
	cases := []struct {
		expr string
		x, y float64
		want bool
	}{
		{"x < y", 1, 2, true},
		{"x < y", 2, 1, false},
		{"x <= y", 2, 2, true},
		{"x <= y", 3, 2, false},
		{"x > y", 3, 2, true},
		{"x > y", 2, 3, false},
		{"x >= y", 2, 2, true},
		{"x >= y", 1, 2, false},
		{"x == y", 2, 2, true},
		{"x == y", 2, 3, false},
		{"x != y", 2, 3, true},
		{"x != y", 2, 2, false},
	}
	for _, tc := range cases {
		e, err := ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", tc.expr, err)
		}
		got, err := e.Eval(map[string]float64{"x": tc.x, "y": tc.y})
		if err != nil {
			t.Fatalf("Eval(%q): %v", tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("%s with x=%g y=%g: got %v want %v", tc.expr, tc.x, tc.y, got, tc.want)
		}
	}
}

// ---- Short-circuit --------------------------------------------------------

func TestEval_AndShortCircuit(t *testing.T) {
	// a == 1 and (b / zero == 0)  — if 'a' is false, 'b' must not be read.
	e, err := ParseExpr("a == 1 and b == 0")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	// Snapshot intentionally missing 'b'; if short-circuit works, Eval
	// never touches it and we get a clean false. If not, ErrMissingPoint.
	got, err := e.Eval(map[string]float64{"a": 0})
	if err != nil {
		t.Fatalf("Eval short-circuit returned err: %v", err)
	}
	if got {
		t.Fatalf("a=0, and-chain: want false, got true")
	}
}

func TestEval_OrShortCircuit(t *testing.T) {
	e, err := ParseExpr("a == 1 or b == 0")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	got, err := e.Eval(map[string]float64{"a": 1})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !got {
		t.Fatalf("a=1, or-chain: want true, got false")
	}
}

// ---- Missing point --------------------------------------------------------

func TestEval_MissingPoint(t *testing.T) {
	e, err := ParseExpr("fws.deltat < 4")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	_, err = e.Eval(map[string]float64{}) // fws.deltat absent
	if !errors.Is(err, ErrMissingPoint) {
		t.Fatalf("want ErrMissingPoint, got %v", err)
	}
}

func TestEval_MissingPoint_OneOfMany(t *testing.T) {
	e, err := ParseExpr("a > 1 and b > 1")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	_, err = e.Eval(map[string]float64{"a": 2}) // b missing
	if !errors.Is(err, ErrMissingPoint) {
		t.Fatalf("want ErrMissingPoint, got %v", err)
	}
}

// ---- Parse failures -------------------------------------------------------

func TestParseExpr_Errors(t *testing.T) {
	bad := []string{
		"",            // empty
		"(",           // unclosed paren
		"a +",         // dangling operand
		"a and",       // dangling logical
		"a and b and", // trailing and
		"a >",         // dangling cmp
		"a > b > c",   // chained cmp not allowed by grammar
		"a < < b",     // malformed
		"@bad",        // illegal character
		"a === b",     // not an op
		"and",         // bare keyword
		"or",          // bare keyword
		"1 + 2 3",     // trailing junk
		"(a",          // unclosed
	}
	for _, s := range bad {
		if _, err := ParseExpr(s); err == nil {
			t.Fatalf("ParseExpr(%q) accepted, want error", s)
		}
	}
}

// ---- Division by zero -----------------------------------------------------

func TestEval_DivByZero(t *testing.T) {
	e, err := ParseExpr("x / y > 0")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	_, err = e.Eval(map[string]float64{"x": 1, "y": 0})
	if err == nil {
		t.Fatalf("want division-by-zero error")
	}
	if errors.Is(err, ErrMissingPoint) {
		t.Fatalf("div-by-zero should not be reported as missing-point")
	}
}

// ---- Refs ordering + dedup -----------------------------------------------

func TestRefs_OrderAndDedup(t *testing.T) {
	e, err := ParseExpr("a > b and a < c and b > 0")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	got := e.Refs()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Refs len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Refs[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// ---- Realistic spec-003 example ------------------------------------------

func TestEval_SpecExample(t *testing.T) {
	// spec-003 §3: expr: "fws.deltat < 4"
	e, err := ParseExpr("fws.deltat < 4")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	for _, tc := range []struct {
		v    float64
		want bool
	}{
		{3.5, true},
		{4, false},
		{5, false},
	} {
		got, err := e.Eval(map[string]float64{"fws.deltat": tc.v})
		if err != nil {
			t.Fatalf("Eval(%g): %v", tc.v, err)
		}
		if got != tc.want {
			t.Fatalf("fws.deltat=%g: got %v want %v", tc.v, got, tc.want)
		}
	}
	// Refs should match what spec-003 says the CE subject is built
	// from (Refs()[0] joined with asset path; PRMT-020 §4.3).
	if r := e.Refs(); len(r) != 1 || r[0] != "fws.deltat" {
		t.Fatalf("Refs() = %v, want [fws.deltat]", r)
	}
}
