// Package alarm — expr.go: self-comparison expression engine for
// AlarmRule spec.expr (spec-003 §3, PRMT-020 §4.2). The grammar is
// deliberately small: arithmetic on relative point identifiers and
// numeric literals, comparison operators, and and/or. This is the
// "self-comparator" trade-off locked in L66 — it avoids pulling in
// PromQL because alarm evaluation must keep working when the TSDB
// (VictoriaMetrics) is degraded (spec-006 §1.1).
//
// Arithmetic leaves (number / ident / binop) are constructed using
// pkg/arith's concrete types (NumLit, Ident, BinOp) and evaluated
// via their Eval methods. This is the PRMT-025 R2 (方向 C) shape:
// alarm keeps its own recursive-descent parser (the two grammars
// differ — alarm has cmp + and/or, rules is pure arith), and only
// delegates the EVALUATION semantics (missing-variable, division-
// by-zero) to arith. There is no embeddable Scanner and the
// parseFactor / parseCmp logic is unchanged from PRMT-020 — only
// the constructed node types and their eval methods moved.
package alarm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yurimeng/cios/pkg/arith"
)

// ErrMissingPoint is returned by Expr.Eval when the snapshot is
// missing one of the identifiers the expression references. Callers
// (engine.Observe) treat this as "condition not satisfied" per
// spec-003 §3 ("data missing is not satisfaction").
//
// It is an alias of arith.ErrUndefined so `errors.Is(err,
// alarm.ErrMissingPoint)` resolves via the arith sentinel. The
// rules package uses the same alias of the same sentinel for
// ErrMissingInput.
var ErrMissingPoint = arith.ErrUndefined

// Expr is a compiled alarm expression. Eval evaluates against a
// snapshot of relative-point → value pairs. Refs returns the set of
// identifiers the expression reads, so the engine can detect missing
// data without trying to evaluate.
type Expr interface {
	Eval(snapshot map[string]float64) (bool, error)
	Refs() []string
}

// --- grammar (spec-003 §3, PRMT-020 §4.2) -----------------------------------
//
//   bool_expr := cmp (("and"|"or") cmp)*
//   cmp       := arith (("<"|"<="|">"|">="|"=="|"!=") arith)
//   arith     := term (("+"|"-") term)*
//   term      := factor (("*"|"/") factor)*
//   factor    := number | ident | "(" bool_expr | "(" arith ")"
//   ident     := [a-z0-9.]+   (e.g. "fws.deltat", "status")
//
// Precedence (low → high): or, and, cmp, +, -, *, /, unary negate,
// parenthesised sub-expression, atom (number / ident).

// --- AST --------------------------------------------------------------------
//
// cmp / logical / compiled form the alarm-specific AST. cmp.lhs /
// cmp.rhs must accept BOTH the arithmetic leaves (arith.NumLit /
// arith.Ident / arith.BinOp) AND the parenthesised-bool case where
// the operand is alarm's own logical (e.g. "(a or b) == 1"). The
// parenthesised-bool is a logical node, not an arith one, so a wider
// local interface is required — the union of "can be evaluated to
// (float64, error)" covers both. Engine's type assertions
// (extractCmpLiterals) still target the concrete arith types and
// work over the wider interface unchanged.
//
// Per PRMT-025 R2 (方向 C): arithmetic leaves are arith.* and their
// Eval semantics (missing-variable → ErrUndefined, division by zero
// → plain error) come from pkg/arith — alarm does not re-implement
// them. The logical node's Eval is a thin 1/0 wrapper that mirrors
// the top-level compiled.Eval short-circuit logic.

// evalNode is the union interface satisfied by arith.Node and by
// alarm's logical. cmp operands and the parser's arith-return
// functions all use it. Method signatures match arith.Node exactly
// so an arith.Node is implicitly assignable.
type evalNode interface {
	Eval(map[string]float64) (float64, error)
	Refs() []string
}

// cmp is the comparison node: lhs (cmp_op) rhs. Both sides are
// evalNodes — either an arith sub-expression or a parenthesised
// logical (e.g. "(a or b) == 1").
type cmp struct {
	op       string // "<", "<=", ">", ">=", "==", "!="
	lhs, rhs evalNode
}

// eval returns 1.0 for true, 0.0 for false. A missing-point error
// from the arith layer propagates as-is.
func (c cmp) eval(snap map[string]float64) (float64, error) {
	x, err := c.lhs.Eval(snap)
	if err != nil {
		return 0, err
	}
	y, err := c.rhs.Eval(snap)
	if err != nil {
		return 0, err
	}
	switch c.op {
	case "<":
		return boolToFloat(x < y), nil
	case "<=":
		return boolToFloat(x <= y), nil
	case ">":
		return boolToFloat(x > y), nil
	case ">=":
		return boolToFloat(x >= y), nil
	case "==":
		return boolToFloat(x == y), nil
	case "!=":
		return boolToFloat(x != y), nil
	}
	return 0, fmt.Errorf("alarm: unknown cmp op %q", c.op)
}

// logical is the AST for and/or. It is a flat slice rather than a
// binary tree so short-circuiting is straightforward: evaluate left
// to right, stop on the first result that determines the answer.
// kind is "and" or "or".
type logical struct {
	kind string
	xs   []cmp
}

// Eval implements evalNode so a parenthesised bool (e.g. "(a or b)")
// can appear as a cmp operand. Returns 1.0 / 0.0 and short-circuits
// like the top-level compiled.Eval; missing-point errors propagate
// as-is (translated to ErrMissingPoint via the arith sentinel
// aliasing).
func (l logical) Eval(snap map[string]float64) (float64, error) {
	for _, x := range l.xs {
		v, err := x.eval(snap)
		if err != nil {
			return 0, err
		}
		if l.kind == "or" && v != 0 {
			return 1, nil
		}
		if l.kind == "and" && v == 0 {
			return 0, nil
		}
	}
	if l.kind == "and" {
		return 1, nil
	}
	return 0, nil
}

// Refs implements evalNode. Used by collectArithRefs when a cmp
// operand is a parenthesised logical (e.g. "(a or b) == 1" — the
// outer cmp's lhs is a logical, and Refs() must walk its
// sub-cmps).
func (l logical) Refs() []string {
	seen := map[string]struct{}{}
	var out []string
	for i := range l.xs {
		collectArithRefs(l.xs[i].lhs, seen, &out)
		collectArithRefs(l.xs[i].rhs, seen, &out)
	}
	return out
}

type compiled struct {
	root    logical
	refs    []string
	refsSet map[string]struct{}
}

// --- Refs ------------------------------------------------------------------

// Refs returns the deduplicated, source-order list of relative point
// identifiers the expression reads. Used by the engine for missing-
// point detection and for picking the CE subject prefix (PRMT-020
// §4.3 Event.PointPath).
func (c *compiled) Refs() []string { return append([]string(nil), c.refs...) }

// --- Eval ------------------------------------------------------------------

// Eval returns (true, nil) when the expression evaluates to true,
// (false, nil) when it evaluates to false, and (_, ErrMissingPoint)
// when a referenced point is absent. Any other error is a real
// evaluation failure (e.g. division by zero) — callers should treat
// it like a parse error: skip this sample for this rule.
func (c *compiled) Eval(snap map[string]float64) (bool, error) {
	for _, x := range c.root.xs {
		v, err := x.eval(snap)
		if err != nil {
			return false, err
		}
		// Short-circuit.
		if c.root.kind == "or" && v != 0 {
			return true, nil
		}
		if c.root.kind == "and" && v == 0 {
			return false, nil
		}
	}
	// All clauses evaluated.
	if c.root.kind == "and" {
		return true, nil
	}
	// "or" reached end without any true → false.
	return false, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// --- Parser ----------------------------------------------------------------

// ParseExpr compiles an expression string. The grammar is fixed by
// PRMT-020 §4.2; the only freedom is whitespace, which is ignored.
func ParseExpr(s string) (Expr, error) {
	p := &parser{src: s}
	p.trim()
	if p.eof() {
		return nil, fmt.Errorf("alarm: empty expression")
	}
	root, err := p.parseBool()
	if err != nil {
		return nil, err
	}
	p.trim()
	if !p.eof() {
		return nil, fmt.Errorf("alarm: unexpected trailing input at %d: %q", p.pos, p.remaining())
	}
	// Collect refs in source order, dedup preserving first occurrence.
	seen := map[string]struct{}{}
	var refs []string
	collectRefs(&root, seen, &refs)
	return &compiled{root: root, refs: refs, refsSet: seen}, nil
}

// collectRefs walks a logical (and its embedded cmps) and gathers
// the ident Refs from each sub-tree in left-to-right order. The
// sub-trees are evalNodes — arith sub-expressions (NumLit/Ident/
// BinOp) or nested parenthesised logicals. collectArithRefs is
// called on the cmp sides and recurses through whichever type it
// finds; logicals forward through collectArithRefs → case logical
// → walk the inner cmp pair.
func collectRefs(l *logical, seen map[string]struct{}, out *[]string) {
	for i := range l.xs {
		collectArithRefs(l.xs[i].lhs, seen, out)
		collectArithRefs(l.xs[i].rhs, seen, out)
	}
}

func collectArithRefs(n evalNode, seen map[string]struct{}, out *[]string) {
	if n == nil {
		return
	}
	switch x := n.(type) {
	case *arith.Ident:
		if _, ok := seen[x.Name]; !ok {
			seen[x.Name] = struct{}{}
			*out = append(*out, x.Name)
		}
	case arith.NumLit:
		// no refs
	case *arith.BinOp:
		collectArithRefs(x.L, seen, out)
		collectArithRefs(x.R, seen, out)
	case logical:
		for i := range x.xs {
			collectArithRefs(x.xs[i].lhs, seen, out)
			collectArithRefs(x.xs[i].rhs, seen, out)
		}
	}
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

func (p *parser) consume(want byte) error {
	p.trim()
	if p.eof() || p.peek() != want {
		return fmt.Errorf("alarm: expected %q at %d, got %q", want, p.pos, p.remaining())
	}
	p.pos++
	return nil
}

// parseBool := cmp (("and"|"or") cmp)*
func (p *parser) parseBool() (logical, error) {
	first, err := p.parseCmp()
	if err != nil {
		return logical{}, err
	}
	out := logical{kind: "and", xs: []cmp{first}}
	for {
		p.trim()
		if p.eof() {
			return out, nil
		}
		// and / or are right next to a comparison; if next token is
		// something else (e.g. ')') we stop.
		save := p.pos
		word, ok := p.tryKeyword()
		if !ok {
			p.pos = save
			return out, nil
		}
		switch word {
		case "and":
			out.kind = "and"
		case "or":
			out.kind = "or"
		default:
			p.pos = save
			return out, nil
		}
		next, err := p.parseCmp()
		if err != nil {
			return logical{}, err
		}
		out.xs = append(out.xs, next)
	}
}

// tryKeyword reads [a-z]+ and returns it; resets p.pos on mismatch.
func (p *parser) tryKeyword() (string, bool) {
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if c >= 'a' && c <= 'z' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", false
	}
	return p.src[start:p.pos], true
}

// parseCmp := arith (("<"|"<="|">"|">="|"=="|"!=") arith)?
// Cmp is optional — a bare arithmetic expression is allowed and
// acts as a numeric identity (non-zero = true, zero = false) for
// "expr: 1" style simple always-true rules. Spec-003 §3 doesn't
// explicitly cover this but it's the natural completion of the
// grammar and matches how "expr: status == 3" is written today.
func (p *parser) parseCmp() (cmp, error) {
	lhs, err := p.parseArith()
	if err != nil {
		return cmp{}, err
	}
	p.trim()
	if p.eof() {
		return cmp{op: "!=", lhs: lhs, rhs: arith.NumLit{V: 0}}, nil
	}
	c := p.peek()
	var op string
	switch c {
	case '<':
		op = "<"
		p.pos++
		if !p.eof() && p.peek() == '=' {
			op = "<="
			p.pos++
		}
	case '>':
		op = ">"
		p.pos++
		if !p.eof() && p.peek() == '=' {
			op = ">="
			p.pos++
		}
	case '=':
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == '=' {
			op = "=="
			p.pos += 2
		}
	case '!':
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == '=' {
			op = "!="
			p.pos += 2
		}
	}
	if op == "" {
		// No cmp operator → treat as "(lhs != 0)" predicate.
		return cmp{op: "!=", lhs: lhs, rhs: arith.NumLit{V: 0}}, nil
	}
	rhs, err := p.parseArith()
	if err != nil {
		return cmp{}, err
	}
	return cmp{op: op, lhs: lhs, rhs: rhs}, nil
}

// parseArith := term (("+"|"-") term)*
func (p *parser) parseArith() (evalNode, error) {
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
		// Disambiguate unary minus: if '-' follows '(' or an operator,
		// it's unary. At this point we've already consumed an operand,
		// so '-' here is always binary.
		p.pos++
		rhs, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		lhs = &arith.BinOp{Op: c, L: lhs, R: rhs}
	}
}

// parseTerm := factor (("*"|"/") factor)*
func (p *parser) parseTerm() (evalNode, error) {
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
		lhs = &arith.BinOp{Op: c, L: lhs, R: rhs}
	}
}

// parseFactor := number | ident | "(" (bool | arith) ")"
// Unary minus is handled here so "fws.deltat - 1" doesn't have to
// disambiguate vs "-fws.deltat".
func (p *parser) parseFactor() (evalNode, error) {
	p.trim()
	if p.eof() {
		return nil, fmt.Errorf("alarm: unexpected end of expression")
	}
	c := p.peek()
	if c == '(' {
		p.pos++
		p.trim()
		// Try bool first; if no 'and'/'or' before the matching ')',
		// parseArith will succeed and the parser naturally falls
		// through to "(". We always parseBool for clarity.
		inner, err := p.parseBool()
		if err != nil {
			return nil, err
		}
		if err := p.consume(')'); err != nil {
			return nil, err
		}
		// Wrap as a synthetic cmp by synthesising a "!= 0" wrapper.
		return logical{kind: inner.kind, xs: inner.xs}, nil
	}
	if c == '-' {
		p.pos++
		inner, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return &arith.BinOp{Op: '*', L: inner, R: arith.NumLit{V: -1}}, nil
	}
	if c == '+' {
		p.pos++
		return p.parseFactor()
	}
	if (c >= '0' && c <= '9') || c == '.' {
		return p.parseNumber()
	}
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return p.parseIdent()
	}
	return nil, fmt.Errorf("alarm: unexpected character %q at %d", string(c), p.pos)
}

func (p *parser) parseNumber() (evalNode, error) {
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
		return nil, fmt.Errorf("alarm: bad number %q: %w", lit, err)
	}
	return arith.NumLit{V: v}, nil
}

// parseIdent reads [a-z0-9.]+ (PRMT-020 §4.2). Identifiers are
// lower-case only because that's what the spec calls out; we don't
// silently lowercase "STATUS" because that would mask rule-author
// typos. (cpath quantities are all lowercase by dict convention.)
func (p *parser) parseIdent() (evalNode, error) {
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' {
			p.pos++
			continue
		}
		break
	}
	name := p.src[start:p.pos]
	if name == "" {
		return nil, fmt.Errorf("alarm: empty identifier at %d", start)
	}
	// Reject bare-keyword identifiers like "and", "or" so they can't
	// be used as point names. The grammar already routes them into
	// logical operators; this is a defence in depth.
	if name == "and" || name == "or" {
		return nil, fmt.Errorf("alarm: reserved word %q used as identifier at %d", name, start)
	}
	return &arith.Ident{Name: name}, nil
}

// helper used in tests and rule.go (the compiler itself doesn't
// need it but ParseExpr callers sometimes want to know if a string
// is parseable without doing Eval).
func isParseable(s string) bool {
	_, err := ParseExpr(s)
	return err == nil
}

// stringsTrimRight is a small helper used by tests and main to
// normalise paths. Kept here so the alarm package stays a single
// leaf without dragging in pkg/cpath for trivial string ops.
func trimDot(s string) string { return strings.TrimRight(s, ".") }
