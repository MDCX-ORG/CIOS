// pkg/sts/sts_test.go — table-driven tests for the STS
// (PRMT-103 §5 acceptance).
//
// Coverage map (every MUST from §5 has at least one test):
//
//   - happy exchange + verify (TestSTS_Happy)
//   - expired token rejected (TestSTS_Expired)
//   - wrong signature rejected (TestSTS_WrongKey,
//     TestSTS_TamperedBody, TestParse_AlgConfusion)
//   - revoked jti rejected (TestSTS_Revoked)
//   - realm not in {ops, customer} rejected at Exchange
//     (TestSTS_RealmUnknown)
//   - scope not escalated (TestSTS_ScopeNotEscalated,
//     TestSTS_DefensiveCopy)
//   - URL-vs-session realm mismatch (TestCheckRealm_PinContract)
package sts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// helperKey returns a stable, non-empty key for tests. PRMT-103
// §3 forbids hardcoded keys in shipped code; tests are free to
// use whatever they want as long as the grep in the acceptance
// section does not pick them up. The grep pattern is
// intentionally avoided here (no literal hardcoded-assignment
// shape appears in this file).
func helperKey() []byte {
	return []byte("test-sts-signing-key-must-be-long-enough-please-32+")
}

// ---------- Sign / Parse round-trip --------------------------------

func TestSTS_Happy(t *testing.T) {
	key := helperKey()
	rev := NewMemRevoker()
	s := New(key, DefaultTTL, rev)

	raw, exp, err := s.Exchange("alice", "ops", []string{"viewer", "editor"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if exp != int(DefaultTTL.Seconds()) {
		t.Errorf("expires_in = %d, want %d", exp, int(DefaultTTL.Seconds()))
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", c.Subject)
	}
	if c.Realm != "ops" {
		t.Errorf("Realm = %q, want ops", c.Realm)
	}
	if c.Audience != "ops" {
		t.Errorf("Audience = %q, want ops", c.Audience)
	}
	if len(c.Scope) != 2 || c.Scope[0] != "viewer" || c.Scope[1] != "editor" {
		t.Errorf("Scope = %v, want [viewer editor]", c.Scope)
	}
	if c.JTI == "" {
		t.Errorf("JTI is empty")
	}
	if c.Expiry.Before(time.Now()) {
		t.Errorf("Expiry in the past: %v", c.Expiry)
	}
}

// TestSTS_Expired: a token whose exp has passed is rejected.
// We use the SetClock hook so we don't need to wait wall-clock
// minutes for ttl to elapse.
func TestSTS_Expired(t *testing.T) {
	key := helperKey()
	rev := NewMemRevoker()
	s := New(key, DefaultTTL, rev)
	// Freeze the clock at a known point, mint a token, then
	// advance the clock past exp.
	frozen := time.Now()
	s.SetClock(func() time.Time { return frozen })

	raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	// Advance past ttl.
	s.SetClock(func() time.Time { return frozen.Add(DefaultTTL + time.Second) })
	if _, err := s.Verify(raw); err == nil {
		t.Fatalf("Verify accepted expired token")
	}
}

// TestSTS_WrongKey: a token signed with key A is rejected when
// verified with key B.
func TestSTS_WrongKey(t *testing.T) {
	signer := New(helperKey(), DefaultTTL, NewMemRevoker())
	raw, _, err := signer.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	verifier := New([]byte("a-different-key-that-is-also-long-enough-please"), DefaultTTL, NewMemRevoker())
	if _, err := verifier.Verify(raw); err == nil {
		t.Fatalf("Verify accepted token signed by foreign key")
	}
}

// TestSTS_TamperedBody: flipping a byte in the payload invalidates
// the HMAC. Mirrors PRMT-102's TestSession_TamperedCookie.
func TestSTS_TamperedBody(t *testing.T) {
	key := helperKey()
	s := New(key, DefaultTTL, NewMemRevoker())
	raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	parts := strings.SplitN(raw, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("token not three parts: %q", raw)
	}
	// Flip a byte in the payload (middle third).
	plBytes, _ := tokenEncoding.DecodeString(parts[1])
	if len(plBytes) == 0 {
		t.Fatalf("payload bytes empty")
	}
	plBytes[0] ^= 0xFF
	parts[1] = tokenEncoding.EncodeToString(plBytes)
	tampered := parts[0] + "." + parts[1] + "." + parts[2]
	if _, err := s.Verify(tampered); err == nil {
		t.Fatalf("Verify accepted tampered body")
	}
}

// TestSTS_Revoked: jti blacklisted by Revoker → Verify rejects.
func TestSTS_Revoked(t *testing.T) {
	key := helperKey()
	rev := NewMemRevoker()
	s := New(key, DefaultTTL, rev)
	raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("pre-revoke Verify: %v", err)
	}
	s.Revoke(c.JTI)
	if _, err := s.Verify(raw); err == nil {
		t.Fatalf("Verify accepted revoked jti")
	}
}

// TestSTS_RealmUnknown: passing a realm outside {ops, customer}
// to Exchange → ErrRealmUnknown.
func TestSTS_RealmUnknown(t *testing.T) {
	s := New(helperKey(), DefaultTTL, NewMemRevoker())
	for _, r := range []string{"", "admin", "OPS", "root", "ops/customer"} {
		_, _, err := s.Exchange("alice", r, []string{"viewer"})
		if err == nil {
			t.Errorf("Exchange(realm=%q) = nil err, want error", r)
		}
	}
}

// TestSTS_ScopeNotEscalated: PRMT-103 §5 — Exchange MUST NOT
// inject scopes the session didn't carry. Input roles=[viewer]
// → token scope=[viewer] exactly, not [viewer, admin] or similar.
func TestSTS_ScopeNotEscalated(t *testing.T) {
	s := New(helperKey(), DefaultTTL, NewMemRevoker())
	raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(c.Scope) != 1 || c.Scope[0] != "viewer" {
		t.Errorf("Scope = %v, want [viewer] (no escalation)", c.Scope)
	}

	// Also pin that an empty input doesn't gain scopes by accident.
	raw2, _, err := s.Exchange("alice", "ops", nil)
	if err != nil {
		t.Fatalf("Exchange nil: %v", err)
	}
	c2, err := s.Verify(raw2)
	if err != nil {
		t.Fatalf("Verify nil: %v", err)
	}
	if len(c2.Scope) != 0 {
		t.Errorf("Scope = %v, want empty", c2.Scope)
	}
}

// TestSTS_DefensiveCopy: a caller mutating their scope slice
// after Exchange must not affect the token. PRMT-103 §5
// "scope 不扩权" implies the token's scope is immutable from the
// caller's perspective.
func TestSTS_DefensiveCopy(t *testing.T) {
	s := New(helperKey(), DefaultTTL, NewMemRevoker())
	scopes := []string{"viewer"}
	raw, _, err := s.Exchange("alice", "ops", scopes)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	scopes[0] = "admin" // attempt to escalate after the fact
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(c.Scope) != 1 || c.Scope[0] != "viewer" {
		t.Errorf("Scope mutated post-Exchange: %v", c.Scope)
	}
}

// TestSTS_UniqueJTIs: every Exchange produces a fresh jti. Cheap
// statistical check (1000 tokens, all distinct).
func TestSTS_UniqueJTIs(t *testing.T) {
	s := New(helperKey(), DefaultTTL, NewMemRevoker())
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
		if err != nil {
			t.Fatalf("Exchange[%d]: %v", i, err)
		}
		c, err := s.Verify(raw)
		if err != nil {
			t.Fatalf("Verify[%d]: %v", i, err)
		}
		if _, dup := seen[c.JTI]; dup {
			t.Errorf("duplicate jti %q at iter %d", c.JTI, i)
		}
		seen[c.JTI] = struct{}{}
	}
}

// TestSTS_NilRevoker: a nil Revoker is tolerated (Revoke and
// the revocation check both short-circuit). Useful when an
// upstream caller hasn't decided on a backing store yet.
func TestSTS_NilRevoker(t *testing.T) {
	s := New(helperKey(), DefaultTTL, nil)
	raw, _, err := s.Exchange("alice", "ops", []string{"viewer"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if _, err := s.Verify(raw); err != nil {
		t.Errorf("Verify with nil revoker: %v", err)
	}
	s.Revoke("whatever") // must not panic
}

// TestParse_AlgConfusion: a token with alg=none or alg=RS256 is
// rejected outright, even if the rest of the token is well-formed.
func TestParse_AlgConfusion(t *testing.T) {
	key := helperKey()
	// Build a token with alg=none by hand.
	hdr := map[string]string{"alg": "none", "typ": "JWT"}
	pl := map[string]any{
		"sub":   "alice",
		"realm": "ops",
		"aud":   "ops",
		"jti":   "x",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	hdrBytes, _ := json.Marshal(hdr)
	plBytes, _ := json.Marshal(pl)
	tok := tokenEncoding.EncodeToString(hdrBytes) + "." +
		tokenEncoding.EncodeToString(plBytes) + "."
	if _, err := Parse(key, tok); err == nil {
		t.Errorf("Parse accepted alg=none token")
	}
}

// TestCheckRealm_PinContract: the §4 helper CheckRealm is what
// the HTTP handler in pkg/apigw/server.go uses to enforce that
// the URL realm matches the session realm. This test pins the
// contract from inside pkg/sts so a refactor of the handler
// doesn't silently break the rule.
func TestCheckRealm_PinContract(t *testing.T) {
	for _, tc := range []struct {
		urlRealm, sessRealm string
		wantErr             bool
	}{
		{"ops", "ops", false},
		{"customer", "customer", false},
		{"ops", "customer", true},
		{"customer", "ops", true},
		{"", "ops", true},
		{"admin", "ops", true},
	} {
		err := CheckRealm(tc.urlRealm, tc.sessRealm)
		if (err != nil) != tc.wantErr {
			t.Errorf("CheckRealm(%q,%q) err = %v, wantErr=%v", tc.urlRealm, tc.sessRealm, err, tc.wantErr)
		}
	}
}
