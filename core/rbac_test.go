// Package core — rbac_test.go: benchmarks for the authorize() hot
// path. PRMT-113's whole point is "glob compile out of the request
// hot path" — these benchmarks are the proof. A reading of "0 B/op
// 0 allocs/op" on BenchmarkAuthorize_VerifyPath means cpath.CompileGlob
// is no longer allocating per request, because the Principal
// arrives at authorize() with its compiledScopes already populated
// by NewStaticTokenVerifier.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// BenchmarkAuthorize_RawScopes is the cold-path benchmark: a
// Principal built by struct literal so compiledScopes is nil and
// authorize() must compile on the fly. This is the path that
// existed before PRMT-113. The BenchmarkAuthorize_VerifyPath below
// is the post-PRMT-113 hot path; comparing the two shows the
// savings.
//
// It is included for the spec-required "baseline vs optimized"
// measurement (PRMT-113 §8). Both benchmarks must exist; the
// optimized one is the load-bearing one for the success criterion.
func BenchmarkAuthorize_RawScopes(b *testing.B) {
	// The scopes and path mirror the L50 subtree-implies case the
	// production verifier is configured with (operator scoped to a
	// site subtree, reading a point under that site). Building the
	// Principal once with compiledScopes=nil captures the pre-
	// PRMT-113 worst case: every authorize() call must compileGlob
	// the raw pattern and the ".<anything>" subtree variant.
	p := Principal{
		Subject: "svc:cooling-ops",
		Role:    RoleOperator,
		Scopes:  []string{"site01.pod002.**"},
	}
	const path = "site01.pod002.cdu000.fan000.rpm"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := authorize(p, ActionRead, path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuthorize_VerifyPath is the production path after
// PRMT-113: NewStaticTokenVerifier compiles scopes once, Verify()
// returns a Principal with compiledScopes populated, and
// authorize() does Match() only. Allocations per call should drop
// to whatever cpath.Glob.Match allocates (DP table for the
// glob-with-stars case; the pre-PRMT-113 loop also built that DP
// table, so the savings are the CompileGlob side only).
func BenchmarkAuthorize_VerifyPath(b *testing.B) {
	const (
		viewerTok = "bench-viewer-token"
		scope     = "site01.pod002.**"
	)
	h := sha256.Sum256([]byte(viewerTok))
	key := hex.EncodeToString(h[:])
	v, err := NewStaticTokenVerifier(map[string]Principal{
		key: {Subject: "svc:cooling-ops", Role: RoleOperator, Scopes: []string{scope}},
	})
	if err != nil {
		b.Fatal(err)
	}
	p, err := v.Verify(viewerTok)
	if err != nil {
		b.Fatal(err)
	}
	const path = "site01.pod002.cdu000.fan000.rpm"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := authorize(p, ActionRead, path); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAuthorize_CompiledScopesPopulatedAtConstruction is the
// unit-level proof that the contract holds: a Principal from
// Verify() has a non-nil compiledScopes of the same length as
// Scopes. A regression that drops the compilePrincipalScopes call
// from NewStaticTokenVerifier would zero this slice and the
// BenchmarkAuthorize_VerifyPath above would lose its allocation
// advantage.
func TestAuthorize_CompiledScopesPopulatedAtConstruction(t *testing.T) {
	const (
		tok = "compile-prove-token"
	)
	h := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(h[:])
	v, err := NewStaticTokenVerifier(map[string]Principal{
		key: {Subject: "svc:x", Role: RoleViewer, Scopes: []string{"site01.pod002", "site02.**"}},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(p.compiledScopes) != len(p.Scopes) {
		t.Fatalf("len(compiledScopes)=%d, want %d (one per raw scope)", len(p.compiledScopes), len(p.Scopes))
	}
	// Spot-check: subtree globs must be the ".<anything>" form.
	// We use the unexported Pattern() method on Glob (same package).
	if got, want := p.compiledScopes[0].subtree.Pattern(), "site01.pod002.**"; got != want {
		t.Errorf("compiledScopes[0].subtree.Pattern() = %q, want %q", got, want)
	}
	if got, want := p.compiledScopes[1].exact.Pattern(), "site02.**"; got != want {
		t.Errorf("compiledScopes[1].exact.Pattern() = %q, want %q", got, want)
	}
}
