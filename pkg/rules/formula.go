// Package rules — formula.go: pure-arithmetic formula parser/evaluator
// for derived quantities (spec-002 §9, LOCKED L23).
//
// The grammar is intentionally minimal — `+ - * /`, parens, numeric
// literals, and relative-point identifiers (e.g. "return.temp",
// "supply.temp"). No comparisons, no booleans, no and/or — derived
// quantities are pure arithmetic reductions of inputs, not
// predicates. The PRMT-020 alarm expr grammar is a strict superset
// of this one; both delegate their arithmetic layer to pkg/arith
// (PRMT-025 — single source of truth per LOCKED L23).
//
// Like alarm.Expr, Formula is a one-shot compilation: LoadDerived
// parses once and caches the AST. Compute is then a tight Eval on
// the (path × loop) bucket's input map.
package rules

import (
	"github.com/yurimeng/cios/pkg/arith"
)

// ErrMissingInput is returned by Eval when a referenced identifier
// is absent from the input snapshot. The caller (Compute / cmd loop)
// translates this into "skip this bucket" per PRMT-021 §2-bis #3.
//
// It is an alias of arith.ErrUndefined so that `errors.Is(err,
// rules.ErrMissingInput)` continues to resolve across the
// refactor — pkg/alarm's ErrMissingPoint is the same alias of the
// same sentinel, by design.
var ErrMissingInput = arith.ErrUndefined

// Formula is the compiled form of a derived-quantity arithmetic
// expression. Eval is pure (no I/O, no global state); the same
// Formula value is safe to call from multiple goroutines.
type Formula interface {
	// Eval resolves all Refs() against inputs and returns the
	// computed value. Missing identifiers → ErrMissingInput;
	// division by zero → a wrapped error so the caller can
	// distinguish from missing-data.
	Eval(inputs map[string]float64) (float64, error)
	// Refs returns the relative-point identifiers the formula
	// reads, in left-to-right AST order, deduplicated. The order
	// is used by cmd to drive the VM input-quantity discovery
	// step (refs[0]'s quantity is queried first, etc.).
	Refs() []string
}

// compiled wraps an arith.Node to satisfy the Formula interface.
// Refs are cached at parse time so the cmd loop (which reads them
// on every tick to drive the VM input-quantity discovery step)
// doesn't re-walk the tree.
type compiled struct {
	root arith.Node
	refs []string
}

func (c *compiled) Eval(s map[string]float64) (float64, error) { return c.root.Eval(s) }
func (c *compiled) Refs() []string                             { return c.refs }

// ParseFormula parses a derived-quantity arithmetic expression per
// PRMT-021 §4.2. The grammar (fixed) lives in pkg/arith; this
// function is a thin wrapper that pre-computes Refs() for the
// caller's hot path.
//
//	arith  := term (("+"|"-") term)*
//	term   := factor (("*"|"/") factor)*
//	factor := number | ident | "(" arith ")" | "-" factor | "+" factor
//	ident  := [a-z0-9.]+      (relative-point name, no leading digit)
//
// Unary `+`/`-` are accepted as part of factor (the factor rule
// allows a leading sign before a sub-expression). Whitespace
// between tokens is permitted; the parser skips it.
func ParseFormula(s string) (Formula, error) {
	node, err := arith.Parse(s)
	if err != nil {
		return nil, err
	}
	return &compiled{root: node, refs: node.Refs()}, nil
}
