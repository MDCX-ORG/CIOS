//go:build !lab

package apigw

import "net/http"

// devBypassAvailable is false in production builds: the lab
// CIOS_APIGW_DEV_NO_AUTH inject path is not compiled in. LoadConfig
// refuses DevNoAuth when this is false. PRMT-217 (report S-1).
const devBypassAvailable = false

// maybeInjectDevNoAuthClaims is a no-op in production builds.
// Defence in depth: even if SnapshotDevNoAuth(true) were called,
// pass-through AuthMiddleware never stamps claims. PRMT-217.
func maybeInjectDevNoAuthClaims(r *http.Request) *http.Request { return r }
