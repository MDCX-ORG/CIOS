package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestL109_AuthMap_AdminCanReachSurfaces is the dogfood gate:
// L109 admin routes must be registered in mapRequest so authMW
// attaches Principal. Unmapped /v1 paths skip auth → requireOrgAdmin
// always 403 — acceptance #1–3 cannot pass through real Handler().
func TestL109_AuthMap_AdminCanReachSurfaces(t *testing.T) {
	// Isolate model-studio artifacts for layout writeback.
	studio := t.TempDir()
	oldStudio := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = oldStudio })

	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedTenant(t, srv, "acme", "ACME Inc", "label")
	seedOrg(t, srv, "og_DEFAULTACME00001", "acme", DefaultOrgName)

	// 1) Tenants list
	r := doOrgReq(t, ts, http.MethodGet, "/v1/tenants", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("GET /v1/tenants: %d %s", r.code, r.body)
	}

	// 2) Create org under acme
	r = doOrgReq(t, ts, http.MethodPost, "/v1/orgs",
		`{"tenant_id":"acme","name":"ops"}`, adminTok, "")
	if r.code != http.StatusCreated {
		t.Fatalf("POST /v1/orgs: %d %s", r.code, r.body)
	}
	var org Org
	if err := json.Unmarshal([]byte(r.body), &org); err != nil {
		t.Fatal(err)
	}

	// 3) Site → org
	r = doOrgReq(t, ts, http.MethodPost, "/v1/site-orgs",
		`{"site":"sgp01","org_id":"`+org.ID+`"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("POST /v1/site-orgs: %d %s", r.code, r.body)
	}
	r = doOrgReq(t, ts, http.MethodGet, "/v1/site-orgs", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("GET /v1/site-orgs: %d %s", r.code, r.body)
	}

	// 4) Role binding
	r = doOrgReq(t, ts, http.MethodPost, "/v1/role-bindings",
		`{"subject":"svc:op","scope":"sgp01.**","origin":"legacy"}`, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("POST /v1/role-bindings: %d %s", r.code, r.body)
	}

	// 5) Model packs catalog (must not 403)
	r = doOrgReq(t, ts, http.MethodGet, "/v1/model-packs", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("GET /v1/model-packs: %d %s", r.code, r.body)
	}

	// 6) Site layout put + writeback
	layout := `{"site":"sgp01","instances":[{"id":"i1","path":"sgp01.pod000","type":"pod","model":"DC45","x":10,"y":20,"rot":0},{"id":"i2","path":"sgp01.pod000.cdu000","type":"cdu","x":40,"y":20,"rot":0}],"edges":[{"id":"e1","from_id":"i1","to_id":"i2","relation":"feeds"}]}`
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/site-layouts/sgp01",
		strings.NewReader(layout))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("PUT layout: %d %s", resp.StatusCode, string(buf[:n]))
	}

	r = doOrgReq(t, ts, http.MethodPost, "/v1/site-layouts/sgp01:writeback",
		layout, adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("writeback: %d %s", r.code, r.body)
	}
	a, ok, err := srv.st.GetAsset(context.Background(), "sgp01.pod000")
	if err != nil || !ok {
		t.Fatalf("cmdb asset after writeback: ok=%v err=%v", ok, err)
	}
	if a.Spec["type"] != "pod" {
		t.Fatalf("asset spec: %+v", a.Spec)
	}

	// 7) Scene rebuild kick — must not be auth-denied
	r = doOrgReq(t, ts, http.MethodPost, "/v1/site-layouts/sgp01:rebuild-scene",
		"", adminTok, "")
	if r.code == http.StatusForbidden || r.code == http.StatusUnauthorized {
		t.Fatalf("rebuild-scene auth fail: %d %s", r.code, r.body)
	}
	if r.code != http.StatusAccepted {
		t.Logf("rebuild-scene status=%d (expected 202; soft note) body=%s", r.code, r.body)
	}
}
