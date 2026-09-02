package rules

import (
	"errors"
	"math"
	"testing"
)

// approxEq is a small float comparator that tolerates the last-bit
// rounding that IEEE-754 introduces for the basic ops. 1e-9 is more
// than enough for derived-quantity temperatures and powers.
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustFormula(t *testing.T, s string) Formula {
	t.Helper()
	f, err := ParseFormula(s)
	if err != nil {
		t.Fatalf("ParseFormula(%q): %v", s, err)
	}
	return f
}

func TestParseFormula_Arith(t *testing.T) {
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
		// + is a no-op (spec permits it)
		{"+5", nil, 5},
		{"2 + +3", nil, 5},
		// floating point
		{"0.5 + 0.25", nil, 0.75},
		{"1.5 * 2", nil, 3.0},
		// single ident
		{"x", map[string]float64{"x": 7}, 7},
		// dotted ident
		{"return.temp", map[string]float64{"return.temp": 34}, 34},
		// the spec-002 §9 example: deltat = return.temp - supply.temp
		{"return.temp - supply.temp", map[string]float64{"return.temp": 34, "supply.temp": 30}, 4},
		// multi-arg with reuse: (a+b) * a - a
		{"(a + b) * a - a", map[string]float64{"a": 3, "b": 4}, 3*7 - 3}, // 18
		// division is float
		{"1 / 4", nil, 0.25},
		// nested
		{"((1 + 2) * (3 + 4)) / 7", nil, 3},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := mustFormula(t, tc.expr).Eval(tc.inputs)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !approxEq(got, tc.want) {
				t.Fatalf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormula_Refs_OrderAndDedup(t *testing.T) {
	// First-seen order matters: cmd uses refs to discover input
	// quantities in left-to-right traversal. Repeated refs must
	// collapse to a single entry (compute bucket builder uses
	// the unique set).
	cases := []struct {
		expr string
		want []string
	}{
		{"return.temp - supply.temp", []string{"return.temp", "supply.temp"}},
		{"a + b + a + c", []string{"a", "b", "c"}},
		{"(x - y) * (x + y)", []string{"x", "y"}},
		{"42", nil},
		{"(a)", []string{"a"}},
		// unary neg must not introduce spurious refs
		{"-x + y", []string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got := mustFormula(t, tc.expr).Refs()
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

func TestFormula_MissingInput(t *testing.T) {
	// Single missing ref surfaces as ErrMissingInput — the caller
	// (cmd loop) translates this into "skip this bucket".
	f := mustFormula(t, "a - b")
	_, err := f.Eval(map[string]float64{"a": 1})
	if !errors.Is(err, ErrMissingInput) {
		t.Fatalf("want ErrMissingInput, got %v", err)
	}
	// Multiple refs, one missing: still ErrMissingInput, not a
	// silent zero.
	f2 := mustFormula(t, "(a + b) * c")
	_, err = f2.Eval(map[string]float64{"a": 1, "c": 3})
	if !errors.Is(err, ErrMissingInput) {
		t.Fatalf("want ErrMissingInput, got %v", err)
	}
}

func TestFormula_AllPresent(t *testing.T) {
	f := mustFormula(t, "a + b + c")
	v, err := f.Eval(map[string]float64{"a": 1, "b": 2, "c": 3})
	if err != nil || !approxEq(v, 6) {
		t.Fatalf("Eval = %v, err = %v; want 6/nil", v, err)
	}
}

func TestFormula_DivByZero(t *testing.T) {
	// Distinct from ErrMissingInput so the caller can log it as
	// a data-quality issue (a zero IS present, it's just wrong).
	f := mustFormula(t, "a / b")
	_, err := f.Eval(map[string]float64{"a": 1, "b": 0})
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if errors.Is(err, ErrMissingInput) {
		t.Fatalf("div-by-zero must NOT be classified as ErrMissingInput: %v", err)
	}
}

func TestParseFormula_Errors(t *testing.T) {
	cases := []struct {
		expr string
		// We don't pin the exact error string; we just want a
		// parse error. A successful parse here is a test failure.
	}{
		{""},                  // empty
		{"+"},                 // unary with no operand
		{"1 +"},               // arith tail missing term
		{"(1 + 2"},            // missing )
		{"1 + 2)"},            // stray )
		{"a + + + b"},         // runaway signs
		{"1.2.3"},             // malformed number
		{"1a"},                // number then ident (no operator)
		{"a..b"},              // empty segment in ident
		{"5m"},                // ident starting with digit
		{"return.temp -"},     // tail missing
		{"return..temp"},      // consecutive dots
		{"* 2"},               // leading op
		{"return.temp + * x"}, // op in the middle with no RHS
		{"中"},                 // non-ASCII byte — illegal in our subset
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			if _, err := ParseFormula(tc.expr); err == nil {
				t.Fatalf("ParseFormula(%q) returned nil error", tc.expr)
			}
		})
	}
}

func TestParseFormula_EmptyInput(t *testing.T) {
	// A formula with no refs is valid — it's just a constant
	// (e.g. "42" or "1 + 2"). Cmd treats it as a no-op data
	// reduction; this test pins the contract.
	f, err := ParseFormula("42")
	if err != nil {
		t.Fatalf("ParseFormula: %v", err)
	}
	if refs := f.Refs(); len(refs) != 0 {
		t.Fatalf("Refs = %v, want []", refs)
	}
	v, err := f.Eval(nil)
	if err != nil || !approxEq(v, 42) {
		t.Fatalf("Eval = %v, err = %v; want 42/nil", v, err)
	}
}
