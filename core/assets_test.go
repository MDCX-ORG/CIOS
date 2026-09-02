// Asset lifecycle state-machine tests (PRMT-039). Coverage:
//   - state machine: legal/illegal lifecycle transitions
//   - 422 on illegal transition (covers the "invalid-transition" type)
//   - 422 on unknown `to` value
//   - default "planned" lifecycle on PUT when Spec["lifecycle"] absent
//   - 400 on PUT when Spec["lifecycle"] is not in the closed set
//   - 422 on illegal transition (e.g. planned→active is not allowed;
//     must walk planned→installed→active)
//   - authmw wiring: POST :lifecycle without a token returns 401
//     (regression guard against the PRMT-037 RBAC漏接 class of bug)
//   - mapRequest unit test: POST :lifecycle maps to ActionApply on
//     the bare path (no ":lifecycle" suffix leaks into the auth scope)
//   - isListScopeEndpoint: :lifecycle is NOT a list-scope endpoint
//     (full authorize, not role-floor; mirrors /v1/points/:set shape)
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- allowedLifecycleTransition unit tests -------------------------------

func TestAllowedLifecycleTransition_Legal(t *testing.T) {
	cases := []struct{ from, to string }{
		{"planned", "installed"},
		{"installed", "active"},
		{"active", "maintenance"},
		{"active", "retired"},
		{"maintenance", "active"},
		{"maintenance", "retired"},
	}
	for _, tc := range cases {
		if !allowedLifecycleTransition(tc.from, tc.to) {
			t.Errorf("expected %s->%s legal, got false", tc.from, tc.to)
		}
	}
}

func TestAllowedLifecycleTransition_Illegal(t *testing.T) {
	cases := []struct{ from, to string }{
		// same-state
		{"planned", "planned"},
		{"installed", "installed"},
		{"active", "active"},
		{"maintenance", "maintenance"},
		{"retired", "retired"},
		// skipping forward
		{"planned", "active"},
		{"planned", "maintenance"},
		{"planned", "retired"},
		{"installed", "maintenance"},
		{"installed", "retired"},
		// backward
		{"installed", "planned"},
		{"active", "planned"},
		{"active", "installed"},
		{"maintenance", "planned"},
		{"maintenance", "installed"},
		// retired is terminal
		{"retired", "planned"},
		{"retired", "installed"},
		{"retired", "active"},
		{"retired", "maintenance"},
		// unknown from
		{"banana", "active"},
		{"", "active"},
	}
	for _, tc := range cases {
		if allowedLifecycleTransition(tc.from, tc.to) {
			t.Errorf("expected %s->%s illegal, got true", tc.from, tc.to)
		}
	}
}

// --- PUT lifecycle default + validation ----------------------------------

// TestAssets_Put_DefaultsLifecycleToPlanned confirms the §4 contract:
// if Spec["lifecycle"] is absent on PUT, the stored asset has it set
// to "planned". This is the no-spec-lifecycle case the prompt calls
// out explicitly.
func TestAssets_Put_DefaultsLifecycleToPlanned(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	var a Asset
	mustJSON(t, r.body, &a)
	if got, _ := a.Spec["lifecycle"].(string); got != "planned" {
		t.Errorf("Spec.lifecycle = %q, want planned (default)", got)
	}
}

// TestAssets_Put_AcceptsEachLegalLifecycle walks every member of the
// allowedLifecycle closed set and confirms the PUT writes it through.
func TestAssets_Put_AcceptsEachLegalLifecycle(t *testing.T) {
	_, ts := newTestServer(t)
	for _, lc := range []string{"planned", "installed", "active", "maintenance", "retired"} {
		body := `{"spec":{"type":"cdu","lifecycle":"` + lc + `"}}`
		r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", body)
		if r.code/100 != 2 {
			t.Fatalf("PUT lifecycle=%s: %d %s", lc, r.code, r.body)
		}
		var a Asset
		mustJSON(t, r.body, &a)
		if got, _ := a.Spec["lifecycle"].(string); got != lc {
			t.Errorf("Spec.lifecycle = %q, want %q", got, lc)
		}
	}
}

// TestAssets_Put_RejectsUnknownLifecycleValue: 400 + bad-request when
// Spec["lifecycle"] is set to something not in the closed set.
func TestAssets_Put_RejectsUnknownLifecycleValue(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu","lifecycle":"banana"}}`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

// TestAssets_Put_RejectsNonStringLifecycle: 400 + bad-request when
// Spec["lifecycle"] is present but not a string.
func TestAssets_Put_RejectsNonStringLifecycle(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu","lifecycle":42}}`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

// --- POST :lifecycle state machine ---------------------------------------

// TestAssets_Lifecycle_LegalWalk: planned→installed→active→maintenance→active→retired.
// The "retired→X" cases are not in the walk because retired is terminal.
func TestAssets_Lifecycle_LegalWalk(t *testing.T) {
	_, ts := newTestServer(t)
	// Seed asset at "planned" (the PUT default).
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	steps := []struct{ to, want string }{
		{"installed", "installed"},
		{"active", "active"},
		{"maintenance", "maintenance"},
		{"active", "active"},
		{"retired", "retired"},
	}
	for _, s := range steps {
		r = doReq(t, ts, http.MethodPost,
			"/v1/assets/site01.pod000.cdu000:lifecycle",
			`{"to":"`+s.to+`"}`)
		if r.code != http.StatusOK {
			t.Fatalf("to=%s: %d %s", s.to, r.code, r.body)
		}
		var a Asset
		mustJSON(t, r.body, &a)
		if got, _ := a.Spec["lifecycle"].(string); got != s.want {
			t.Errorf("after to=%s: Spec.lifecycle = %q, want %q", s.to, got, s.want)
		}
	}
}

// TestAssets_Lifecycle_IllegalTransitions: from a freshly-created
// (planned) asset, every illegal target must return 422 with
// type=invalid-transition. The legal set is small (installed only);
// everything else is 422.
func TestAssets_Lifecycle_IllegalTransitions(t *testing.T) {
	cases := []struct{ from, to string }{
		{"planned", "planned"},     // same-state
		{"planned", "active"},      // skip
		{"planned", "maintenance"}, // skip
		{"planned", "retired"},     // skip
		{"retired", "active"},      // terminal
		{"retired", "maintenance"}, // terminal
		{"retired", "planned"},     // terminal
	}
	for _, tc := range cases {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			_, ts := newTestServer(t)
			// Seed with a known starting state.
			putBody := `{"spec":{"type":"cdu","lifecycle":"` + tc.from + `"}}`
			r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", putBody)
			if r.code/100 != 2 {
				t.Fatalf("seed PUT: %d %s", r.code, r.body)
			}
			r = doReq(t, ts, http.MethodPost,
				"/v1/assets/site01.pod000.cdu000:lifecycle",
				`{"to":"`+tc.to+`"}`)
			if r.code != http.StatusUnprocessableEntity {
				t.Fatalf("%s->%s: code = %d, want 422; body=%s",
					tc.from, tc.to, r.code, r.body)
			}
			mustProblem(t, r.body, "invalid-transition")
		})
	}
}

// TestAssets_Lifecycle_UnknownToValue: the `to` value is not a member
// of allowedLifecycle. 422 + invalid-transition (the membership check
// runs before the transition check per §4).
func TestAssets_Lifecycle_UnknownToValue(t *testing.T) {
	_, ts := newTestServer(t)
	// Seed an asset first so we don't get a 404 instead.
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"banana"}`)
	if r.code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "invalid-transition")
}

// TestAssets_Lifecycle_MissingAsset_404: the lifecycle endpoint
// requires an existing asset (spec-001 §asset model — lifecycle
// is a property of an existing asset).
func TestAssets_Lifecycle_MissingAsset_404(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`)
	if r.code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "path-not-found")
}

// TestAssets_Lifecycle_BadPath_400: the lifecycle endpoint must reject
// non-asset paths up front (defense in depth: cpath ParseAssetPath
// gate).
func TestAssets_Lifecycle_BadPath_400(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost,
		"/v1/assets/garbage..path:lifecycle",
		`{"to":"installed"}`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-path")
}

// TestAssets_Lifecycle_BadJSON_400: malformed body.
func TestAssets_Lifecycle_BadJSON_400(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`not json`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

// TestAssets_Lifecycle_BumpsResourceVersion: each successful
// transition must bump the asset's ResourceVersion, so a follow-up
// PUT with the original version produces a 409 (PRMT-039 §4: write
// with expectVersion=cur.ResourceVersion).
func TestAssets_Lifecycle_BumpsResourceVersion(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	var a0 Asset
	mustJSON(t, r.body, &a0)
	r = doReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`)
	if r.code != http.StatusOK {
		t.Fatalf("transition: %d %s", r.code, r.body)
	}
	var a1 Asset
	mustJSON(t, r.body, &a1)
	if a1.ResourceVersion <= a0.ResourceVersion {
		t.Errorf("version did not bump: v0=%d v1=%d", a0.ResourceVersion, a1.ResourceVersion)
	}
}

// TestAssets_Lifecycle_AuthMW_NoToken_401 is the regression guard for
// the PRMT-037 RBAC漏接 class of bug: an auth-enabled server without
// a bearer token must reject POST :lifecycle at the auth middleware
// (401), not let it through to the inner handler. Without the
// mapRequest registration in core/authmw.go this test would fail with
// 200 (or 404 if no asset, but never 401).
func TestAssets_Lifecycle_AuthMW_NoToken_401(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`)
	if r.code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (PRMT-037-style RBAC regression guard); body=%s",
			r.code, r.body)
	}
	if !strings.Contains(r.body, `"type":"https://cios.dev/errors/unauthorized"`) {
		t.Errorf("body missing unauthorized type tail: %s", r.body)
	}
}

// TestAssets_Lifecycle_AuthMW_Admin_OK: per the prompt's §7 Q9
// decision (subject to spec-008 v0.3 sign-off) the lifecycle
// endpoint maps to ActionApply, which L50 reserves for the admin
// role. Operator cannot apply — the test below pins the negative
// side. Admin bypasses scope; seeded asset transitions to
// "installed" successfully.
func TestAssets_Lifecycle_AuthMW_Admin_OK(t *testing.T) {
	v, _, _, adminTok := buildVerifierForRoles(t, nil, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu", "lifecycle": "planned"},
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := authReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`, adminTok)
	if r.code != http.StatusOK {
		t.Fatalf("admin transition: code = %d, want 200; body=%s", r.code, r.body)
	}
	var a Asset
	mustJSON(t, r.body, &a)
	if got, _ := a.Spec["lifecycle"].(string); got != "installed" {
		t.Errorf("Spec.lifecycle = %q, want installed", got)
	}
}

// TestAssets_Lifecycle_AuthMW_OperatorCannotApply_403: per L50 +
// ActionApply semantics, only admin can apply. Operator with
// site01.** scope still gets 403 on the :lifecycle endpoint.
func TestAssets_Lifecycle_AuthMW_OperatorCannotApply_403(t *testing.T) {
	v, _, operatorTok, _ := buildVerifierForRoles(t,
		nil,
		[]string{"site01.**"},
		nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu", "lifecycle": "planned"},
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := authReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`, operatorTok)
	if r.code != http.StatusForbidden {
		t.Fatalf("operator apply: code = %d, want 403; body=%s", r.code, r.body)
	}
}

// TestAssets_Lifecycle_AuthMW_ViewerWrite_403: viewer cannot apply.
func TestAssets_Lifecycle_AuthMW_ViewerWrite_403(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t,
		[]string{"site01.**"},
		nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := authReq(t, ts, http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle",
		`{"to":"installed"}`, viewerTok)
	if r.code != http.StatusForbidden {
		t.Fatalf("viewer transition: code = %d, want 403; body=%s", r.code, r.body)
	}
}

// --- mapRequest + isListScopeEndpoint unit coverage -----------------------

// TestMapRequest_Lifecycle: pins the (action, path) tuple that
// POST /v1/assets/{path}:lifecycle maps to. The :lifecycle suffix
// must be stripped so authorize() targets the bare asset path, and
// the action must be ActionApply (admin role), per §4 + §5 of the
// prompt.
func TestMapRequest_Lifecycle(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle", nil)
	a, p, isAPI := mapRequest(req)
	if !isAPI {
		t.Fatalf("isAPI = false, want true (route must be API-gated)")
	}
	if a != ActionApply {
		t.Errorf("action = %q, want %q (admin apply)", a, ActionApply)
	}
	if p != "site01.pod000.cdu000" {
		t.Errorf("path = %q, want %q (no :lifecycle suffix)", p, "site01.pod000.cdu000")
	}
	// Non-POST on the same URL must NOT match the lifecycle branch;
	// it falls through to PUT/DELETE/GET handling (ActionApply on
	// PUT/DELETE, ActionRead on GET).
	req, _ = http.NewRequest(http.MethodPut,
		"/v1/assets/site01.pod000.cdu000:lifecycle", nil)
	a, p, _ = mapRequest(req)
	if a != ActionApply {
		t.Errorf("PUT :lifecycle action = %q, want %q", a, ActionApply)
	}
	if p != "site01.pod000.cdu000:lifecycle" {
		t.Errorf("PUT :lifecycle path = %q, want the raw segment (no strip)", p)
	}
}

// TestIsListScopeEndpoint_Lifecycle: :lifecycle is a single-resource
// write and must NOT enter the role-floor branch (admin-only via
// ActionApply, no per-item handler filter). Mirrors the
// /v1/points/...:set shape.
func TestIsListScopeEndpoint_Lifecycle(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost,
		"/v1/assets/site01.pod000.cdu000:lifecycle", nil)
	if isListScopeEndpoint(req) {
		t.Errorf("isListScopeEndpoint(POST :lifecycle) = true, want false " +
			"(single-resource write → full authorize)")
	}
}

// --- PRMT-067: ops-search filter quartet (type/lifecycle/prefix/limit) ---

// listAssetsWire mirrors listAssetsResponse in production code so
// tests can decode the wire body without depending on the
// unexported type.
type listAssetsWire struct {
	Items         []Asset `json:"items"`
	NextPageToken string  `json:"next_page_token"`
}

// doAuthedGetRaw does an authed GET and decodes JSON into `into`
// when the response is 200. Returns (status, body). Inlined from
// authz_list_test.go's doAuthedGet so this file does not need a
// shared test-fixture file just for the auth header.
func doAuthedGetRaw(t *testing.T, ts *httptest.Server, path, token string, into any) (int, string) {
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
	b, _ := io.ReadAll(resp.Body)
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, into); err != nil {
			t.Fatalf("decode %s: %v\nbody: %s", path, err, string(b))
		}
	}
	return resp.StatusCode, string(b)
}

// TestAssets_ListFilterByType: ?type=cdu narrows the set to cdu
// leaves only. The store-level test seeds 3 cdu + 1 chiller; type
// filter returns the 3 cdu items.
func TestAssets_ListFilterByType(t *testing.T) {
	_, ts := newTestServer(t)
	// Use the public store (newTestServer returned the server too but
	// the simplest path is to PUT three cdu + one chiller via HTTP).
	for _, p := range []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site01.pod000.cdu002",
	} {
		r := doReq(t, ts, http.MethodPut, "/v1/assets/"+p,
			`{"spec":{"type":"cdu"}}`)
		if r.code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", p, r.code, r.body)
		}
	}
	// chiller is a site-level type (per types.yaml: chiller parents=[site, pod]).
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.chiller000",
		`{"spec":{"type":"chiller"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT chiller: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?type=cdu", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 3 {
		t.Errorf("type=cdu len(items)=%d, want 3 (got %+v)", len(got.Items), got.Items)
	}
	for _, a := range got.Items {
		if !strings.HasPrefix(a.Path, "site01.pod000.cdu") {
			t.Errorf("type=cdu leaked non-cdu path %q", a.Path)
		}
	}
}

// TestAssets_ListFilterByType_UnknownYieldsEmpty: per the prompt's
// §2 contract, an unknown/illegal type value is NOT a 400 — it
// just yields the empty set. The dict has no "banana" type, so no
// item's leaf type will ever match.
func TestAssets_ListFilterByType_UnknownYieldsEmpty(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?type=banana", "")
	if r.code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (empty set on unknown type)", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 0 {
		t.Errorf("type=banana len(items)=%d, want 0 (got %+v)", len(got.Items), got.Items)
	}
}

// TestAssets_ListFilterByLifecycle: ?lifecycle=active narrows the
// set to assets whose Spec.lifecycle == "active" (or the default
// "planned" if absent — but here we explicitly set them so the
// check is direct).
func TestAssets_ListFilterByLifecycle(t *testing.T) {
	_, ts := newTestServer(t)
	seed := []struct{ path, lc string }{
		{"site01.pod000.cdu000", "planned"},
		{"site01.pod000.cdu001", "active"},
		{"site01.pod000.cdu002", "active"},
		{"site01.pod000.cdu003", "retired"},
	}
	for _, s := range seed {
		body := `{"spec":{"type":"cdu","lifecycle":"` + s.lc + `"}}`
		r := doReq(t, ts, http.MethodPut, "/v1/assets/"+s.path, body)
		if r.code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", s.path, r.code, r.body)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/assets?lifecycle=active", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 2 {
		t.Errorf("lifecycle=active len(items)=%d, want 2 (got %+v)", len(got.Items), got.Items)
	}
	for _, a := range got.Items {
		lc, _ := a.Spec["lifecycle"].(string)
		if lc != "active" {
			t.Errorf("lifecycle=active leaked Spec.lifecycle=%q path=%q", lc, a.Path)
		}
	}
}

// TestAssets_ListFilterByLifecycle_DefaultsToPlanned: when an asset
// has no Spec.lifecycle on PUT, the server defaults it to "planned"
// (PRMT-039 §4). ?lifecycle=planned must therefore include that
// asset.
func TestAssets_ListFilterByLifecycle_DefaultsToPlanned(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?lifecycle=planned", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 1 {
		t.Errorf("lifecycle=planned len(items)=%d, want 1 (got %+v)", len(got.Items), got.Items)
	}
}

// TestAssets_ListFilterByLifecycle_BadValue_400: an unknown lifecycle
// value must return 400 (per the prompt's §2 contract and to mirror
// the PUT validation policy).
func TestAssets_ListFilterByLifecycle_BadValue_400(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/assets?lifecycle=banana", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 (bad lifecycle)", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

// TestAssets_ListFilterByPrefix: ?prefix=site01.pod000 narrows to
// that subtree. The prompt uses "sgp01.pod002" as the example; the
// path grammar is the same.
func TestAssets_ListFilterByPrefix(t *testing.T) {
	_, ts := newTestServer(t)
	seed := []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site01.pod001.cdu000",
		"site02.pod000.cdu000",
	}
	for _, p := range seed {
		r := doReq(t, ts, http.MethodPut, "/v1/assets/"+p,
			`{"spec":{"type":"cdu"}}`)
		if r.code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", p, r.code, r.body)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/assets?prefix=site01.pod000", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 2 {
		t.Errorf("prefix=site01.pod000 len(items)=%d, want 2 (got %+v)", len(got.Items), got.Items)
	}
	for _, a := range got.Items {
		if !strings.HasPrefix(a.Path, "site01.pod000") {
			t.Errorf("prefix=site01.pod000 leaked path %q", a.Path)
		}
	}
}

// TestAssets_ListFilterByPrefix_Empty: a prefix that matches
// nothing returns 200 + empty items.
func TestAssets_ListFilterByPrefix_Empty(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?prefix=site99", "")
	if r.code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 0 {
		t.Errorf("prefix=site99 len(items)=%d, want 0", len(got.Items))
	}
}

// TestAssets_ListFilterCombined_TypeAndLifecycle: filters stack
// with AND. type=cdu AND lifecycle=active must return only items
// satisfying both.
func TestAssets_ListFilterCombined_TypeAndLifecycle(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu","lifecycle":"active"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT cdu active: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu001",
		`{"spec":{"type":"cdu","lifecycle":"planned"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT cdu planned: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodPut, "/v1/assets/site01.chiller000",
		`{"spec":{"type":"chiller","lifecycle":"active"}}`)
	if r.code/100 != 2 {
		t.Fatalf("PUT chiller active: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?type=cdu&lifecycle=active", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 1 {
		t.Errorf("type=cdu&lifecycle=active len(items)=%d, want 1 (got %+v)", len(got.Items), got.Items)
	}
	if len(got.Items) >= 1 && got.Items[0].Path != "site01.pod000.cdu000" {
		t.Errorf("got path %q, want site01.pod000.cdu000", got.Items[0].Path)
	}
}

// TestAssets_ListFilterCombined_PrefixAndType: prefix + type stack.
// Useful for "show me every cdu under sgp01.pod002" workflows.
func TestAssets_ListFilterCombined_PrefixAndType(t *testing.T) {
	_, ts := newTestServer(t)
	// Two cdus under site01.pod000, one under site01.pod001.
	for _, p := range []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site01.pod001.cdu000",
	} {
		r := doReq(t, ts, http.MethodPut, "/v1/assets/"+p,
			`{"spec":{"type":"cdu"}}`)
		if r.code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", p, r.code, r.body)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/assets?prefix=site01.pod000&type=cdu", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 2 {
		t.Errorf("prefix+type len(items)=%d, want 2 (got %+v)", len(got.Items), got.Items)
	}
}

// TestAssets_ListFilter_LimitCapsResults: ?limit=N caps the page.
// Seed 5 items, ?limit=2 returns 2 items + a non-empty next_page_token.
func TestAssets_ListFilter_LimitCapsResults(t *testing.T) {
	_, ts := newTestServer(t)
	for i := 0; i < 5; i++ {
		p := "site01.pod000.cdu" + fmt.Sprintf("%03d", i)
		r := doReq(t, ts, http.MethodPut, "/v1/assets/"+p,
			`{"spec":{"type":"cdu"}}`)
		if r.code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", p, r.code, r.body)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/assets?limit=2", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 2 {
		t.Errorf("limit=2 len(items)=%d, want 2", len(got.Items))
	}
	if got.NextPageToken == "" {
		t.Errorf("limit=2 must set next_page_token (5 items, capped to 2)")
	}
}

// TestAssets_ListFilter_LimitOver1000_400: limit > 1000 is 400,
// mirroring the page_size cap.
func TestAssets_ListFilter_LimitOver1000_400(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/assets?limit=1001", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

// TestAssets_ListFilter_LimitZeroOrNegative_400: limit=0 and
// limit=-1 are 400.
func TestAssets_ListFilter_LimitZeroOrNegative_400(t *testing.T) {
	_, ts := newTestServer(t)
	for _, bad := range []string{"0", "-1", "abc"} {
		r := doReq(t, ts, http.MethodGet, "/v1/assets?limit="+bad, "")
		if r.code != http.StatusBadRequest {
			t.Errorf("limit=%s status=%d body=%s, want 400", bad, r.code, r.body)
		}
	}
}

// TestAssets_ListFilter_ScopeStillEnforced: the per-item scope
// check must run AFTER the field filter (so it cannot be bypassed
// by filtering). A viewer scoped to site01.** must NOT see
// site02 items even when the filter matches them.
func TestAssets_ListFilter_ScopeStillEnforced(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t, []string{"site01.**"}, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Seed directly via the store (PUT is apply-role gated in auth
	// mode; bypass to seed test data — mirrors the pattern in
	// authz_list_test.go's seedAssets helper).
	for _, p := range []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site02.pod000.cdu000",
		"site02.pod000.cdu001",
	} {
		if _, err := srv.st.PutAsset(context.Background(), Asset{Path: p, Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	// type=cdu matches all 4, but viewer scope keeps only site01 (2).
	r := authReq(t, ts, http.MethodGet, "/v1/assets?type=cdu", "", viewerTok)
	if r.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.code, r.body)
	}
	var got listAssetsWire
	mustJSON(t, r.body, &got)
	if len(got.Items) != 2 {
		t.Errorf("type=cdu (scoped viewer) len(items)=%d, want 2 (got %+v)",
			len(got.Items), got.Items)
	}
	for _, a := range got.Items {
		if !strings.HasPrefix(a.Path, "site01.") {
			t.Errorf("scope bypassed: path %q leaked past filter", a.Path)
		}
	}
	// prefix=site02 matches the out-of-scope set; the request must
	// return 200 + empty items (not 403, not 200 with items) — the
	// filter narrows the set, then scope drops everything.
	r = authReq(t, ts, http.MethodGet, "/v1/assets?prefix=site02&type=cdu", "", viewerTok)
	if r.code != http.StatusOK {
		t.Fatalf("prefix+type (out-of-scope): status=%d body=%s", r.code, r.body)
	}
	mustJSON(t, r.body, &got)
	if len(got.Items) != 0 {
		t.Errorf("prefix=site02&type=cdu (out-of-scope viewer) len(items)=%d, want 0",
			len(got.Items))
	}
}
