// L109 P802/P803 HTTP smoke: site-orgs attach + role-bindings put/list/delete.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminHTTP_SiteOrgsAndRoleBindings(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	fs := st.(*fileStore)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{ID: "acme", DisplayName: "ACME", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.orgs["og_AAAAAAAAAAAAAA01"] = Org{ID: "og_AAAAAAAAAAAAAA01", TenantID: "acme", Name: "default", CreatedAt: now}
	fs.mu.Unlock()

	// Minimal server with auth: inject principal via ServeHTTP wrapper.
	srv := &Server{st: st}
	// Build handler chain manually: no auth mw — call handlers with principal in context.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/site-orgs", srv.serveSiteOrgs)
	mux.HandleFunc("/v1/role-bindings", srv.serveRoleBindings)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, ciPrincipal(ciAdmin))
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// POST site-org
	body := []byte(`{"site":"sgp01","org_id":"og_AAAAAAAAAAAAAA01"}`)
	res, err := http.Post(ts.URL+"/v1/site-orgs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST site-orgs status=%d", res.StatusCode)
	}

	// GET site-orgs
	res2, err := http.Get(ts.URL + "/v1/site-orgs")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("GET site-orgs status=%d", res2.StatusCode)
	}
	var list struct {
		Items []SiteOrg `json:"items"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Site != "sgp01" {
		t.Fatalf("list site-orgs: %+v", list.Items)
	}

	// POST role-binding
	rbBody := []byte(`{"subject":"svc:op","scope":"sgp01.**","origin":"legacy"}`)
	res3, err := http.Post(ts.URL+"/v1/role-bindings", "application/json", bytes.NewReader(rbBody))
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("POST role-bindings status=%d", res3.StatusCode)
	}

	res4, err := http.Get(ts.URL + "/v1/role-bindings")
	if err != nil {
		t.Fatal(err)
	}
	defer res4.Body.Close()
	var rlist struct {
		Items []RoleBinding `json:"items"`
	}
	if err := json.NewDecoder(res4.Body).Decode(&rlist); err != nil {
		t.Fatal(err)
	}
	if len(rlist.Items) != 1 {
		t.Fatalf("list role-bindings: %+v", rlist.Items)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/role-bindings?subject=svc:op&scope=sgp01.**", nil)
	res5, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res5.Body.Close()
	if res5.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE role-bindings status=%d", res5.StatusCode)
	}
}
