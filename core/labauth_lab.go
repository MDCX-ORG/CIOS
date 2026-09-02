//go:build lab

package core

import (
	"context"
	"net/http"
)

// labBypassAvailable reports whether this binary was built with the
// lab auth bypass compiled in. See labauth_prod.go for the production
// variant. PRMT-217 (report S-1).
const labBypassAvailable = true

// labNoAuthAdminPrincipal attaches a synthetic platform-admin Principal
// when core runs without RBAC (-allow-no-auth). Required for L109
// requireOrgAdmin surfaces (tenants/orgs/site-orgs/role-bindings/…).
func labNoAuthAdminPrincipal(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); !ok {
			p := Principal{
				Subject: "dev-no-auth",
				Role:    RoleAdmin,
				Scopes:  []string{"**"},
			}
			ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
			r = r.WithContext(ctx)
		}
		inner.ServeHTTP(w, r)
	})
}
