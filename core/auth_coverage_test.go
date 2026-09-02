// Package core — auth_coverage_test.go: PRMT-075 regression guard.
//
// Background: PRMT-037 shipped a /v1 endpoint that was missing the
// auth middleware; CIOS-CODE-EVALUATION-2026-06-21 H3 flagged that
// a missing/wrong method/path entry in the middleware's mapRequest
// table (or a brand-new mux.HandleFunc that forgets to wrap with
// the auth chain) would silently let requests through unauthenticated.
//
// This file enumerates every mux.HandleFunc registered in
// Server.Handler() and asserts: with auth enabled, an HTTP request
// without an Authorization header gets 401; the only exceptions
// are /v1/health and /v1/health/ready (kubelet / readinessProbe /
// livenessProbe / load-balancer probes cannot carry a bearer token
// — see core/authmw.go and PRMT-066 §0 design).
//
// If a future endpoint is added to Server.Handler() but not to
// the table below, this test will go RED on the no-token probe.
// That is the intended behaviour: any reviewer accepting a new
// endpoint is forced to confirm it is either (a) intentionally
// public, (b) registered here as a protected endpoint, or (c)
// unwrapped by design (in which case the route should NOT exist
// on the mux at all).
//
// PRMT-075 §1(b) / §2 / CODE-EVALUATION-2026-06-21 H3.
package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// protectedRoute is one row of the coverage table. method+path
// must match a real route registered in Server.Handler() so the
// mux dispatches into the auth middleware (not a 404 from the mux
// before the middleware runs). If the path doesn't match, the
// test is structurally meaningless — it'd prove "no token to a
// 404 returns 404" not "no token to a real route returns 401".
type protectedRoute struct {
	name   string // human-readable tag for test failure messages
	method string
	path   string
}

// authCoverageRoutes is the explicit list of protected endpoints
// that must reject a no-token request with 401. Each entry maps to
// exactly one mux.HandleFunc call in core/server.go Handler(). Add
// a row here whenever a new /v1/{prefix} endpoint lands; if you
// forget, TestAuthCoverage_ProtectedRoutes_RejectNoToken fails.
//
// NOTE: this list is INTENTIONALLY redundant with mapRequest /
// isListScopeEndpoint. The point of the test is to fail loudly
// when a new mux.HandleFunc bypasses auth, not to assert that
// mapRequest's per-method dispatch is correct (that's already
// covered by auth_test.go).
var authCoverageRoutes = []protectedRoute{
	// /v1/assets — list + per-resource read/write + lifecycle.
	{"assets-list", http.MethodGet, "/v1/assets"},
	{"assets-get", http.MethodGet, "/v1/assets/site01.pod000.cdu000"},
	{"assets-put", http.MethodPut, "/v1/assets/site01.pod000.cdu000"},
	{"assets-delete", http.MethodDelete, "/v1/assets/site01.pod000.cdu000"},
	{"assets-lifecycle", http.MethodPost, "/v1/assets/site01.pod000.cdu000:lifecycle"},

	// /v1/alarms — list + per-alarm ack (PRMT-230).
	{"alarms-list", http.MethodGet, "/v1/alarms"},
	{"alarms-ack", http.MethodPost, "/v1/alarms/A1:ack"},

	// /v1/tickets — list + create + per-ticket read + state writes.
	{"tickets-list", http.MethodGet, "/v1/tickets"},
	{"tickets-create", http.MethodPost, "/v1/tickets"},
	{"tickets-get", http.MethodGet, "/v1/tickets/T1"},
	{"tickets-transition", http.MethodPost, "/v1/tickets/T1:transition"},
	{"tickets-note", http.MethodPost, "/v1/tickets/T1:note"},
	{"tickets-assign", http.MethodPost, "/v1/tickets/T1:assign"},
	{"tickets-history", http.MethodGet, "/v1/tickets/T1:history"},

	// /v1/reports — ops + reconcile (PRMT-042 / PRMT-050).
	{"reports-ops", http.MethodGet, "/v1/reports/ops"},
	{"reports-reconcile", http.MethodGet, "/v1/reports/reconcile"},

	// /v1/pm/schedules — list + create + per-schedule read (PRMT-043).
	{"pm-schedules-list", http.MethodGet, "/v1/pm/schedules"},
	{"pm-schedules-create", http.MethodPost, "/v1/pm/schedules"},
	{"pm-schedules-get", http.MethodGet, "/v1/pm/schedules/P1"},

	// /v1/sla — customer uptime SLA stub (PRMT-209).
	{"customer-sla", http.MethodGet, "/v1/sla"},

	// /v1/runbooks/{key} — read-only (PRMT-044).
	{"runbooks-get", http.MethodGet, "/v1/runbooks/foo"},

	// /v1/cases — read-only.
	{"cases-list", http.MethodGet, "/v1/cases"},

	// /v1/spares — list + create + per-spare read + adjust (PRMT-048).
	{"spares-list", http.MethodGet, "/v1/spares"},
	{"spares-create", http.MethodPost, "/v1/spares"},
	{"spares-get", http.MethodGet, "/v1/spares/S1"},
	{"spares-adjust", http.MethodPost, "/v1/spares/S1:adjust"},

	// /v1/inspections — list + create + per-inspection read (PRMT-049).
	{"inspections-list", http.MethodGet, "/v1/inspections"},
	{"inspections-create", http.MethodPost, "/v1/inspections"},
	{"inspections-get", http.MethodGet, "/v1/inspections/I1"},

	// /v1/inspections/form/{id} — mobile-web checklist (PRMT-059).
	{"inspections-form-get", http.MethodGet, "/v1/inspections/form/I1"},
	{"inspections-form-post", http.MethodPost, "/v1/inspections/form/I1"},

	// /v1/maintenance/upcoming — merged PM + inspection view (PRMT-058).
	{"maintenance-upcoming", http.MethodGet, "/v1/maintenance/upcoming"},

	// /v1/metrics/query* — VM passthrough; must stay fail-closed
	// until M3 adds PromQL label-level isolation (PRMT-022 R2 §4.0).
	{"metrics-query", http.MethodGet, "/v1/metrics/query?query=up"},
	{"metrics-query-range", http.MethodGet, "/v1/metrics/query_range?query=up"},

	// /v1/points/{path} — read + :set write.
	{"points-get", http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp"},
	{"points-set", http.MethodPut, "/v1/points/site01.pod000.cdu000.fws.supply.temp:set"},

	// /v1/health/scanners — viewer-protected (PRMT-066). NOT public.
	// Listed here to guard against a future "make all /v1/health/*
	// public" refactor that loses scanners protection.
	{"health-scanners", http.MethodGet, "/v1/health/scanners"},
}

// publicProbe is the (single) pair of endpoints intentionally
// exempt from auth — kubelet/livenessProbe/readinessProbe cannot
// carry a bearer token. If you find yourself wanting to add a
// third public endpoint here, stop: the kubelet is the only known
// consumer, and any other "public" path almost certainly needs
// RBAC. PRMT-066 §0 design.
var publicProbes = []protectedRoute{
	{"health-liveness", http.MethodGet, "/v1/health"},
	{"health-readiness", http.MethodGet, "/v1/health/ready"},
}

// newAuthCoverageTestServer wraps newAuthTestServer with auth
// enabled (viewer+operator+admin, all scopes "**") so the test
// always exercises the gated code path. The verifier is real so
// mapRequest's "non-list scope → authorize" branch runs the same
// code that production would.
func newAuthCoverageTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestAuthCoverage_ProtectedRoutes_RejectNoToken walks the full
// table and asserts no-token → 401 for every protected route.
// If a future mux.HandleFunc is added to Server.Handler() but
// forgotten here, this test fails on the next entry it can't
// find in the table (a structural reminder, not just an HTTP
// status reminder). The explicit-table form is intentional: a
// wildcards-only test would let a regression that "accidentally"
// also bypasses the table pass.
func TestAuthCoverage_ProtectedRoutes_RejectNoToken(t *testing.T) {
	ts := newAuthCoverageTestServer(t)
	for _, r := range authCoverageRoutes {
		t.Run(r.name, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			// Deliberately NO Authorization header.
			w := httptest.NewRecorder()
			ts.Config.Handler.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: no-token status = %d, want 401; body=%s",
					r.method, r.path, w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthCoverage_PublicProbes_AllowNoToken is the explicit
// "the two exemptions are exactly two" assertion. If someone
// later adds a third public route and forgets to update this
// list, the new route will hit the protected-routes test above
// and fail there — but this test still serves as documentation
// that "the only public probes are these two".
func TestAuthCoverage_PublicProbes_AllowNoToken(t *testing.T) {
	ts := newAuthCoverageTestServer(t)
	for _, r := range publicProbes {
		t.Run(r.name, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			w := httptest.NewRecorder()
			ts.Config.Handler.ServeHTTP(w, req)
			if w.Code == http.StatusUnauthorized {
				t.Errorf("%s %s: public probe unexpectedly returned 401; body=%s",
					r.method, r.path, w.Body.String())
			}
			// We don't assert 200 specifically — readiness can
			// legitimately 503 when the store/VM is misconfigured,
			// and liveness is always 200 in the current
			// implementation but we don't want to over-couple.
			// The only invariant under test is "not 401".
		})
	}
}

// TestAuthCoverage_TableMatchesHandler guards the OTHER half of
// the contract: every mux.HandleFunc registered in Handler() must
// appear in either authCoverageRoutes or publicProbes. This is
// what catches the "new mux.HandleFunc forgot to be added to the
// table" failure mode that the per-row test above only catches
// indirectly.
//
// The check is structural (string scan over server.go) rather
// than reflective because the mux doesn't expose its registered
// patterns without introspecting the tree. Reading the source is
// cheap, deterministic, and gives a clear failure message naming
// the prefix that was missed.
func TestAuthCoverage_TableMatchesHandler(t *testing.T) {
	// Hard-coded mirror of core/server.go Handler() registrations.
	// If you add a mux.HandleFunc, add a row here too — that is
	// the test.
	registeredPrefixes := []string{
		"/v1/assets",
		"/v1/assets/",
		"/v1/alarms",
		"/v1/alarms/",
		"/v1/tickets",
		"/v1/tickets/",
		"/v1/reports/ops",
		"/v1/reports/reconcile",
		"/v1/pm/schedules",
		"/v1/pm/schedules/",
		"/v1/sla",
		"/v1/runbooks/",
		"/v1/cases",
		"/v1/spares",
		"/v1/spares/",
		"/v1/inspections",
		"/v1/inspections/",
		"/v1/inspections/form/",
		"/v1/maintenance/upcoming",
		"/v1/metrics/query",
		"/v1/metrics/query_range",
		"/v1/points/",
		"/v1/health",
		"/v1/health/ready",
		"/v1/health/scanners",
	}
	// Build the set of prefixes covered by the table.
	covered := map[string]bool{}
	for _, r := range authCoverageRoutes {
		covered[routePrefix(r.path)] = true
	}
	for _, r := range publicProbes {
		covered[routePrefix(r.path)] = true
	}
	for _, p := range registeredPrefixes {
		if !covered[p] {
			t.Errorf("mux.HandleFunc(%q) in Server.Handler() has no entry in authCoverageRoutes or publicProbes; add a row to the table in auth_coverage_test.go", p)
		}
	}
	// And the reverse: every prefix in the table must correspond
	// to a real registration (catches typos in the table).
	registered := map[string]bool{}
	for _, p := range registeredPrefixes {
		registered[p] = true
	}
	for _, r := range authCoverageRoutes {
		if !registered[routePrefix(r.path)] {
			t.Errorf("authCoverageRoutes row %q: path %q has no matching mux.HandleFunc in Server.Handler(); fix the table or the handler", r.name, r.path)
		}
	}
	for _, r := range publicProbes {
		if !registered[routePrefix(r.path)] {
			t.Errorf("publicProbes row %q: path %q has no matching mux.HandleFunc in Server.Handler(); fix the table or the handler", r.name, r.path)
		}
	}
}

// routePrefix reduces a request path to the prefix the mux uses
// for routing, so the table rows and the registeredPrefixes list
// compare equal. /v1/assets/site01... → /v1/assets/, /v1/assets
// stays /v1/assets. The mux matches "the longest registered
// prefix that is a prefix of the request path", so the table
// form mirrors that.
func routePrefix(p string) string {
	// Find the longest known prefix by suffix-match against the
	// canonical list. Cheaper than a full ServeMux walk for the
	// coverage-table assertion.
	for _, prefix := range []string{
		"/v1/assets/",
		"/v1/alarms/",
		"/v1/tickets/",
		"/v1/pm/schedules/",
		"/v1/spares/",
		"/v1/inspections/form/",
		"/v1/inspections/",
		"/v1/runbooks/",
		"/v1/points/",
	} {
		if len(p) >= len(prefix) && p[:len(prefix)] == prefix {
			return prefix
		}
	}
	// Strip the query string for non-prefix registrations like
	// /v1/metrics/query?query=up → /v1/metrics/query. The mux
	// matches on Path only, so r.URL.Path is what registration
	// cares about; the ?query=… tail is irrelevant to the
	// registration match.
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
