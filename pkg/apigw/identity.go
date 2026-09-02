// Identity: per-request carrier for the verified caller identity
// produced by AuthMiddleware (PRMT-104). PRMT-105 §4 pins the
// contract: AuthMiddleware injects the sts.TokenClaims via
// WithClaims immediately after sts.Verify succeeds; downstream
// handlers (handleSites) recover them via ClaimsFrom and forward
// them to core /v1 through Upstream.GetV1As. This is the ONLY
// mechanism by which caller identity crosses the Gateway → core
// boundary — the Gateway holds no resource-scope logic of its
// own (spec-009 §7.1 red line; L34/L50 authority stays in core).
//
// PRMT-114: in addition to the verified claims, AuthMiddleware
// injects the raw JWS bearer via WithRawToken so upstream.GetV1As
// / GetV1AsTenant can forward a verifiable token to core /v1
// (rather than the bare claims.Subject string). The Gateway still
// does not re-sign or re-scope; rawToken is the original STS-
// issued JWS, carried as opaque text.
package apigw

import (
	"context"

	"github.com/yurimeng/cios/pkg/sts"
)

// ctxKey is a private type so external packages cannot collide
// with our context keys by accident. PRMT-105 §4 fixes the
// concrete value (claimsKey = 0); the type itself is unexported
// to keep the keyspace closed. PRMT-114 §4 adds a sibling key
// (rawTokenKey = 1) for the original JWS bearer — independent
// from claims so the two are never conflated.
type ctxKey int

const (
	claimsKey   ctxKey = 0
	rawTokenKey ctxKey = 1
)

// WithClaims returns a derived context carrying c. AuthMiddleware
// is the only intended caller; downstream handlers retrieve via
// ClaimsFrom. Passing a zero TokenClaims is allowed (it is
// semantically "no verified identity") but in practice
// AuthMiddleware already returns 401 before reaching this code
// path when no token is present.
func WithClaims(ctx context.Context, c sts.TokenClaims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}

// ClaimsFrom returns the TokenClaims previously injected by
// WithClaims, plus a boolean indicating presence. The boolean
// matters because GetV1As distinguishes "no claims" (return 401
// per PRMT-105 §5) from "empty claims with sub/realm populated"
// (forward as a verifiable identity to /v1).
func ClaimsFrom(ctx context.Context) (sts.TokenClaims, bool) {
	c, ok := ctx.Value(claimsKey).(sts.TokenClaims)
	return c, ok
}

// WithRawToken returns a derived context carrying the original
// STS-issued JWS (raw). AuthMiddleware is the only intended
// caller — it injects this immediately after sts.Verify succeeds,
// alongside WithClaims. Downstream callers (GetV1As, GetV1AsTenant)
// recover the value via RawTokenFrom and forward it to core /v1
// as a verifiable bearer.
//
// PRMT-114 §4: the key is intentionally independent of claimsKey
// — the two carriers represent distinct facts (verified
// assertions vs. the original signed token) and MUST NOT be
// conflated. An empty raw is allowed in the carrier (defensive
// branch), but in practice AuthMiddleware already populates this
// from the verified Authorization header so a successful
// verify-path always carries the original JWS.
func WithRawToken(ctx context.Context, raw string) context.Context {
	return context.WithValue(ctx, rawTokenKey, raw)
}

// RawTokenFrom returns the raw JWS bearer previously injected by
// WithRawToken, plus a boolean indicating presence. The boolean
// matters because the upstream helpers must distinguish "no
// verified token" (omit Authorization header; the 401 contract
// applies upstream) from "verified token present" (attach it
// as the bearer).
func RawTokenFrom(ctx context.Context) (string, bool) {
	raw, ok := ctx.Value(rawTokenKey).(string)
	return raw, ok
}
