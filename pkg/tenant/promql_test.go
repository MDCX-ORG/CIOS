// pkg/tenant/promql_test.go — table-driven coverage for the
// label-tier PromQL injector. PRMT-109 §5 + §7 acceptance.
//
// Coverage map (every MUST from §5 has at least one test):
//
//   - happy single selector injection (TestInject_HappySingleSelector)
//   - empty selector (TestInject_EmptySelector)
//   - selector with existing matchers → comma + tenant matcher
//     (TestInject_SelectorWithExistingMatchers)
//   - recording-rule prefix (TestInject_RecordingRulePrefix)
//   - whitespace tolerance (TestInject_WhitespaceTolerance)
//   - already-present tenant= matcher → reject (threat #1)
//     (TestInject_RejectsPreExistingTenant)
//   - top-level `or` / `and` / `unless` → reject (threat #2)
//     (TestInject_RejectsBinaryVectorOp)
//   - parenthesised group / subquery → reject (threat #3)
//     (TestInject_RejectsParenthesisedGroup)
//   - `#` comment → reject (threat #4) (TestInject_RejectsComment)
//   - tenant id with `"` / `\` / `\n` → reject (threat #5)
//     (TestInject_RejectsHostileTenantID)
//   - empty query / empty tenant id → reject
//     (TestInject_RejectsEmptyInputs)
//   - string literals and numeric literals pass through
//     (TestInject_StringAndNumericPassThrough)
package tenant

import (
	"errors"
	"testing"
)

func TestInject_HappySingleSelector(t *testing.T) {
	got, err := InjectTenantLabel(`up`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `up{tenant="acme"}`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_EmptySelector(t *testing.T) {
	// `up{}` is valid PromQL — a selector with no matchers.
	got, err := InjectTenantLabel(`up{}`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `up{tenant="acme"}`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_SelectorWithExistingMatchers(t *testing.T) {
	got, err := InjectTenantLabel(`up{job="prometheus"}`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `up{job="prometheus",tenant="acme"}`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_SelectorWithExistingMatcherRegex(t *testing.T) {
	got, err := InjectTenantLabel(`up{job=~"prom.*"}`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `up{job=~"prom.*",tenant="acme"}`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_RecordingRulePrefix(t *testing.T) {
	// `record_name:metric{...}` is legal in recording-rule evaluation.
	got, err := InjectTenantLabel(`rate:up{job="prom"}`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `rate:up{job="prom",tenant="acme"}`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_WhitespaceTolerance(t *testing.T) {
	got, err := InjectTenantLabel(`  up { job = "prom" }  `, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel: %v", err)
	}
	want := `  up { job = "prom",tenant="acme" }  `
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_RejectsPreExistingTenant(t *testing.T) {
	cases := []string{
		`up{tenant="A"}`,
		`up{job="prom",tenant="A"}`,
		`up{tenant=~"A.*"}`,
		`up{job="prom",tenant="B"}`,
	}
	for _, in := range cases {
		_, err := InjectTenantLabel(in, "A")
		if err == nil {
			t.Errorf("InjectTenantLabel(%q): nil err, want ErrPromQLBypass", in)
			continue
		}
		if !errors.Is(err, ErrPromQLBypass) {
			t.Errorf("InjectTenantLabel(%q): err = %v, want ErrPromQLBypass", in, err)
		}
	}
}

func TestInject_RejectsBinaryVectorOp(t *testing.T) {
	cases := []string{
		`up or vector(1)`,
		`up{job="prom"} or down{job="prom"}`,
		`up{job="prom"} and down{job="prom"}`,
		`up{job="prom"} unless down{job="prom"}`,
	}
	for _, in := range cases {
		_, err := InjectTenantLabel(in, "acme")
		if err == nil {
			t.Errorf("InjectTenantLabel(%q): nil err, want ErrPromQLBypass", in)
			continue
		}
		if !errors.Is(err, ErrPromQLBypass) {
			t.Errorf("InjectTenantLabel(%q): err = %v, want ErrPromQLBypass", in, err)
		}
	}
}

func TestInject_RejectsParenthesisedGroup(t *testing.T) {
	cases := []string{
		`(up)`,
		`up{job="prom"}[5m]`,
		`sum(up)`,
		`sum by (job) (up{job="prom"})`,
		`rate(up{job="prom"}[5m])`,
	}
	for _, in := range cases {
		_, err := InjectTenantLabel(in, "acme")
		if err == nil {
			t.Errorf("InjectTenantLabel(%q): nil err, want ErrPromQLBypass", in)
			continue
		}
		if !errors.Is(err, ErrPromQLBypass) {
			t.Errorf("InjectTenantLabel(%q): err = %v, want ErrPromQLBypass", in, err)
		}
	}
}

func TestInject_RejectsComment(t *testing.T) {
	cases := []string{
		`up{job="prom"} # inject`,
		`# nothing`,
		`up{job="prom", # inline
tenant="acme"}`,
	}
	for _, in := range cases {
		_, err := InjectTenantLabel(in, "acme")
		if err == nil {
			t.Errorf("InjectTenantLabel(%q): nil err, want ErrPromQLBypass", in)
			continue
		}
		if !errors.Is(err, ErrPromQLBypass) {
			t.Errorf("InjectTenantLabel(%q): err = %v, want ErrPromQLBypass", in, err)
		}
	}
}

func TestInject_RejectsHostileTenantID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"double_quote", `A" or vector(1)`},
		{"backslash", `A\B`},
		{"newline", "A\nB"},
		{"carriage_return", "A\rB"},
		{"tab", "A\tB"},
		{"control_byte", "A\x00B"},
	}
	for _, tc := range cases {
		_, err := InjectTenantLabel(`up`, tc.id)
		if err == nil {
			t.Errorf("%s: InjectTenantLabel accepted hostile id %q", tc.name, tc.id)
			continue
		}
		if !errors.Is(err, ErrPromQLBypass) {
			t.Errorf("%s: err = %v, want ErrPromQLBypass", tc.name, err)
		}
	}
}

func TestInject_RejectsEmptyInputs(t *testing.T) {
	if _, err := InjectTenantLabel("", "acme"); err == nil {
		t.Errorf("empty query: nil err, want error")
	}
	if _, err := InjectTenantLabel(`up`, ""); err == nil {
		t.Errorf("empty tenant id: nil err, want error")
	}
}

func TestInject_StringAndNumericPassThrough(t *testing.T) {
	// A scalar literal with no selector must be rejected (no metric
	// to attach the tenant label to).
	if _, err := InjectTenantLabel(`1`, "acme"); err == nil {
		t.Errorf("InjectTenantLabel(`1`): nil err, want ErrPromQLBypass")
	}
	// A vector + scalar arithmetic IS allowed: the selector gets
	// the tenant label, the scalar tail is passthrough. Whitespace
	// around the operator is preserved verbatim — the injector
	// only mutates the boundary between metric name and selector,
	// never the arithmetic tail.
	got, err := InjectTenantLabel(`up + 1`, "acme")
	if err != nil {
		t.Fatalf("InjectTenantLabel(`up + 1`): %v", err)
	}
	want := `up{tenant="acme"} + 1`
	if got != want {
		t.Errorf("InjectTenantLabel = %q, want %q", got, want)
	}
}

func TestInject_UnterminatedSelector(t *testing.T) {
	if _, err := InjectTenantLabel(`up{job="prom"`, "acme"); err == nil {
		t.Errorf("InjectTenantLabel unterminated: nil err, want ErrPromQLBypass")
	}
}
