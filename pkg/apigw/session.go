// Session seam: a minimal in-package projection of a verified
// session cookie, decoupled from pkg/authn.
//
// PRMT-117 R2: pkg/apigw previously imported pkg/authn solely to
// decode the session cookie at /auth/{realm}/token — that
// dependency inverts spec-006 §5's package direction (apigw
// depends on authn rather than the other way round). The R1
// attempt to relocate `Session` into a new package hit an
// import cycle (Session's internal Realm/Claims still live in
// authn). R2 inverts the seam instead: apigw declares the
// minimum projection it actually consumes (Subject/Realm/Claims),
// and main.go (which legitimately imports both packages) wires
// a SessionDecoder that adapts authn.DecodeSession.
//
// Production pkg/apigw therefore has no import of pkg/authn;
// behavior at /auth/{realm}/token is unchanged because the
// adapter passes the same cookie through the same
// authn.DecodeSession code path with the same key.
package apigw

// SessionInfo is the apigw-local minimal projection of a
// verified session. It carries exactly the three fields
// handleToken (and sessionRoles) need:
//
//   - Subject : the OIDC `sub` claim (passed to STS.Exchange).
//   - Realm   : the realm string ("ops" | "customer") the
//     session was minted for; already validated by
//     the decoder. We keep it as a plain string so
//     apigw doesn't need the authn.Realm type.
//   - Claims  : the verified id_token claims, read-only by
//     convention (the decoder's caller MUST NOT
//     mutate the returned map).
type SessionInfo struct {
	Subject string
	Realm   string
	Claims  map[string]any
}

// SessionDecoder verifies and decodes a session cookie value,
// returning the SessionInfo projection above. Implementations
// are supplied by the composition root (cmd/cios-apigw/main.go)
// which adapts pkg/authn.DecodeSession; nil means "not
// configured" and produces a 500 at /auth/{realm}/token.
//
// The error return covers every failure mode (bad version,
// HMAC mismatch, malformed body, expired, unknown realm) —
// callers MUST NOT branch on the underlying error and MUST NOT
// surface it to the browser (spec-004 §4 — no information
// leakage about why a cookie was rejected).
type SessionDecoder func(key []byte, cookie string) (SessionInfo, error)

// Local realm string constants. These mirror the literal values
// of authn.RealmOps / authn.RealmCustomer ("ops" and "customer")
// without importing pkg/authn. If pkg/authn's realm set ever
// changes, these constants must move with it; the duplication
// is the price of the seam.
const (
	realmOps      = "ops"
	realmCustomer = "customer"
)
