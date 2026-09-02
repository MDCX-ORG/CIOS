// Package core — maintenance_test.go: end-to-end + unit tests for
// the /v1/maintenance/upcoming handler (PRMT-058).
//
// Coverage (per PRMT-058 §6 acceptance):
//   - merge PM + inspection into a single list
//   - disabled schedules/templates are excluded
//   - sort by next_due ascending (deterministic tie-breaker)
//   - overdue filter: only NextDue < now
//   - before filter: only NextDue <= before
//   - both filters together: intersection
//   - per-item scope: out-of-scope items silently dropped
//   - bad before / overdue → 400 (RFC 7807)
//   - no bearer on the endpoint → 401 (PRMT-037 lesson)
package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upcomingRow is the local mirror of maintenanceUpcomingItem for
// JSON decoding in the handler tests. Field tags are the wire
// contract; keep in sync with maintenance.go.
type upcomingRow struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	AssetPath string    `json:"asset_path"`
	Title     string    `json:"title"`
	NextDue   time.Time `json:"next_due"`
	Overdue   bool      `json:"overdue"`
}

type upcomingResp struct {
	Items []upcomingRow `json:"items"`
}

func seedUpcomingStore(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now().UTC()
	pm := []PMSchedule{
		{
			ID: newPMScheduleID(), AssetPath: "site01.pod001.cdu000.fws.supply.flow",
			Kind: "calendar", IntervalDays: 7,
			NextDue:  now.Add(-2 * time.Hour), // overdue
			Title:    "pm overdue",
			Severity: "minor", Enabled: true,
		},
		{
			ID: newPMScheduleID(), AssetPath: "site01.pod002.cdu000.fan000.rpm",
			Kind: "calendar", IntervalDays: 30,
			NextDue:  now.Add(48 * time.Hour), // future
			Title:    "pm future",
			Severity: "major", Enabled: true,
		},
		{
			ID: newPMScheduleID(), AssetPath: "site01.pod003.cdu000.fan000.rpm",
			Kind: "calendar", IntervalDays: 30,
			NextDue:  now.Add(24 * time.Hour), // future
			Title:    "pm disabled",
			Severity: "minor", Enabled: false, // excluded
		},
	}
	for _, p := range pm {
		if err := s.st.PutPMSchedule(context.Background(), p); err != nil {
			t.Fatalf("seed pm %s: %v", p.ID, err)
		}
	}
	tpl := []InspectionTemplate{
		{
			ID: newInspectionID(), AssetPath: "site01.pod001.cdu000.fws.return.flow",
			Title:    "insp overdue",
			Items:    []string{"x"},
			Interval: 24 * time.Hour,
			NextDue:  now.Add(-30 * time.Minute), // overdue
			Enabled:  true,
		},
		{
			ID: newInspectionID(), AssetPath: "site01.pod004.cdu000.fan000.rpm",
			Title:    "insp future",
			Items:    []string{"x"},
			Interval: 168 * time.Hour,
			NextDue:  now.Add(72 * time.Hour), // future
			Enabled:  true,
		},
		{
			ID: newInspectionID(), AssetPath: "site01.pod005.cdu000.fan000.rpm",
			Title:    "insp disabled",
			Items:    []string{"x"},
			Interval: 168 * time.Hour,
			NextDue:  now.Add(-1 * time.Hour), // would be overdue if enabled
			Enabled:  false,                   // excluded
		},
	}
	for _, it := range tpl {
		if err := s.st.PutInspectionTemplate(context.Background(), it); err != nil {
			t.Fatalf("seed inspection %s: %v", it.ID, err)
		}
	}
}

func TestMaintenance_MergeAndSort(t *testing.T) {
	s := newPMServer(t)
	seedUpcomingStore(t, s)

	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got upcomingResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 5 enabled items: 2 PM (overdue, future) + 3 inspection (overdue,
	// future, disabled-excluded). Disabled rows are NOT in the
	// response. Expected: 4.
	if len(got.Items) != 4 {
		t.Fatalf("items=%d, want 4 (2 enabled PM + 2 enabled inspection)", len(got.Items))
	}
	// Asc by next_due: overdue items come first, then future.
	for i := 1; i < len(got.Items); i++ {
		if got.Items[i-1].NextDue.After(got.Items[i].NextDue) {
			t.Errorf("items not sorted asc: idx %d (%v) > idx %d (%v)",
				i-1, got.Items[i-1].NextDue, i, got.Items[i].NextDue)
		}
	}
	// The two disabled seeds must NOT appear.
	for _, it := range got.Items {
		if it.Title == "pm disabled" || it.Title == "insp disabled" {
			t.Errorf("disabled item leaked: %+v", it)
		}
	}
	// Overdue flag must match NextDue < now (now is captured at
	// request time, but everything seeded with negative delta is
	// guaranteed to be overdue).
	now := time.Now().UTC()
	for _, it := range got.Items {
		wantOverdue := it.NextDue.Before(now)
		if it.Overdue != wantOverdue {
			t.Errorf("%s overdue=%v, want %v (next_due=%v, now=%v)",
				it.Title, it.Overdue, wantOverdue, it.NextDue, now)
		}
	}
}

func TestMaintenance_DisabledExcluded(t *testing.T) {
	s := newPMServer(t)
	seedUpcomingStore(t, s)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	var got upcomingResp
	mustJSON(t, w.Body.String(), &got)
	for _, it := range got.Items {
		if strings.Contains(it.Title, "disabled") {
			t.Errorf("disabled item %q should be excluded", it.Title)
		}
	}
}

func TestMaintenance_OverdueFilter(t *testing.T) {
	s := newPMServer(t)
	seedUpcomingStore(t, s)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming?overdue=true", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got upcomingResp
	mustJSON(t, w.Body.String(), &got)
	if len(got.Items) == 0 {
		t.Fatalf("overdue=true returned 0 items; want >=1")
	}
	for _, it := range got.Items {
		if !it.Overdue {
			t.Errorf("overdue=true but got non-overdue: %+v", it)
		}
	}
	// Overdue=false → no overdue items in the response.
	r2 := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming?overdue=false", nil)
	w2 := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
	var got2 upcomingResp
	mustJSON(t, w2.Body.String(), &got2)
	for _, it := range got2.Items {
		if it.Overdue {
			t.Errorf("overdue=false but got overdue: %+v", it)
		}
	}
}

func TestMaintenance_BeforeFilter(t *testing.T) {
	s := newPMServer(t)
	seedUpcomingStore(t, s)
	// Cutoff at ~now+60h: should include the overdue items and the
	// 24h-future item, but exclude the 48h and 72h future items.
	cutoff := time.Now().UTC().Add(60 * time.Hour).Format(time.RFC3339)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming?before="+cutoff, nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got upcomingResp
	mustJSON(t, w.Body.String(), &got)
	if len(got.Items) == 0 {
		t.Fatalf("before=%s returned 0 items; want >=1", cutoff)
	}
	for _, it := range got.Items {
		if it.NextDue.After(time.Now().UTC().Add(60 * time.Hour).Add(time.Second)) {
			t.Errorf("before=%s but got %+v past cutoff", cutoff, it)
		}
	}
	// Before in the past → nothing matches the enabled-and-not-yet-
	// filter (overdue is a separate filter). Combined set should
	// still include the overdue items because overdue is not
	// constrained here.
	past := time.Now().UTC().Add(-1000 * time.Hour).Format(time.RFC3339)
	r2 := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming?before="+past, nil)
	w2 := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
	var got2 upcomingResp
	mustJSON(t, w2.Body.String(), &got2)
	if len(got2.Items) != 0 {
		t.Errorf("before=past should return 0 enabled items, got %d", len(got2.Items))
	}
}

func TestMaintenance_OverdueAndBeforeCombined(t *testing.T) {
	s := newPMServer(t)
	seedUpcomingStore(t, s)
	// overdue=true ∩ before=now+1h: only the very-recently-due
	// items (overdue AND next_due <= now+1h). Future items are
	// excluded because overdue=false; future-overdue doesn't exist
	// in the seed.
	cutoff := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming?before="+cutoff+"&overdue=true", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got upcomingResp
	mustJSON(t, w.Body.String(), &got)
	for _, it := range got.Items {
		if !it.Overdue {
			t.Errorf("combined overdue+before: got non-overdue %+v", it)
		}
		if it.NextDue.After(time.Now().UTC().Add(1*time.Hour + time.Second)) {
			t.Errorf("combined overdue+before: got past cutoff %+v", it)
		}
	}
}

func TestMaintenance_BadBefore_400(t *testing.T) {
	s, ts := newTestServer(t)
	seedUpcomingStore(t, s)
	r := doReq(t, ts, http.MethodGet, "/v1/maintenance/upcoming?before=not-a-time", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

func TestMaintenance_BadOverdue_400(t *testing.T) {
	s, ts := newTestServer(t)
	seedUpcomingStore(t, s)
	// strconv.ParseBool accepts "1", "t", "T", "TRUE", "true", "True",
	// "0", "f", "F", "FALSE", "false", "False" — so those are all
	// valid. Only unparseable values must yield 400.
	for _, v := range []string{"maybe", "yes", "no", "banana"} {
		r := doReq(t, ts, http.MethodGet, "/v1/maintenance/upcoming?overdue="+v, "")
		if r.code != http.StatusBadRequest {
			t.Errorf("overdue=%q → status=%d body=%s, want 400", v, r.code, r.body)
		}
	}
}

func TestMaintenance_MethodNotAllowed(t *testing.T) {
	s := newPMServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/maintenance/upcoming", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", w.Code)
	}
}

func TestMaintenance_EmptyStoreReturnsEmpty(t *testing.T) {
	s := newPMServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/maintenance/upcoming", nil)
	w := httptest.NewRecorder()
	s.serveMaintenanceUpcoming(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got upcomingResp
	mustJSON(t, w.Body.String(), &got)
	if got.Items == nil {
		t.Errorf("items = nil, want []")
	}
	if len(got.Items) != 0 {
		t.Errorf("items len = %d, want 0", len(got.Items))
	}
}

func TestMaintenance_ScopeFilter_DropsOutOfScope(t *testing.T) {
	// End-to-end with auth: viewer scoped to site01.pod001.** sees
	// only PM + inspection whose asset_path is under site01.pod001.
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.pod001.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	seedUpcomingStore(t, srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/maintenance/upcoming", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got upcomingResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// The seed has 4 enabled items: only the 2 whose asset_path
	// starts with site01.pod001. should remain.
	if len(got.Items) != 2 {
		t.Fatalf("items=%d, want 2 (in-scope only): %+v", len(got.Items), got.Items)
	}
	for _, it := range got.Items {
		if !strings.HasPrefix(it.AssetPath, "site01.pod001.") {
			t.Errorf("out-of-scope leaked: %+v", it)
		}
	}
}

func TestMaintenance_NoBearer_401(t *testing.T) {
	// PRMT-037 lesson: an unregistered route is a silent auth
	// bypass. Pin that /v1/maintenance/upcoming returns 401 when
	// no Bearer is supplied.
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/maintenance/upcoming", "")
	if r.code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", r.code, r.body)
	}
}
