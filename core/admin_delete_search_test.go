// PRMT-220: hard delete site-orgs/tenants + list search filters.
package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPRMT220_SiteOrgDetachAndSearch(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_DEFAULTACME00001", "acme", DefaultOrgName)

	r := doOrgReq(t, ts, http.MethodPost, "/v1/site-orgs",
		`{"site":"sgp01","org_id":"og_DEFAULTACME00001"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("attach sgp01: %d %s", r.code, r.body)
	}
	r = doOrgReq(t, ts, http.MethodPost, "/v1/site-orgs",
		`{"site":"fra02","org_id":"og_DEFAULTACME00001"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("attach fra02: %d %s", r.code, r.body)
	}

	// Search q=sgp
	r = doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs?q=sgp", "", adminTok, "")
	if r.code != 200 {
		t.Fatalf("GET search: %d %s", r.code, r.body)
	}
	var list listSiteOrgsResponse
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Site != "sgp01" {
		t.Fatalf("search items = %+v, want [sgp01]", list.Items)
	}

	// Detach
	r = doOrgReq(t, ts, http.MethodDelete, "/v1/site-orgs?site=sgp01", "", adminTok, "")
	if r.code != http.StatusNoContent {
		t.Fatalf("DELETE site: %d %s", r.code, r.body)
	}
	if _, ok, _ := srv.st.GetSiteOrg(t.Context(), "sgp01"); ok {
		t.Fatal("sgp01 still mapped after detach")
	}
	auds := findOrgAudit(auditsFor(t, srv, "acme"), "org_reattach")
	found := false
	for _, a := range auds {
		if strings.Contains(a.Detail, "sgp01:") && strings.HasSuffix(a.Detail, "→") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected org_reattach unbind audit, got %+v", auds)
	}

	// 404 on unknown
	r = doOrgReq(t, ts, http.MethodDelete, "/v1/site-orgs?site=zzz99", "", adminTok, "")
	if r.code != http.StatusNotFound {
		t.Fatalf("DELETE unknown: %d %s", r.code, r.body)
	}
}

func TestPRMT220_TenantDeleteGuardsAndSearch(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Create via API so default org exists
	r := doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"beta","display_name":"Beta Co"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Fatalf("create beta: %d %s", r.code, r.body)
	}
	var created tenantCreateResponse
	if err := json.Unmarshal([]byte(r.body), &created); err != nil {
		t.Fatal(err)
	}
	r = doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"gamma","display_name":"Gamma LLC"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Fatalf("create gamma: %d %s", r.code, r.body)
	}

	// Search by display name substring
	r = doOrgReq(t, ts, http.MethodGet, "/v1/tenants?q=gamma", "", adminTok, "")
	if r.code != 200 {
		t.Fatalf("GET tenants q: %d %s", r.code, r.body)
	}
	var list listTenantsResponse
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "gamma" {
		t.Fatalf("search items = %+v, want gamma", list.Items)
	}

	// Delete tenant while orgs remain → 409
	r = doOrgReq(t, ts, http.MethodDelete, "/v1/tenants/beta", "", adminTok, "")
	if r.code != http.StatusConflict || !strings.Contains(r.body, "tenant-owns-orgs") {
		t.Fatalf("DELETE tenant with orgs: %d %s", r.code, r.body)
	}

	// Delete default org then tenant
	r = doOrgReq(t, ts, http.MethodDelete, "/v1/orgs/"+created.DefaultOrg.ID, "", adminTok, "")
	if r.code != http.StatusNoContent {
		t.Fatalf("DELETE org: %d %s", r.code, r.body)
	}
	r = doOrgReq(t, ts, http.MethodDelete, "/v1/tenants/beta", "", adminTok, "")
	if r.code != http.StatusNoContent {
		t.Fatalf("DELETE tenant: %d %s", r.code, r.body)
	}
	if _, ok, _ := srv.st.GetTenant(t.Context(), "beta"); ok {
		t.Fatal("beta still present")
	}
	auds := findOrgAudit(auditsFor(t, srv, "beta"), "tenant_status")
	found := false
	for _, a := range auds {
		if a.Detail == "deleted" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected tenant_status deleted audit, got %+v", auds)
	}
}
