// pkg/sts/client_credentials.go — OAuth2 client_credentials grant on
// top of the PRMT-103 STS.
//
// PRMT-108 §1 / §2: portal tokens (PRMT-103) and CLI bearer tokens
// (this file) must be minted by the SAME STS so revocation,
// TTL, and jti handling stay unified. CLI does not carry a
// session cookie — it presents a service-account client_id /
// client_secret — so we add a separate IssueClientCredentials entry
// point and a separate HTTP route. The signing/TTL/jti/Revoker
// path is exactly the same: IssueClientCredentials builds a
// TokenClaims and calls into the same Sign function PRMT-103 uses,
// so a Verify call on the resulting token is indistinguishable
// from a portal-issued one.
//
// Security posture (PRMT-108 §5 MUST / §6 MUST NOT):
//   - Secret comparison goes through SecretHash (HMAC-SHA256 keyed
//     by the STS signing key); we never store or compare plaintext.
//   - Requested scopes are intersected with the account's MaxScope
//     rather than copied verbatim. If a requested scope is NOT a
//     subset of MaxScope, the call is rejected with ErrScopeExceeded
//     (handler → 403). We do NOT silently truncate to the empty
//     set, and we do NOT widen MaxScope with the request.
//   - No new third-party dependencies (PRMT-108 §6).
package sts

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
)

// ServiceAccount is the per-client record held by the gateway.
// Secrets live in hashed form (SecretHash); plain secret is never
// persisted or compared. MaxScope is the upper bound of scope the
// account may ever receive — IssueClientCredentials intersects the
// request against this set and refuses to widen.
//
// PRMT-108 §4 pins the field set exactly: ClientID, SecretHash,
// MaxScope, Realm. Realm defaults to "ops" because machine clients
// are operator-side; the gateway may host a different realm later
// (e.g. customer-side batch jobs) by setting Realm per account.
type ServiceAccount struct {
	ClientID   string
	SecretHash []byte
	MaxScope   []string
	Realm      string
}

// AccountStore is the lookup surface IssueClientCredentials reads
// against. The interface is intentionally narrow so a config-file
// implementation, a Vault-backed one, or a database one can all
// satisfy it without taking on a dependency on this package.
//
// Lookup returns (account, true) on hit and (_, false) on miss.
// A miss MUST be indistinguishable from a wrong-secret hit from the
// handler's point of view: both translate to 401.
type AccountStore interface {
	Lookup(clientID string) (ServiceAccount, bool)
}

// ErrBadCredentials is returned when the client_id is unknown OR
// the supplied secret does not match the stored hash. The two
// cases collapse to one error so the handler (and any audit log
// that observes it) cannot distinguish "unknown client" from
// "wrong secret" — that distinction would leak the existence of
// client_ids to a probing caller.
//
// PRMT-108 §4 mandates this exact symbol.
var ErrBadCredentials = errors.New("invalid client credentials")

// ErrScopeExceeded is returned when at least one requested scope
// is not in the account's MaxScope. The handler translates this
// to 403 (the caller's identity is verified; the request simply
// asks for more than the account is allowed).
//
// PRMT-108 §4 mandates this exact symbol; §5 spells out the
// non-silent-truncate semantics.
var ErrScopeExceeded = errors.New("requested scope exceeds account maximum")

// HashSecret returns the keyed hash used as SecretHash. The HMAC
// key is the STS signing key so two STS instances configured with
// the same key agree on what the hash is. The same key is used to
// Sign tokens, which keeps the hash check inside the same trust
// boundary as token verification (one operator-controlled secret
// bootstraps both halves).
//
// HashSecret is exported so a config loader (main.go or a future
// PRMT) can pre-compute hashes at install time without holding
// plaintext in memory longer than necessary.
func HashSecret(key []byte, secret string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}

// IssueClientCredentials validates a client_id / secret pair against
// store and, on success, mints a scoped API token through the same
// STS signing path PRMT-103 uses.
//
// Semantics (PRMT-108 §4):
//
//   - client_id unknown → ErrBadCredentials (handler → 401).
//   - secret hash mismatch → ErrBadCredentials (handler → 401).
//   - requested scope ⊄ MaxScope → ErrScopeExceeded (handler → 403).
//   - happy path → token signed with s.key, TTL = s.ttl, jti fresh,
//     realm = account.Realm, scope = reqScope (already proven
//     subset of MaxScope by the check above), subject = client_id.
//
// The returned expires_in is in seconds and matches PRMT-103's
// Exchange output format so the /auth/token handler emits the
// same JSON shape as /auth/{realm}/token.
func (s *STS) IssueClientCredentials(store AccountStore, clientID, secret string, reqScope []string) (string, int, error) {
	if store == nil {
		return "", 0, ErrBadCredentials
	}
	if clientID == "" || secret == "" {
		return "", 0, ErrBadCredentials
	}
	acct, ok := store.Lookup(clientID)
	if !ok {
		return "", 0, ErrBadCredentials
	}
	// Hash comparison: same trust boundary as the rest of the
	// package (HMAC keyed by the STS signing key), constant-time
	// via subtle.ConstantTimeCompare.
	want := HashSecret(s.key, secret)
	if len(acct.SecretHash) == 0 || subtle.ConstantTimeCompare(acct.SecretHash, want) != 1 {
		return "", 0, ErrBadCredentials
	}
	// Scope check BEFORE signing: fail closed on out-of-bounds
	// requests (PRMT-108 §5 "不静默裁剪到空也不扩权"). An empty
	// reqScope is always allowed (intersection of empty set with
	// any set is empty set).
	for _, want := range reqScope {
		if !scopeContains(acct.MaxScope, want) {
			return "", 0, ErrScopeExceeded
		}
	}
	// Realm defaults to "ops" if the account didn't pin one — the
	// prompt's §4 notes "通常 ops (机器侧)" and TokenClaims.Realm
	// must be in the realmAllowlist, which PRMT-103 enforces at
	// Verify time. Empty Realm would fail Verify, so substitute.
	realm := acct.Realm
	if realm == "" {
		realm = "ops"
	}
	// Reuse Exchange's exact path: same defensive copy, same
	// Sign call, same ttl handling. This guarantees the issued
	// token is byte-identical in shape to a portal token of the
	// same scope, so Verify / Revoke are uniform across issuance
	// sources (PRMT-108 §1 "与门户 token 同一吊销面").
	return s.Exchange(clientID, realm, reqScope)
}

// scopeContains reports whether set contains s. Linear scan; sets
// are small (a handful of role strings) so we don't need a map.
// Lives here (not in Exchange) because Exchange copies scopes
// verbatim and does not own an allowlist to check against.
func scopeContains(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
