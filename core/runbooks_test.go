// Package core — runbooks_test.go: /v1/runbooks + /v1/cases
// (PRMT-044 §6 acceptance).
//
// Covers:
//   - runbook content read from disk
//   - path traversal blocked (.., absolute paths)
//   - missing file → 404
//   - empty runbookDir → 404
//   - cases = closed tickets only, in scope
//   - cases empty when no tickets
//   - manual ticket runbook defaults to "" (carry via PUT body)
package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

func newRunbookServer(t *testing.T) *Server {
	t.Helper()
	root := moduleRoot(t)
	dict, err := cpath.LoadDict(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	storePath := filepath.Join(t.TempDir(), "store.json")
	st, err := NewFileStore(storePath)
	if err != nil {
		t.Fatalf("newFileStore: %v", err)
	}
	return NewServer(st, dict, "")
}

func writeRunbook(t *testing.T, dir, key, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(key)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRunbookServeReadsContent(t *testing.T) {
	s := newRunbookServer(t)
	dir := t.TempDir()
	writeRunbook(t, dir, "rb/cdu-deltat-low", "# CDU deltaT low\ndo this.\n")
	s.SetRunbookDir(dir)
	r := httptest.NewRequest(http.MethodGet, "/v1/runbooks/rb/cdu-deltat-low", nil)
	w := httptest.NewRecorder()
	s.serveRunbook(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CDU deltaT low") {
		t.Errorf("body missing content: %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestRunbookServeToleratesMdSuffix(t *testing.T) {
	s := newRunbookServer(t)
	dir := t.TempDir()
	writeRunbook(t, dir, "rb/x", "hi")
	s.SetRunbookDir(dir)
	r := httptest.NewRequest(http.MethodGet, "/v1/runbooks/rb/x.md", nil)
	w := httptest.NewRecorder()
	s.serveRunbook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("with .md suffix: %d", w.Code)
	}
}

func TestRunbookServeRejectsTraversal(t *testing.T) {
	s := newRunbookServer(t)
	dir := t.TempDir()
	s.SetRunbookDir(dir)
	cases := []struct {
		name string
		path string
	}{
		{"parent", "/v1/runbooks/../etc/passwd"},
		{"absolute", "/v1/runbooks//etc/passwd"},
		{"dotdot-mid", "/v1/runbooks/rb/../../escape"},
		{"empty", "/v1/runbooks/"},
		{"traversal-encoded-ish", "/v1/runbooks/.."},
		{"leading-slash", "/v1/runbooks//x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			s.serveRunbook(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRunbookServeMissing(t *testing.T) {
	s := newRunbookServer(t)
	s.SetRunbookDir(t.TempDir())
	r := httptest.NewRequest(http.MethodGet, "/v1/runbooks/rb/nope", nil)
	w := httptest.NewRecorder()
	s.serveRunbook(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRunbookServeEmptyDir(t *testing.T) {
	s := newRunbookServer(t)
	// Don't call SetRunbookDir → runbookDir stays "".
	r := httptest.NewRequest(http.MethodGet, "/v1/runbooks/rb/anything", nil)
	w := httptest.NewRecorder()
	s.serveRunbook(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when runbook dir not configured, got %d", w.Code)
	}
}

func TestRunbookServeRejectsWrongMethod(t *testing.T) {
	s := newRunbookServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/runbooks/rb/x", nil)
	w := httptest.NewRecorder()
	s.serveRunbook(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestCasesOnlyClosedTickets(t *testing.T) {
	s := newRunbookServer(t)
	closed := Ticket{
		ID:        newTicketIDForTest(),
		AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title:     "closed one",
		Severity:  "minor",
		State:     "closed",
		OpenedAt:  time.Now().UTC(),
		ClosedAt:  tsPtr(time.Now().UTC()),
	}
	open := Ticket{
		ID:        newTicketIDForTest(),
		AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title:     "still open",
		Severity:  "major",
		State:     "open",
		OpenedAt:  time.Now().UTC(),
	}
	if _, err := s.st.PutTicket(context.Background(), closed, 0); err != nil {
		t.Fatalf("put closed: %v", err)
	}
	if _, err := s.st.PutTicket(context.Background(), open, 0); err != nil {
		t.Fatalf("put open: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/cases", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "closed one") {
		t.Errorf("body missing closed ticket: %s", body)
	}
	if strings.Contains(body, "still open") {
		t.Errorf("body should not contain open ticket: %s", body)
	}
}

func TestCasesEmpty(t *testing.T) {
	s := newRunbookServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/cases", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("expected empty items array, got %s", w.Body.String())
	}
}

func TestCasesRejectsWrongMethod(t *testing.T) {
	s := newRunbookServer(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/cases", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestTicketRunbookRoundtrip(t *testing.T) {
	// PUT a ticket with a runbook, read it back, ensure the
	// field persists. Uses the existing fileStore path so the
	// wire shape is exercised end-to-end.
	s := newRunbookServer(t)
	now := time.Now().UTC()
	tk := Ticket{
		ID:        newTicketIDForTest(),
		AssetPath: "a.b.c",
		Title:     "with runbook",
		Severity:  "minor",
		State:     "open",
		OpenedAt:  now,
		Runbook:   "rb/cdu-deltat-low",
	}
	if _, err := s.st.PutTicket(context.Background(), tk, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := s.st.GetTicket(context.Background(), tk.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Runbook != "rb/cdu-deltat-low" {
		t.Errorf("Runbook roundtrip: got %q want %q", got.Runbook, "rb/cdu-deltat-low")
	}
}

func TestCasesScopeFilter(t *testing.T) {
	// Without auth (s has auth==nil) every ticket is in scope,
	// so this test verifies the unfiltered path. The authz
	// list-scope path is exercised by the existing alarm/ticket
	// scope tests — cases inherits the same shape.
	s := newRunbookServer(t)
	for _, st := range []string{"closed", "closed", "open"} {
		tk := Ticket{
			ID:        newTicketIDForTest(),
			AssetPath: "a.b.c",
			Title:     "t",
			Severity:  "minor",
			State:     st,
			OpenedAt:  time.Now().UTC(),
		}
		if _, err := s.st.PutTicket(context.Background(), tk, 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/cases", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	body := w.Body.String()
	// Two closed tickets, one open → expect 2 items.
	if strings.Count(body, `"id":`) != 2 {
		t.Errorf("expected 2 closed items, got body: %s", body)
	}
}

// ctx isn't used here, but the import is needed for the build
// (future expansion uses it).
var _ = context.Background

// newTicketIDForTest re-uses the production helper. Local copy
// (instead of importing the unexported newTicketID from the same
// package) keeps the test self-contained.
func newTicketIDForTest() string { return newTicketID() }

// tsPtr is the test-side helper for *time.Time literals.
// pg_store.go has timePtr (sql.NullTime → *time.Time) with a
// different signature; we keep a separate name to avoid the
// declaration clash.
func tsPtr(t time.Time) *time.Time { return &t }

// --- PRMT-053: /v1/cases query params + CSV -------------------------

// seedClosedTicket writes a closed ticket with the given fields
// into the store. Centralises the boilerplate so the PRMT-053
// filter tests stay readable.
func seedClosedTicket(t *testing.T, s *Server, assetPath, severity, runbook string, closedAt time.Time) Ticket {
	t.Helper()
	tk := Ticket{
		ID:        newTicketIDForTest(),
		AssetPath: assetPath,
		Title:     "case " + severity,
		Severity:  severity,
		State:     "closed",
		OpenedAt:  closedAt.Add(-time.Hour),
		ClosedAt:  tsPtr(closedAt),
		Runbook:   runbook,
	}
	if _, err := s.st.PutTicket(context.Background(), tk, 0); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	return tk
}

func TestCasesSeverityFilter(t *testing.T) {
	s := newRunbookServer(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	seedClosedTicket(t, s, "a.b.c", "critical", "rb/x", t0)
	seedClosedTicket(t, s, "a.b.d", "minor", "rb/y", t0)

	r := httptest.NewRequest(http.MethodGet, "/v1/cases?severity=critical", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, `"id":`) != 1 {
		t.Errorf("expected 1 critical item, got: %s", body)
	}
	if !strings.Contains(body, "a.b.c") {
		t.Errorf("expected a.b.c in body: %s", body)
	}
	if strings.Contains(body, "a.b.d") {
		t.Errorf("did not expect a.b.d in body: %s", body)
	}
}

func TestCasesBadSeverity(t *testing.T) {
	s := newRunbookServer(t)
	seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", time.Now().UTC())

	r := httptest.NewRequest(http.MethodGet, "/v1/cases?severity=bogus", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad severity, got %d body=%s",
			w.Code, w.Body.String())
	}
}

func TestCasesAssetPrefixFilter(t *testing.T) {
	s := newRunbookServer(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	seedClosedTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow", "minor", "rb/x", t0)
	seedClosedTicket(t, s, "sgp02.pod001.cdu000.fws.supply.flow", "minor", "rb/y", t0)
	seedClosedTicket(t, s, "sgp01.pod002.cdu000.fws.supply.flow", "major", "rb/z", t0)

	r := httptest.NewRequest(http.MethodGet, "/v1/cases?asset_prefix=sgp01.pod001", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, `"id":`) != 1 {
		t.Errorf("expected 1 item under sgp01.pod001, got: %s", body)
	}
	if !strings.Contains(body, "sgp01.pod001.cdu000.fws.supply.flow") {
		t.Errorf("expected pod001 asset in body: %s", body)
	}
	if strings.Contains(body, "sgp02") {
		t.Errorf("did not expect sgp02 in body: %s", body)
	}
}

func TestCasesSinceUntilFilter(t *testing.T) {
	s := newRunbookServer(t)
	day1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", day1)
	seedClosedTicket(t, s, "a.b.d", "minor", "rb/y", day2)
	seedClosedTicket(t, s, "a.b.e", "minor", "rb/z", day3)

	// Window: day2 only (since=day2, until=day2 end-of-day exclusive
	// boundary is hard to express without a half-open convention —
	// we use day3 as until so day2 and day3 are included, and assert
	// the negative by filtering on a window with no matches).
	urlStr := "/v1/cases?since=" + day2.Format(time.RFC3339) +
		"&until=" + day3.Format(time.RFC3339)
	r := httptest.NewRequest(http.MethodGet, urlStr, nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "a.b.d") {
		t.Errorf("expected a.b.d (closed day2) in body: %s", body)
	}
	if !strings.Contains(body, "a.b.e") {
		t.Errorf("expected a.b.e (closed day3) in body: %s", body)
	}
	if strings.Contains(body, "a.b.c") {
		t.Errorf("did not expect a.b.c (closed day1) in body: %s", body)
	}
}

func TestCasesBadSinceUntil(t *testing.T) {
	s := newRunbookServer(t)
	seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", time.Now().UTC())

	for _, u := range []string{
		"/v1/cases?since=not-a-time",
		"/v1/cases?until=2026-99-99T00:00:00Z",
	} {
		r := httptest.NewRequest(http.MethodGet, u, nil)
		w := httptest.NewRecorder()
		s.serveCases(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d body=%s",
				u, w.Code, w.Body.String())
		}
	}
}

func TestCasesLimitCap(t *testing.T) {
	s := newRunbookServer(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", t0)
	}

	// limit=2 → exactly 2 items returned
	r := httptest.NewRequest(http.MethodGet, "/v1/cases?limit=2", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := strings.Count(w.Body.String(), `"id":`); got != 2 {
		t.Errorf("expected 2 items with limit=2, got %d: %s",
			got, w.Body.String())
	}

	// limit > hard cap (1000) is silently capped, not rejected.
	r = httptest.NewRequest(http.MethodGet, "/v1/cases?limit=99999", nil)
	w = httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("limit > hard cap should cap silently, got %d body=%s",
			w.Code, w.Body.String())
	}
}

func TestCasesBadLimit(t *testing.T) {
	s := newRunbookServer(t)
	for _, v := range []string{"0", "-1", "abc"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/cases?limit="+v, nil)
		w := httptest.NewRecorder()
		s.serveCases(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for limit=%q, got %d", v, w.Code)
		}
	}
}

func TestCasesCSVFormat(t *testing.T) {
	s := newRunbookServer(t)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", t0)
	seedClosedTicket(t, s, "a.b.d", "major", "rb/y", t0.Add(time.Minute))

	r := httptest.NewRequest(http.MethodGet, "/v1/cases?format=csv", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv...", ct)
	}
	body := w.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	// 1 header + 2 data rows
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 rows), got %d: %q",
			len(lines), body)
	}
	if !strings.HasPrefix(lines[0], "id,severity,asset_path,title") {
		t.Errorf("header row wrong: %q", lines[0])
	}
	if !strings.Contains(body, "a.b.c") || !strings.Contains(body, "a.b.d") {
		t.Errorf("missing ticket asset_paths in body: %q", body)
	}
	if !strings.Contains(body, t0.Format(time.RFC3339)) {
		t.Errorf("expected RFC3339 timestamp in body: %q", body)
	}
}

func TestCasesCSVEmpty(t *testing.T) {
	s := newRunbookServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/cases?format=csv", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected only header row, got %d lines: %q",
			len(lines), w.Body.String())
	}
}

func TestCasesBadFormat(t *testing.T) {
	s := newRunbookServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/cases?format=xml", nil)
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad format, got %d", w.Code)
	}
}

func TestCasesScopeStillEffective(t *testing.T) {
	// Without auth (s has auth==nil) every ticket is in scope, so
	// the unfiltered path was already covered. This test exercises
	// the authz branch by attaching a Principal with a tight scope
	// directly to the request context, then verifying the per-item
	// scope filter drops out-of-scope tickets AFTER the field
	// filters have run.
	s := newRunbookServer(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	seedClosedTicket(t, s, "a.b.c", "minor", "rb/x", t0) // in scope
	seedClosedTicket(t, s, "x.y.z", "minor", "rb/y", t0) // out of scope

	principal := Principal{
		Subject: "svc:test",
		Role:    RoleViewer,
		Scopes:  []string{"a.b.**"},
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/cases", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, principal))
	w := httptest.NewRecorder()
	s.serveCases(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "a.b.c") {
		t.Errorf("expected in-scope ticket a.b.c in body: %s", body)
	}
	if strings.Contains(body, "x.y.z") {
		t.Errorf("did not expect out-of-scope ticket x.y.z in body: %s", body)
	}
}
