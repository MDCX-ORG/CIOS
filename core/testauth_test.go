package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// the fixed CI accounts. Tests MUST assert against these
// instead of relying on the absence of a principal ("anonymous") or
// on whatever a lab auth-bypass happens to inject ("dev-no-auth").
const (
	ciAdmin    = "svc:ci-admin"
	ciOperator = "svc:ci-operator"
	ciViewer   = "svc:ci-viewer"
)

func ciPrincipal(subject string) Principal {
	role := RoleViewer
	switch subject {
	case ciAdmin:
		role = RoleAdmin
	case ciOperator:
		role = RoleOperator
	}
	return Principal{Subject: subject, Role: role, Scopes: []string{"**"}}
}

// asPrincipal attaches a fixed CI account to an in-process request
// (httptest.NewRequest + h.ServeHTTP style).
func asPrincipal(req *http.Request, subject string) *http.Request {
	return req.WithContext(context.WithValue(
		req.Context(), ctxKeyPrincipal, ciPrincipal(subject)))
}

// principalHandler wraps a handler so every request carries a fixed CI
// account. Needed for httptest.NewServer-based tests, where the caller
// cannot set a request context. Injects BEFORE the server chain, so
// labNoAuthAdminPrincipal (which only fills an empty slot) is a no-op.
func principalHandler(inner http.Handler, subject string) http.Handler {
	p := ciPrincipal(subject)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), ctxKeyPrincipal, p)))
	})
}

// newTestServerAs mirrors newTestServer but every request arrives as
// the given fixed CI account.
func newTestServerAs(t *testing.T, subject string) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServerWith(t, func(h http.Handler) http.Handler {
		return principalHandler(h, subject)
	})
}
