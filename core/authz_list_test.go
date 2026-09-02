// Package core — authz_list_test.go: end-to-end tests for the
// PRMT-022 R2 list-endpoint scope filter.
//
// Background (PRMT-019 §4.4 + PRMT-022 R2 §1): the middleware used
// to call authorize(principal, ActionRead, "**") for collection
// endpoints; that path is unreachable for any non-admin viewer
// (see the PRMT-022 list-scope filter decision
// for the probe data). R2 splits the gate in two: the middleware
// applies a role floor (roleAllows) for collection endpoints, and
// the handler does the per-item scope check. This file pins down
// the resulting behaviour on both sides of that contract plus a
// critical regression: /v1/metrics/query* MUST stay fail-closed.
//
// Test helpers (newAuthTestServer, buildVerifierForRoles,
// mustAuthTestDict) live in auth_test.go and are reused here to
// keep the auth surface tested through a single source of truth.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// listAssetsResp mirrors listAssetsResponse in production code so
// we can decode the wire body without depending on the unexported
// type. Field tags are the public wire contract.
type listAssetsResp struct {
	Items         []Asset `json:"items"`
	NextPageToken string  `json:"next_page_token"`
}

type listAlarmsResp struct {
	Items         []Alarm `json:"items"`
	NextPageToken string  `json:"next_page_token"`
}

// seedAssets writes n cdu assets via the public Store (no auth
// needed) under (prefix).pod000.cduNNN where NNN is a 3-digit
// zero-padded index. Returns the paths seeded. n must be <= 999.
// The path format is the same one TestAssets_ListFilterAndPage
// uses (site01.pod000.cdu001 etc.) so the seeded data is exactly
// what the in-handler cpath.ParseAssetPath / glob.Match pipeline
// expects.
func seedAssets(t *testing.T, srv *Server, prefix string, n int) []string {
	t.Helper()
	if n > 999 {
		t.Fatalf("seedAssets: n=%d > 999 (three-digit cdu suffix cap)", n)
	}
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("%s.pod000.cdu%03d", prefix, i)
		if _, err := srv.st.PutAsset(context.Background(), Asset{Path: path, Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
			t.Fatalf("PutAsset %s: %v", path, err)
		}
		paths = append(paths, path)
	}
	return paths
}

// buildR2Verifier returns a verifier plus three tokens: a viewer
// scoped to viewerScopes, an operator scoped to operatorScopes,
// and an admin with no scopes. Distinct from
// buildVerifierForRoles in auth_test.go only in that the
// plaintext tokens are deterministic (handy for failure messages).
func buildR2Verifier(t *testing.T, viewerScopes, operatorScopes []string) (TokenVerifier, string, string, string) {
	t.Helper()
	const (
		viewerTok   = "r2-viewer-token"
		operatorTok = "r2-operator-token"
		adminTok    = "r2-admin-token"
	)
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	v, err := NewStaticTokenVerifier(map[string]Principal{
		h(viewerTok):   {Subject: "svc:viewer", Role: RoleViewer, Scopes: viewerScopes},
		h(operatorTok): {Subject: "svc:operator", Role: RoleOperator, Scopes: operatorScopes},
		h(adminTok):    {Subject: "svc:admin", Role: RoleAdmin},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	return v, viewerTok, operatorTok, adminTok
}

// doAuthedGet is a thin GET helper that returns status + decoded
// envelope (or raw body on non-200). token=="" omits the header
// (used by the Auth==nil tests).
func doAuthedGet(t *testing.T, ts *httptest.Server, path, token string, into any) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v\nbody: %s", path, err, string(body))
		}
	}
	return resp.StatusCode, string(body)
}

// --- /v1/assets list: middleware allows, handler filters -----------

// TestListAssets_ViewerScoped_AllowedByMiddleware_AndFiltered:
// the critical R2 case. A viewer with site01.** can actually reach
// the handler now (the role floor passes) and the handler drops
// every site02 item.
func TestListAssets_ViewerScoped_AllowedByMiddleware_AndFiltered(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 3)
	seedAssets(t, srv, "site02", 3)

	var got listAssetsResp
	code, body := doAuthedGet(t, ts, "/v1/assets", viewerTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (R2 must let the viewer IN)", code, body)
	}
	if len(got.Items) != 3 {
		t.Fatalf("len(items)=%d, want 3 (got %+v)", len(got.Items), pathsOf(got.Items))
	}
	for _, a := range got.Items {
		if !strings.HasPrefix(a.Path, "site01.") {
			t.Errorf("viewer saw out-of-scope path %q", a.Path)
		}
	}
}

// TestListAssets_ViewerDisjointScope_EmptyItems: viewer with a
// scope that has no intersection with the seeded assets gets 200
// + empty items (NOT 403 — that would be a R2 regression).
func TestListAssets_ViewerDisjointScope_EmptyItems(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site09.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 2)
	seedAssets(t, srv, "site02", 2)

	var got listAssetsResp
	code, body := doAuthedGet(t, ts, "/v1/assets", viewerTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 with empty items (NOT 403)", code, body)
	}
	if len(got.Items) != 0 {
		t.Errorf("len(items)=%d, want 0 (got %+v)", len(got.Items), pathsOf(got.Items))
	}
}

func TestListAssets_AdminSeesAll(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 3)
	seedAssets(t, srv, "site02", 3)

	var got listAssetsResp
	code, body := doAuthedGet(t, ts, "/v1/assets", adminTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(got.Items) != 6 {
		t.Errorf("admin len(items)=%d, want 6 (got %+v)", len(got.Items), pathsOf(got.Items))
	}
}

// TestListAssets_AuthDisabled_PassesEverything: M0 no-regression
// guarantee. Auth==nil ⇒ no per-item filter at all.
func TestListAssets_AuthDisabled_PassesEverything(t *testing.T) {
	srv := newAuthTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 2)
	seedAssets(t, srv, "site02", 2)

	var got listAssetsResp
	code, body := doAuthedGet(t, ts, "/v1/assets", "", &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(got.Items) != 4 {
		t.Errorf("Auth==nil len(items)=%d, want 4 (got %+v)", len(got.Items), pathsOf(got.Items))
	}
}

// TestListAssets_OperatorReadScope: operator with site01.** reads
// only site01 items (operator role is allowed read; scope filter
// applies identically to viewer).
func TestListAssets_OperatorReadScope(t *testing.T) {
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site01.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 2)
	seedAssets(t, srv, "site02", 2)

	var got listAssetsResp
	code, body := doAuthedGet(t, ts, "/v1/assets", operatorTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(got.Items) != 2 {
		t.Fatalf("operator len(items)=%d, want 2 (got %+v)", len(got.Items), pathsOf(got.Items))
	}
	for _, a := range got.Items {
		if !strings.HasPrefix(a.Path, "site01.") {
			t.Errorf("operator saw out-of-scope path %q", a.Path)
		}
	}
}

// TestListAssets_Pagination_BasedOnFilteredSet: 4 in-scope + 4
// out-of-scope items, page_size=2. Viewer must walk exactly 2
// pages of 2 in-scope items each — never 4 pages with empty ones.
func TestListAssets_Pagination_BasedOnFilteredSet(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedAssets(t, srv, "site01", 4)
	seedAssets(t, srv, "site02", 4)

	seen := map[string]bool{}
	next := ""
	pages := 0
	for pages < 10 {
		pages++
		u := "/v1/assets?page_size=2"
		if next != "" {
			u += "&page_token=" + next
		}
		var got listAssetsResp
		code, body := doAuthedGet(t, ts, u, viewerTok, &got)
		if code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", pages, code, body)
		}
		if len(got.Items) == 0 {
			t.Fatalf("page %d empty — filter must have eliminated items", pages)
		}
		if len(got.Items) > 2 {
			t.Errorf("page %d len(items)=%d, want <= 2", pages, len(got.Items))
		}
		for _, a := range got.Items {
			if !strings.HasPrefix(a.Path, "site01.") {
				t.Errorf("page %d leaked out-of-scope %q", pages, a.Path)
			}
			if seen[a.Path] {
				t.Errorf("page %d duplicate %q", pages, a.Path)
			}
			seen[a.Path] = true
		}
		next = got.NextPageToken
		if next == "" {
			break
		}
	}
	if len(seen) != 4 {
		t.Errorf("walked %d unique site01 paths, want 4", len(seen))
	}
	if pages > 3 {
		t.Errorf("walked %d pages to cover 4 site01 items — pagination likely on unfiltered set", pages)
	}
}

// --- /v1/alarms list: same per-item filter -------------------------

func TestListAlarms_ViewerScopedFiltersOutOfScope(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_ = srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "x"},
		{ID: "A2", Path: "site02.pod000.cdu000", Severity: "critical", State: "firing", Summary: "y"},
	})

	var got listAlarmsResp
	code, body := doAuthedGet(t, ts, "/v1/alarms", viewerTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "A1" {
		t.Fatalf("got %+v, want exactly [A1]", got.Items)
	}
}

func TestListAlarms_AuthDisabled_PassesEverything(t *testing.T) {
	srv := newAuthTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_ = srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing"},
		{ID: "A2", Path: "site02.pod000.cdu000", Severity: "critical", State: "firing"},
	})

	var got listAlarmsResp
	code, body := doAuthedGet(t, ts, "/v1/alarms", "", &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if len(got.Items) != 2 {
		t.Errorf("Auth==nil len(items)=%d, want 2", len(got.Items))
	}
}

// --- /v1/metrics/query* must stay fail-closed ---------------------

// TestMetrics_ViewerStillForbidden is the R2 regression guard:
// /v1/metrics/query* is NOT a list-scope endpoint, so it still
// calls the full authorize. Scoped viewers must remain 403, both
// with a "**" scope and with a site01-only scope, so a future
// "treat all '**' path mappings as list" change is caught.
func TestMetrics_ViewerStillForbidden(t *testing.T) {
	for _, name := range []string{"site01-scoped", "global-scope"} {
		t.Run(name, func(t *testing.T) {
			var scopes []string
			if name == "global-scope" {
				scopes = []string{"**"}
			} else {
				scopes = []string{"site01.**"}
			}
			v, viewerTok, _, _ := buildR2Verifier(t, scopes, nil)
			srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			code, body := doAuthedGet(t, ts, "/v1/metrics/query?query=up", viewerTok, nil)
			if code != http.StatusForbidden {
				t.Errorf("viewer(%s) GET /v1/metrics/query status=%d body=%s, want 403 (R2 must NOT relax metrics gating)",
					name, code, body)
			}
		})
	}
}

// TestMetrics_AdminCanQuery confirms the role bypass still works
// for metrics — admin must remain able to use the endpoint.
// Uses newTestServer (which stands up a fake VM) and injects auth
// by setting Server.auth after construction; the field is exported
// for the same reason in PRMT-019's auth_test.go (see
// TestServer_AuthEnabled_GatesEverything).
func TestMetrics_AdminCanQuery(t *testing.T) {
	srv, ts := newTestServer(t)
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv.auth = &AuthConfig{Verifier: v}
	// The Handler() chain was captured with auth==nil at newTestServer
	// time; close that test server and start a fresh one so the
	// middleware is included.
	ts.Close()
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	code, body := doAuthedGet(t, ts, "/v1/metrics/query?query=up", adminTok, nil)
	if code != http.StatusOK {
		t.Errorf("admin GET /v1/metrics/query status=%d body=%s, want 200 (admin role bypass)", code, body)
	}
}

// --- isListScopeEndpoint direct unit test -------------------------

// TestIsListScopeEndpoint_Matrix pins the exact endpoint set the
// helper matches. A future refactor that re-includes
// /v1/metrics/query* would silently relax scope isolation on a
// production metric store; this test fails loud.
func TestIsListScopeEndpoint_Matrix(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/v1/assets", true},
		{http.MethodGet, "/v1/alarms", true},
		{http.MethodGet, "/v1/reports/ops", true},
		// methods other than GET on the list URLs are not list-scope
		// (they fall through to 405 in the inner handler); gating still
		// uses full authorize, which is correct for those.
		{http.MethodPost, "/v1/assets", false},
		{http.MethodDelete, "/v1/assets", false},
		{http.MethodPost, "/v1/reports/ops", false},
		// /v1/metrics/query* MUST stay out of the list-scope branch.
		{http.MethodGet, "/v1/metrics/query", false},
		{http.MethodGet, "/v1/metrics/query_range", false},
		// single-resource paths always use full authorize.
		{http.MethodGet, "/v1/assets/site01.pod000.cdu000", false},
		{http.MethodGet, "/v1/points/site01.pod000.cdu000.fan000.rpm", false},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, tc.path, nil)
		got := isListScopeEndpoint(req)
		if got != tc.want {
			t.Errorf("isListScopeEndpoint(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// --- helpers --------------------------------------------------------

func pathsOf(items []Asset) []string {
	out := make([]string, len(items))
	for i, a := range items {
		out[i] = a.Path
	}
	return out
}

// --- /v1/reports/ops RBAC wiring (PRMT-038) -------------------------------
//
// PRMT-037 wired the handler + per-item scope filter, but forgot to
// register the route in authmw.go → middleware bypassed auth → scoped
// viewers saw every site's tickets+alarms. PRMT-038 is the surgical
// fix: register /v1/reports/ops as a list-scope endpoint (same shape
// as /v1/alarms). These tests pin down the resulting behaviour at the
// HTTP boundary.

// reportsOpsRBACResp mirrors the wire body for /v1/reports/ops just
// enough to verify the per-item scope filter fires in reports.go.
// Kept in this file (not reports_test.go) to keep PRMT-038's RBAC
// proof self-contained.
type reportsOpsRBACResp struct {
	TicketCounts struct {
		ByState map[string]int `json:"by_state"`
	} `json:"ticket_counts"`
	AlarmTop []struct {
		Path  string `json:"path"`
		Count int    `json:"count"`
	} `json:"alarm_top"`
}

// seedReportsOpsFixtures writes two tickets (one site01, one site99)
// and two alarms (one site01, one site99) to the store, bypassing the
// HTTP API so RBAC on POST /v1/tickets cannot interfere with the
// setup. Mirrors TestReports_ScopeFilter_OutOfScopeDropped.
func seedReportsOpsFixtures(t *testing.T, srv *Server) {
	t.Helper()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: "tk_in", AssetPath: "site01.pod000.cdu000", State: "open",
		Severity: "major", OpenedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: "tk_out", AssetPath: "site99.pod000.cdu000", State: "open",
		Severity: "major", OpenedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000.fws.supply.flow", State: "firing"},
		{ID: "A2", Path: "site99.pod000.cdu000.fws.supply.flow", State: "firing"},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReportsOps_NoToken_Returns401 is the security regression that
// prompted PRMT-038: before the fix, mapRequest("/v1/reports/ops")
// returned isAPI=false → middleware bypassed → 200 with the full
// store. Now the route is /v1/api-gated, so missing token → 401.
func TestReportsOps_NoToken_Returns401(t *testing.T) {
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	seedReportsOpsFixtures(t, srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	code, body := doAuthedGet(t, ts, "/v1/reports/ops", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401 (PRMT-038 regression guard)", code, body)
	}
}

// TestReportsOps_ViewerScoped_SeesOnlyInScopeTicketsAndAlarms is
// the headline acceptance: a viewer with site01.** must reach the
// handler (role floor passes) and then the per-item filter in
// reports.go must drop every site99 row. Confirms the authmw +
// reports.go contract is wired end-to-end.
func TestReportsOps_ViewerScoped_SeesOnlyInScopeTicketsAndAlarms(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	seedReportsOpsFixtures(t, srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var got reportsOpsRBACResp
	code, body := doAuthedGet(t, ts, "/v1/reports/ops", viewerTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (role floor must let viewer in)", code, body)
	}
	if got.TicketCounts.ByState["open"] != 1 {
		t.Errorf("by_state[open]=%d, want 1 (out-of-scope site99 ticket leaked)", got.TicketCounts.ByState["open"])
	}
	if len(got.AlarmTop) != 1 {
		t.Fatalf("alarm_top len=%d, want 1 (out-of-scope site99 alarm leaked): %+v",
			len(got.AlarmTop), got.AlarmTop)
	}
	if got.AlarmTop[0].Path != "site01.pod000.cdu000.fws.supply.flow" {
		t.Errorf("alarm_top[0].path=%q, want site01 path", got.AlarmTop[0].Path)
	}
}

// TestReportsOps_AdminSeesAll: admin (no scopes) gets the full
// store; per-item filter only fires for non-admin with explicit
// scope (the role floor already passes them).
func TestReportsOps_AdminSeesAll(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	seedReportsOpsFixtures(t, srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var got reportsOpsRBACResp
	code, body := doAuthedGet(t, ts, "/v1/reports/ops", adminTok, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (admin bypass)", code, body)
	}
	if got.TicketCounts.ByState["open"] != 2 {
		t.Errorf("admin by_state[open]=%d, want 2 (admin must see all tickets)", got.TicketCounts.ByState["open"])
	}
	if len(got.AlarmTop) != 2 {
		t.Errorf("admin alarm_top len=%d, want 2 (admin must see all alarms)", len(got.AlarmTop))
	}
}

// TestReportsOps_AuthDisabled_PassesEverything is the M0 no-regress
// guarantee (same shape as TestListAssets_AuthDisabled_PassesEverything):
// when Auth==nil the handler should still reach the store and
// return 200 with both tickets and both alarms. This must NOT 401.
func TestReportsOps_AuthDisabled_PassesEverything(t *testing.T) {
	srv := newAuthTestServer(t, nil)
	seedReportsOpsFixtures(t, srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var got reportsOpsRBACResp
	code, body := doAuthedGet(t, ts, "/v1/reports/ops", "", &got)
	if code != http.StatusOK {
		t.Fatalf("Auth==nil status=%d body=%s, want 200 (M0 backward compat)", code, body)
	}
	if got.TicketCounts.ByState["open"] != 2 {
		t.Errorf("Auth==nil by_state[open]=%d, want 2", got.TicketCounts.ByState["open"])
	}
	if len(got.AlarmTop) != 2 {
		t.Errorf("Auth==nil alarm_top len=%d, want 2", len(got.AlarmTop))
	}
}

// TestReportsOps_ViewerDisjointScope_EmptyItems mirrors the
// /v1/assets disjoint-scope contract: a viewer whose scope does not
// match any row gets 200 with empty items (NOT 403 — full authorize
// would 403 because path="**"). This is the live test that
// mapRequest("/v1/reports/ops") is list-scope, not full authorize.
func TestReportsOps_ViewerDisjointScope_EmptyItems(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site42.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	seedReportsOpsFixtures(t, srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var got reportsOpsRBACResp
	code, body := doAuthedGet(t, ts, "/v1/reports/ops", viewerTok, &got)
	if code != http.StatusOK {
		t.Fatalf("disjoint scope status=%d body=%s, want 200 (NOT 403; list-scope path)", code, body)
	}
	if got.TicketCounts.ByState["open"] != 0 {
		t.Errorf("disjoint by_state[open]=%d, want 0", got.TicketCounts.ByState["open"])
	}
	if len(got.AlarmTop) != 0 {
		t.Errorf("disjoint alarm_top len=%d, want 0", len(got.AlarmTop))
	}
}
