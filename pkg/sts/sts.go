// pkg/sts/sts.go — STS struct and Exchange / Verify.
//
// PRMT-103 §4 pins the surface: STS holds a signing key, a TTL,
// and a Revoker; Exchange takes a verified session and mints a
// scoped API token; Verify checks signature, expiry, and the
// revocation list.
//
// The struct is intentionally tiny. There is no caching (every
// Exchange mints a fresh jti) and no goroutine (revocation is
// caller-driven).
package sts

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// STS mints and verifies scoped API tokens. Construct with New;
// pass the same instance to every HTTP handler that needs to
// issue or verify tokens so the Revoker is shared.
//
// STS is safe for concurrent use: the underlying Revoker is
// goroutine-safe (RWMutex in memRevoker) and Sign / Parse are
// pure functions of (key, claims) / (key, raw).
type STS struct {
	key []byte
	ttl time.Duration
	rev Revoker
	// now is overridable for tests; nil ⇒ time.Now. Not part of
	// the §4 interface surface; safe to add without changing the
	// public contract.
	now func() time.Time
}

// New constructs an STS. key is reused across Sign and Parse so
// both halves of the round-trip must see the same bytes. ttl is
// the lifetime of every freshly-minted token; rev is the
// revocation backing store (use NewMemRevoker for the in-memory
// implementation).
//
// PRMT-103 §3 forbids hardcoded keys; New returns an error on an
// empty key so a buggy caller fails fast.
func New(key []byte, ttl time.Duration, rev Revoker) *STS {
	return &STS{key: key, ttl: ttl, rev: rev, now: time.Now}
}

// SetClock lets tests substitute a deterministic clock. Returns
// the receiver so a test can write sts.New(...).SetClock(fake).
// Not part of the §4 surface.
func (s *STS) SetClock(now func() time.Time) *STS {
	s.now = now
	return s
}

// DefaultTTL is the spec-pinned token lifetime when none is
// supplied. PRMT-103 §5 says "short TTL (default 15m)".
const DefaultTTL = 15 * time.Minute

// realmAllowlist mirrors pkg/authn's realm set. We keep the
// slice here (instead of importing authn) so the dependency
// arrow stays apigw → {authn, sts}, never authn ↔ sts.
var realmAllowlist = map[string]struct{}{
	"ops":      {},
	"customer": {},
}

// ErrRealmUnknown is returned by Exchange when the supplied
// realm is not in {ops, customer}. The HTTP handler translates
// this to 403 (PRMT-103 §2-bis).
var ErrRealmUnknown = errors.New("sts: realm not in {ops, customer}")

// CheckRealm enforces that the URL-supplied realm matches the
// session's realm and that both are in the allowlist. The
// /auth/{realm}/token handler calls this once per request
// before invoking Exchange, so a session minted for one realm
// cannot be presented at the other realm's token endpoint
// (PRMT-103 §2-bis: "realm 与 session.Realm 不符 → 403").
//
// Both arguments must be in {"ops", "customer"}; any other
// value returns ErrRealmUnknown (handler → 403). Equal values
// return nil.
func CheckRealm(urlRealm, sessionRealm string) error {
	if urlRealm != sessionRealm {
		return ErrRealmUnknown
	}
	if _, ok := realmAllowlist[urlRealm]; !ok {
		return ErrRealmUnknown
	}
	return nil
}

// Exchange mints a scoped API token. Inputs:
//
//   - sub   : session subject (OIDC sub).
//   - realm : session realm; must be "ops" or "customer" and
//     must match the realm on the URL path (the HTTP
//     handler enforces the second condition; this
//     function enforces the first).
//   - scope : roles carried by the session. Copied verbatim
//     into the token — Exchange MUST NOT add scopes the
//     session didn't carry (PRMT-103 §5 / §6).
//
// Returns the compact JWS, the lifetime in seconds, and any
// error. On success the token's aud = realm.
func (s *STS) Exchange(sub, realm string, scope []string) (string, int, error) {
	if _, ok := realmAllowlist[realm]; !ok {
		return "", 0, ErrRealmUnknown
	}
	if sub == "" {
		return "", 0, errors.New("sts: subject is empty")
	}
	// Defensive copy of scope so a caller mutating their slice
	// after Exchange returns can't reach into our token. Scope
	// is small (a handful of role strings) so the allocation is
	// negligible.
	scopes := make([]string, len(scope))
	copy(scopes, scope)

	jti, err := newJTI()
	if err != nil {
		return "", 0, fmt.Errorf("sts: mint jti: %w", err)
	}
	now := s.now()
	raw, err := Sign(s.key, TokenClaims{
		Subject:  sub,
		Realm:    realm,
		Audience: realm,
		Scope:    scopes,
		JTI:      jti,
		Expiry:   now.Add(s.ttl),
	})
	if err != nil {
		return "", 0, fmt.Errorf("sts: sign: %w", err)
	}
	return raw, int(s.ttl.Seconds()), nil
}

// Verify parses a raw token, checks exp, and consults the
// Revoker. Any failure → a non-nil error; the caller MUST NOT
// inspect the underlying error to make authorization decisions
// (PRMT-103 §6).
func (s *STS) Verify(raw string) (TokenClaims, error) {
	c, err := Parse(s.key, raw)
	if err != nil {
		return TokenClaims{}, err
	}
	// Honour the injected clock for the exp check so tests can
	// freeze time without sleeping. Parse uses time.Now()
	// internally; we re-check exp here against s.now() and
	// reject anything whose Expiry has passed per the injected
	// clock.
	if !c.Expiry.After(s.now()) {
		return TokenClaims{}, ErrTokenInvalid
	}
	if _, ok := realmAllowlist[c.Realm]; !ok {
		return TokenClaims{}, ErrTokenInvalid
	}
	if s.rev != nil && s.rev.IsRevoked(c.JTI) {
		return TokenClaims{}, ErrTokenInvalid
	}
	return c, nil
}

// Revoke marks the given jti as revoked. Convenience wrapper so
// HTTP handlers don't have to reach through s.rev.
func (s *STS) Revoke(jti string) {
	if s.rev != nil {
		s.rev.Revoke(jti)
	}
}

// newJTI returns a 16-byte random identifier, base64url-encoded.
// 16 bytes (128 bits) is more than enough entropy for an in-
// process token identifier; the JTI only needs to be unique
// across the revocation window (≤ ttl).
func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
