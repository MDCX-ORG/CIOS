// Package authn implements the CIOS experience-layer OIDC
// authentication surface (spec-009 §7.1, PRMT-102). It is the
// single IdP-agnostic place where Authorization Code + PKCE
// login, id_token verification (JWKS), and HttpOnly/Secure
// session cookies live. Downstream PRMTs (103 STS, 104 PDP)
// consume Session to make authorization / token-exchange
// decisions; this package itself never makes them.
package authn

import "fmt"

// Realm is one of two authentication contexts the CIOS Portal
// recognises. The realm is decided by the URL prefix the
// browser is on (/auth/ops/* vs /auth/customer/*) and is
// propagated to the session cookie and the id_token issuer
// check (see oidc.go / session.go). Cross-realm replay — e.g.
// an ops-realm state value presented at /auth/customer/callback
// — must be rejected; the handler enforces this in handler.go.
//
// Mapping to provider concepts (spec-009 §7.1):
//   - Keycloak   → realm
//   - Auth0      → organization
//   - Cognito    → user pool
type Realm string

// RealmOps is the operations-team realm (E3.5 运维门户).
const RealmOps Realm = "ops"

// RealmCustomer is the tenant / customer realm (E3.4 客户门户).
const RealmCustomer Realm = "customer"

// sessionCookieName is the name of the session cookie written
// after a successful OIDC login. Realm is encoded INTO the
// cookie value (Session.Realm) so a cookie minted for one
// realm cannot be replayed against the other.
const sessionCookieName = "cios_session"

// stateCookieName is the short-lived name of the cookie that
// carries the OIDC state/nonce/PKCE-verifier triple between
// /login and /callback. It is HttpOnly+Secure+SameSite=Lax
// like the session cookie and is cleared on /callback.
const stateCookieName = "cios_oidc_state"

// ErrUnknownRealm is returned by ParseRealm for any value
// other than "ops" or "customer". The handler translates this
// to a 401 RFC7807 "unauthorized" problem (spec-004 §4).
var ErrUnknownRealm = fmt.Errorf("authn: unknown realm")

// ParseRealm validates a route-supplied realm string and
// returns the corresponding Realm constant, or ErrUnknownRealm
// if the value is anything else. This is the only public way
// to obtain a Realm — direct casts of unknown strings would
// defeat the validation the handler depends on.
func ParseRealm(s string) (Realm, error) {
	switch s {
	case string(RealmOps):
		return RealmOps, nil
	case string(RealmCustomer):
		return RealmCustomer, nil
	default:
		return "", ErrUnknownRealm
	}
}

// String makes Realm satisfy fmt.Stringer so it can be
// embedded in logs and errors without leaking the underlying
// type name.
func (r Realm) String() string { return string(r) }
