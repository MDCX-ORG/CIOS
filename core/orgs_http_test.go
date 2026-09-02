// Package core — orgs_http_test.go: end-to-end tests for the
// /v1/orgs admin CRUD surface (PRMT-185 §7).
//
// Coverage matrix (fileStore parity; pg path guarded by DSN env):
//
//   - admin list (single tenant; absent + admin = platform-wide ok)
//   - admin create → 201, one org_create audit row
//   - admin create duplicate name → 409 "org-name-conflict"
//   - admin create bad slug → 400
//   - admin create unknown tenant → 404
//   - non-admin → 403 on every operation
//   - admin get one / unknown id 404
//   - admin rename → 200 + one org_rename audit
//   - admin rename conflict → 409
//   - admin delete (0 sites) → 204 + one org_delete audit
//   - admin delete (≥1 site) → 409 "org-owns-resources" no delete no audit
//   - default org: renameable when owning; deletable only when empty
//   - tenant scoping (R1): X-CIOS-Tenant present matches all ops;
//     mismatch (path against header) → 403 "tenant-scope-mismatch";
//     body tenant_id != header tenant_id on POST → 403
//
// §7 acceptance: `go test ./core/ -run 'Orgs' -v`.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedOrg inserts one Org record directly into the in-memory
// fileStore so tests can drive GET/RENAME/DELETE without going
// through the create endpoint.
func seedOrg(t *testing.T, srv *Server, id, tenantID, name string) Org {
	t.Helper()
	fs, ok := srv.st.(*fileStore)
	if !ok {
		t.Fatalf("seedOrg: expected *fileStore, got %T", srv.st)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	o := Org{ID: id, TenantID: tenantID, Name: name, CreatedAt: now}
	fs.mu.Lock()
	fs.orgs[id] = o
	fs.mu.Unlock()
	return o
}

// findOrgAudit filters tenant_audit rows by op (org_create /
// org_rename / org_delete) so tests can assert exact row counts.
func findOrgAudit(rows []TenantAudit, op string) []TenantAudit {
	out := make([]TenantAudit, 0, len(rows))
	for _, r := range rows {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

// orgHTTPResp is the response capture from doOrgReq.
type orgHTTPResp struct {
	code int
	body string
}

// doOrgReq performs an HTTP request with optional bearer token and
// optional X-CIOS-Tenant header. Mirrors doReqWithAuth in
// core/spares_test.go but adds the tenant header parameter for the
// R1 tenant-scoping coverage.
func doOrgReq(t *testing.T, ts *httptest.Server, method, path, body, token, tenant string) orgHTTPResp {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant != "" {
		req.Header.Set(tenantHeaderName, tenant)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	bb, _ := io.ReadAll(resp.Body)
	return orgHTTPResp{code: resp.StatusCode, body: string(bb)}
}

// --- admin list/create happy path --------------------------------------

func TestOrgsHTTP_AdminCreate_HappyPath_AuditRow(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"acme","name":"emea"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s, want 201", r.code, r.body)
	}
	var got Org
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, r.body)
	}
	if got.ID == "" || !strings.HasPrefix(got.ID, "og_") {
		t.Errorf("ID = %q, want og_ prefix", got.ID)
	}
	if got.TenantID != "acme" || got.Name != "emea" {
		t.Errorf("created = %+v, want {acme,emea}", got)
	}

	// One org_create audit row.
	rows := findOrgAudit(auditsFor(t, srv, "acme"), "org_create")
	if len(rows) != 1 {
		t.Fatalf("org_create rows = %d, want 1 (rows=%+v)", len(rows), rows)
	}
	if rows[0].Principal != "svc:admin" {
		t.Errorf("audit principal = %q, want svc:admin", rows[0].Principal)
	}
}

func TestOrgsHTTP_AdminList_NameAscending(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "zeta")
	seedOrg(t, srv, "og_BBBBBBBBBBBBBBBB", "acme", "alpha")
	seedOrg(t, srv, "og_CCCCCCCCCCCCCCCC", "acme", "mu")

	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s, want 200", r.code, r.body)
	}
	var resp listOrgsResponse
	if err := json.Unmarshal([]byte(r.body), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, r.body)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("len(items)=%d, want 3 (got %+v)", len(resp.Items), resp.Items)
	}
	gotNames := []string{resp.Items[0].Name, resp.Items[1].Name, resp.Items[2].Name}
	wantNames := []string{"alpha", "mu", "zeta"}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Errorf("items[%d].Name = %q, want %q (full=%v)", i, gotNames[i], wantNames[i], gotNames)
		}
	}
}

// --- duplicate name 409 / bad slug 400 / unknown tenant 404 ------------

func TestOrgsHTTP_CreateDuplicate_409(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"acme","name":"emea"}`, adminTok, "")
	if r.code != http.StatusConflict {
		t.Fatalf("duplicate: code=%d body=%s, want 409", r.code, r.body)
	}
	if !strings.Contains(r.body, "org-name-conflict") {
		t.Errorf("body = %q, want org-name-conflict tail", r.body)
	}
	// No new audit row beyond the seed (seed uses no audit).
	rows := findOrgAudit(auditsFor(t, srv, "acme"), "org_create")
	if len(rows) != 0 {
		t.Errorf("duplicate produced %d org_create rows, want 0", len(rows))
	}
}

func TestOrgsHTTP_CreateBadSlug_400(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"acme","name":"EMEA"}`, adminTok, "")
	if r.code != http.StatusBadRequest {
		t.Errorf("bad slug: code=%d body=%s, want 400", r.code, r.body)
	}
}

func TestOrgsHTTP_CreateUnknownTenant_404(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"ghost","name":"emea"}`, adminTok, "")
	if r.code != http.StatusNotFound {
		t.Errorf("unknown tenant: code=%d body=%s, want 404", r.code, r.body)
	}
}

// --- get one: 200 / 404 ------------------------------------------------

func TestOrgsHTTP_GetOne_200_And_404(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("get ok: code=%d body=%s, want 200", r.code, r.body)
	}
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs/og_DDDDDDDDDDDDDDDD", "", adminTok, "")
	if r.code != http.StatusNotFound {
		t.Errorf("get ghost: code=%d body=%s, want 404", r.code, r.body)
	}
}

// --- rename: 200 + audit + conflict ------------------------------------

func TestOrgsHTTP_Rename_HappyPath_AuditRow(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs/og_AAAAAAAAAAAAAAAA:rename",
		`{"name":"eu-west"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("rename: code=%d body=%s, want 200", r.code, r.body)
	}
	var got Org
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, r.body)
	}
	if got.Name != "eu-west" {
		t.Errorf("rename response name = %q, want eu-west", got.Name)
	}
	// One org_rename row with detail "emea→eu-west".
	rows := findOrgAudit(auditsFor(t, srv, "acme"), "org_rename")
	if len(rows) != 1 {
		t.Fatalf("org_rename rows = %d, want 1 (rows=%+v)", len(rows), rows)
	}
	if rows[0].Detail != "emea→eu-west" {
		t.Errorf("audit detail = %q, want emea→eu-west", rows[0].Detail)
	}
}

func TestOrgsHTTP_RenameConflict_409(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")
	seedOrg(t, srv, "og_BBBBBBBBBBBBBBBB", "acme", "eu-west")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs/og_AAAAAAAAAAAAAAAA:rename",
		`{"name":"eu-west"}`, adminTok, "")
	if r.code != http.StatusConflict {
		t.Errorf("rename conflict: code=%d body=%s, want 409", r.code, r.body)
	}
	if !strings.Contains(r.body, "org-name-conflict") {
		t.Errorf("body = %q, want org-name-conflict", r.body)
	}
	// No audit row.
	if got := findOrgAudit(auditsFor(t, srv, "acme"), "org_rename"); len(got) != 0 {
		t.Errorf("conflict produced %d org_rename rows, want 0", len(got))
	}
}

func TestOrgsHTTP_RenameBadSlug_400(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs/og_AAAAAAAAAAAAAAAA:rename",
		`{"name":"EU-WEST"}`, adminTok, "")
	if r.code != http.StatusBadRequest {
		t.Errorf("rename bad slug: code=%d body=%s, want 400", r.code, r.body)
	}
}

// --- delete: 204 + audit, blocked 409 ----------------------------------

func TestOrgsHTTP_DeleteEmpty_204_AuditRow(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	r := doOrgReq(t, ts, http.MethodDelete, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "")
	if r.code != http.StatusNoContent {
		t.Fatalf("delete empty: code=%d body=%s, want 204", r.code, r.body)
	}

	// Org gone.
	if _, ok, _ := srv.st.GetOrg(context.Background(), "og_AAAAAAAAAAAAAAAA"); ok {
		t.Errorf("delete left org row behind")
	}
	// One org_delete audit row.
	rows := findOrgAudit(auditsFor(t, srv, "acme"), "org_delete")
	if len(rows) != 1 {
		t.Fatalf("org_delete rows = %d, want 1 (rows=%+v)", len(rows), rows)
	}
	if rows[0].Detail != "emea" {
		t.Errorf("audit detail = %q, want emea", rows[0].Detail)
	}
}

func TestOrgsHTTP_DeleteOwnsSites_409(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")
	// Attach one site to the org so CountSitesByOrg returns 1.
	if err := srv.st.AttachSiteToOrg(context.Background(), "sgp01", "og_AAAAAAAAAAAAAAAA", "svc:admin"); err != nil {
		t.Fatalf("AttachSiteToOrg: %v", err)
	}

	r := doOrgReq(t, ts, http.MethodDelete, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "")
	if r.code != http.StatusConflict {
		t.Fatalf("delete owned: code=%d body=%s, want 409", r.code, r.body)
	}
	if !strings.Contains(r.body, "org-owns-resources") {
		t.Errorf("body = %q, want org-owns-resources tail", r.body)
	}
	// Org still present.
	if _, ok, _ := srv.st.GetOrg(context.Background(), "og_AAAAAAAAAAAAAAAA"); !ok {
		t.Errorf("blocked delete removed the row")
	}
	// No audit row.
	if got := findOrgAudit(auditsFor(t, srv, "acme"), "org_delete"); len(got) != 0 {
		t.Errorf("blocked delete produced %d org_delete rows, want 0", len(got))
	}
}

// --- default org: renamable; deletable only when empty -----------------

func TestOrgsHTTP_DefaultOrg_BlockedWhenOwning_DeletableWhenEmpty(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "default")

	// Blocked-delete path (owns a site).
	if err := srv.st.AttachSiteToOrg(context.Background(), "sgp01", "og_AAAAAAAAAAAAAAAA", "svc:admin"); err != nil {
		t.Fatalf("AttachSiteToOrg: %v", err)
	}
	r := doOrgReq(t, ts, http.MethodDelete, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "")
	if r.code != http.StatusConflict {
		t.Fatalf("default owned: code=%d body=%s, want 409", r.code, r.body)
	}
	if !strings.Contains(r.body, "org-owns-resources") {
		t.Errorf("body = %q, want org-owns-resources", r.body)
	}

	// Detach the site, then default is deletable.
	// fileStore has no detach primitive; emulate by deleting the
	// siteOrgs entry directly. fileStore is the test backend.
	fs := srv.st.(*fileStore)
	fs.mu.Lock()
	for site, so := range fs.siteOrgs {
		if so.OrgID == "og_AAAAAAAAAAAAAAAA" {
			delete(fs.siteOrgs, site)
			break
		}
	}
	fs.mu.Unlock()

	r = doOrgReq(t, ts, http.MethodDelete, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "")
	if r.code != http.StatusNoContent {
		t.Fatalf("default empty: code=%d body=%s, want 204", r.code, r.body)
	}
}

func TestOrgsHTTP_DefaultOrg_RenameWorks(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "default")

	r := doOrgReq(t, ts, http.MethodPost, "/v1/orgs/og_AAAAAAAAAAAAAAAA:rename",
		`{"name":"fallback"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("default rename: code=%d body=%s, want 200", r.code, r.body)
	}
	if got := findOrgAudit(auditsFor(t, srv, "acme"), "org_rename"); len(got) != 1 {
		t.Errorf("default rename produced %d org_rename rows, want 1", len(got))
	}
}

// --- non-admin: 403 on every operation --------------------------------

func TestOrgsHTTP_NonAdmin_ForbiddenOnEveryOp(t *testing.T) {
	v, viewerTok, operatorTok, _ := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/v1/orgs?tenant_id=acme", ""},
		{"create", http.MethodPost, "/v1/orgs", `{"tenant_id":"acme","name":"x"}`},
		{"get", http.MethodGet, "/v1/orgs/og_AAAAAAAAAAAAAAAA", ""},
		{"rename", http.MethodPost, "/v1/orgs/og_AAAAAAAAAAAAAAAA:rename", `{"name":"y"}`},
		{"delete", http.MethodDelete, "/v1/orgs/og_AAAAAAAAAAAAAAAA", ""},
	}
	for _, c := range cases {
		for _, tok := range []string{viewerTok, operatorTok} {
			r := doOrgReq(t, ts, c.method, c.path, c.body, tok, "")
			if r.code != http.StatusForbidden {
				t.Errorf("%s/%s: code=%d body=%s, want 403", c.name, tok, r.code, r.body)
			}
		}
	}
	// Anonymous also forbidden on every op. With core/authmw.go
	// in the §3 whitelist (mapRequest maps /v1/orgs → isAPI=true),
	// the middleware rejects a missing token with 401 *before* the
	// handler runs. The §4.2 admin-gate is the second line of
	// defence (handler 403 on Principal present but not admin).
	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme", "", "", "")
	if r.code != http.StatusUnauthorized {
		t.Errorf("anonymous list: code=%d body=%s, want 401", r.code, r.body)
	}
}

// --- tenant-scoping R1 -------------------------------------------------

func TestOrgsHTTP_TenantScoping_PresentMatches(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedTenant(t, srv, "globex", "Globex", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")
	seedOrg(t, srv, "og_BBBBBBBBBBBBBBBB", "globex", "emea")

	// X-CIOS-Tenant=acme scopes list to acme; the query param is
	// not required when header is present.
	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs", "", adminTok, "acme")
	if r.code != http.StatusOK {
		t.Fatalf("list with tenant header: code=%d body=%s, want 200", r.code, r.body)
	}
	var resp listOrgsResponse
	if err := json.Unmarshal([]byte(r.body), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, r.body)
	}
	if len(resp.Items) != 1 || resp.Items[0].TenantID != "acme" {
		t.Errorf("scoped list = %+v, want exactly one acme org", resp.Items)
	}

	// Mismatched query: header=acme, query=globex → 403.
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=globex", "", adminTok, "acme")
	if r.code != http.StatusForbidden {
		t.Errorf("mismatch: code=%d body=%s, want 403", r.code, r.body)
	}
	if !strings.Contains(r.body, "tenant-scope-mismatch") {
		t.Errorf("body = %q, want tenant-scope-mismatch tail", r.body)
	}

	// Create with header=acme, body tenant_id=globex → 403. Name
	// must satisfy validTenantSlug ([a-z][a-z0-9-]{1,30}, ≥2 chars)
	// so the handler reaches the tenant-scope-mismatch branch
	// rather than 400-ing on a bad slug.
	r = doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"globex","name":"emea"}`, adminTok, "acme")
	if r.code != http.StatusForbidden {
		t.Errorf("create mismatch: code=%d body=%s, want 403", r.code, r.body)
	}
	if !strings.Contains(r.body, "tenant-scope-mismatch") {
		t.Errorf("create mismatch body = %q, want tenant-scope-mismatch tail", r.body)
	}

	// Get org whose tenant != header → 403.
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs/og_BBBBBBBBBBBBBBBB", "", adminTok, "acme")
	if r.code != http.StatusForbidden {
		t.Errorf("get cross-tenant: code=%d body=%s, want 403", r.code, r.body)
	}

	// Get org whose tenant == header → 200.
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs/og_AAAAAAAAAAAAAAAA", "", adminTok, "acme")
	if r.code != http.StatusOK {
		t.Errorf("get in-tenant: code=%d body=%s, want 200", r.code, r.body)
	}
}

func TestOrgsHTTP_TenantScoping_AbsentAdminPlatformWide(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedTenant(t, srv, "globex", "Globex", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAAAA", "acme", "emea")

	// No X-CIOS-Tenant + admin + query tenant_id=acme → 200,
	// platform-wide scope honoured from the query.
	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("ops-realm list: code=%d body=%s, want 200", r.code, r.body)
	}

	// No tenant anywhere → 400 (refuse implicit platform-wide).
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs", "", adminTok, "")
	if r.code != http.StatusBadRequest {
		t.Errorf("missing tenant everywhere: code=%d body=%s, want 400", r.code, r.body)
	}

	// Create with body tenant_id + no header (ops-realm) → 201.
	r = doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"globex","name":"emea2"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Errorf("ops-realm create: code=%d body=%s, want 201", r.code, r.body)
	}
}
