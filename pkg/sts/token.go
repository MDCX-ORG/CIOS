// Package sts implements the CIOS experience-layer Security Token
// Service (spec-009 §7.1, PRMT-103). It is a gateway-side, IdP-
// agnostic token exchange: given an already-verified Session
// (pkg/authn) it mints a short-TTL signed JWT that downstream
// PRMTs (PRMT-104 PDP, PRMT-105 v1 RBAC, PRMT-107 Omniverse)
// consume.
//
// Scope (PRMT-103 §1, §2):
//   - JWT HS256 sign / verify with claims {sub, realm, aud,
//     scope, exp, jti}. RS256 left as OPEN per §8.
//   - Revocation hook (Revoker interface, in-memory impl) so a
//     jti can be invalidated before exp.
//   - No authorization decisions (allow/deny = PRMT-104 OPA).
//   - No third-party JWT lib (PRMT-102 §6 prohibits them; we
//     follow the same stdlib-only discipline here).
//
// Out of scope (PRMT-103 §6 MUST NOT):
//   - Resource-scope RBAC (L34/L50 still authoritative via
//     PRMT-105 path-glob).
//   - RFC 8693 token exchange — the gateway STS replaces the
//     IdP-native variant to remain IdP-agnostic.
package sts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TokenClaims is the public claim set carried by an API token.
// Field semantics (PRMT-103 §4):
//
//   - Subject  : OIDC `sub` (session subject).
//   - Realm    : "ops" | "customer" — kept consistent with
//     Session.Realm (pkg/authn). The API token cannot
//     span realms.
//   - Audience : same as Realm in this PRMT (a future PRMT can
//     split ops/customer audiences further if a third
//     verifier joins).
//   - Scope    : minimum set of role strings copied verbatim
//     from session.Claims. The exchange MUST NOT add
//     scopes the session didn't carry (PRMT-103 §5
//     "scope 不扩权").
//   - JTI      : per-token unique identifier, used by the
//     Revoker hook.
//   - Expiry   : absolute UTC expiry. Verify rejects anything
//     whose Expiry has passed.
//   - Tenant        : tenant id; sourced from the tenant record at
//     Exchange time (PRMT-109 §4). Empty means
//     "no tenant scope" — pkg/tenant's
//     TenantFromClaims treats this as fail-closed
//     (PRMT-109 §5).
//   - IsolationTier : "db" | "row" | "label" (PRMT-109 §4).
//     Sourced from the same tenant record. Any
//     value outside that allowlist is rejected by
//     pkg/tenant.ParseTier (PRMT-109 §5 fail-closed).
//   - Org           : Organization the identity belongs to under
//     the tenant (L84 / D35; PRMT-110 §4). Carries
//     the cross-site grouping needed by the R6
//     site switcher. Empty means "no org claim" —
//     pkg/tenant.OrgFromClaims treats this as
//     fail-closed for site switching (PRMT-110 §5).
//   - Sites         : site codes (L36) reachable by the identity
//     under the named Org. PRMT-110 §5: site-set
//     switching is fail-closed; an empty or absent
//     Sites list means the identity cannot switch
//     to any site. Resource scope inside a site
//     remains core RBAC's job (L81 red line).
type TokenClaims struct {
	Subject       string    `json:"sub"`
	Realm         string    `json:"realm"`
	Audience      string    `json:"aud"`
	Scope         []string  `json:"scope,omitempty"`
	JTI           string    `json:"jti"`
	Expiry        time.Time `json:"exp"`
	Tenant        string    `json:"tenant,omitempty"`
	IsolationTier string    `json:"tier,omitempty"`
	Org           string    `json:"org,omitempty"`
	Sites         []string  `json:"sites,omitempty"`
}

// tokenHeader is the JOSE header for our HS256 tokens. We pin
// alg=HS256 and typ=JWT so a Verify call cannot be tricked into
// accepting a none/alg-confusion token by a maliciously-crafted
// header. (RFC 7519 §5.1 requires typ optional; we set it for
// clarity in Wireshark-style logs.)
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// tokenEncoding mirrors pkg/authn/session.go's encoding choice
// (base64url, no padding) so cookies / Authorization headers can
// all share one tokenizer.
var tokenEncoding = base64.RawURLEncoding

// ErrTokenInvalid is the umbrella error returned by Parse for any
// failure: bad shape, bad base64, bad signature, expired. Callers
// MUST NOT inspect the underlying error to make authorization
// decisions — that would leak which failure path the verifier
// took. PRMT-103 §6 explicitly bans authz decisions in this
// package; surface area is identity-only.
var ErrTokenInvalid = errors.New("sts: token invalid")

// Sign produces an HS256-signed compact JWS for c. The signature
// is HMAC-SHA256(key, header_b64 + "." + payload_b64).
//
// Empty key is rejected as defence-in-depth so a buggy caller
// cannot accidentally mint unverifiable tokens (cf. PRMT-102
// session.go for the same discipline).
func Sign(key []byte, c TokenClaims) (string, error) {
	if len(key) == 0 {
		return "", errors.New("sts: signing key is empty")
	}
	if c.Subject == "" {
		return "", errors.New("sts: Subject is empty")
	}
	if c.Realm == "" {
		return "", errors.New("sts: Realm is empty")
	}
	if c.JTI == "" {
		return "", errors.New("sts: JTI is empty")
	}
	if c.Expiry.IsZero() {
		return "", errors.New("sts: Expiry is zero")
	}
	hdr := tokenHeader{Alg: "HS256", Typ: "JWT"}
	hdrBytes, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("sts: marshal header: %w", err)
	}
	// exp is emitted as unix-seconds (RFC 7519 §4.1.4); the rest
	// of the claim fields use the natural Go JSON encoding. We
	// use a wire struct so the JSON shape is pinned (the test
	// suite asserts on field presence).
	type wireClaims struct {
		Sub   string   `json:"sub"`
		Realm string   `json:"realm"`
		Aud   string   `json:"aud"`
		Scope []string `json:"scope,omitempty"`
		JTI   string   `json:"jti"`
		Exp   int64    `json:"exp"`
		Iat   int64    `json:"iat,omitempty"`
		// PRMT-109 §4: tenant + isolation_tier passthrough. Both
		// are sourced from the tenant record upstream (PRMT-104 /
		// a future /v1/tenants) and forwarded verbatim; pkg/sts
		// does not validate or transform them. The `omitempty`
		// matches TokenClaims so a pre-PRMT-109 token (no
		// tenant/tier fields) round-trips byte-identical.
		// PRMT-110 §4: org + sites passthrough. Same discipline
		// as tenant/tier — sourced upstream from the IdP / CIOS
		// org table and forwarded verbatim. pkg/sts does not
		// validate the org name or the site set; the PDP +
		// pkg/tenant.OrgFromClaims own the fail-closed contract.
		Tenant string   `json:"tenant,omitempty"`
		Tier   string   `json:"tier,omitempty"`
		Org    string   `json:"org,omitempty"`
		Sites  []string `json:"sites,omitempty"`
	}
	wc := wireClaims{
		Sub:    c.Subject,
		Realm:  c.Realm,
		Aud:    c.Audience,
		Scope:  c.Scope,
		JTI:    c.JTI,
		Exp:    c.Expiry.Unix(),
		Iat:    time.Now().Unix(),
		Tenant: c.Tenant,
		Tier:   c.IsolationTier,
		Org:    c.Org,
		Sites:  c.Sites,
	}
	payloadBytes, err := json.Marshal(wc)
	if err != nil {
		return "", fmt.Errorf("sts: marshal claims: %w", err)
	}
	hdrB64 := tokenEncoding.EncodeToString(hdrBytes)
	plB64 := tokenEncoding.EncodeToString(payloadBytes)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(hdrB64))
	mac.Write([]byte{'.'})
	mac.Write([]byte(plB64))
	sigB64 := tokenEncoding.EncodeToString(mac.Sum(nil))
	return hdrB64 + "." + plB64 + "." + sigB64, nil
}

// Parse verifies the signature, decodes the claims, and checks
// exp > now. Any failure collapses to ErrTokenInvalid — see the
// rationale on ErrTokenInvalid.
//
// Callers that need a per-reason audit log can wrap Parse and
// re-run with logging instrumentation; PRMT-103 forbids leaking
// the reason to the response body.
func Parse(key []byte, raw string) (TokenClaims, error) {
	if len(key) == 0 {
		return TokenClaims{}, ErrTokenInvalid
	}
	parts := splitDots(raw)
	if len(parts) != 3 {
		return TokenClaims{}, ErrTokenInvalid
	}
	hdrBytes, err := tokenEncoding.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	var hdr tokenHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	// Reject anything but HS256. RFC 7519 §6 / CVE-2015-9235
	// ("alg=none") — even though we only ever Sign HS256, an
	// attacker could present a forged token with alg=none or
	// alg=RS256 pointing at a public key as the HMAC secret.
	if hdr.Alg != "HS256" {
		return TokenClaims{}, ErrTokenInvalid
	}
	payloadBytes, err := tokenEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	// Wire-side struct so we can pull exp as int64 and the rest
	// as natural JSON values.
	var wire struct {
		Sub    string   `json:"sub"`
		Realm  string   `json:"realm"`
		Aud    string   `json:"aud"`
		Scope  []string `json:"scope"`
		JTI    string   `json:"jti"`
		Exp    int64    `json:"exp"`
		Tenant string   `json:"tenant"`
		Tier   string   `json:"tier"`
		Org    string   `json:"org"`
		Sites  []string `json:"sites"`
	}
	if err := json.Unmarshal(payloadBytes, &wire); err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	sigBytes, err := tokenEncoding.DecodeString(parts[2])
	if err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	mac.Write([]byte{'.'})
	mac.Write([]byte(parts[1]))
	// hmac.Equal is constant-time so callers can't probe
	// signature prefixes by timing (PRMT-103 §6 same discipline
	// as PRMT-102 session decode).
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return TokenClaims{}, ErrTokenInvalid
	}
	if wire.Sub == "" || wire.Realm == "" || wire.JTI == "" {
		return TokenClaims{}, ErrTokenInvalid
	}
	if wire.Exp <= time.Now().Unix() {
		return TokenClaims{}, ErrTokenInvalid
	}
	return TokenClaims{
		Subject:       wire.Sub,
		Realm:         wire.Realm,
		Audience:      wire.Aud,
		Scope:         wire.Scope,
		JTI:           wire.JTI,
		Expiry:        time.Unix(wire.Exp, 0).UTC(),
		Tenant:        wire.Tenant,
		IsolationTier: wire.Tier,
		Org:           wire.Org,
		Sites:         wire.Sites,
	}, nil
}

// splitDots mirrors pkg/authn/session.go's helper. It exists so
// pkg/sts does not need to import pkg/authn (the dependency arrow
// is the other way: apigw → both, sts → stdlib only).
func splitDots(s string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	if len(out) > 3 {
		return out[:3]
	}
	return out
}
