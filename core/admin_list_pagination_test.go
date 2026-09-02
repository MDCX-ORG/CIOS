// Package core — admin list pagination (PRMT-218).
//
// Four surfaces: GET /v1/tenants, /v1/orgs, /v1/site-orgs, /v1/role-bindings.
// Default page_size = MaxPageSize (1000); next_page_token omitempty.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func adminPagServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, adminTok
}

func decodeItems[T any](t *testing.T, body string) (items []T, next string, raw map[string]json.RawMessage) {
	t.Helper()
	raw = map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, body)
	}
	if _, ok := raw["next_page_token"]; ok {
		_ = json.Unmarshal(raw["next_page_token"], &next)
		// strip quotes already handled by Unmarshal into string
	}
	var env struct {
		Items         []T    `json:"items"`
		NextPageToken string `json:"next_page_token"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	return env.Items, env.NextPageToken, raw
}

func hasNextKey(raw map[string]json.RawMessage) bool {
	_, ok := raw["next_page_token"]
	return ok
}

// --- tenants ---

func TestTenantsList_Pagination_FourCells(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	fs := srv.st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// Cell 1: n < 1000, no token key
	seedTenant(t, srv, "acme", "ACME", "label")
	seedOrg(t, srv, "og_DEFAULTACME0001", "acme", DefaultOrgName)
	r := doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell1: %d %s", r.code, r.body)
	}
	items, next, raw := decodeItems[tenantListItem](t, r.body)
	if len(items) != 1 || next != "" || hasNextKey(raw) {
		t.Fatalf("cell1: n=%d next=%q hasKey=%v body=%s", len(items), next, hasNextKey(raw), r.body)
	}
	if items[0].Orgs == nil {
		t.Fatal("cell1: orgs must be [] not null")
	}

	// Cell 2: n > 1000 → 1000 + token
	fs.mu.Lock()
	for i := 0; i < 1005; i++ {
		id := fmt.Sprintf("t%04d", i)
		fs.tenants[id] = Tenant{
			ID: id, DisplayName: id, IsolationTier: "label", Status: "active",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	fs.mu.Unlock()
	r = doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell2: %d %s", r.code, r.body)
	}
	items, next, raw = decodeItems[tenantListItem](t, r.body)
	// acme + 1005 = 1006 total → first page 1000 + token
	if len(items) != MaxPageSize || next == "" || !hasNextKey(raw) {
		t.Fatalf("cell2: n=%d next=%q hasKey=%v want %d + token", len(items), next, hasNextKey(raw), MaxPageSize)
	}

	// Cell 3: page_size=10 walk to end; union == full set
	seen := map[string]bool{}
	tok := ""
	pages := 0
	for {
		path := "/v1/tenants?page_size=10"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r = doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		if r.code != http.StatusOK {
			t.Fatalf("cell3 page: %d %s", r.code, r.body)
		}
		var page []tenantListItem
		page, tok, _ = decodeItems[tenantListItem](t, r.body)
		for _, it := range page {
			if seen[it.ID] {
				t.Fatalf("cell3: duplicate %s", it.ID)
			}
			seen[it.ID] = true
		}
		pages++
		if tok == "" {
			break
		}
		if pages > 200 {
			t.Fatal("cell3: too many pages")
		}
	}
	// Full set size without paging
	all, err := srv.st.ListTenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Fatalf("cell3: walked %d want %d", len(seen), len(all))
	}

	// Cell 4: bad token
	r = doOrgReq(t, ts, http.MethodGet, "/v1/tenants?page_token=garbage", "", adminTok, "")
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "bad page_token") {
		t.Fatalf("cell4: %d %s", r.code, r.body)
	}
}

// --- orgs ---

func TestOrgsList_Pagination_FourCells(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	seedTenant(t, srv, "acme", "ACME", "label")
	fs := srv.st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// Cell 1: few orgs
	seedOrg(t, srv, "og_AAAAAAAAAAAAAA01", "acme", "alpha")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAA02", "acme", "beta")
	r := doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell1: %d %s", r.code, r.body)
	}
	items, next, raw := decodeItems[Org](t, r.body)
	if len(items) != 2 || next != "" || hasNextKey(raw) {
		t.Fatalf("cell1: n=%d next=%q key=%v", len(items), next, hasNextKey(raw))
	}

	// Cell 2: >1000 orgs under acme
	fs.mu.Lock()
	for i := 0; i < 1005; i++ {
		name := fmt.Sprintf("o%04d", i)
		id := fmt.Sprintf("og_%04dxxxxxxxx", i)
		fs.orgs[id] = Org{ID: id, TenantID: "acme", Name: name, CreatedAt: now}
	}
	fs.mu.Unlock()
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell2: %d %s", r.code, r.body)
	}
	items, next, raw = decodeItems[Org](t, r.body)
	if len(items) != MaxPageSize || next == "" || !hasNextKey(raw) {
		t.Fatalf("cell2: n=%d next=%q", len(items), next)
	}

	// Cell 3: page_size=10 full walk
	seen := map[string]bool{}
	tok := ""
	for pages := 0; ; pages++ {
		path := "/v1/orgs?tenant_id=acme&page_size=10"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r = doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		if r.code != http.StatusOK {
			t.Fatalf("cell3: %d %s", r.code, r.body)
		}
		var page []Org
		page, tok, _ = decodeItems[Org](t, r.body)
		for _, o := range page {
			if seen[o.Name] {
				t.Fatalf("dup %s", o.Name)
			}
			seen[o.Name] = true
		}
		if tok == "" {
			break
		}
		if pages > 300 {
			t.Fatal("too many pages")
		}
	}
	all, err := srv.st.ListOrgs(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Fatalf("walked %d want %d", len(seen), len(all))
	}

	// Cell 4
	r = doOrgReq(t, ts, http.MethodGet, "/v1/orgs?tenant_id=acme&page_token=!!!", "", adminTok, "")
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "bad page_token") {
		t.Fatalf("cell4: %d %s", r.code, r.body)
	}
}

// --- site-orgs ---

func TestSiteOrgsList_Pagination_FourCells(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	seedTenant(t, srv, "acme", "ACME", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAA01", "acme", DefaultOrgName)
	fs := srv.st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// Cell 1
	fs.mu.Lock()
	fs.siteOrgs["sgp01"] = SiteOrg{Site: "sgp01", OrgID: "og_AAAAAAAAAAAAAA01", CreatedAt: now, UpdatedAt: now}
	fs.mu.Unlock()
	r := doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell1: %d %s", r.code, r.body)
	}
	items, next, raw := decodeItems[SiteOrg](t, r.body)
	if len(items) != 1 || next != "" || hasNextKey(raw) {
		t.Fatalf("cell1: n=%d next=%q", len(items), next)
	}

	// Cell 2
	fs.mu.Lock()
	for i := 0; i < 1005; i++ {
		site := fmt.Sprintf("s%04d", i)
		fs.siteOrgs[site] = SiteOrg{Site: site, OrgID: "og_AAAAAAAAAAAAAA01", CreatedAt: now, UpdatedAt: now}
	}
	fs.mu.Unlock()
	r = doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs", "", adminTok, "")
	items, next, raw = decodeItems[SiteOrg](t, r.body)
	if len(items) != MaxPageSize || next == "" || !hasNextKey(raw) {
		t.Fatalf("cell2: n=%d next=%q", len(items), next)
	}

	// Cell 3
	seen := map[string]bool{}
	tok := ""
	for pages := 0; ; pages++ {
		path := "/v1/site-orgs?page_size=10"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r = doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		var page []SiteOrg
		page, tok, _ = decodeItems[SiteOrg](t, r.body)
		for _, so := range page {
			if seen[so.Site] {
				t.Fatalf("dup %s", so.Site)
			}
			seen[so.Site] = true
		}
		if tok == "" {
			break
		}
		if pages > 300 {
			t.Fatal("too many pages")
		}
	}
	all, err := srv.st.ListSiteOrgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Fatalf("walked %d want %d", len(seen), len(all))
	}

	// Cell 4
	r = doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs?page_token=not-a-token", "", adminTok, "")
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "bad page_token") {
		t.Fatalf("cell4: %d %s", r.code, r.body)
	}
}

// --- role-bindings ---

func TestRoleBindingsList_Pagination_FourCells(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// Cell 1
	_ = srv.st.PutRoleBinding(ctx, RoleBinding{
		Subject: "svc:a", Scope: "site01.**", Origin: "legacy", CreatedAt: now, UpdatedAt: now,
	})
	r := doOrgReq(t, ts, http.MethodGet, "/v1/role-bindings", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("cell1: %d %s", r.code, r.body)
	}
	items, next, raw := decodeItems[RoleBinding](t, r.body)
	if len(items) != 1 || next != "" || hasNextKey(raw) {
		t.Fatalf("cell1: n=%d next=%q", len(items), next)
	}

	// Cell 2: >1000 rows
	for i := 0; i < 1005; i++ {
		_ = srv.st.PutRoleBinding(ctx, RoleBinding{
			Subject: fmt.Sprintf("svc:u%04d", i), Scope: "site01.**", Origin: "legacy",
			CreatedAt: now, UpdatedAt: now,
		})
	}
	r = doOrgReq(t, ts, http.MethodGet, "/v1/role-bindings", "", adminTok, "")
	items, next, raw = decodeItems[RoleBinding](t, r.body)
	if len(items) != MaxPageSize || next == "" || !hasNextKey(raw) {
		t.Fatalf("cell2: n=%d next=%q", len(items), next)
	}

	// Cell 3
	type key struct{ s, sc string }
	seen := map[key]bool{}
	tok := ""
	for pages := 0; ; pages++ {
		path := "/v1/role-bindings?page_size=10"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r = doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		var page []RoleBinding
		page, tok, _ = decodeItems[RoleBinding](t, r.body)
		for _, rb := range page {
			k := key{rb.Subject, rb.Scope}
			if seen[k] {
				t.Fatalf("dup %+v", k)
			}
			seen[k] = true
		}
		if tok == "" {
			break
		}
		if pages > 300 {
			t.Fatal("too many pages")
		}
	}
	all, err := srv.st.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Fatalf("walked %d want %d", len(seen), len(all))
	}

	// Cell 4
	r = doOrgReq(t, ts, http.MethodGet, "/v1/role-bindings?page_token=zzzz", "", adminTok, "")
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "bad page_token") {
		t.Fatalf("cell4: %d %s", r.code, r.body)
	}
}

// --- filter × pagination ---

func TestOrgsList_FilterThenPage(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	seedTenant(t, srv, "acme", "A", "label")
	seedTenant(t, srv, "beta", "B", "label")
	// acme: 12 orgs; beta: 3 orgs
	for i := 0; i < 12; i++ {
		seedOrg(t, srv, fmt.Sprintf("og_acme%02dxxxxxxxx", i), "acme", fmt.Sprintf("a%02d", i))
	}
	for i := 0; i < 3; i++ {
		seedOrg(t, srv, fmt.Sprintf("og_beta%02dxxxxxxxx", i), "beta", fmt.Sprintf("b%02d", i))
	}
	// Walk acme with page_size=5 — must only see acme orgs
	seen := map[string]bool{}
	tok := ""
	for {
		path := "/v1/orgs?tenant_id=acme&page_size=5"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r := doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		if r.code != http.StatusOK {
			t.Fatalf("%d %s", r.code, r.body)
		}
		page, next, _ := decodeItems[Org](t, r.body)
		for _, o := range page {
			if o.TenantID != "acme" {
				t.Fatalf("leaked tenant %s", o.TenantID)
			}
			seen[o.Name] = true
		}
		tok = next
		if tok == "" {
			break
		}
	}
	if len(seen) != 12 {
		t.Fatalf("acme orgs walked %d want 12", len(seen))
	}
}

func TestSiteOrgsList_FilterThenPage(t *testing.T) {
	srv, ts, adminTok := adminPagServer(t)
	seedTenant(t, srv, "acme", "A", "label")
	seedOrg(t, srv, "og_AAAAAAAAAAAAAA01", "acme", "eng")
	seedOrg(t, srv, "og_BBBBBBBBBBBBBBBB", "acme", "ops")
	fs := srv.st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	for i := 0; i < 12; i++ {
		site := fmt.Sprintf("e%02d", i)
		fs.siteOrgs[site] = SiteOrg{Site: site, OrgID: "og_AAAAAAAAAAAAAA01", CreatedAt: now, UpdatedAt: now}
	}
	for i := 0; i < 4; i++ {
		site := fmt.Sprintf("o%02d", i)
		fs.siteOrgs[site] = SiteOrg{Site: site, OrgID: "og_BBBBBBBBBBBBBBBB", CreatedAt: now, UpdatedAt: now}
	}
	fs.mu.Unlock()

	seen := map[string]bool{}
	tok := ""
	for {
		path := "/v1/site-orgs?org_id=og_AAAAAAAAAAAAAA01&page_size=5"
		if tok != "" {
			path += "&page_token=" + tok
		}
		r := doOrgReq(t, ts, http.MethodGet, path, "", adminTok, "")
		if r.code != http.StatusOK {
			t.Fatalf("%d %s", r.code, r.body)
		}
		page, next, _ := decodeItems[SiteOrg](t, r.body)
		if len(page) == 0 && next != "" {
			t.Fatal("empty page with next token — filter/page order bug")
		}
		for _, so := range page {
			if so.OrgID != "og_AAAAAAAAAAAAAA01" {
				t.Fatalf("leaked org %s", so.OrgID)
			}
			seen[so.Site] = true
		}
		tok = next
		if tok == "" {
			break
		}
	}
	if len(seen) != 12 {
		t.Fatalf("filtered sites %d want 12", len(seen))
	}
}

func TestAdminList_EmptyItemsNotNull(t *testing.T) {
	_, ts, adminTok := adminPagServer(t)
	// No tenants seeded → empty list
	r := doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !strings.Contains(r.body, `"items":[]`) {
		t.Fatalf("want items:[] got %s", r.body)
	}
	if strings.Contains(r.body, `"items":null`) {
		t.Fatal("items null")
	}
	if strings.Contains(r.body, "next_page_token") {
		t.Fatalf("empty list must omit next_page_token: %s", r.body)
	}
	// Store-level empty site-orgs / role-bindings
	r = doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs", "", adminTok, "")
	if !strings.Contains(r.body, `"items":[]`) || strings.Contains(r.body, "next_page_token") {
		t.Fatalf("site-orgs empty: %s", r.body)
	}
	r = doOrgReq(t, ts, http.MethodGet, "/v1/role-bindings", "", adminTok, "")
	if !strings.Contains(r.body, `"items":[]`) || strings.Contains(r.body, "next_page_token") {
		t.Fatalf("role-bindings empty: %s", r.body)
	}
}
