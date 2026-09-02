// PromQL label-tier enforcement. PRMT-109 §4 / §5.
//
// InjectTenantLabel rewrites a PromQL expression so that every metric
// selector carries an additional tenant="<id>" matcher. This is the
// execution path for tier=label (L53).
//
// Threat model (PRMT-109 §5 / §6 MUST / MUST NOT):
//
//  1. Attacker submits a query that already contains a tenant="..."
//     matcher set to a different tenant → MUST be rejected, never
//     silently overwritten. (Pretending to be another tenant.)
//  2. Attacker submits `up{tenant="A"} or vector(...)` where
//     `vector(...)` references the raw, unrestricted time series.
//     We MUST reject the binary OR at the top-level vector expression
//     because the right-hand side is not under our selector injection.
//  3. Attacker hides a sub-expression inside a subquery `(expr[5m])`
//     or in a parenthesised group — anything we cannot audit on the
//     happy path MUST be rejected (fail-closed).
//  4. Attacker uses a `# comment` to bury malformed bytes after a
//     legitimate prefix → reject (the comment means the line is
//     arbitrary, and PromQL parsers will happily accept it).
//  5. Attacker inserts a literal `\n` or `"` inside the supplied
//     tenant id so the resulting PromQL becomes `tenant="A\" or
//     vector(...)"` → the tenant id MUST be escape-checked before
//     splicing.
//  6. No third-party parser (PRMT-109 §6). We use a hand-rolled
//     selector scanner that is intentionally conservative — any
//     ambiguity fails closed.
//
// Implementation: a single forward pass over the input. We walk the
// expression at the top level and detect:
//
//   - `{` … `}` : metric / vector selector. Parse its label matchers
//     in a nested scan; reject any pre-existing `tenant=` matcher;
//     emit the selector with our `tenant="<id>"` matcher appended.
//   - `or` / `and` / `unless` as a binary vector operator at the top
//     level → reject. These compose vectors from multiple sub-
//     expressions; if any sub-expression is not under our label
//     injection, the result leaks.
//   - `(...)` as a parenthesised group / subquery / function call →
//     reject. The inside may contain a bare number (`vector(1)`),
//     a metric selector (`vector(up)`), or arbitrary PromQL; we
//     cannot audit it without a full parser.
//   - `#` to end-of-line → reject (see threat #4).
//
// The resulting PromQL is byte-identical to the input for the
// rejected cases (we never partially rewrite). On success, every
// `{` in the input is paired with a `}` and gets an injected matcher.
package tenant

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPromQLBypass is returned by InjectTenantLabel for any input that
// we cannot prove safe to rewrite. The HTTP handler maps this to
// 403 (PRMT-109 §5 fail-closed). The message is intentionally
// non-revealing — we do not echo the offending fragment back, since
// that would leak input bytes to a probing caller.
//
// Sentinel so callers (pkg/apigw) can branch on the cause without
// string-matching the error text. The concrete reason is in the
// wrapped error's Error() string, which goes to the audit log.
var ErrPromQLBypass = errors.New("tenant: PromQL rejected (label-tier bypass risk)")

// tenantLabelName is the canonical label name we inject. L53 reserves
// the tenant label name in the PromQL label set; this constant is the
// single source of truth.
const tenantLabelName = "tenant"

// InjectTenantLabel returns a rewritten PromQL expression with
// tenant="<id>" appended to every metric selector. On any unsafe
// input (per the threat model above) it returns (zero, error).
//
// The returned string is safe to pass to VictoriaMetrics' query
// endpoint: every selector carries the tenant matcher, no other
// selector is reachable from this expression, and no comment /
// subquery / parenthesised group can hide an unlabelled series.
func InjectTenantLabel(promql, tenantID string) (string, error) {
	if promql == "" {
		return "", fmt.Errorf("%w: empty query", ErrPromQLBypass)
	}
	if tenantID == "" {
		// Defensive: TenantFromClaims already filters empty ids, but
		// the function is exported; refuse to mint an injection that
		// has no value to splice.
		return "", fmt.Errorf("%w: empty tenant id", ErrPromQLBypass)
	}
	if err := validateTenantIDForLabel(tenantID); err != nil {
		return "", fmt.Errorf("%w: %v", ErrPromQLBypass, err)
	}

	var out strings.Builder
	out.Grow(len(promql) + len(tenantLabelName) + len(tenantID) + 8)

	i, sawSelector, err := injectScan(promql, tenantID, &out)
	if err != nil {
		return "", err
	}
	if i != len(promql) {
		// Trailing junk we couldn't classify. Conservative: refuse.
		return "", fmt.Errorf("%w: trailing content at byte %d", ErrPromQLBypass, i)
	}
	if !sawSelector {
		// A label-tier query that has no metric selector at all
		// (e.g. `1`, `0.5*2`, or an arithmetic over constants) leaks
		// nothing — but also is meaningless against telemetry. We
		// refuse rather than silently return the original because
		// a tenant-scoped query must always carry the tenant label.
		return "", fmt.Errorf("%w: no metric selector in query", ErrPromQLBypass)
	}
	return out.String(), nil
}

// validateTenantIDForLabel rejects tenant ids that cannot be safely
// spliced into a PromQL label value. Prometheus label values are
// arbitrary UTF-8 strings; we forbid the few bytes that would let
// a caller break out of the `"..."` quoting or splice a second
// matcher — `\`, `"`, `\n`, and any whitespace / control byte.
func validateTenantIDForLabel(id string) error {
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b == '\\' || b == '"' || b == '\n' || b == '\r' || b == '\t' {
			return fmt.Errorf("tenant id byte 0x%02x at offset %d", b, i)
		}
		if b < 0x20 {
			return fmt.Errorf("tenant id control byte 0x%02x at offset %d", b, i)
		}
	}
	return nil
}

// escapeTenantIDForLabel mirrors core/capacity.go:escapeLabelValue
// (PRMT-078). We duplicate it here so pkg/tenant does not depend
// on pkg/core (the dependency arrow is core → tenant at most, not
// the other way). The escape is defence-in-depth — validateTenantIDForLabel
// already forbids the offending bytes, so this only runs against
// inputs that have already cleared the validator.
func escapeTenantIDForLabel(id string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(id)
}

// injectScan is the forward pass. Returns (bytesConsumed, sawSelector, err).
//
// The state machine recognises:
//   - whitespace (skipped, copied verbatim)
//   - identifiers (e.g. metric names; copied verbatim)
//   - string literals `"..."` (copied verbatim, single-line only;
//     multi-line strings are rejected)
//   - numeric literals (copied verbatim)
//   - `{` ... `}` selectors (rewritten with tenant matcher)
//   - binary vector operators `or` / `and` / `unless` at the top
//     level → rejected (they could chain an unrestricted sibling)
//   - `(...)` groups / subqueries → rejected
//   - `#` comments → rejected
//   - any other operator byte (`+`, `-`, `*`, `/`, `%`, `^`, `=`,
//     `>`, `<`, `!`, `,`) → copied verbatim — these are scalar
//     arithmetic or list separators and cannot introduce a bare
//     metric selector.
//
// Bare-metric handling: a bare identifier like `up` (no `{...}`
// immediately following) is interpreted as `up{...}`. We inject a
// synthetic `{tenant="<id>"}` selector right after the metric name
// so the lookup still carries the tenant matcher. If the identifier
// IS followed by whitespace-then-`{`, we let the `{` branch handle
// it as a single selector (no double-injection).
func injectScan(s, tenantID string, out *strings.Builder) (int, bool, error) {
	sawSelector := false
	i := 0
	for i < len(s) {
		c := s[i]

		// Whitespace: copy verbatim.
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			out.WriteByte(c)
			i++
			continue
		}

		// Comment: reject outright (threat #4).
		if c == '#' {
			return 0, false, fmt.Errorf("%w: comment at byte %d", ErrPromQLBypass, i)
		}

		// Parenthesised group / subquery: reject (threat #3).
		if c == '(' {
			return 0, false, fmt.Errorf("%w: parenthesised group at byte %d", ErrPromQLBypass, i)
		}

		// Range vector suffix `[...]` or subquery `[...:...]`:
		// reject (threat #3 — anything past a `[` lives in a
		// sub-expression we cannot audit, and PromQL also allows
		// `expr[5m]` as a subquery applied to the rewritten
		// selector, which would defeat the tenant label).
		if c == '[' {
			return 0, false, fmt.Errorf("%w: range vector or subquery at byte %d", ErrPromQLBypass, i)
		}

		// Selector: parse the metric prefix and the {...} body.
		if c == '{' {
			selEnd, err := rewriteSelector(s, i, tenantID, out)
			if err != nil {
				return 0, false, err
			}
			sawSelector = true
			i = selEnd
			continue
		}

		// String literal: copy verbatim, but enforce single-line.
		if c == '"' {
			i2, err := copyStringLiteral(s, i, out)
			if err != nil {
				return 0, false, err
			}
			i = i2
			continue
		}

		// Identifier: scan and emit. If it matches a vector binary
		// operator (or/and/unless), reject.
		if isIdentStart(c) {
			j := i + 1
			for j < len(s) && isIdentCont(s[j]) {
				j++
			}
			word := s[i:j]
			switch word {
			case "or", "and", "unless":
				return 0, false, fmt.Errorf("%w: binary vector operator %q at byte %d", ErrPromQLBypass, word, i)
			}
			out.WriteString(word)
			i = j

			// Look ahead: skip whitespace and see what comes next.
			//   - `{` : the selector branch will rewrite it; no
			//           synthetic injection needed.
			//   - EOF / operator / another identifier : this is a
			//           bare metric — inject `{tenant="<id>"}`.
			//   - `,` / `)` / `]` / etc. : not legal PromQL here;
			//           reject so we don't silently emit garbage.
			k := i
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
				k++
			}
			if k >= len(s) {
				// Bare metric at end of input — inject.
				injectSyntheticSelector(out, tenantID)
				sawSelector = true
			} else if s[k] == '{' {
				// Selector follows; let the `{` branch handle it.
				_ = sawSelector
			} else if isOperatorStart(s[k]) {
				if sawSelector {
					// e.g. `up{tenant="A"} + 1` — selector already
					// accounted for; the operator tail is scalar.
					_ = sawSelector
				} else {
					// Bare metric followed by an operator (e.g.
					// `up + 1`): inject synthetic selector so the
					// tenant label reaches the metric, then let
					// the operator pass through.
					injectSyntheticSelector(out, tenantID)
					sawSelector = true
				}
			} else if isIdentStart(s[k]) {
				// Two bare identifiers in a row (e.g. `up down`)
				// is not legal PromQL — reject to surface the
				// ambiguity rather than emit `up{...}down{...}`.
				return 0, false, fmt.Errorf("%w: bare metric %q followed by identifier at byte %d", ErrPromQLBypass, word, k)
			} else {
				// Any other byte after the metric name (`;`, `[`,
				// `(`, `,`, etc.) is invalid in a top-level
				// expression. Reject to surface the malformed
				// input rather than silently corrupting it.
				return 0, false, fmt.Errorf("%w: byte 0x%02x after metric %q at byte %d", ErrPromQLBypass, s[k], word, k)
			}
			continue
		}

		// Numeric literal: copy until non-numeric / non-exponent.
		if c == '-' || c == '+' || (c >= '0' && c <= '9') {
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E' || s[j] == '-' || s[j] == '+') {
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}

		// Any other single-byte operator: copy verbatim.
		out.WriteByte(c)
		i++
	}
	return i, sawSelector, nil
}

// injectSyntheticSelector emits a flush `{tenant="<id>"}` selector
// at the current end of out. Used by injectScan when a bare metric
// name has no `{...}` of its own.
func injectSyntheticSelector(out *strings.Builder, tenantID string) {
	out.WriteByte('{')
	out.WriteString(tenantLabelName)
	out.WriteString(`="`)
	out.WriteString(escapeTenantIDForLabel(tenantID))
	out.WriteString(`"}`)
}

// isOperatorStart reports whether c is a scalar-arithmetic operator
// that can legally follow a bare metric name in PromQL. This is
// the set we treat as "the metric is followed by an arithmetic
// tail, inject the synthetic selector now".
func isOperatorStart(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '%', '^':
		return true
	}
	return false
}

// rewriteSelector consumes `{...}` starting at openIdx (which must
// point at the `{`) and writes a rewritten selector to out. The
// selector body is scanned for label matchers; if any matcher
// already sets the tenant label, the call is rejected (threat #1).
//
// Returns the index immediately AFTER the closing `}`.
//
// The selector body is parsed with a fresh local scanner that
// tracks whether any matcher has been written yet — that boolean
// (`sawMatcher`) tells us whether to emit `,tenant="<id>"` or
// `tenant="<id>"` when the closing `}` arrives. We never mutate
// the existing matcher list.
//
// When the closing `}` is preceded by trailing whitespace / commas,
// the inject runs in two steps: it removes the trailing
// separators from `out` so the comma separator can sit flush
// against the previous matcher's last byte, then re-emits the
// separator run (if any) AFTER the tenant matcher so the original
// spacing before `}` is preserved. Example: input ` job = "prom" `
// becomes ` job = "prom",tenant="acme" ` — the trailing space
// before `}` survives.
func rewriteSelector(s string, openIdx int, tenantID string, out *strings.Builder) (int, error) {
	out.WriteByte('{')

	bodyStart := openIdx + 1
	j := bodyStart
	sawMatcher := false
	for j < len(s) {
		c := s[j]

		// Whitespace and commas: copy verbatim.
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			out.WriteByte(c)
			j++
			continue
		}

		// Comment inside a selector: same reject as outside.
		if c == '#' {
			return 0, fmt.Errorf("%w: comment in selector at byte %d", ErrPromQLBypass, j)
		}

		// Nested selector: a `{` inside a selector is a syntax error
		// in real PromQL, so we can safely reject — but a stray `{`
		// also means we are no longer reading a flat matcher list.
		if c == '{' {
			return 0, fmt.Errorf("%w: nested '{' in selector at byte %d", ErrPromQLBypass, j)
		}

		// Closing brace: stash any trailing separator run, emit
		// the tenant matcher flush against the previous byte,
		// then re-emit the trailing separator run before `}`.
		if c == '}' {
			trail := trimTrailingSeparators(out)
			if sawMatcher {
				out.WriteByte(',')
			}
			out.WriteString(tenantLabelName)
			out.WriteString(`="`)
			out.WriteString(escapeTenantIDForLabel(tenantID))
			out.WriteString(`"`)
			out.WriteString(trail)
			out.WriteByte('}')
			return j + 1, nil
		}

		// Label matcher: scan `<name><op>"<value>"` and check for
		// tenant=<something>.
		if isIdentStart(c) {
			nameStart := j
			j2 := j + 1
			for j2 < len(s) && isIdentCont(s[j2]) {
				j2++
			}
			name := s[nameStart:j2]

			// Operator: =, !=, =~, !~. Skip whitespace around it.
			j3 := j2
			for j3 < len(s) && (s[j3] == ' ' || s[j3] == '\t') {
				j3++
			}
			if j3 >= len(s) {
				return 0, fmt.Errorf("%w: matcher %q missing operator at byte %d", ErrPromQLBypass, name, j3)
			}
			op := s[j3]
			if op != '=' && op != '!' {
				return 0, fmt.Errorf("%w: matcher %q bad operator at byte %d", ErrPromQLBypass, name, j3)
			}
			j4 := j3 + 1
			if j4 < len(s) && s[j4] == '~' {
				j4++
			}
			// Optional whitespace before the value.
			for j4 < len(s) && (s[j4] == ' ' || s[j4] == '\t') {
				j4++
			}
			if j4 >= len(s) {
				return 0, fmt.Errorf("%w: matcher %q missing value at byte %d", ErrPromQLBypass, name, j4)
			}
			// Value: must be a quoted string (PromQL does accept
			// bare identifiers for regex matchers, but a bare-tenant
			// matcher is the bypass we're guarding against — so we
			// require quoting).
			if s[j4] != '"' {
				return 0, fmt.Errorf("%w: matcher %q value must be quoted at byte %d", ErrPromQLBypass, name, j4)
			}
			valEnd, err := scanQuotedString(s, j4)
			if err != nil {
				return 0, err
			}
			// Reject a pre-existing tenant matcher (threat #1).
			if name == tenantLabelName {
				return 0, fmt.Errorf("%w: tenant label already set in selector at byte %d", ErrPromQLBypass, nameStart)
			}
			// Copy the matcher bytes verbatim (we are appending our
			// own matcher, not editing existing ones).
			out.WriteString(s[nameStart:valEnd])
			j = valEnd
			sawMatcher = true
			continue
		}

		// Any other byte inside a selector: copy verbatim. (PromQL
		// allows `_` in identifiers, but isIdentStart covers that.)
		out.WriteByte(c)
		j++
	}
	return 0, fmt.Errorf("%w: unterminated selector at byte %d", ErrPromQLBypass, openIdx)
}

// trimTrailingSeparators removes any trailing whitespace or commas
// from out, stopping at the first non-separator byte, and returns
// the trimmed suffix (so the caller can re-emit it later). It is
// a no-op (returns "") when out ends on a non-separator byte.
//
// The opening `{` is never trimmed: trimTrailingSeparators stops
// at index 0 if everything else is a separator.
func trimTrailingSeparators(out *strings.Builder) string {
	cur := out.String()
	idx := len(cur) - 1
	for idx > 0 && (cur[idx] == ' ' || cur[idx] == '\t' || cur[idx] == '\n' || cur[idx] == '\r' || cur[idx] == ',') {
		idx--
	}
	if idx < len(cur)-1 {
		trail := cur[idx+1:]
		out.Reset()
		out.WriteString(cur[:idx+1])
		return trail
	}
	return ""
}

// copyStringLiteral copies a `"..."` literal starting at openIdx
// to out and returns the index immediately AFTER the closing `"`.
// Multi-line strings (`\n` inside) are rejected.
func copyStringLiteral(s string, openIdx int, out *strings.Builder) (int, error) {
	out.WriteByte('"')
	i := openIdx + 1
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			// Escape sequence: copy the backslash and the next byte
			// verbatim, but refuse embedded newlines.
			if s[i+1] == '\n' {
				return 0, fmt.Errorf("%w: multi-line string literal at byte %d", ErrPromQLBypass, i)
			}
			out.WriteByte(c)
			out.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '"' {
			out.WriteByte('"')
			return i + 1, nil
		}
		if c == '\n' {
			return 0, fmt.Errorf("%w: multi-line string literal at byte %d", ErrPromQLBypass, i)
		}
		out.WriteByte(c)
		i++
	}
	return 0, fmt.Errorf("%w: unterminated string literal at byte %d", ErrPromQLBypass, openIdx)
}

// scanQuotedString returns the index immediately AFTER the closing
// `"` of a string literal starting at openIdx (which must point at
// the opening `"`). Multi-line literals are rejected.
func scanQuotedString(s string, openIdx int) (int, error) {
	i := openIdx + 1
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			if s[i+1] == '\n' {
				return 0, fmt.Errorf("%w: multi-line string literal at byte %d", ErrPromQLBypass, i)
			}
			i += 2
			continue
		}
		if c == '"' {
			return i + 1, nil
		}
		if c == '\n' {
			return 0, fmt.Errorf("%w: multi-line string literal at byte %d", ErrPromQLBypass, i)
		}
		i++
	}
	return 0, fmt.Errorf("%w: unterminated string literal at byte %d", ErrPromQLBypass, openIdx)
}

// isIdentStart / isIdentCont mirror PromQL identifier rules: ASCII
// letter or `_` to start, then ASCII letter / digit / `_` / `:`.
// The `:` is included because PromQL permits `record_name:metric_name`
// in recording-rule expressions; we don't enforce recording rules
// here, but copying the byte is harmless.
func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == ':'
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
