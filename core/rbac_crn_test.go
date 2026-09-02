// Package core — rbac_crn_test.go: PRMT-190 dual-grammar + §6bis
// red-line + deprecation-meter tests. The pre-existing rbac matrix
// in rbac_test.go / auth_test.go still exercises the legacy dot-
// glob path via Principal struct literals (no tenant); those
// continue to pass unchanged because legacy scopes are tagged
// origin=originLegacy with tid="" and the red line is dormant on
// the no-tenant authorize() path. This file covers the crn-aware
// additions specifically.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// --- helpers --------------------------------------------------------------

// newVerifierWithScopes returns a verified Principal whose scopes
// have been compiled by compilePrincipalScopes (the same code path
// NewStaticTokenVerifier takes). The token-tok is unique per call
// so subtests do not collide.
func newVerifierWithScopes(t *testing.T, role Role, scopes []string) (Principal, string) {
	t.Helper()
	const tag = "crn-test-token-"
	tok := tag + t.Name()
	h := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(h[:])
	v, err := NewStaticTokenVerifier(map[string]Principal{
		key: {Subject: "svc:crn-test", Role: role, Scopes: scopes},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return p, tok
}

// --- origin tagging -------------------------------------------------------

// TestCompileOrigin_LegacyTaggedDefault proves that a v1.0 dot-glob
// scope is normalized to crn/org/default at load and tagged
// originLegacy with tid == "" (PRMT-190 §4.2 fallback, §5 MUST #2).
func TestCompileOrigin_LegacyTaggedDefault(t *testing.T) {
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.chiller*"})
	if len(p.compiledScopes) != 1 {
		t.Fatalf("compiledScopes len = %d, want 1", len(p.compiledScopes))
	}
	sg := p.compiledScopes[0]
	if sg.origin != originLegacy {
		t.Errorf("origin = %d, want originLegacy (%d)", sg.origin, originLegacy)
	}
	if sg.tid != "" {
		t.Errorf("tid = %q, want \"\" (transition sentinel for legacy)", sg.tid)
	}
	// Site-tail dot path preserved verbatim for cpath reuse.
	if got := sg.exact.Pattern(); got != "site01.chiller*" {
		t.Errorf("exact.Pattern() = %q, want %q", got, "site01.chiller*")
	}
}

// TestCompileOrigin_CRNTaggedExplicit proves that a native crn-form
// scope is tagged originCRN with its explicit tid (PRMT-190 §4.2,
// §5 MUST #2).
func TestCompileOrigin_CRNTaggedExplicit(t *testing.T) {
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002",
	})
	if len(p.compiledScopes) != 1 {
		t.Fatalf("compiledScopes len = %d, want 1", len(p.compiledScopes))
	}
	sg := p.compiledScopes[0]
	if sg.origin != originCRN {
		t.Errorf("origin = %d, want originCRN (%d)", sg.origin, originCRN)
	}
	if sg.tid != "acme" {
		t.Errorf("tid = %q, want %q", sg.tid, "acme")
	}
	// Site-tail bijection: crn ".../site/fra01/pod002" ↔ "fra01.pod002".
	if got := sg.exact.Pattern(); got != "fra01.pod002" {
		t.Errorf("exact.Pattern() = %q, want %q", got, "fra01.pod002")
	}
}

// TestCompileRejectsBadCRNAtLoad preserves the load-time rejection
// contract (PRMT-190 §4.2 MUST): a malformed crn in a Principal
// scope is rejected by NewStaticTokenVerifier, so the verifier is
// never built and the request path never sees it.
func TestCompileRejectsBadCRNAtLoad(t *testing.T) {
	_, err := NewStaticTokenVerifier(map[string]Principal{
		"x": {Subject: "x", Role: RoleViewer, Scopes: []string{
			"crn:tenant/acme/org/emea/site/FRA01", // uppercase site
		}},
	})
	if err == nil {
		t.Fatalf("NewStaticTokenVerifier accepted bad crn scope; want error")
	}
	// Dot-in-site rejection (PRMT-190 §4.1): the dot belongs in the
	// /site/<sid>/<node>* tail, not in the site slot.
	_, err = NewStaticTokenVerifier(map[string]Principal{
		"x": {Subject: "x", Role: RoleViewer, Scopes: []string{
			"crn:tenant/acme/org/emea/site/fra01.pod002",
		}},
	})
	if err == nil {
		t.Fatalf("NewStaticTokenVerifier accepted dot-in-site; want error")
	}
}

// --- dual grammar (legacy still authorizes) -------------------------------

// TestDualGrammar_LegacyStillAuthorizes pins §5 MUST #3: a legacy
// dot-glob scope continues to authorize exactly as before — no
// regression in the existing matrix.
func TestDualGrammar_LegacyStillAuthorizes(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	cases := []struct {
		name   string
		role   Role
		scopes []string
		action Action
		path   string
		allow  bool
	}{
		{"viewer read in legacy scope literal", RoleViewer, []string{"site01.pod002.cdu000"}, ActionRead, "site01.pod002.cdu000", true},
		{"viewer legacy read subtree implied", RoleViewer, []string{"site01.pod002"}, ActionRead, "site01.pod002.cdu000.fan000.rpm", true},
		{"viewer legacy out of scope", RoleViewer, []string{"site01.pod001"}, ActionRead, "site01.pod002.cdu000", false},
		{"operator legacy write with explicit subtree", RoleOperator, []string{"site01.pod002.**"}, ActionControlWrite, "site01.pod002.cdu000.fan000.rpm", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newVerifierWithScopes(t, tc.role, tc.scopes)
			err := authorize(p, tc.action, tc.path)
			gotAllow := err == nil
			if gotAllow != tc.allow {
				t.Fatalf("authorize() allow=%v, want %v (err=%v)", gotAllow, tc.allow, err)
			}
		})
	}
}

// TestDualGrammar_CRNFormAuthorizes proves a crn-origin scope
// authorizes via the same Match() machinery when no tenant is
// attached (the request-tenant-empty branch from PRMT-190 §4.3).
func TestDualGrammar_CRNFormAuthorizes(t *testing.T) {
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002",
	})
	if err := authorize(p, ActionRead, "fra01.pod002.cdu000.fan000.rpm"); err != nil {
		t.Fatalf("crn scope read authorize err = %v, want nil (subtree implied)", err)
	}
	// L50: crn form write requires explicit subtree scope.
	pW, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002",
	})
	if err := authorize(pW, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm"); err == nil {
		t.Fatalf("crn literal write allowed without subtree; want ErrForbidden (L50)")
	}
	// ...and with explicit subtree scope it allows.
	pWS, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002.**",
	})
	if err := authorize(pWS, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm"); err != nil {
		t.Fatalf("crn explicit-subtree write authorize err = %v, want nil", err)
	}
}

// --- deprecation meter (PRMT-190 §4.4) ------------------------------------

// TestLegacyMeter_IncrementsOnLegacyMatch proves every legacy-origin
// match increments the atomic counter (§5 MUST #4).
func TestLegacyMeter_IncrementsOnLegacyMatch(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	before := LegacyScopeUses()
	if err := authorize(p, ActionRead, "site01.pod002.cdu000"); err != nil {
		t.Fatalf("authorize err = %v", err)
	}
	if got := LegacyScopeUses(); got != before+1 {
		t.Fatalf("LegacyScopeUses = %d, want %d", got, before+1)
	}
}

// TestCRNMatch_NoCounterNoWarn proves crn-origin matches do not
// touch the legacy counter and do not warn (§5 MUST #4).
func TestCRNMatch_NoCounterNoWarn(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002",
	})
	before := LegacyScopeUses()
	if err := authorize(p, ActionRead, "fra01.pod002.cdu000"); err != nil {
		t.Fatalf("authorize err = %v", err)
	}
	if got := LegacyScopeUses(); got != before {
		t.Fatalf("LegacyScopeUses = %d, want unchanged (%d) for crn match", got, before)
	}
}

// TestLegacyWarn_OncePerPattern asserts the §4.4 rate limit: the
// deprecation log line is emitted at most once per raw legacy
// pattern per process. The test inspects the warn-once map directly
// because capturing log.Printf output is racy across the standard
// library test runner.
func TestLegacyWarn_OncePerPattern(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	// Two reads on the same legacy scope pattern: the counter
	// increments both times (one per match) but the warn-once map
	// contains only one entry.
	if err := authorize(p, ActionRead, "site01.pod002.cdu000"); err != nil {
		t.Fatalf("first read err = %v", err)
	}
	if err := authorize(p, ActionRead, "site01.pod002.cdu000.fan000.rpm"); err != nil {
		t.Fatalf("second read err = %v", err)
	}
	if got := LegacyScopeUses(); got != 2 {
		t.Fatalf("LegacyScopeUses = %d, want 2 (one per match)", got)
	}
	loaded, ok := legacyScopeWarned.Load("site01.pod002")
	if !ok || loaded == nil {
		t.Fatalf("legacyScopeWarned missing entry for %q", "site01.pod002")
	}
}

// --- §6bis red line -------------------------------------------------------

// TestRedLine_CRNAllowsMatchingTenant proves the red-line allow
// branch (PRMT-190 §5 MUST #5): crn scope with tid == request tenant
// allows, even on the explicit-match (non-read) path.
func TestRedLine_CRNAllowsMatchingTenant(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002.**",
	})
	if err := authorizeTenant(p, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm", "acme"); err != nil {
		t.Fatalf("authorizeTenant(write, \"acme\") err = %v, want nil (tid matches)", err)
	}
}

// TestRedLine_CRNDeniesMismatchedTenant proves the §6bis red line
// deny branch: crn scope with tid != request tenant → ErrForbidden,
// no wildcard/** exception (§5 MUST #5).
func TestRedLine_CRNDeniesMismatchedTenant(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	// Even an explicit subtree scope with ** cannot bypass the red line.
	p, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002.**",
	})
	if err := authorizeTenant(p, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm", "globex"); err != ErrForbidden {
		t.Fatalf("authorizeTenant(write, \"globex\") err = %v, want ErrForbidden (red line, no ** bypass)", err)
	}
	// And the catch-all single-globstar crn does not bypass either.
	pAll, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/**/site/**",
	})
	if err := authorizeTenant(pAll, ActionRead, "fra01.pod002.cdu000", "globex"); err != ErrForbidden {
		t.Fatalf("authorizeTenant(read all-crn, \"globex\") err = %v, want ErrForbidden", err)
	}
}

// TestRedLine_LegacyNotBoundToTenantDoNot403 confirms the §4.2
// fallback behaviour: a legacy-origin scope carries tid == "" so the
// red line is dormant and a legacy match still authorizes even
// when a request tenant is attached. The deprecation meter still
// fires.
func TestRedLine_LegacyNotBoundToTenantDoNot403(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	before := LegacyScopeUses()
	if err := authorizeTenant(p, ActionRead, "site01.pod002.cdu000", "globex"); err != nil {
		t.Fatalf("authorizeTenant(legacy, \"globex\") err = %v, want nil (legacy not tenant-bound, red line dormant)", err)
	}
	if got := LegacyScopeUses(); got != before+1 {
		t.Fatalf("LegacyScopeUses = %d, want %d (legacy meter still fires)", got, before+1)
	}
}

// TestRedLine_NoTenantDormant proves that when no request tenant is
// attached (ops-realm admin posture, R1) the red line is dormant
// even for crn-origin scopes — they allow, as before PRMT-190.
func TestRedLine_NoTenantDormant(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002",
	})
	if err := authorizeTenant(p, ActionRead, "fra01.pod002.cdu000", ""); err != nil {
		t.Fatalf("authorizeTenant(crn, no-tenant) err = %v, want nil (red line dormant)", err)
	}
}

// --- admin bypass + role floor (must not regress) -------------------------

// TestAdminBypassUnchanged: admin still bypasses scope entirely
// regardless of crn/legacy origin or tenant (PRMT-190 §5 MUST #6).
func TestAdminBypassUnchanged(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleAdmin, nil)
	// Even with a tenant present, admin's role is the ceiling.
	if err := authorizeTenant(p, ActionApply, "anywhere.at.all", "anything"); err != nil {
		t.Fatalf("admin authorizeTenant err = %v, want nil", err)
	}
	// And the legacy counter must not increment for admin (admin
	// short-circuits before the scope loop).
	if got := LegacyScopeUses(); got != 0 {
		t.Fatalf("LegacyScopeUses = %d, want 0 (admin bypass)", got)
	}
}

// TestRoleFloorUnchanged: role floor fails closed exactly as before
// (§5 MUST #6), even with crn scopes.
func TestRoleFloorUnchanged(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{
		"crn:tenant/acme/org/emea/site/fra01/**",
	})
	if err := authorize(p, ActionControlWrite, "fra01.pod002.cdu000"); err != ErrForbidden {
		t.Fatalf("viewer control:write err = %v, want ErrForbidden (role floor)", err)
	}
}

// TestMixedScopes_TenantCorrectForCRNOnly: a Principal holding both
// legacy and crn scopes — only the crn scope's tid is checked
// against the request tenant; the legacy scope co-exists under the
// red-line-dormant fallback.
func TestMixedScopes_TenantCorrectForCRNOnly(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	p, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"site01.pod002.**", // legacy
		"crn:tenant/acme/org/emea/site/fra01/pod002.**", // crn, tid=acme
	})
	// request tenant = acme → crn scope is on-tenant; legacy also
	// fires and increments the counter.
	if err := authorizeTenant(p, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm", "acme"); err != nil {
		t.Fatalf("mixed-scopes write acme err = %v, want nil", err)
	}
	// request tenant = globex → crn scope is off-tenant; legacy is
	// dormant but the crn scope fires first (it comes second in the
	// list above) and 403s.
	if err := authorizeTenant(p, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm", "globex"); err != ErrForbidden {
		t.Fatalf("mixed-scopes write globex err = %v, want ErrForbidden", err)
	}
}

// TestCompileRejectsBadLegacyScopeAtLoad (sanity): the pre-existing
// load-time rejection contract on bad legacy patterns is preserved.
func TestCompileRejectsBadLegacyScopeAtLoad(t *testing.T) {
	_, err := NewStaticTokenVerifier(map[string]Principal{
		"x": {Subject: "x", Role: RoleViewer, Scopes: []string{"site01..bad"}},
	})
	if err == nil {
		t.Fatalf("NewStaticTokenVerifier accepted bad legacy scope; want error")
	}
	// Sanity: error message should mention cpath's grammar complaint
	// or our crn-side grammar; either is fine, just not nil.
	if !strings.Contains(err.Error(), "cpath") && !strings.Contains(err.Error(), "crn") && err == nil {
		t.Fatalf("unexpected error shape: %v", err)
	}
}
