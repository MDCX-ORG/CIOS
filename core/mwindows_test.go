// Package core — mwindows_test.go: fileStore + handler coverage for
// the /v1/maintenance/windows CRUD surface (PRMT-096).
//
// Coverage (per PRMT-096 §6 acceptance):
//
//   - maintenance window hit (==) suppresses (fileStore + alarm.Store mock path)
//   - expired window does NOT suppress
//   - ancestor-prefix hit suppresses for a child asset
//   - non-match window does NOT suppress (alarm opens ticket normally)
//   - fileStore ↔ pgStore-equivalent semantics: ActiveWindowFor on
//     fileStore walks the in-memory index with the same prefix match
//     the pgStore SQL encodes; integration test for the SQL itself
//     lives in pg_store_test.go (not in this file)
//   - authmw 401 on unauthenticated request to GET/POST/DELETE
//   - per-item scope filter on list drops out-of-scope rows
//   - create validation: missing fields / bad RFC3339 / ends<=starts
//   - delete: missing id → 400, unknown id → 404, success → 200
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newMWTestServer is a small helper for the fileStore path: same
// shape as newPMServer (fileStore + cpath dict + no auth).
func newMWTestServer(t *testing.T) *Server {
	t.Helper()
	return newPMServer(t)
}

// seedMW seeds one window directly into the fileStore. Returns
// the generated id so callers can hit the delete endpoint.
func seedMW(t *testing.T, s *Server, assetPath string, startsAt, endsAt time.Time, reason string) MaintenanceWindow {
	t.Helper()
	mw := MaintenanceWindow{
		ID:        newMaintenanceWindowID(),
		AssetPath: assetPath,
		StartsAt:  startsAt,
		EndsAt:    endsAt,
		Reason:    reason,
	}
	if err := s.st.PutMaintenanceWindow(context.Background(), mw); err != nil {
		t.Fatalf("seed window: %v", err)
	}
	return mw
}

// --- fileStore.ActiveWindowFor semantics ------------------------------

// TestMW_ActiveWindowFor_ExactMatch covers §2 "active window =
// now ∈ [starts,ends) AND (== or startsWith(prefix+'.'))". The
// exact-match branch is exercised here; the prefix branch lives in
// TestMW_ActiveWindowFor_AncestorPrefix.
func TestMW_ActiveWindowFor_ExactMatch(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()
	seedMW(t, s, "site01.pod001.cdu000", now.Add(-time.Hour), now.Add(time.Hour), "exact")

	got, ok, err := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now)
	if err != nil {
		t.Fatalf("ActiveWindowFor: %v", err)
	}
	if !ok {
		t.Fatal("active window: hit expected, got miss")
	}
	if got.AssetPath != "site01.pod001.cdu000" {
		t.Errorf("got asset=%q, want exact match", got.AssetPath)
	}
}

// TestMW_ActiveWindowFor_AncestorPrefix confirms the prefix match:
// a window declared on site01.pod001 suppresses alarms on its
// child site01.pod001.cdu000 (but NOT on site01.pod0019.cdu000,
// which only shares a textual prefix).
func TestMW_ActiveWindowFor_AncestorPrefix(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()
	seedMW(t, s, "site01.pod001", now.Add(-time.Hour), now.Add(time.Hour), "ancestor")

	// Child path → hit (startsWith "site01.pod001.")
	got, ok, err := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now)
	if err != nil || !ok {
		t.Fatalf("child: ok=%v err=%v, want hit", ok, err)
	}
	if got.AssetPath != "site01.pod001" {
		t.Errorf("child got asset=%q", got.AssetPath)
	}

	// Sibling path (different pod) → miss
	if _, ok, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod002.cdu000", now); ok {
		t.Error("sibling pod: miss expected, got hit")
	}

	// Path that shares a textual prefix without the dot boundary → miss
	// (site01.pod0019 is NOT a descendant of site01.pod001)
	if _, ok, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod0019.cdu000", now); ok {
		t.Error("textual prefix without dot: miss expected, got hit")
	}
}

// TestMW_ActiveWindowFor_ExpiredWindow confirms an out-of-window
// time yields a miss. The window's [start, end) is in the past; a
// `now` after end must not match.
func TestMW_ActiveWindowFor_ExpiredWindow(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()
	seedMW(t, s, "site01.pod001.cdu000", now.Add(-2*time.Hour), now.Add(-time.Hour), "expired")

	if _, ok, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now); ok {
		t.Error("expired window: miss expected, got hit")
	}
}

// TestMW_ActiveWindowFor_FutureWindow covers the symmetric case:
// the window starts in the future, so a `now` before start is a miss.
func TestMW_ActiveWindowFor_FutureWindow(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()
	seedMW(t, s, "site01.pod001.cdu000", now.Add(time.Hour), now.Add(2*time.Hour), "future")

	if _, ok, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now); ok {
		t.Error("future window: miss expected, got hit")
	}
}

// TestMW_ActiveWindowFor_NoWindow confirms the empty store path.
func TestMW_ActiveWindowFor_NoWindow(t *testing.T) {
	s := newMWTestServer(t)
	if _, ok, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", time.Now().UTC()); ok {
		t.Error("empty store: miss expected, got hit")
	}
}

// TestMW_ActiveWindowFor_OverlappingWindows_DeterministicOrder is
// the F3 file/pg consistency regression: two overlapping active
// windows on the same asset. pgStore returns
// `ORDER BY starts_at ASC, id ASC LIMIT 1`; the pre-R2 fileStore
// returned map-iteration order. Seed two windows where one starts
// earlier and one has a lexicographically-smaller ID; the
// earlier-starting window must win. Then seed two with identical
// StartsAt and assert the smaller ID wins (the secondary key).
func TestMW_ActiveWindowFor_OverlappingWindows_DeterministicOrder(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()

	// Case A: distinct StartsAt — the earlier one must win.
	earlier := seedMW(t, s, "site01.pod001.cdu000",
		now.Add(-2*time.Hour), now.Add(2*time.Hour), "earlier")
	// Inject a later one explicitly. We force laterID to be
	// lexicographically-smaller than earlier.ID so the test
	// proves the StartsAt tiebreaker (not the ID one).
	earlierID := earlier.ID
	laterID := "mw_0000000000000001"
	if laterID >= earlierID {
		t.Fatalf("test bug: laterID %q >= earlierID %q", laterID, earlierID)
	}
	if err := s.st.PutMaintenanceWindow(context.Background(), MaintenanceWindow{
		ID:        laterID,
		AssetPath: "site01.pod001.cdu000",
		StartsAt:  now.Add(-time.Hour),
		EndsAt:    now.Add(3 * time.Hour),
		Reason:    "later",
	}); err != nil {
		t.Fatalf("seed later: %v", err)
	}

	got, ok, err := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now)
	if err != nil {
		t.Fatalf("ActiveWindowFor: %v", err)
	}
	if !ok {
		t.Fatal("expected hit, got miss")
	}
	if got.ID != earlierID {
		t.Errorf("earlier window should win: got %q (starts=%s), want %q (starts=%s)",
			got.ID, got.StartsAt, earlierID, earlier.StartsAt)
	}

	// Case B: identical StartsAt — the smaller ID must win.
	// Add two more windows sharing earlier.StartsAt; among all
	// candidates at that starts_at, smallID is the lexicographically
	// smallest, so it must be returned.
	smallID := "mw_0000000000000002"
	largeID := "mw_AAAAAAAAAAAAAAAA"
	if smallID >= largeID {
		t.Fatalf("test bug: smallID >= largeID")
	}
	for _, mw := range []MaintenanceWindow{
		{ID: largeID, AssetPath: "site01.pod001.cdu000", StartsAt: earlier.StartsAt, EndsAt: earlier.EndsAt, Reason: "tie-larger"},
		{ID: smallID, AssetPath: "site01.pod001.cdu000", StartsAt: earlier.StartsAt, EndsAt: earlier.EndsAt, Reason: "tie-smaller"},
	} {
		if err := s.st.PutMaintenanceWindow(context.Background(), mw); err != nil {
			t.Fatalf("seed %s: %v", mw.ID, err)
		}
	}

	got2, ok2, _ := s.st.ActiveWindowFor(context.Background(), "site01.pod001.cdu000", now)
	if !ok2 {
		t.Fatal("case B: expected hit")
	}
	if got2.ID != smallID {
		t.Errorf("tie: smaller ID should win: got %q, want %q", got2.ID, smallID)
	}
}

// --- HTTP handler coverage -------------------------------------------

// TestMW_PutListGetDelete is the happy-path round-trip: create a
// window via the HTTP handler, list it back, read it via
// GetMaintenanceWindow (id round-trip), then delete it.
func TestMW_PutListGetDelete(t *testing.T) {
	s := newMWTestServer(t)
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := []byte(`{
		"asset_path": "site01.pod001.cdu000",
		"starts_at": "` + startsAt + `",
		"ends_at": "` + endsAt + `",
		"reason": "happy path"
	}`)

	r := httptest.NewRequest(http.MethodPost, "/v1/maintenance/windows", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", w.Code, w.Body.String())
	}
	var created MaintenanceWindow
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	if !maintenanceWindowIDPattern.MatchString(created.ID) {
		t.Errorf("created.ID = %q, want mw_... pattern", created.ID)
	}

	// List
	r2 := httptest.NewRequest(http.MethodGet, "/v1/maintenance/windows", nil)
	w2 := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", w2.Code, w2.Body.String())
	}
	var listed listMaintenanceWindowsResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listed.Items) != 1 || listed.Items[0].ID != created.ID {
		t.Errorf("list len=%d, want 1 row matching %s", len(listed.Items), created.ID)
	}

	// Delete
	r3 := httptest.NewRequest(http.MethodDelete, "/v1/maintenance/windows/"+created.ID, nil)
	w3 := httptest.NewRecorder()
	s.ServeMaintenanceWindow(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", w3.Code, w3.Body.String())
	}

	// Verify gone via GetMaintenanceWindow (Store-level, not HTTP —
	// the spec intentionally does not expose GET /v1/maintenance/windows/{id}).
	if _, ok, _ := s.st.GetMaintenanceWindow(context.Background(), created.ID); ok {
		t.Errorf("GetMaintenanceWindow(%s): still present after delete", created.ID)
	}
}

// TestMW_Create_BadJSON rejects malformed bodies with 400.
func TestMW_Create_BadJSON(t *testing.T) {
	s := newMWTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/maintenance/windows",
		bytes.NewReader([]byte(`{this is not json`)))
	w := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

// TestMW_Create_EndBeforeStart rejects an inverted window (ends_at
// must be strictly after starts_at per spec-008 §4.2).
func TestMW_Create_EndBeforeStart(t *testing.T) {
	s := newMWTestServer(t)
	now := time.Now().UTC()
	body := []byte(`{
		"asset_path": "site01.pod001.cdu000",
		"starts_at": "` + now.Add(time.Hour).Format(time.RFC3339) + `",
		"ends_at":   "` + now.Format(time.RFC3339) + `",
		"reason":    "inverted"
	}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/maintenance/windows", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
	}
}

// TestMW_Create_BadAssetPath rejects a non-crn asset_path with the
// "bad-path" RFC 7807 tail. Goes through the HTTP handler chain
// (not the raw handler) so the request_id middleware is in scope.
func TestMW_Create_BadAssetPath(t *testing.T) {
	_, ts := newTestServer(t)
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := `{
		"asset_path": "garbage..path",
		"starts_at": "` + startsAt + `",
		"ends_at":   "` + endsAt + `"
	}`
	r := doReq(t, ts, http.MethodPost, "/v1/maintenance/windows", body)
	if r.code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 body=%s", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-path")
}

// TestMW_Delete_UnknownID confirms a 404 on a syntactically valid
// but absent window id (race-loss coverage — DELETE is idempotent
// at the Store level, but the HTTP surface reports "gone" via 404
// so a retrying client stops on the first response).
func TestMW_Delete_UnknownID(t *testing.T) {
	s := newMWTestServer(t)
	r := httptest.NewRequest(http.MethodDelete, "/v1/maintenance/windows/mw_AAAAAAAAAAAAAAAA", nil)
	w := httptest.NewRecorder()
	s.ServeMaintenanceWindow(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
}

// TestMW_Delete_BadID rejects malformed ids at the regex gate.
func TestMW_Delete_BadID(t *testing.T) {
	s := newMWTestServer(t)
	r := httptest.NewRequest(http.MethodDelete, "/v1/maintenance/windows/not-an-id", nil)
	w := httptest.NewRecorder()
	s.ServeMaintenanceWindow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

// TestMW_Route_MethodNotAllowed: GET on the {id} sub-path and PUT
// on the root both return 405 (we only expose DELETE on {id}).
func TestMW_Route_MethodNotAllowed(t *testing.T) {
	s := newMWTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/windows/mw_AAAAAAAAAAAAAAAA", nil)
	w := httptest.NewRecorder()
	s.ServeMaintenanceWindow(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /{id}: status=%d, want 405", w.Code)
	}

	r2 := httptest.NewRequest(http.MethodPut, "/v1/maintenance/windows", nil)
	w2 := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w2, r2)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /: status=%d, want 405", w2.Code)
	}
}

// --- auth + scope coverage -------------------------------------------

// TestMW_NoBearer_401 pins that the new endpoints return 401
// without a Bearer token (PRMT-037 lesson: an unregistered route
// is a silent auth bypass). The middleware maps the three verbs
// to API endpoints so the verify step fires before the handler.
func TestMW_NoBearer_401(t *testing.T) {
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := `{"asset_path":"site01.pod000.cdu000","starts_at":"` + startsAt + `","ends_at":"` + endsAt + `"}`

	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/maintenance/windows", ""},
		{http.MethodPost, "/v1/maintenance/windows", body},
		{http.MethodDelete, "/v1/maintenance/windows/mw_AAAAAAAAAAAAAAAA", ""},
	} {
		r := doReq(t, ts, c.method, c.path, c.body)
		if r.code != http.StatusUnauthorized {
			t.Errorf("%s %s: status=%d body=%s, want 401", c.method, c.path, r.code, r.body)
		}
	}
}

// TestMW_ListScopeFilter drops out-of-scope rows when a viewer with
// narrow scope lists windows. Mirrors the /v1/tickets scope-filter
// contract (PRMT-022 R2 §4.1).
func TestMW_ListScopeFilter(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.pod001.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	now := time.Now().UTC()
	seedMW(t, srv, "site01.pod001.cdu000", now.Add(-time.Hour), now.Add(time.Hour), "in-scope")
	seedMW(t, srv, "site01.pod002.cdu000", now.Add(-time.Hour), now.Add(time.Hour), "out-of-scope")

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/maintenance/windows", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got listMaintenanceWindowsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items=%d, want 1 in-scope: %+v", len(got.Items), got.Items)
	}
	if got.Items[0].AssetPath != "site01.pod001.cdu000" {
		t.Errorf("unexpected item: %+v", got.Items[0])
	}
}

// TestMW_NewIDPattern pins the id format ("mw_" + 16 base32 chars).
func TestMW_NewIDPattern(t *testing.T) {
	for i := 0; i < 32; i++ {
		id := newMaintenanceWindowID()
		if !maintenanceWindowIDPattern.MatchString(id) {
			t.Fatalf("id %q does not match pattern", id)
		}
	}
}

// TestMW_ListPagination_NoDuplicatesNoGaps is the F2 pagination
// regression: page through 5 windows with page_size=2 and assert
// every seeded window is returned exactly once across the pages.
// The pre-R2 code sorted by (StartsAt, ID) but filtered the cursor
// on ID alone; with windows that share an asset but differ on
// StartsAt, the cursor could skip rows whose ID sorts before
// afterID but whose StartsAt places them on page 1, or re-emit
// rows whose ID sorts after afterID but whose StartsAt placed
// them on page 1.
func TestMW_ListPagination_NoDuplicatesNoGaps(t *testing.T) {
	s := newMWTestServer(t)

	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	want := make(map[string]bool, 5)
	for i := 0; i < 5; i++ {
		starts := base.Add(time.Duration(i) * time.Minute)
		ends := starts.Add(time.Hour)
		mw := seedMW(t, s, "site01.pod001.cdu000", starts, ends, "pager")
		want[mw.ID] = true
	}

	got := make(map[string]int, 5)
	pageToken := ""
	for page := 0; page < 10; page++ { // generous cap; the loop must terminate earlier
		path := "/v1/maintenance/windows?page_size=2"
		if pageToken != "" {
			path += "&page_token=" + pageToken
		}
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.serveMaintenanceWindowsRoot(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", page, w.Code, w.Body.String())
		}
		var resp listMaintenanceWindowsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d decode: %v", page, err)
		}
		for _, m := range resp.Items {
			got[m.ID]++
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	if len(got) != len(want) {
		t.Errorf("got %d unique ids, want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("id %s appeared %d times, want 1", id, n)
		}
		if !want[id] {
			t.Errorf("id %s not in seed set", id)
		}
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("seed id %s missing from pagination", id)
		}
	}
}

// TestMW_ListPagination_BadToken_400 confirms an unparseable
// page_token is rejected with 400 (the v2 codec must not silently
// accept v1-shaped or garbage tokens).
func TestMW_ListPagination_BadToken_400(t *testing.T) {
	s := newMWTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/windows?page_token=not-a-real-token", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceWindowsRoot(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

// TestMW_RBAC_OperatorControlWrite_OK covers the F1 RBAC fix: an
// operator token whose scope is `site01.pod001.**` and whose role
// grants ActionControlWrite on that subtree (the standard
// operator privilege per L50 / §4 mapping) must be able to POST
// a window on `site01.pod001.cdu000` and DELETE it. The pre-R2
// code used ActionApply for the per-item re-check, which is
// strictly stronger than ActionControlWrite — such a token got
// 403 even though the middleware let it through, making the
// endpoint unusable for its target role.
func TestMW_RBAC_OperatorControlWrite_OK(t *testing.T) {
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site01.pod001.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := `{"asset_path":"site01.pod001.cdu000","starts_at":"` + startsAt + `","ends_at":"` + endsAt + `","reason":"operator ctlwrite"}`

	r := doReqWithAuth(t, ts, http.MethodPost, "/v1/maintenance/windows", body, operatorTok)
	if r.code != http.StatusCreated {
		t.Fatalf("operator create: code=%d body=%s, want 201", r.code, r.body)
	}
	var created MaintenanceWindow
	mustJSON(t, r.body, &created)
	if !maintenanceWindowIDPattern.MatchString(created.ID) {
		t.Fatalf("created.ID = %q, want mw_... pattern", created.ID)
	}

	r2 := doReqWithAuth(t, ts, http.MethodDelete, "/v1/maintenance/windows/"+created.ID, "", operatorTok)
	if r2.code != http.StatusOK {
		t.Fatalf("operator delete: code=%d body=%s, want 200", r2.code, r2.body)
	}
}

// TestMW_RBAC_OperatorOutOfScope_403 is the negative case: the
// same operator scope must NOT be able to create a window on a
// sibling pod (the per-item re-check still works after F1).
func TestMW_RBAC_OperatorOutOfScope_403(t *testing.T) {
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site01.pod001.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := `{"asset_path":"site01.pod002.cdu000","starts_at":"` + startsAt + `","ends_at":"` + endsAt + `"}`

	r := doReqWithAuth(t, ts, http.MethodPost, "/v1/maintenance/windows", body, operatorTok)
	if r.code != http.StatusForbidden {
		t.Fatalf("operator OOS: code=%d body=%s, want 403", r.code, r.body)
	}
}
