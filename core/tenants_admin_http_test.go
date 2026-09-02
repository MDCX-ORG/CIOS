package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTenantsAdmin_ListCreate(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_DEFAULTACME0001", "acme", DefaultOrgName)

	r := doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	var list listTenantsResponse
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "acme" {
		t.Fatalf("list items: %+v", list.Items)
	}
	if list.Items[0].DefaultOrg == nil || list.Items[0].DefaultOrg.Name != DefaultOrgName {
		t.Fatalf("default_org: %+v", list.Items[0].DefaultOrg)
	}

	r = doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"beta","display_name":"Beta Ltd"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var created tenantCreateResponse
	if err := json.Unmarshal([]byte(r.body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "beta" || created.IsolationTier != "label" || created.Status != "active" {
		t.Fatalf("created tenant: %+v", created.Tenant)
	}
	if created.DefaultOrg.Name != DefaultOrgName || created.DefaultOrg.TenantID != "beta" {
		t.Fatalf("default org: %+v", created.DefaultOrg)
	}

	// duplicate
	r = doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"beta","display_name":"Again"}`, adminTok, "")
	if r.code != http.StatusConflict {
		t.Fatalf("dup: %d %s", r.code, r.body)
	}

	// bad slug
	r = doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"Bad","display_name":"X"}`, adminTok, "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("bad slug: %d %s", r.code, r.body)
	}

	// tenant-scoped caller cannot create
	r = doOrgReq(t, ts, http.MethodPost, "/v1/tenants",
		`{"id":"gamma","display_name":"G"}`, adminTok, "acme")
	if r.code != http.StatusForbidden {
		t.Fatalf("scoped create: %d %s", r.code, r.body)
	}

	// list now 2
	r = doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list2: %d %s", r.code, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want 2 tenants, got %+v", list.Items)
	}
}

func TestCreateTenant_Store(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	tnt, org, err := st.CreateTenant(context.Background(), "gamma", "Gamma Co", "svc:admin")
	if err != nil {
		t.Fatal(err)
	}
	if tnt.ID != "gamma" || org.Name != DefaultOrgName {
		t.Fatalf("%+v %+v", tnt, org)
	}
	_, _, err = st.CreateTenant(context.Background(), "gamma", "Again", "svc:admin")
	if err != ErrTenantExists {
		t.Fatalf("want ErrTenantExists, got %v", err)
	}
	orgs, err := st.ListOrgs(context.Background(), "gamma")
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs: %+v err=%v", orgs, err)
	}
}

// TestTenantsList_EmptyOrgsIsJSONArray guards the wire contract that a
// tenant with zero orgs still serializes "orgs":[] (never null). Uses
// the platform-admin batch path (ListOrgsAll + buildTenantListItem).
func TestTenantsList_EmptyOrgsIsJSONArray(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Insert tenant with no org rows (bypass CreateTenant which always
	// seeds default).
	fs := srv.st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["lonely"] = Tenant{
		ID: "lonely", DisplayName: "Lonely", IsolationTier: "label", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	fs.mu.Unlock()

	r := doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	// Raw wire: orgs must be [] not null for the empty-org tenant.
	if strings.Contains(r.body, `"id":"lonely"`) && strings.Contains(r.body, `"orgs":null`) {
		t.Fatalf("lonely row has orgs:null; want [] — body=%s", r.body)
	}
	if !strings.Contains(r.body, `"orgs":[]`) {
		// May share the page with other tenants; require at least one empty array.
		// Decode and check lonely specifically.
	}
	var list listTenantsResponse
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range list.Items {
		if it.ID != "lonely" {
			continue
		}
		found = true
		if it.Orgs == nil {
			t.Fatalf("lonely.Orgs is nil after decode (wire was null?) body=%s", r.body)
		}
		if len(it.Orgs) != 0 {
			t.Fatalf("lonely.Orgs=%+v want empty", it.Orgs)
		}
		if it.DefaultOrg != nil {
			t.Fatalf("lonely.DefaultOrg=%+v want nil", it.DefaultOrg)
		}
	}
	if !found {
		t.Fatalf("lonely tenant missing from list: %s", r.body)
	}
	// Positive wire proof: the lonely object fragment uses "orgs":[]
	if !strings.Contains(r.body, `"orgs":[]`) {
		t.Fatalf(`body missing "orgs":[] — got %s`, r.body)
	}
}
