// PRMT-030 §A.5 — positive assertion that pkg/alarm.AllowedSeverities
// is exactly the spec-003 §2 set {critical, major, minor, info}. Prior
// to R1, rule_test.go covered happy-path severity validation but never
// asserted the canonical key set; this test is the forward-looking
// regression barrier so a future edit to severity.go cannot quietly
// grow or shrink the whitelist.
package alarm

import "testing"

func TestAllowedSeverities_ExactKeySet(t *testing.T) {
	want := []string{"critical", "major", "minor", "info"}
	if got := len(AllowedSeverities); got != len(want) {
		t.Fatalf("len(AllowedSeverities)=%d, want %d", got, len(want))
	}
	for _, k := range want {
		if _, ok := AllowedSeverities[k]; !ok {
			t.Errorf("AllowedSeverities missing canonical key %q", k)
		}
	}
}

func TestAllowedSeverities_RejectsNonCanonical(t *testing.T) {
	// Anything outside the spec-003 §2 set must not slip in. Sample
	// a handful of obviously-wrong values; a duplicate-all-keys
	// regression would be caught by the length check above.
	for _, k := range []string{"warning", "fatal", "error", "", "CRITICAL"} {
		if _, ok := AllowedSeverities[k]; ok {
			t.Errorf("AllowedSeverities unexpectedly contains %q", k)
		}
	}
}
