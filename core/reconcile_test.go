// Package core — reconcile_test.go: coverage for the PRMT-050
// /v1/reports/reconcile handler.
//
// Coverage (mirrors PRMT-050 §6 acceptance list):
//
//   - happy path: registered_no_telemetry / telemetry_no_asset /
//     ok classification, per-asset lifecycle echo
//   - lifecycle filter: planned/retired/instaled assets do not
//     appear (only active|maintenance)
//   - VM unreachable: per-asset unknown + response-wide degraded
//   - auth: GET without bearer → 401 (PRMT-037 regression guard)
//   - orphans scope: viewer (roleAllows ActionControlWrite fails)
//     sees empty Orphans + OrphansRestricted=true; operator/admin
//     see real orphan list
//   - bad window: 400
//   - POST: 405
//   - mapRequest + isListScopeEndpoint unit tests for the new URL
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// --- helpers (kept local to avoid collisions) -----------------------------

// hashTokReconcile mirrors the auth_test pattern.
func hashTokReconcile(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// buildReconcileVerifier returns a verifier with three deterministic
// tokens (viewer / operator / admin).
func buildReconcileVerifier(t *testing.T, viewerScopes, operatorScopes, adminScopes []string) (TokenVerifier, string, string, string) {
	t.Helper()
	const (
		viewerTok   = "rec-viewer-token"
		operatorTok = "rec-operator-token"
		adminTok    = "rec-admin-token"
	)
	v, err := NewStaticTokenVerifier(map[string]Principal{
		hashTokReconcile(viewerTok):   {Subject: "svc:viewer", Role: RoleViewer, Scopes: viewerScopes},
		hashTokReconcile(operatorTok): {Subject: "svc:operator", Role: RoleOperator, Scopes: operatorScopes},
		hashTokReconcile(adminTok):    {Subject: "svc:admin", Role: RoleAdmin, Scopes: adminScopes},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	return v, viewerTok, operatorTok, adminTok
}

// newReconcileTestServer returns a Server + httptest.Server pair
// with auth disabled and the supplied VM URL wired in.
func newReconcileTestServer(t *testing.T, vmURL string) (*Server, *httptest.Server) {
	t.Helper()
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, vmURL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// newReconcileAuthTestServer is the auth-enabled variant; returns
// the Server only so the test can attach the httptest.Server.
func newReconcileAuthTestServer(t *testing.T, withAuth *AuthConfig, vmURL string) *Server {
	t.Helper()
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, vmURL)
	srv.auth = withAuth
	return srv
}

// doAuthedReconcileGet mirrors doAuthedCapacityGet — kept local so
// the file is self-contained.
func doAuthedReconcileGet(t *testing.T, ts *httptest.Server, path, token string, into any) (int, string) {
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

// seedReconcileAssets writes one Asset per path with the supplied
// spec map.
func seedReconcileAssets(t *testing.T, srv *Server, paths []string, spec map[string]any) {
	t.Helper()
	for _, p := range paths {
		if _, err := srv.st.PutAsset(context.Background(), Asset{Path: p, Spec: spec}, 0); err != nil {
			t.Fatalf("PutAsset %s: %v", p, err)
		}
	}
}

// vmReconcileServer builds a fake VM whose handler inspects the
// query string and returns deterministic success/empty/label-values
// results. The dispatcher is intentionally simple: per-asset
// presence queries get a 1.0 (present); label_values queries get
// the supplied set of paths. The test can set paths via the
// closure.
func vmReconcileServer(t *testing.T, presentPaths, allLabelPaths []string) *httptest.Server {
	t.Helper()
	present := make(map[string]bool, len(presentPaths))
	for _, p := range presentPaths {
		present[p] = true
	}
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		// label_values(...) returns the union we handed in.
		if strings.HasPrefix(q, "label_values(") {
			results := make([]string, 0, len(allLabelPaths))
			for _, p := range allLabelPaths {
				results = append(results,
					`{"metric":{"asset_path":"`+p+`"},"value":[1700000000,"1"]}`)
			}
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+
				strings.Join(results, ",")+`]}}`)
			return
		}
		// PRMT-086: recognise BOTH the per-asset exact matcher
		// (legacy / sink helpers) and the batch regex matcher
		// (production). For the batch form, emit one series per
		// requested path: present if it's in the present set,
		// absent otherwise. The assetHasTelemetryBatch parser
		// keys on series.Metric["asset_path"].
		paths := parseAssetPathMatcher(q)
		if paths == nil {
			http.Error(w, "unrecognised matcher", http.StatusBadRequest)
			return
		}
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			if present[p] {
				parts = append(parts, `{"metric":{"asset_path":"`+p+`"},"value":[1700000000,"1"]}`)
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+
			strings.Join(parts, ",")+`]}}`)
	}))
	t.Cleanup(vm.Close)
	return vm
}

// --- 401 regression guard ------------------------------------------------

func TestReconcile_NoBearer_Returns401(t *testing.T) {
	v, _, _, _ := buildReconcileVerifier(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	var seen Principal
	var mu sync.Mutex
	mw := &authMW{verifier: v, inner: passthroughInnerReconcile(&seen, &mu), auditLog: sink.log}
	h := withRequestID(mw)
	req := httptest.NewRequest(http.MethodGet, "/v1/reports/reconcile", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (PRMT-037 regression guard)", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(w.Body.String(), `"type":"https://cios.dev/errors/unauthorized"`) {
		t.Errorf("body missing unauthorized type tail: %s", w.Body.String())
	}
}

// passthroughInnerReconcile mirrors passthroughInner from auth_test.go.
func passthroughInnerReconcile(seen *Principal, mu *sync.Mutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFromContext(r.Context()); ok {
			mu.Lock()
			*seen = p
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// --- mapRequest + isListScopeEndpoint unit tests ------------------------

func TestReconcile_MapRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/reports/reconcile", nil)
	act, path, isAPI := mapRequest(req)
	if !isAPI {
		t.Fatalf("mapRequest: isAPI=false, want true")
	}
	if act != ActionRead {
		t.Errorf("mapRequest action = %q, want read", act)
	}
	if path != "**" {
		t.Errorf("mapRequest path = %q, want **", path)
	}
}

func TestReconcile_IsListScopeEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/reports/reconcile", nil)
	if !isListScopeEndpoint(req) {
		t.Errorf("GET /v1/reports/reconcile must be list-scope (per-item filter in handler)")
	}
	post := httptest.NewRequest(http.MethodPost, "/v1/reports/reconcile", nil)
	if isListScopeEndpoint(post) {
		t.Errorf("POST /v1/reports/reconcile must NOT be list-scope")
	}
}

// --- classification: ok / registered_no_telemetry / telemetry_no_asset ---

func TestReconcile_HappyPath_ClassifiesEntries(t *testing.T) {
	// registered: A (active), B (active); C is the orphan.
	// telemetry: A and C have samples; B does not (offline).
	vm := vmReconcileServer(t,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu999"},
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu999"},
	)
	srv, ts := newReconcileTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", r.code, r.body)
	}
	var got reconcileResponse
	mustJSON(t, r.body, &got)
	if got.Window != "7d" {
		t.Errorf("window = %q, want 7d (default)", got.Window)
	}
	if got.Degraded {
		t.Errorf("degraded = true, want false (VM happy)")
	}
	if got.OrphansRestricted {
		t.Errorf("orphans_restricted = true, want false (auth disabled)")
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(got.Entries))
	}
	byPath := map[string]reconcileEntry{}
	for _, e := range got.Entries {
		byPath[e.Path] = e
	}
	if a := byPath["site01.pod000.cdu000"]; a.State != "ok" || !a.TelemetryPresent {
		t.Errorf("A (active+telemetry): %+v, want ok/present", a)
	}
	if b := byPath["site01.pod000.cdu001"]; b.State != "registered_no_telemetry" || b.TelemetryPresent {
		t.Errorf("B (active+no_telemetry): %+v, want registered_no_telemetry/absent", b)
	}
	if len(got.Orphans) != 1 || got.Orphans[0].Path != "site01.pod000.cdu999" {
		t.Errorf("orphans = %+v, want one orphan at site01.pod000.cdu999", got.Orphans)
	}
}

// --- lifecycle filter: planned/retired/instaled excluded ------------------

func TestReconcile_LifecycleFilter_OnlyActiveAndMaintenance(t *testing.T) {
	vm := vmReconcileServer(t,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		nil,
	)
	srv, ts := newReconcileTestServer(t, vm.URL)
	for _, c := range []struct {
		path      string
		lifecycle string
	}{
		{"site01.pod000.cdu000", "active"},
		{"site01.pod000.cdu001", "maintenance"},
		{"site01.pod000.cdu002", "planned"},
		{"site01.pod000.cdu003", "retired"},
		{"site01.pod000.cdu004", "installed"},
	} {
		if _, err := srv.st.PutAsset(context.Background(), Asset{
			Path: c.path,
			Spec: map[string]any{"type": "cdu", "lifecycle": c.lifecycle},
		}, 0); err != nil {
			t.Fatalf("put %s: %v", c.path, err)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.code)
	}
	var got reconcileResponse
	mustJSON(t, r.body, &got)
	if len(got.Entries) != 2 {
		t.Errorf("entries len = %d, want 2 (active+maintenance only); got=%+v", len(got.Entries), got.Entries)
	}
	for _, e := range got.Entries {
		if e.Lifecycle != "active" && e.Lifecycle != "maintenance" {
			t.Errorf("leaked non-counted lifecycle %q at %q", e.Lifecycle, e.Path)
		}
	}
}

// --- VM unreachable → degraded + per-asset unknown ------------------------

func TestReconcile_VMUnreachable_Degraded(t *testing.T) {
	// Point at a non-listening port → fetchVM fails → per-asset
	// TelemetryUnknown=true + response-wide Degraded=true. The
	// handler MUST NOT 500.
	srv, ts := newReconcileTestServer(t, "http://127.0.0.1:1")
	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-soft); body=%s", r.code, r.body)
	}
	var got reconcileResponse
	mustJSON(t, r.body, &got)
	if !got.Degraded {
		t.Errorf("degraded = false, want true (VM unreachable)")
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(got.Entries))
	}
	if !got.Entries[0].TelemetryUnknown {
		t.Errorf("entry telemetry_unknown = false, want true")
	}
}

// --- orphans scope: viewer hidden, operator visible ----------------------

func TestReconcile_OrphansScope_ViewerRestricted(t *testing.T) {
	// Auth-enabled. Operator sees the orphan list; viewer does not.
	vm := vmReconcileServer(t,
		[]string{"site01.pod000.cdu000"},
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu999"},
	)
	v, viewerTok, operatorTok, _ := buildReconcileVerifier(t,
		[]string{"**"},
		[]string{"**"},
		nil,
	)
	srv := newReconcileAuthTestServer(t, &AuthConfig{Verifier: v}, vm.URL)
	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Viewer path: Orphans is empty + OrphansRestricted=true.
	viewerStatus, viewerBody := doAuthedReconcileGet(t, ts, "/v1/reports/reconcile", viewerTok, nil)
	if viewerStatus != http.StatusOK {
		t.Fatalf("viewer status=%d body=%s", viewerStatus, viewerBody)
	}
	var vresp reconcileResponse
	mustJSON(t, viewerBody, &vresp)
	if !vresp.OrphansRestricted {
		t.Errorf("viewer orphans_restricted = false, want true")
	}
	if len(vresp.Orphans) != 0 {
		t.Errorf("viewer orphans len = %d, want 0 (suppressed)", len(vresp.Orphans))
	}
	// Entries still visible to viewer (the per-item scope matches **).
	if len(vresp.Entries) != 1 {
		t.Errorf("viewer entries len = %d, want 1", len(vresp.Entries))
	}

	// Operator path: Orphans has the real orphan.
	opStatus, opBody := doAuthedReconcileGet(t, ts, "/v1/reports/reconcile", operatorTok, nil)
	if opStatus != http.StatusOK {
		t.Fatalf("operator status=%d body=%s", opStatus, opBody)
	}
	var oresp reconcileResponse
	mustJSON(t, opBody, &oresp)
	if oresp.OrphansRestricted {
		t.Errorf("operator orphans_restricted = true, want false")
	}
	if len(oresp.Orphans) != 1 || oresp.Orphans[0].Path != "site01.pod000.cdu999" {
		t.Errorf("operator orphans = %+v, want one orphan", oresp.Orphans)
	}
}

// --- bad window → 400 ----------------------------------------------------

func TestReconcile_BadWindow_400(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil)
	_, ts := newReconcileTestServer(t, vm.URL)
	for _, bad := range []string{"7", "7dd", "banana", "-1d", "0d", "999d"} {
		r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile?window="+bad, "")
		if r.code != http.StatusBadRequest {
			t.Errorf("window=%q: status=%d body=%s, want 400", bad, r.code, r.body)
		}
	}
}

// --- POST → 405 ----------------------------------------------------------

func TestReconcile_PostReturns405(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil)
	_, ts := newReconcileTestServer(t, vm.URL)
	r := doReq(t, ts, http.MethodPost, "/v1/reports/reconcile", "{}")
	if r.code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", r.code)
	}
}

// --- authz: viewer with disjoint scope sees empty entries ---------------

func TestReconcile_ScopeFilter_DropsOutOfScope(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil)
	v, viewerTok, _, _ := buildReconcileVerifier(t,
		[]string{"site99.**"}, nil, nil,
	)
	srv := newReconcileAuthTestServer(t, &AuthConfig{Verifier: v}, vm.URL)
	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	status, body := doAuthedReconcileGet(t, ts, "/v1/reports/reconcile", viewerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got reconcileResponse
	mustJSON(t, body, &got)
	if len(got.Entries) != 0 {
		t.Errorf("entries len = %d, want 0 (viewer with disjoint scope)", len(got.Entries))
	}
}

// --- Drift auto-ticket scanner (PRMT-057 / M2 E2.6 闭环) -----
//
// Coverage matrix (matches the §7 acceptance regex
// `Reconcile.*Scan|DriftTicket`):
//
//   - scanReconcileTick fires one ticket per registered_no_telemetry
//     asset and skips ok assets
//   - scanReconcileTick is idempotent: a second tick on the same
//     drift does NOT open a duplicate (dedup via reconcile:<path>)
//   - scanReconcileTick skips the tick entirely when degraded
//     (VM unreachable) — drift classification is unreliable
//   - Restored telemetry does NOT auto-close the open ticket
//     (spec-008 Q5)
//   - Dedup releases after the existing ticket is closed: a
//     fresh drift event opens a new ticket
//   - Dedup key namespacing: an open ticket with a different
//     alarm_id (alarm-driven, spare:, etc.) does NOT block firing
//   - RunReconcileScanner returns immediately when interval<=0
//     (default-off contract)
//   - RunReconcileScanner exits cleanly on ctx.Done

// newDriftScannerTestServer mirrors newReconcileTestServer but
// reuses the per-test fake VM wired in. We need it because the
// drift scanner hits the VM exactly the same way the HTTP
// endpoint does (assetHasTelemetry → count_over_time), so the
// fake VM's presence list is the only knob controlling what
// "registered_no_telemetry" means.
func newDriftScannerTestServer(t *testing.T, vmURL string) *Server {
	t.Helper()
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return NewServer(st, dict, vmURL)
}

// TestReconcileScan_FiresOnDrift plants one drifting asset (no
// telemetry) and one healthy asset (has telemetry). One tick must
// open exactly one ticket — pinned to the drifting asset only.
func TestReconcileScan_FiresOnDrift(t *testing.T) {
	vm := vmReconcileServer(t,
		[]string{"site01.pod000.cdu000"}, // only CDU000 has telemetry
		nil,
	)
	srv := newDriftScannerTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	srv.scanReconcileTick(context.Background(), time.Now().UTC(), "7d")

	tickets, err := srv.st.ListTickets(context.Background())
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket (only the drifting asset), got %d", len(tickets))
	}
	got := tickets[0]
	if got.AssetPath != "site01.pod000.cdu001" {
		t.Errorf("AssetPath = %q, want the drifting path cdu001", got.AssetPath)
	}
	if got.Severity != "major" {
		t.Errorf("Severity = %q, want major", got.Severity)
	}
	if got.State != "open" {
		t.Errorf("State = %q, want open", got.State)
	}
	if got.AlarmID != reconcileAlarmID("site01.pod000.cdu001") {
		t.Errorf("AlarmID = %q, want %q (dedup namespace)",
			got.AlarmID, reconcileAlarmID("site01.pod000.cdu001"))
	}
	if got.Title != "No telemetry: site01.pod000.cdu001" {
		t.Errorf("Title = %q, want %q", got.Title, "No telemetry: site01.pod000.cdu001")
	}
}

// TestReconcileScan_DedupNoDuplicate ticks twice on the same
// drifting asset — the second tick must NOT open a duplicate
// because the open ticket from the first tick already pins the
// alarm_id="reconcile:<path>" dedup key.
func TestReconcileScan_DedupNoDuplicate(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil) // no telemetry for any asset
	srv := newDriftScannerTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	ctx := context.Background()
	now := time.Now().UTC()
	srv.scanReconcileTick(ctx, now, "7d")
	srv.scanReconcileTick(ctx, now.Add(time.Minute), "7d")
	srv.scanReconcileTick(ctx, now.Add(2*time.Minute), "7d")
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket (dedup), got %d", len(tickets))
	}
	if tickets[0].AlarmID != reconcileAlarmID("site01.pod000.cdu001") {
		t.Errorf("dedup key drifted: alarm_id=%q", tickets[0].AlarmID)
	}
}

// TestReconcileScan_DegradedSkipsOpen wires a VM at an
// unreachable port so computeReconcile returns degraded=true.
// The scanner must skip the tick entirely — opening a ticket on
// a degraded classification would risk false positives
// (transient VM outage → spurious ticket).
func TestReconcileScan_DegradedSkipsOpen(t *testing.T) {
	srv := newDriftScannerTestServer(t, "http://127.0.0.1:1")
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	srv.scanReconcileTick(context.Background(), time.Now().UTC(), "7d")
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 0 {
		t.Errorf("degraded tick must NOT open tickets; got %d", len(tickets))
	}
}

// TestReconcileScan_RecoveryDoesNotAutoClose covers spec-008
// Q5: telemetry coming back does NOT auto-close the open drift
// ticket. The scanner leaves it for human acknowledgement; if
// it is then closed AND drift recurs, a fresh ticket opens.
// This is the "every drift event gets a ticket" guarantee.
//
// We exercise this in three phases against the SAME Store (so
// the open ticket persists across phases):
//  1. Phase A: empty VM → drift tick opens a ticket.
//  2. Phase B: a present VM (same Store) — the scanner sees
//     the asset as ok, fires nothing, and there is no auto-close
//     code path in scanReconcileTick at all, so the ticket stays
//     open until a human transitions it.
//  3. Phase C: operator closes the ticket, VM goes empty again,
//     drift recurs → fresh ticket opens (dedup gate released).
func TestReconcileScan_RecoveryDoesNotAutoClose(t *testing.T) {
	const assetPath = "site01.pod000.cdu001"
	// One shared Store across phases so the ticket from phase A
	// is visible in phases B and C.
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedReconcileAssets(t,
		&Server{st: st, d: dict, vmURL: "http://unused"}, // vmURL set per-phase below
		[]string{assetPath},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)

	// Phase A: empty VM → drift ticket opens.
	vmEmpty := vmReconcileServer(t, nil, nil)
	srvA := NewServer(st, dict, vmEmpty.URL)
	now := time.Now().UTC()
	srvA.scanReconcileTick(context.Background(), now, "7d")
	tickets, _ := st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("phase A: expected 1 ticket, got %d", len(tickets))
	}
	first := tickets[0]
	if first.State != "open" {
		t.Fatalf("phase A ticket state = %q, want open", first.State)
	}

	// Phase B: VM reports telemetry → asset is now ok. The
	// scanner fires nothing AND does not auto-close — there is no
	// close path in scanReconcileTick. The ticket stays open.
	vmPresent := vmReconcileServer(t, []string{assetPath}, nil)
	srvB := NewServer(st, dict, vmPresent.URL)
	srvB.scanReconcileTick(context.Background(), now.Add(time.Minute), "7d")
	tickets, _ = st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("phase B: expected 1 ticket still open, got %d", len(tickets))
	}
	if tickets[0].ID != first.ID {
		t.Errorf("phase B: ticket id drifted; recovery must NOT auto-close")
	}
	if tickets[0].State != "open" {
		t.Errorf("phase B: state = %q, want open (no auto-close)", tickets[0].State)
	}

	// Operator closes the ticket (manual ack → close).
	closedAt := time.Now().UTC()
	first.State = "closed"
	first.ClosedAt = &closedAt
	if _, err := st.PutTicket(context.Background(), first, 0); err != nil {
		t.Fatalf("close ticket: %v", err)
	}

	// Phase C: VM goes empty again → drift recurs → fresh ticket.
	srvC := NewServer(st, dict, vmEmpty.URL)
	srvC.scanReconcileTick(context.Background(), now.Add(2*time.Minute), "7d")
	tickets, _ = st.ListTickets(context.Background())
	if len(tickets) != 2 {
		t.Fatalf("phase C: expected 2 tickets (closed + re-fire), got %d", len(tickets))
	}
	var openCount int
	for _, tk := range tickets {
		if tk.State == "open" {
			openCount++
			if tk.ID == first.ID {
				t.Errorf("re-fire must mint a new id; got the closed ticket id back")
			}
			if tk.AlarmID != reconcileAlarmID(assetPath) {
				t.Errorf("re-fire alarm_id = %q, want %q",
					tk.AlarmID, reconcileAlarmID(assetPath))
			}
		}
	}
	if openCount != 1 {
		t.Errorf("expected 1 open ticket after re-fire, got %d", openCount)
	}
}

// TestReconcileScan_DedupIgnoresForeignAlarmID pins the
// "reconcile:" namespace convention: an existing open ticket
// with a DIFFERENT alarm_id (alarm-driven, spare:, etc.) must
// NOT block the drift scanner from firing. Mirrors the spare
// low-stock scanner's same guarantee.
func TestReconcileScan_DedupIgnoresForeignAlarmID(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil)
	srv := newDriftScannerTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	// Plant an open ticket with a foreign alarm_id (spare-shape
	// noise; dedup key MUST ignore it).
	foreign := Ticket{
		ID:        "tk_AAAAAAAAAAAAAAAA",
		AlarmID:   "spare:sp_FOREIGN0000001",
		AssetPath: "site01.pod000.cdu001",
		Title:     "spare low-stock pinned to same path",
		Severity:  "minor",
		State:     "open",
		OpenedAt:  time.Now().UTC(),
	}
	if _, err := srv.st.PutTicket(context.Background(), foreign, 0); err != nil {
		t.Fatalf("PutTicket foreign: %v", err)
	}
	srv.scanReconcileTick(context.Background(), time.Now().UTC(), "7d")
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 2 {
		t.Fatalf("expected 2 tickets (foreign + drift), got %d", len(tickets))
	}
	var sawDrift bool
	for _, tk := range tickets {
		if tk.AlarmID == reconcileAlarmID("site01.pod000.cdu001") {
			sawDrift = true
		}
	}
	if !sawDrift {
		t.Errorf("drift ticket not opened; foreign alarm_id must not block")
	}
}

// TestReconcileScan_DefaultOffWhenIntervalZero pins the
// "empty=off" contract: interval<=0 must return immediately
// (no goroutine started, no startup tick, no log spam beyond the
// disabled notice). Matches the report-scheduler convention.
func TestReconcileScan_DefaultOffWhenIntervalZero(t *testing.T) {
	srv := newDriftScannerTestServer(t, "http://127.0.0.1:1")
	done := make(chan struct{})
	go func() {
		srv.RunReconcileScanner(context.Background(), 0, "7d")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunReconcileScanner(interval=0) did not return promptly")
	}
}

// TestRunReconcileScannerExitsOnCtx asserts the long-lived
// scanner returns when its ctx is cancelled (mirrors PM/SLA/
// spare scanner shutdown contract).
func TestRunReconcileScannerExitsOnCtx(t *testing.T) {
	vm := vmReconcileServer(t, nil, nil)
	srv := newDriftScannerTestServer(t, vm.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.RunReconcileScanner(ctx, 50*time.Millisecond, "7d")
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunReconcileScanner did not exit on ctx cancel")
	}
}

// --- PRMT-086: aggregate VM query + map dispatch --------------------------

// TestReconcile_BatchQuery_OneQueryForAllAssets verifies that the
// production assetHasTelemetryBatch path is exercised: a single
// VM call returns a vector keyed by asset_path, the handler
// dispatches per-asset presence without a second network call.
// The fake VM counts queries; a correctly-aggregated reconcile
// for N assets issues exactly 1 (presence) + 0 (no label_values
// in auth-disabled mode if no orphans are needed... actually we
// also call label_values for the orphan set, so the count
// depends on the role floor. In auth-disabled mode the handler
// always pulls the orphan set, so we expect 1 + 1 = 2 queries.)
func TestReconcile_BatchQuery_OneQueryForAllAssets(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q, "label_values(") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		// Presence batch: all queried paths are present (1.0).
		paths := parseAssetPathMatcher(q)
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			parts = append(parts, `{"metric":{"asset_path":"`+p+`"},"value":[1700000000,"1"]}`)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(parts, ",")+`]}}`)
	}))
	t.Cleanup(vm.Close)

	srv, ts := newReconcileTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001", "site01.pod000.cdu002"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", r.code, r.body)
	}
	mu.Lock()
	defer mu.Unlock()
	// Auth-disabled path: 1 presence batch + 1 label_values = 2
	// queries. The presence query must use the batch regex form.
	if len(queries) != 2 {
		t.Errorf("VM queries = %d, want 2 (1 presence + 1 label_values); got=%v", len(queries), queries)
	}
	var sawBatch bool
	for _, q := range queries {
		if strings.HasPrefix(q, "label_values(") {
			continue
		}
		if strings.Contains(q, `asset_path=~"`) {
			sawBatch = true
		}
	}
	if !sawBatch {
		t.Errorf("no batch presence query seen; got=%v", queries)
	}
}

// TestReconcile_BatchQuery_MissingPathClassifiedAbsent verifies
// that a path the in-scope set requested but VM didn't return is
// classified as "registered_no_telemetry" (NOT "unknown") — same
// shape as the legacy per-asset empty-vector path.
func TestReconcile_BatchQuery_MissingPathClassifiedAbsent(t *testing.T) {
	// VM returns a presence series for cdu000 only; cdu001 is
	// in-scope but VM has no data for it. The handler must mark
	// cdu001 as registered_no_telemetry, not unknown.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q, "label_values(") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		paths := parseAssetPathMatcher(q)
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			if p == "site01.pod000.cdu000" {
				parts = append(parts, `{"metric":{"asset_path":"`+p+`"},"value":[1700000000,"1"]}`)
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(parts, ",")+`]}}`)
	}))
	t.Cleanup(vm.Close)

	srv, ts := newReconcileTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", r.code, r.body)
	}
	var got reconcileResponse
	mustJSON(t, r.body, &got)
	if got.Degraded {
		t.Errorf("degraded = true, want false (VM happy, just no data for cdu001)")
	}
	byPath := map[string]reconcileEntry{}
	for _, e := range got.Entries {
		byPath[e.Path] = e
	}
	if a := byPath["site01.pod000.cdu000"]; a.State != "ok" || !a.TelemetryPresent {
		t.Errorf("cdu000: %+v, want ok/present", a)
	}
	if b := byPath["site01.pod000.cdu001"]; b.State != "registered_no_telemetry" || b.TelemetryPresent || b.TelemetryUnknown {
		t.Errorf("cdu001: %+v, want registered_no_telemetry/absent/known", b)
	}
}

// TestReconcile_BatchQuery_VMUnreachable_AllUnknown verifies the
// fail-soft contract on a dead VM: every per-asset entry is
// unknown, response is 200, response-wide Degraded=true.
func TestReconcile_BatchQuery_VMUnreachable_AllUnknown(t *testing.T) {
	srv, ts := newReconcileTestServer(t, "http://127.0.0.1:1") // dead
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-soft); body=%s", r.code, r.body)
	}
	var got reconcileResponse
	mustJSON(t, r.body, &got)
	if !got.Degraded {
		t.Errorf("degraded = false, want true (VM unreachable)")
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(got.Entries))
	}
	for _, e := range got.Entries {
		if !e.TelemetryUnknown {
			t.Errorf("entry %q telemetry_unknown = false, want true", e.Path)
		}
	}
}

// TestReconcile_BatchQuery_RegexSafeMatcher_QuotesDotsAndAnchors
// is the FINAL-RULING regression guard (architect 2026-06-21):
// the batch query uses a regex `=~` matcher, so each path token
// must be regexp.QuoteMeta'd (literal `.`, not "any char") and
// the whole alternation must be anchored `^(...)$` so each path
// is an exact match. The previous escapeLabelValue-only matcher
// over-matched (any `.` matched any char) and was unanchored
// (substring match — a path contained inside another would also
// hit). This test asserts on the wire form directly: it would
// FAIL with the old matcher and PASS with the new one.
func TestReconcile_BatchQuery_RegexSafeMatcher_QuotesDotsAndAnchors(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q, "label_values(") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		paths := parseAssetPathMatcher(q)
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			parts = append(parts, `{"metric":{"asset_path":"`+p+`"},"value":[1700000000,"1"]}`)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(parts, ",")+`]}}`)
	}))
	t.Cleanup(vm.Close)

	srv, ts := newReconcileTestServer(t, vm.URL)
	// Two paths that differ only by dot-position. Under an
	// unescaped `.` (regex "any char") the alternation would
	// conflate them; with regexp.QuoteMeta the `.` is literal
	// and they are distinct. The seed paths are legal under
	// cpath so the production code accepts them.
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01pod000.cdu000"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/reconcile", "")
	if r.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", r.code, r.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) == 0 {
		t.Fatalf("no VM queries captured")
	}
	var sawBatch bool
	for _, q := range queries {
		if strings.HasPrefix(q, "label_values(") {
			continue
		}
		sawBatch = true
		// The alternation body must start with `^(` and end with
		// `)$` — anchored exact match.
		if !strings.Contains(q, `=~"^(`) {
			t.Errorf("batch matcher not anchored with `^(`: %q", q)
		}
		if !strings.Contains(q, `)$"`) {
			t.Errorf("batch matcher not anchored with `)$`: %q", q)
		}
		// Each `.` in the path must be regex-escaped to `\.`.
		// We check that the alternation body contains an escaped
		// dot — if the matcher is not regex-safe the alternation
		// would have a bare `.` between path segments.
		if !strings.Contains(q, `\.pod000`) && !strings.Contains(q, `\.cdu000`) {
			t.Errorf("batch matcher did not escape `.` in path; query=%q", q)
		}
	}
	if !sawBatch {
		t.Errorf("no batch presence query seen; got=%v", queries)
	}
}

// --- PRMT-094: VM call ctx propagation ----------------------------------
//
// assetHasTelemetryBatch / fetchVMAssetPaths must take the upstream
// ctx (scanner ctx for the drift scanner, r.Context() for the HTTP
// handler) instead of context.Background(), so shutdown or client
// disconnect cancels the in-flight VM query rather than idling for
// the full 5s upstream timeout.

// blockingVM serves a request only after the supplied ctx is done.
// Used to assert the upstream cancel propagates into fetchVM via
// http.NewRequestWithContext — the http.Client.Do call returns
// promptly when the caller-side ctx is cancelled.
type blockingVM struct {
	release chan struct{}
	got     chan struct{} // closed when the VM has accepted the request
	served  chan struct{} // closed when the handler has returned
}

func newBlockingVM(t *testing.T) (*blockingVM, *httptest.Server) {
	t.Helper()
	b := &blockingVM{
		release: make(chan struct{}),
		got:     make(chan struct{}),
		served:  make(chan struct{}),
	}
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(b.got)
		select {
		case <-b.release:
		case <-r.Context().Done():
		}
		// VM unreachable / aborted ⇒ handler returns without
		// writing a body. The client sees a transport-level
		// failure, which is what fetchVM surfaces as degraded.
		close(b.served)
	}))
	t.Cleanup(vm.Close)
	return b, vm
}

// TestReconcile_CtxCancel_AssetHasTelemetryBatchCancels wires a
// blocking VM and calls computeReconcile with a cancelled ctx.
// The function must return promptly (not idle for 5s) and report
// degraded=true, proving the upstream ctx propagates into fetchVM.
func TestReconcile_CtxCancel_AssetHasTelemetryBatchCancels(t *testing.T) {
	b, vm := newBlockingVM(t)
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// computeReconcile should NOT hang on the 5s upstream
		// timeout once we cancel ctx; the in-flight HTTP call
		// returns ctx.Err() promptly.
		rep := srv.computeReconcile(ctx, "7d")
		if !rep.Degraded {
			t.Errorf("Degraded = false, want true on cancel")
		}
		close(done)
	}()
	// Wait until the VM is in-flight, then cancel.
	select {
	case <-b.got:
	case <-time.After(2 * time.Second):
		t.Fatalf("VM never received the request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("computeReconcile did not return on ctx cancel (still idling)")
	}
}

// TestReconcile_CtxCancel_FetchVMAssetPathsCancels mirrors the
// batch test for fetchVMAssetPaths (the orphan enumeration path).
// The label_values call is the only VM query the orphan code
// makes; cancelling ctx must abort it.
func TestReconcile_CtxCancel_FetchVMAssetPathsCancels(t *testing.T) {
	b, vm := newBlockingVM(t)
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, vm.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		paths, degraded := srv.fetchVMAssetPaths(ctx, "7d")
		if !degraded {
			t.Errorf("degraded = false, want true on cancel")
		}
		if paths != nil {
			t.Errorf("paths = %v, want nil on cancel", paths)
		}
		close(done)
	}()
	select {
	case <-b.got:
	case <-time.After(2 * time.Second):
		t.Fatalf("VM never received the request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("fetchVMAssetPaths did not return on ctx cancel")
	}
}

// TestReconcile_CtxNormal_StillWorks is the regression guard: with
// a non-cancelled ctx, the batch helper must complete normally
// (mirrors the per-asset happy path).
func TestReconcile_CtxNormal_StillWorks(t *testing.T) {
	vm := vmReconcileServer(t,
		[]string{"site01.pod000.cdu000"},
		nil,
	)
	srv := newDriftScannerTestServer(t, vm.URL)
	seedReconcileAssets(t, srv,
		[]string{"site01.pod000.cdu000", "site01.pod000.cdu001"},
		map[string]any{"type": "cdu", "lifecycle": "active"},
	)
	rep := srv.computeReconcile(context.Background(), "7d")
	if rep.Degraded {
		t.Errorf("Degraded = true, want false on VM happy")
	}
	if len(rep.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(rep.Entries))
	}
}
