// pkg/sts/client_credentials_test.go — table-driven tests for
// IssueClientCredentials (PRMT-108 §5 acceptance).
//
// Coverage map (every MUST from §5 has at least one test):
//
//   - happy path → 200-shaped token that Verify accepts
//     (TestCC_Happy)
//   - bad secret → ErrBadCredentials
//     (TestCC_BadSecret)
//   - scope exceeds MaxScope → ErrScopeExceeded
//     (TestCC_ScopeExceeded)
//   - issued token passes Verify under the same STS
//     (TestCC_Happy, TestCC_VerifyAfterIssue)
//   - issued token can be revoked and Verify rejects it
//     (TestCC_RevokeAfterIssue)
//   - unknown client_id → ErrBadCredentials (indistinguishable
//     from wrong secret; TestCC_UnknownClient)
//   - empty inputs → ErrBadCredentials (TestCC_EmptyInputs)
//   - empty request scope against a non-empty MaxScope is allowed
//     (TestCC_EmptyRequestScope)
//   - hash helper matches both ways on the same secret/key
//     (TestHashSecret_Stable)
package sts

import (
	"crypto/subtle"
	"testing"
)

// memStore is a tiny AccountStore used by these tests. It is
// process-local and never escapes the test binary, so we can
// safely put hashed bytes in via init / per-test setup.
type memStore struct {
	byID map[string]ServiceAccount
}

func (m *memStore) Lookup(clientID string) (ServiceAccount, bool) {
	a, ok := m.byID[clientID]
	return a, ok
}

// newCCSTS returns an STS pre-configured with helperKey() and an
// in-memory Revoker — the same shape every sts_test.go test uses.
func newCCSTS(t *testing.T) *STS {
	t.Helper()
	return New(helperKey(), DefaultTTL, NewMemRevoker())
}

// acct builds a ServiceAccount whose SecretHash is computed by
// HashSecret under the STS key. This mirrors how a config loader
// would pre-compute the hash at install time.
func acct(t *testing.T, s *STS, clientID, secret string, maxScope []string, realm string) ServiceAccount {
	t.Helper()
	if realm == "" {
		realm = "ops"
	}
	return ServiceAccount{
		ClientID:   clientID,
		SecretHash: HashSecret(s.key, secret),
		MaxScope:   maxScope,
		Realm:      realm,
	}
}

// TestCC_Happy: a well-formed request against a configured account
// produces a token that the same STS can Verify, and the verified
// claims carry the requested scope (which was a subset of MaxScope).
func TestCC_Happy(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer", "editor"}, ""),
	}}
	raw, exp, err := s.IssueClientCredentials(store, "cli-bot", "s3cret", []string{"viewer"})
	if err != nil {
		t.Fatalf("IssueClientCredentials: %v", err)
	}
	if exp != int(DefaultTTL.Seconds()) {
		t.Errorf("expires_in = %d, want %d", exp, int(DefaultTTL.Seconds()))
	}
	if raw == "" {
		t.Fatalf("raw token is empty")
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "cli-bot" {
		t.Errorf("Subject = %q, want cli-bot", c.Subject)
	}
	if c.Realm != "ops" {
		t.Errorf("Realm = %q, want ops", c.Realm)
	}
	if len(c.Scope) != 1 || c.Scope[0] != "viewer" {
		t.Errorf("Scope = %v, want [viewer]", c.Scope)
	}
	if c.JTI == "" {
		t.Errorf("JTI is empty")
	}
}

// TestCC_BadSecret: wrong secret → ErrBadCredentials, no token
// emitted. PRMT-108 §5 "凭据错误 → 401".
func TestCC_BadSecret(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer"}, ""),
	}}
	_, _, err := s.IssueClientCredentials(store, "cli-bot", "wrong", []string{"viewer"})
	if err == nil {
		t.Fatalf("IssueClientCredentials with wrong secret: nil err")
	}
	if err != ErrBadCredentials {
		t.Errorf("err = %v, want ErrBadCredentials", err)
	}
}

// TestCC_UnknownClient: client_id not in the store → ErrBadCredentials
// (indistinguishable from wrong-secret per PRMT-108 §4 — the
// handler cannot distinguish "no such account" from "bad secret",
// so both paths must surface the same sentinel).
func TestCC_UnknownClient(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer"}, ""),
	}}
	_, _, err := s.IssueClientCredentials(store, "does-not-exist", "s3cret", []string{"viewer"})
	if err != ErrBadCredentials {
		t.Errorf("err = %v, want ErrBadCredentials", err)
	}
}

// TestCC_EmptyInputs: empty client_id or secret → ErrBadCredentials.
// Catches a caller that hands in zero values from a misconfigured
// env (e.g. CIOS_STS_SERVICE_ACCOUNTS not loaded).
func TestCC_EmptyInputs(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer"}, ""),
	}}
	for _, tc := range []struct{ cid, sec string }{
		{"", "s3cret"},
		{"cli-bot", ""},
		{"", ""},
	} {
		if _, _, err := s.IssueClientCredentials(store, tc.cid, tc.sec, nil); err != ErrBadCredentials {
			t.Errorf("IssueClientCredentials(%q,%q) err = %v, want ErrBadCredentials", tc.cid, tc.sec, err)
		}
	}
}

// TestCC_ScopeExceeded: a requested scope not in MaxScope →
// ErrScopeExceeded, no token emitted. PRMT-108 §5 "请求 scope
// 超过该账号允许的最大 scope → 403 (不静默裁剪到空也不扩权)".
func TestCC_ScopeExceeded(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer"}, ""),
	}}
	// "admin" not in MaxScope=[viewer] → must reject even though
	// the request also asks for the legitimate "viewer".
	_, _, err := s.IssueClientCredentials(store, "cli-bot", "s3cret", []string{"viewer", "admin"})
	if err != ErrScopeExceeded {
		t.Errorf("err = %v, want ErrScopeExceeded", err)
	}
	// Single out-of-bounds scope → also reject.
	_, _, err = s.IssueClientCredentials(store, "cli-bot", "s3cret", []string{"admin"})
	if err != ErrScopeExceeded {
		t.Errorf("err = %v, want ErrScopeExceeded", err)
	}
}

// TestCC_EmptyRequestScope: requesting zero scope is allowed when
// MaxScope is non-empty (the intersection of {} with any set is
// {}, which is trivially a subset). Mirrors PRMT-103's
// TestSTS_ScopeNotEscalated "empty input doesn't gain scopes".
func TestCC_EmptyRequestScope(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer", "editor"}, ""),
	}}
	raw, _, err := s.IssueClientCredentials(store, "cli-bot", "s3cret", nil)
	if err != nil {
		t.Fatalf("IssueClientCredentials(nil scope): %v", err)
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(c.Scope) != 0 {
		t.Errorf("Scope = %v, want empty", c.Scope)
	}
}

// TestCC_RevokeAfterIssue: a token minted via IssueClientCredentials
// shares the same Revoker as a portal-issued token (PRMT-108 §1
// "同一吊销面"). We mint, Verify (success), Revoke the jti, then
// re-Verify and expect failure.
func TestCC_RevokeAfterIssue(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer"}, ""),
	}}
	raw, _, err := s.IssueClientCredentials(store, "cli-bot", "s3cret", []string{"viewer"})
	if err != nil {
		t.Fatalf("IssueClientCredentials: %v", err)
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

// TestCC_VerifyAfterIssue: the canonical "issued token is
// indistinguishable from a portal-issued one" assertion. We mint
// via IssueClientCredentials and check that Verify passes with no
// special-cased path; the only difference from a portal token is
// the Subject (client_id vs OIDC sub), which Verify doesn't care
// about for the happy path.
func TestCC_VerifyAfterIssue(t *testing.T) {
	s := newCCSTS(t)
	store := &memStore{byID: map[string]ServiceAccount{
		"cli-bot": acct(t, s, "cli-bot", "s3cret", []string{"viewer", "editor"}, ""),
	}}
	raw, _, err := s.IssueClientCredentials(store, "cli-bot", "s3cret", []string{"viewer", "editor"})
	if err != nil {
		t.Fatalf("IssueClientCredentials: %v", err)
	}
	c, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "cli-bot" || c.Realm != "ops" || c.Audience != "ops" {
		t.Errorf("claims not as expected: %+v", c)
	}
	if len(c.Scope) != 2 {
		t.Errorf("Scope = %v, want 2 entries", c.Scope)
	}
}

// TestCC_NilStore: nil store → ErrBadCredentials (no panic).
// Defends against a NewServer that forgot to wire an account
// store before serving /auth/token.
func TestCC_NilStore(t *testing.T) {
	s := newCCSTS(t)
	_, _, err := s.IssueClientCredentials(nil, "cli-bot", "s3cret", nil)
	if err != ErrBadCredentials {
		t.Errorf("err = %v, want ErrBadCredentials", err)
	}
}

// TestHashSecret_Stable: same key + same secret → same hash; the
// hash is not portable across keys (deliberately — keeps the
// trust boundary closed). Used by config loaders to pre-compute
// hashes, so determinism is part of the contract.
func TestHashSecret_Stable(t *testing.T) {
	k := helperKey()
	a := HashSecret(k, "s3cret")
	b := HashSecret(k, "s3cret")
	if len(a) == 0 {
		t.Fatalf("hash is empty")
	}
	if subtle.ConstantTimeCompare(a, b) != 1 {
		t.Errorf("same key+secret produced different hashes")
	}
	c := HashSecret([]byte("a-different-key-that-is-also-long-enough-please"), "s3cret")
	if subtle.ConstantTimeCompare(a, c) == 1 {
		t.Errorf("different key produced same hash (boundary violated)")
	}
}
