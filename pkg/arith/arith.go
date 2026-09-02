// Package arith — pure-arithmetic expression parser/evaluator shared
// by pkg/rules (derived-quantity formulas, PRMT-021 §4.2). The
// grammar is intentionally minimal: `+ - * /`, parentheses, numeric
// literals, relative-point identifiers (`[a-z0-9.]+`), and unary
// `+`/`-`.
//
// This package is the single source of truth for arithmetic in CIOS
// (LOCKED L23 "recording rule/投影统一实现、禁止各处自算/口径漂移",
// applied here to the expression layer). One error sentinel —
// ErrUndefined (missing variable) — and a separate plain "division
// by zero" error are the only failures Eval can return; pkg/rules
// aliases ErrUndefined as its own ErrMissingInput so its callers'
// `errors.Is` checks keep working without rules importing alarm
// (and vice versa).
//
// pkg/alarm ALSO uses arith's concrete node types (NumLit, Ident,
// BinOp) for its own compiled AST: alarm keeps its own recursive-
// descent parser (PRMT-020 §4.2) and only delegates the evaluation
// semantics to arith (PRMT-025 R2, 方向 C). arith has no embeddable
// Scanner — the two consumers own their lexers because their
// grammars differ (rules=纯算术→float; alarm=算术+比较+逻辑→bool).
//
// Unary `-` and `+` lower at parse time to `BinOp{Op: '*', L: x, R:
// NumLit{V: -1}}` and the analogous shape for `+1`. The grammar
// permits it (`factor := ... | "-" factor | "+" factor`) and the
// side-effect is that `Refs()` on a `-(x)` is the same as `Refs()`
// on `x` — this is what callers rely on for input-quantity
// discovery.
package arith

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUndefined is the canonical "expression referenced a variable
// absent from the snapshot" sentinel. The two consumers (rules,
// alarm) re-export this as their own typed error so `errors.Is` is
// callable from outside the package while the wiring stays
// one-place (here).
var ErrUndefined = errors.New("arith: undefined variable")

// Node is the compiled form of one arith expression. Eval is pure;
// the same Node is safe to call from multiple goroutines. Refs
// returns the deduplicated, source-order list of relative-point
// identifiers the expression reads.
type Node interface {
	Eval(vars map[string]float64) (float64, error)
	Refs() []string
}

// NumLit is a numeric literal (float64). The PRMT-020/021 grammars
// don't need integer-specific paths since division can produce
// fractions.
type NumLit struct {
	V float64
}

func (n NumLit) Eval(map[string]float64) (float64, error) { return n.V, nil }
func (n NumLit) Refs() []string                           { return nil }

// Ident is a relative-point identifier. Eval returns ErrUndefined
// when the identifier is absent from the snapshot — NOT a wrapped
// error, so `errors.Is(err, arith.ErrUndefined)` resolves directly
// at the top level and via the consumers' alias sentinels.
type Ident struct {
	Name string
}

func (i Ident) Eval(snap map[string]float64) (float64, error) {
	v, ok := snap[i.Name]
	if !ok {
		return 0, ErrUndefined
	}
	return v, nil
}

func (i Ident) Refs() []string { return []string{i.Name} }

// BinOp covers + - * /. The Op field is one of '+', '-', '*', '/'.
// Division by zero surfaces as a plain error (NOT ErrUndefined) so
// the caller can log it as a data-quality issue distinct from a
// missing-data gap. This is the "数据缺失不视为满足" distinction
// from spec-003 §3 / PRMT-020 §4.2 / PRMT-021 §2-bis #3.
type BinOp struct {
	Op   byte // '+', '-', '*', '/'
	L, R Node
}

func (b BinOp) Eval(s map[string]float64) (float64, error) {
	l, err := b.L.Eval(s)
	if err != nil {
		return 0, err
	}
	r, err := b.R.Eval(s)
	if err != nil {
		return 0, err
	}
	switch b.Op {
	case '+':
		return l + r, nil
	case '-':
		return l - r, nil
	case '*':
		return l * r, nil
	case '/':
		if r == 0 {
			return 0, fmt.Errorf("arith: division by zero")
		}
		return l / r, nil
	}
	return 0, fmt.Errorf("arith: unknown binop %q", b.Op)
}

// Refs walks L then R in order, deduplicating while preserving the
// left-to-right first-seen order. Refs is called from the rules
// engine once per tick (or once at load time, for the same arith
// tree) so a re-walk is fine — no need to cache.
func (b BinOp) Refs() []string {
	out := b.L.Refs()
	seen := make(map[string]struct{}, len(out))
	for _, r := range out {
		seen[r] = struct{}{}
	}
	for _, r := range b.R.Refs() {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

// --- Parser ----------------------------------------------------------------
//
// Parse parses a complete pure-arithmetic expression (the
// rules.formula use case). The grammar (fixed):
//
//	arith  := term (("+"|"-") term)*
//	term   := factor (("*"|"/") factor)*
//	factor := number | ident | "(" arith ")" | "-" factor | "+" factor
//	ident  := [a-z0-9.]+      (lowercase-anchored, no leading digit)
//
// Whitespace between tokens is permitted; the parser skips it.
// Returns an error if the input is empty, has trailing tokens after
// a complete arith, or fails any factor rule (bad number, illegal
// identifier, unbalanced paren, runaway signs). The error message
// includes the position; no special error type is needed because
// the only sentinel consumers check is ErrUndefined (from Eval,
// not Parse).
//
// Parse is the only parser entry point — there is no embeddable
// Scanner face. Consumers that need richer grammar (alarm: cmp +
// and/or) drive their own byte-level parser and use the arith node
// types (NumLit, Ident, BinOp) when constructing their AST.

func Parse(s string) (Node, error) {
	p := &parser{src: s}
	p.trim()
	if p.eof() {
		return nil, fmt.Errorf("arith: empty expression")
	}
	node, err := p.parseArith()
	if err != nil {
		return nil, err
	}
	p.trim()
	if !p.eof() {
		return nil, fmt.Errorf("arith: unexpected trailing input at %d: %q", p.pos, p.remaining())
	}
	return node, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) eof() bool  { return p.pos >= len(p.src) }
func (p *parser) peek() byte { return p.src[p.pos] }
func (p *parser) remaining() string {
	if p.pos >= len(p.src) {
		return ""
	}
	return p.src[p.pos:]
}

func (p *parser) trim() {
	for !p.eof() {
		c := p.peek()
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		return
	}
}

// parseArith := term (("+"|"-") term)*
func (p *parser) parseArith() (Node, error) {
	lhs, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		p.trim()
		if p.eof() {
			return lhs, nil
		}
		c := p.peek()
		if c != '+' && c != '-' {
			return lhs, nil
		}
		// Disambiguate unary minus: at this point we've already
		// consumed an operand, so '-' here is always binary. A
		// stray unary would have been swallowed in parseFactor.
		p.pos++
		rhs, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		lhs = &BinOp{Op: c, L: lhs, R: rhs}
	}
}

// parseTerm := factor (("*"|"/") factor)*
func (p *parser) parseTerm() (Node, error) {
	lhs, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for {
		p.trim()
		if p.eof() {
			return lhs, nil
		}
		c := p.peek()
		if c != '*' && c != '/' {
			return lhs, nil
		}
		p.pos++
		rhs, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		lhs = &BinOp{Op: c, L: lhs, R: rhs}
	}
}

// parseFactor := ["+"|"-"] (number | ident | "(" arith ")")
//
// A single leading "+" is accepted (no-op) and a single leading "-"
// lowers to a multiplication by -1; both forms keep Refs() on the
// underlying ident unchanged. Multiple consecutive signs are a hard
// syntax error — the grammar allows at most one sign per factor,
// and accepting more would let a typo silently reduce to an
// arithmetic shape the author didn't intend.
func (p *parser) parseFactor() (Node, error) {
	p.trim()
	if p.eof() {
		return nil, fmt.Errorf("arith: unexpected end of expression at %d", p.pos)
	}
	c := p.peek()
	negate := false
	if c == '-' {
		negate = true
		p.pos++
		p.trim()
		if p.eof() {
			return nil, fmt.Errorf("arith: unary minus with no operand at %d", p.pos)
		}
		c = p.peek()
	} else if c == '+' {
		p.pos++
		p.trim()
		if p.eof() {
			return nil, fmt.Errorf("arith: unary plus with no operand at %d", p.pos)
		}
		c = p.peek()
	}
	var inner Node
	switch {
	case (c >= '0' && c <= '9') || c == '.':
		n, err := p.parseNumber()
		if err != nil {
			return nil, err
		}
		inner = n
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
		n, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		inner = n
	case c == '(':
		p.pos++ // consume '('
		n, err := p.parseArith()
		if err != nil {
			return nil, err
		}
		p.trim()
		if p.eof() || p.peek() != ')' {
			return nil, fmt.Errorf("arith: expected ')' at %d", p.pos)
		}
		p.pos++ // consume ')'
		inner = n
	default:
		return nil, fmt.Errorf("arith: unexpected character %q at %d", string(c), p.pos)
	}
	if negate {
		// Lower "-x" to "x * -1" so the AST is uniform for Refs().
		return &BinOp{Op: '*', L: inner, R: NumLit{V: -1}}, nil
	}
	return inner, nil
}

// parseNumber reads a numeric literal: digits with at most one '.',
// optional exponent. The form mirrors the rules.formula pre-PRMT-025
// behaviour; this is the shape existing derived-quantity YAML
// values use (spec-002 §9 examples are all plain floats).
func (p *parser) parseNumber() (Node, error) {
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.pos++
			continue
		}
		break
	}
	lit := p.src[start:p.pos]
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return nil, fmt.Errorf("arith: bad number %q at %d: %v", lit, start, err)
	}
	return NumLit{V: v}, nil
}

// parseIdent reads [a-z0-9_.]+ (PRMT-021 §4.2 relative-point name
// shape). Identifiers are lower-case only because that's what
// spec-002 §9 calls out; uppercase letters and '_' are accepted
// for symmetry with the pre-PRMT-025 rules.formula lexer, but
// rules' own validIdent rejects them — the cross-check happens
// there, not here. This keeps arith a leaf parser (no
// type-name coupling) while still rejecting the few shapes that
// are unambiguously garbage ("5m", "1a", "..supply", "").
func (p *parser) parseIdent() (Node, error) {
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' {
			p.pos++
			continue
		}
		break
	}
	name := p.src[start:p.pos]
	if name == "" {
		return nil, fmt.Errorf("arith: empty identifier at %d", start)
	}
	if !validIdent(name) {
		return nil, fmt.Errorf("arith: invalid identifier %q at %d", name, start)
	}
	return &Ident{Name: name}, nil
}

// validIdent enforces the ident shape: lowercase-anchored, no
// leading digit, no consecutive dots. Dot is the relative-point
// separator (loop.side.quantity style). Uppercase and '_' are
// accepted by the lexer for cross-package compatibility (the
// original rules.formula allowed them) but kept here as a
// surface-check; tighter policy is the caller's job.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}
