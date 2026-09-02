//go:build lab

package apigw

import (
	"net/http"

	"github.com/yurimeng/cios/pkg/sts"
	"github.com/yurimeng/cios/pkg/tenant"
)

// devBypassAvailable is true when this binary was built with -tags lab
// and therefore may inject fixed dev claims under CIOS_APIGW_DEV_NO_AUTH.
// PRMT-217 (report S-1).
const devBypassAvailable = true

// newDevNoAuthClaims returns the fixed TokenClaims injected by
// AuthMiddleware's pass-through branch when DevNoAuth is enabled
// (PRMT-173 §4.3). Construction:
//
//   - Subject:       devNoAuthSubject         // dev-only signal; no real user
//   - Realm:         "ops"                    // mirror handler default
//   - Scope:         ["dev"]                  // informative; fail-closed is the cfg gate
//   - Tenant:        devNoAuthTenantID        // non-empty ⇒ tenant.TenantFromClaims returns ok
//   - IsolationTier: string(tenant.TierLabel) // "label" tier = upstream.go label path
//
// Audience / JTI / Expiry / Org / Sites intentionally zero; not
// consumed by TenantFromClaims or handler-layer ClaimsFrom in the
// pass-through path.
func newDevNoAuthClaims() sts.TokenClaims {
	return sts.TokenClaims{
		Subject:       devNoAuthSubject,
		Realm:         "ops",
		Scope:         []string{"dev"},
		Tenant:        devNoAuthTenantID,
		IsolationTier: string(tenant.TierLabel),
	}
}

// maybeInjectDevNoAuthClaims stamps fixed dev claims onto r when the
// lab DevNoAuth flag is enabled and claims are not already present.
// Lab builds only (PRMT-217).
func maybeInjectDevNoAuthClaims(r *http.Request) *http.Request {
	if !devNoAuthEnabled() {
		return r
	}
	if _, ok := ClaimsFrom(r.Context()); ok {
		return r
	}
	return r.WithContext(WithClaims(r.Context(), newDevNoAuthClaims()))
}
