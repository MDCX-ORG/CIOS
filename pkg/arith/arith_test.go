package arith

import (
	"errors"
	"math"
	"testing"
)

// approxEq is a small float comparator that tolerates the last-bit
// rounding that IEEE-754 introduces for the basic ops. 1e-9 is more
// than enough for arith temperatures and powers.
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustParse(t *testing.T, s string) Node {
	t.Helper()
	n, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return n
}

func TestParse_Arith(t *testing.T) {
	cases := []struct {
		expr   string
		inputs map[string]float64
		want   float64
	}{
		// trivial numerics
		{"3 + 4", nil, 7},
		{"10 - 3", nil, 7},
		{"6 * 7", nil, 42},
		{"20 / 4", nil, 5},
		// precedence: * before + / -
		{"1 + 2 * 3", nil, 7},
		{"2 * 3 + 1", nil, 7},
		{"10 - 2 * 3", nil, 4},
		{"(1 + 2) * 3", nil, 9},
		{"10 / (2 + 3)", nil, 2},
		// negative literals + unary
		{"-3 + 5", nil, 2},
		{"3 - -5", nil, 8},
		{"-(2 + 3) * 2", nil, -10},
		// + is a no-op
		{"+5", nil, 5},
		{"2 + +3", nil, 5},
		// floating point
		{"0.5 + 0.25", nil, 0.75},
		{"1.5 * 2", nil, 3.0},
		// single ident
		{"x", map[string]float64{"x": 7}, 7},
		// dotted ident
		{"return.temp", map[string]float64{"return.temp": 34}, 34},
		// the spec-002 §9 example
		{"return.temp - supply.temp", map[string]float64{"return.temp": 34, "supply.temp": 30}, 4},
		// multi-arg with reuse
		{"(a + b) * a - a", map[string]float64{"a": 3, "b": 4}, 3*7 - 3},
		// division is float
		{"1 / 4", nil, 0.25},
		// nested
		{"((1 + 2) * (3 + 4)) / 7", nil, 3},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := mustParse(t, tc.expr).Eval(tc.inputs)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !approxEq(got, tc.want) {
				t.Fatalf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefs_OrderAndDedup(t *testing.T) {
	// First-seen order matters: the engine uses Refs to drive input
	// discovery in left-to-right traversal. Repeated refs collapse
	// to a single entry. Unary minus/plus must not introduce
	// spurious refs.
	cases := []struct {
		expr string
		want []string
	}{
		{"return.temp - supply.temp", []string{"return.temp", "supply.temp"}},
		{"a + b + a + c", []string{"a", "b", "c"}},
		{"(x - y) * (x + y)", []string{"x", "y"}},
		{"42", nil},
		{"(a)", []string{"a"}},
		{"-x + y", []string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got := mustParse(t, tc.expr).Refs()
			if len(got) != len(tc.want) {
				t.Fatalf("Refs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Refs[%d] = %q, want %q (full %v vs %v)", i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}

func TestMissingVariable_ReturnsErrUndefined(t *testing.T) {
	// Single missing ref surfaces as ErrUndefined — the caller
	// translates this into "skip this bucket".
	n := mustParse(t, "a - b")
	_, err := n.Eval(map[string]float64{"a": 1})
	if !errors.Is(err, ErrUndefined) {
		t.Fatalf("want ErrUndefined, got %v", err)
	}
	// Multiple refs, one missing: still ErrUndefined, not a
	// silent zero.
	n2 := mustParse(t, "(a + b) * c")
	_, err = n2.Eval(map[string]float64{"a": 1, "c": 3})
	if !errors.Is(err, ErrUndefined) {
		t.Fatalf("want ErrUndefined, got %v", err)
	}
}

func TestDivByZero_NotErrUndefined(t *testing.T) {
	// Distinct from ErrUndefined so the caller can log it as
	// a data-quality issue (a zero IS present, it's just wrong).
	n := mustParse(t, "a / b")
	_, err := n.Eval(map[string]float64{"a": 1, "b": 0})
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if errors.Is(err, ErrUndefined) {
		t.Fatalf("div-by-zero must NOT be classified as ErrUndefined: %v", err)
	}
}

func TestParse_TrailingInputRejected(t *testing.T) {
	// A complete arith expression followed by non-arith bytes is
	// a syntax error (this is the contract rules.ParseFormula used
	// to enforce; pkg/arith.Parse now owns it).
	if _, err := Parse("1 + 2 x"); err == nil {
		t.Fatalf("Parse(\"1 + 2 x\") returned nil error; want trailing-input error")
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []string{
		"",                  // empty
		"+",                 // unary with no operand
		"-",                 // unary minus with no operand
		"1 +",               // arith tail missing term
		"1 -",               // arith tail missing term (after minus)
		"(1 + 2",            // missing )
		"1 + 2)",            // stray )
		"a + + + b",         // runaway signs
		"1.2.3",             // malformed number
		"1a",                // number then ident (no operator)
		"a..b",              // empty segment in ident
		"5m",                // ident starting with digit
		"return.temp -",     // tail missing
		"return..temp",      // consecutive dots
		"* 2",               // leading op
		"return.temp + * x", // op in the middle with no RHS
		"中",                 // non-ASCII byte — illegal in our subset
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			if _, err := Parse(expr); err == nil {
				t.Fatalf("Parse(%q) returned nil error", expr)
			}
		})
	}
}

func TestParse_EmptyInput(t *testing.T) {
	// A formula with no refs is valid — it's just a constant
	// (e.g. "42" or "1 + 2"). Callers treat it as a no-op data
	// reduction; this test pins the contract.
	n, err := Parse("42")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if refs := n.Refs(); len(refs) != 0 {
		t.Fatalf("Refs = %v, want []", refs)
	}
	v, err := n.Eval(nil)
	if err != nil || !approxEq(v, 42) {
		t.Fatalf("Eval = %v, err = %v; want 42/nil", v, err)
	}
}

func TestParse_NewlineAndTabSkipped(t *testing.T) {
	// Real-world rules will arrive indented; whitespace must not
	// break parsing.
	v, err := Parse("\t 1 +\n 2  ")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := v.Eval(nil)
	if err != nil || !approxEq(got, 3) {
		t.Fatalf("Eval = %v, err = %v; want 3/nil", got, err)
	}
}
