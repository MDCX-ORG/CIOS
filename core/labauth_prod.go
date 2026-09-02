//go:build !lab

package core

import "net/http"

// labBypassAvailable reports whether this binary was built with the
// lab auth bypass compiled in. See labauth_lab.go for the lab
// variant. PRMT-217 (report S-1).
const labBypassAvailable = false

// labNoAuthAdminPrincipal is a no-op in production builds: the lab
// bypass is not compiled in. cmd/cios-core refuses -allow-no-auth
// when labBypassAvailable is false, so this is unreachable defence
// in depth. PRMT-217 (report S-1).
func labNoAuthAdminPrincipal(inner http.Handler) http.Handler { return inner }
