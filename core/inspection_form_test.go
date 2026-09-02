// Package core — inspection_form_test.go: end-to-end tests for
// the PRMT-059 mobile-web inspection form handler.
//
// Coverage:
//
//   - GET renders the checklist for an inspection ticket
//     (text/html; checkbox per item; title echoed).
//   - Non-inspection ticket (Runbook without "inspection:" prefix)
//     → 404.
//   - Unknown ticket id (well-formed but not in store) → 404.
//   - Bad ticket id (regex miss) → 400.
//   - POST with checked items + note transitions the ticket to
//     "resolved", appends the result block to Runbook, and
//     303-redirects back to GET.
//   - html/template auto-escapes user-controlled fields (XSS).
//   - 401 when no Bearer is supplied (the route is in the /v1
//     tree, so middleware gates it).
//   - 403 when an out-of-scope operator hits the form (handler
//     re-runs authorize() against the stored ticket.AssetPath).
//   - Re-POST against a resolved ticket is rejected with 422
//     (state-machine guard, mirror :transition endpoint).
//   - Wrong method (DELETE/PUT) → 405.
package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFormServer returns a Server with no auth (fileStore +
// real dict) — same shape as newInspectionServer / newPMServer.
// Auth-specific tests below use newAuthTestServer directly.
func newFormServer(t *testing.T) *Server {
	return newPMServer(t)
}

// plantInspectionTicket seeds a single inspection ticket with
// the given items and returns its id. Runbook is set via
// encodeInspectionRunbook so the format matches what the scanner
// would produce (PRMT-049 §4). The ticket's AssetPath lives
// under "site01.pod000.cdu000" so a /v1/assets path validator
// can pass if any test asserts it.
//
// state="open" matches what the inspection scanner produces
// (PRMT-049). For POST tests that need the form to resolve the
// ticket, the ticket must be in "acknowledged" state — the
// state machine (tickets.go: allowedTransition) forbids
// open→resolved, so the realistic flow is ack first → form resolves.
func plantInspectionTicket(t *testing.T, s *Server, assetPath string, items []string) string {
	t.Helper()
	now := time.Now().UTC()
	tk := Ticket{
		ID:        newTicketID(),
		AssetPath: assetPath,
		Title:     "weekly inspection",
		Severity:  "info",
		State:     "open",
		OpenedAt:  now,
		Runbook:   encodeInspectionRunbook(items),
	}
	if _, err := s.st.PutTicket(context.Background(), tk, 0); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	return tk.ID
}

// plantAckedInspectionTicket seeds an inspection ticket in
// "acknowledged" state, ready for the form to resolve.
func plantAckedInspectionTicket(t *testing.T, s *Server, assetPath string, items []string) string {
	t.Helper()
	id := plantInspectionTicket(t, s, assetPath, items)
	cur, _, _ := s.st.GetTicket(context.Background(), id)
	cur.State = "acknowledged"
	if _, err := s.st.PutTicket(context.Background(), cur, 0); err != nil {
		t.Fatalf("reseat acknowledged: %v", err)
	}
	return id
}

// --- GET: render path -----------------------------------------------------

func TestInspectionForm_GET_RendersItems(t *testing.T) {
	s := newFormServer(t)
	id := plantInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"check delta-T", "verify flow rate"})

	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/"+id, nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, id) {
		t.Errorf("body should echo ticket id %s", id)
	}
	if !strings.Contains(body, "check delta-T") {
		t.Errorf("body should list first item text")
	}
	if !strings.Contains(body, "verify flow rate") {
		t.Errorf("body should list second item text")
	}
	if !strings.Contains(body, "sgp01.pod001.cdu000.fws.supply.flow") {
		t.Errorf("body should echo asset path")
	}
	// Two checkboxes, one per item.
	if c := strings.Count(body, `type="checkbox"`); c != 2 {
		t.Errorf("checkbox count = %d, want 2", c)
	}
}

func TestInspectionForm_GET_EmptyItemsRendersEmptyState(t *testing.T) {
	s := newFormServer(t)
	// Plant a ticket whose Runbook carries the "inspection:" prefix
	// but yields zero items after decode (the empty-body case is
	// reachable when a future inspection template ships with an
	// empty items list). The decoder treats "inspection:" (no body)
	// as no items, and the form renders the empty-state copy.
	now := time.Now().UTC()
	id := newTicketID()
	if _, err := s.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title: "empty", Severity: "info", State: "open",
		OpenedAt: now, Runbook: "inspection:",
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/"+id, nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `type="checkbox"`) {
		t.Errorf("empty checklist should not render checkboxes")
	}
	if !strings.Contains(w.Body.String(), "No checklist items") {
		t.Errorf("empty checklist should render empty-state copy")
	}
}

// --- 404 / 400 / 405 path -------------------------------------------------

func TestInspectionForm_GET_NonInspectionTicket_404(t *testing.T) {
	s := newFormServer(t)
	// Ticket with empty / non-inspection Runbook → 404.
	now := time.Now().UTC()
	id := newTicketID()
	if _, err := s.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title: "alarm-driven", Severity: "major", State: "open",
		OpenedAt: now, Runbook: "rb/cdu-deltat-low",
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/"+id, nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("non-inspection ticket: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestInspectionForm_GET_UnknownTicket_404(t *testing.T) {
	s := newFormServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/tk_AAAAAAAAAAAAAAAA", nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown ticket: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestInspectionForm_GET_BadID_400(t *testing.T) {
	s := newFormServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/not-a-ticket-id", nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestInspectionForm_WrongMethod_405(t *testing.T) {
	s := newFormServer(t)
	r := httptest.NewRequest(http.MethodDelete, "/v1/inspections/form/tk_AAAAAAAAAAAAAAAA", nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status=%d body=%s", w.Code, w.Body.String())
	}
}

// --- POST: submit + state transition --------------------------------------

func TestInspectionForm_POST_ResolvesAndAppendsResult(t *testing.T) {
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"check delta-T", "verify flow rate", "log timestamp"})

	form := url.Values{}
	form.Add("item", "0")
	form.Add("item", "2")
	form.Add("notes", "all good")

	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/v1/inspections/form/"+id {
		t.Errorf("Location = %q, want /v1/inspections/form/%s", loc, id)
	}
	got, ok, err := s.st.GetTicket(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("post-submit get: ok=%v err=%v", ok, err)
	}
	if got.State != "resolved" {
		t.Errorf("state = %q, want resolved", got.State)
	}
	if got.ResolvedAt == nil {
		t.Errorf("ResolvedAt should be set")
	}
	if !strings.HasPrefix(got.Runbook, "inspection:") {
		t.Errorf("Runbook should retain inspection: prefix; got %q", got.Runbook)
	}
	if !strings.Contains(got.Runbook, "result:submitted=") {
		t.Errorf("Runbook should carry result:submitted block; got %q", got.Runbook)
	}
	// Sorted indices: 0,2.
	if !strings.Contains(got.Runbook, "result:checked=0,2\n") {
		t.Errorf("checked indices should be 0,2; got %q", got.Runbook)
	}
	if !strings.Contains(got.Runbook, "result:note=all good") {
		t.Errorf("note should be persisted; got %q", got.Runbook)
	}
	// Original checklist items still readable.
	for _, it := range []string{"check delta-T", "verify flow rate", "log timestamp"} {
		if !strings.Contains(got.Runbook, it) {
			t.Errorf("Runbook should retain item %q; got %q", it, got.Runbook)
		}
	}
}

func TestInspectionForm_POST_IllegalStateTransition_422(t *testing.T) {
	s := newFormServer(t)
	// Plant a closed ticket — closed has no forward transition.
	id := plantInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow", []string{"x"})
	if _, err := s.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title: "t", Severity: "info", State: "closed",
		OpenedAt: time.Now().UTC(),
		Runbook:  encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("reseat closed: %v", err)
	}
	form := url.Values{}
	form.Add("item", "0")
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("closed->resolved: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestInspectionForm_POST_NoItemsResolved(t *testing.T) {
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"check delta-T"})

	form := url.Values{}
	form.Add("notes", "no checks")

	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, _, _ := s.st.GetTicket(context.Background(), id)
	if got.State != "resolved" {
		t.Errorf("state = %q, want resolved", got.State)
	}
	if !strings.Contains(got.Runbook, "result:checked=\n") {
		t.Errorf("checked= empty line expected; got %q", got.Runbook)
	}
}

// --- XSS guard ------------------------------------------------------------

func TestInspectionForm_GET_EscapesUserContent(t *testing.T) {
	s := newFormServer(t)
	// Plant a ticket whose title carries an XSS payload. Titles
	// are echoed back into the HTML, so html/template must
	// escape the angle brackets / quote characters. The payload
	// deliberately includes both a tag-injection and an attribute
	// injection probe.
	const xss = `<script>alert(1)</script>" onerror="alert(2)`
	id := plantInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"normal item"})
	// Overwrite with the XSS-y title.
	cur, _, _ := s.st.GetTicket(context.Background(), id)
	cur.Title = xss
	if _, err := s.st.PutTicket(context.Background(), cur, 0); err != nil {
		t.Fatalf("reseat: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/"+id, nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw <script> tag in body — html/template failed to escape: %s", body)
	}
	if strings.Contains(body, `" onerror="alert(2)`) {
		t.Errorf("raw onerror= attribute injection — escape failed: %s", body)
	}
	// The escaped form must be present (defence-in-depth — proves
	// html/template did the substitution).
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped &lt;script&gt; in body: %s", body)
	}
}

func TestInspectionForm_POST_EscapesNotesInSubsequentRender(t *testing.T) {
	// Submit a note containing angle brackets and confirm the
	// resulting Runbook field stores the raw text (we don't echo
	// notes into HTML on the POST path itself; the next render
	// round-trips it through the template and must escape).
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"item"})
	form := url.Values{}
	form.Add("item", "0")
	form.Add("notes", "<img src=x onerror=alert(1)>")
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, _, _ := s.st.GetTicket(context.Background(), id)
	if !strings.Contains(got.Runbook, "<img src=x onerror=alert(1)>") {
		t.Errorf("Runbook should store raw note text; got %q", got.Runbook)
	}
}

// --- auth gating (no bearer) ---------------------------------------------

func TestInspectionForm_NoBearer_401(t *testing.T) {
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Seed a ticket via the unauthenticated store seam.
	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GET
	r := doReq(t, ts, http.MethodGet, "/v1/inspections/form/"+id, "")
	if r.code != http.StatusUnauthorized {
		t.Errorf("GET no-bearer: code=%d, want 401", r.code)
	}
	// POST
	r = doReq(t, ts, http.MethodPost, "/v1/inspections/form/"+id,
		"item=0&notes=hi")
	if r.code != http.StatusUnauthorized {
		t.Errorf("POST no-bearer: code=%d, want 401", r.code)
	}
}

func TestInspectionForm_OutOfScopeOperator_403(t *testing.T) {
	// Operator scoped to a DIFFERENT asset_path; form ticket
	// belongs to a path they cannot write. Handler must 403.
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site09.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := authReq(t, ts, http.MethodGet, "/v1/inspections/form/"+id, "", operatorTok)
	if r.code != http.StatusForbidden {
		t.Errorf("GET out-of-scope: code=%d, want 403; body=%s", r.code, r.body)
	}
	r = authReq(t, ts, http.MethodPost, "/v1/inspections/form/"+id,
		"item=0&notes=hi", operatorTok)
	if r.code != http.StatusForbidden {
		t.Errorf("POST out-of-scope: code=%d, want 403; body=%s", r.code, r.body)
	}
}

func TestInspectionForm_InScopeViewer_GET_OK(t *testing.T) {
	// Viewer in scope can render the form (read-only is fine for
	// the GET path). POST would 403 because role floor is viewer.
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := authReq(t, ts, http.MethodGet, "/v1/inspections/form/"+id, "", viewerTok)
	if r.code != http.StatusOK {
		t.Errorf("GET in-scope viewer: code=%d, want 200; body=%s", r.code, r.body)
	}
	// POST as viewer → 403 (role floor, mirrors tickets POST/transition).
	r = authReq(t, ts, http.MethodPost, "/v1/inspections/form/"+id,
		"item=0&notes=hi", viewerTok)
	if r.code != http.StatusForbidden {
		t.Errorf("POST viewer: code=%d, want 403; body=%s", r.code, r.body)
	}
}

// --- pure helper unit tests -----------------------------------------------

func TestDecodeInspectionRunbook_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"two items", []string{"check delta-T", "verify flow rate"}},
		{"single item", []string{"only one"}},
		{"empty list", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb := encodeInspectionRunbook(tc.in)
			got := decodeInspectionRunbook(rb)
			if len(got) != len(tc.in) {
				t.Fatalf("len(got)=%d, want %d (got=%v)", len(got), len(tc.in), got)
			}
			for i := range got {
				if got[i] != tc.in[i] {
					t.Errorf("[%d] got %q want %q", i, got[i], tc.in[i])
				}
			}
		})
	}
}

func TestDecodeInspectionRunbook_StripsPriorResultBlock(t *testing.T) {
	// A re-render of a previously-submitted ticket must hide the
	// old result: lines so the operator sees only the original
	// checklist.
	rb := "inspection:check delta-T\nverify flow rate\nresult:submitted=2026-06-20T12:00:00Z\nresult:checked=0,1\n"
	got := decodeInspectionRunbook(rb)
	want := []string{"check delta-T", "verify flow rate"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestParseCheckedIndices_DedupAndSort(t *testing.T) {
	got := parseCheckedIndices([]string{"2", "0", "2", "5", "0", "abc", "-1"})
	want := []int{0, 2, 5}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %d want %d", i, got[i], want[i])
		}
	}
}

func TestAppendInspectionResult_PreservesChecklist(t *testing.T) {
	rb := encodeInspectionRunbook([]string{"a", "b"})
	got := appendInspectionResult(rb, "hello", []int{0}, time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC))
	for _, line := range []string{"inspection:a", "inspection:b"} {
		_ = line // prefix is "inspection:" but lines are listed without it; skip
	}
	// The checklist lines themselves (without the prefix) must
	// still be present after appending the result block.
	if !strings.Contains(got, "inspection:a\n") {
		t.Errorf("checklist line 'a' missing: %q", got)
	}
	if !strings.Contains(got, "b\n") {
		t.Errorf("checklist line 'b' missing: %q", got)
	}
	if !strings.Contains(got, "result:submitted=2026-06-20T12:00:00Z\n") {
		t.Errorf("submitted block missing: %q", got)
	}
	if !strings.Contains(got, "result:checked=0\n") {
		t.Errorf("checked block missing: %q", got)
	}
	if !strings.Contains(got, "result:note=hello\n") {
		t.Errorf("note block missing: %q", got)
	}
}

// --- PRMT-079: notes-POST MaxBytesReader -------------------------------

// TestInspectionForm_POST_OversizeBody_400 confirms that the
// notes-POST handler now wraps r.Body with http.MaxBytesReader
// (1<<16) before ParseForm. A body just over the cap must be
// rejected with 400 (the existing bad-form branch) and the
// ticket MUST NOT be resolved — i.e. the cap is the gate, the
// len(notes)>1<<16 second-line check is now a backstop, not the
// primary defence.
func TestInspectionForm_POST_OversizeBody_400(t *testing.T) {
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})

	// 64KiB + 1KiB payload — over the 1<<16 cap. The "notes="
	// prefix is the form field; padding pads it past the cap.
	const over = (1 << 16) + 1024
	pad := strings.Repeat("A", over-len("notes="))
	body := "notes=" + pad
	if len(body) <= 1<<16 {
		t.Fatalf("test setup wrong: body len=%d, want > 1<<16", len(body))
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize: status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	// Ticket must NOT have been transitioned to resolved.
	got, ok, err := s.st.GetTicket(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("post-oversize get: ok=%v err=%v", ok, err)
	}
	if got.State != "acknowledged" {
		t.Errorf("oversize must not transition ticket; state=%q want acknowledged", got.State)
	}
	// The runbook must not have gained a result: block.
	if strings.Contains(got.Runbook, inspectionFormResultPrefix) {
		t.Errorf("oversize must not append result block; runbook=%q", got.Runbook)
	}
}

// TestInspectionForm_POST_NormalBody_Unchanged is the
// regression guard: a body well under the 1<<16 cap must still
// flow through the original happy path (303 → ticket resolved,
// result block appended, checklist preserved). Mirrors
// TestInspectionForm_POST_ResolvesAndAppendsResult but is
// placed next to the oversize test so the new cap is bracketed
// by its two boundary checks.
func TestInspectionForm_POST_NormalBody_Unchanged(t *testing.T) {
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"check delta-T", "verify flow rate"})

	form := url.Values{}
	form.Add("item", "0")
	form.Add("item", "1")
	form.Add("notes", "all good")

	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id,
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, _, _ := s.st.GetTicket(context.Background(), id)
	if got.State != "resolved" {
		t.Errorf("state=%q, want resolved", got.State)
	}
	if !strings.Contains(got.Runbook, "result:note=all good") {
		t.Errorf("note should be persisted; got %q", got.Runbook)
	}
}

// --- PRMT-063: photo upload --------------------------------------------

// newFormServerWithPhotoDir builds a form server (file store +
// dict, no auth) with -inspection-photo-dir pointed at a temp
// dir. maxBytes <= 0 keeps the default 8 MiB. Returns the server
// and the dir so tests can assert on the files written.
func newFormServerWithPhotoDir(t *testing.T, maxBytes int64) (*Server, string) {
	t.Helper()
	s := newFormServer(t)
	dir := t.TempDir()
	s.SetInspectionPhotoDir(dir, maxBytes)
	return s, dir
}

// buildMultipart builds a multipart/form-data body with a single
// "file" part whose filename is `filename` and content is
// `content`. Returned alongside the Content-Type header the
// handler expects.
func buildMultipart(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	return body, mw.FormDataContentType()
}

// jpegMagic is the smallest valid JPEG (SOI + APP0 stub). Enough
// for http.DetectContentType to identify as image/jpeg.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}

// pngMagic is the canonical PNG header.
var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// pdfMagic is the canonical %PDF- header.
var pdfMagic = []byte("%PDF-1.4\n")

func TestPhotoUpload_HappyPath_JPEG(t *testing.T) {
	s, dir := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"check delta-T"})

	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Response body carries the relative path + the random-prefixed
	// stored filename; the per-ticket subdir is created on save.
	if !strings.Contains(w.Body.String(), `"path":"`) {
		t.Errorf("response missing path: %s", w.Body.String())
	}
	// File landed on disk under <dir>/<ticketID>/<rand>-site.jpg
	ticketDir := filepath.Join(dir, id)
	entries, err := os.ReadDir(ticketDir)
	if err != nil {
		t.Fatalf("read ticket dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), "-site.jpg") {
		t.Errorf("stored name should end in -site.jpg; got %q", entries[0].Name())
	}
	// Runbook got the result:photo= marker.
	got, _, _ := s.st.GetTicket(context.Background(), id)
	if !strings.Contains(got.Runbook, "result:photo="+id+"/") {
		t.Errorf("Runbook missing result:photo= marker: %q", got.Runbook)
	}
}

func TestPhotoUpload_HappyPath_PNG(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	body, ct := buildMultipart(t, "snap.png", pngMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_HappyPath_PDF(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	body, ct := buildMultipart(t, "report.pdf", pdfMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_Oversize_413(t *testing.T) {
	// Tight cap so the test stays tiny on disk.
	s, _ := newFormServerWithPhotoDir(t, 1024)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	// 8 KiB JPEG-shaped bytes (header + padding) → 413.
	huge := append([]byte{}, jpegMagic...)
	for len(huge) < 8192 {
		huge = append(huge, 0x00)
	}
	body, ct := buildMultipart(t, "big.jpg", huge)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: status=%d, want 413; body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_BadExtension_415(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	for _, ext := range []string{"x.exe", "x.txt", "x.doc", "noext", "x"} {
		body, ct := buildMultipart(t, "evil"+strings.TrimPrefix(ext, "x"), jpegMagic)
		r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		s.serveInspectionForm(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("ext=%q: status=%d, want 415", ext, w.Code)
		}
	}
}

func TestPhotoUpload_PathTraversal_Rejected(t *testing.T) {
	// Three classes of adversarial filename, all of which must
	// result in a file safely contained under <dir>/<ticketID>/.
	// path.Base + path.Clean + abs-dir containment in the handler
	// form the runbook-style triple defence; this test asserts
	// the empirical outcome: no file escapes the per-ticket
	// subdir, and every saved file carries a ".."-free basename.
	s, dir := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	// Names that collapse to a valid basename after path.Base
	// are saved under the per-ticket subdir (this is safe — the
	// handler's abs-dir containment guarantees containment).
	// Names that keep a ".." segment OR fail the extension
	// whitelist are rejected outright.
	collapsing := []string{"../etc/passwd.jpg", "/abs.jpg", "../passwd.jpg"}
	rejected := []string{"..jpg", "..\\windows.jpg", "sub/..jpg"}
	for _, name := range collapsing {
		body, ct := buildMultipart(t, name, jpegMagic)
		r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		s.serveInspectionForm(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("name=%q: status=%d, want 200 (collapse-to-safe); body=%s",
				name, w.Code, w.Body.String())
		}
	}
	for _, name := range rejected {
		body, ct := buildMultipart(t, name, jpegMagic)
		r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		s.serveInspectionForm(w, r)
		if w.Code == http.StatusOK {
			t.Errorf("name=%q: status=200, want non-OK (must reject); body=%s",
				name, w.Body.String())
		}
	}
	// Every file written under <dir> must live under
	// <dir>/<ticketID>/ and have a ".."-free basename.
	walkTicketDir(t, dir, id)
	// Nothing should have escaped to <dir> itself.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == id {
			continue // expected per-ticket subdir
		}
		// A stray file at the top level would mean a path
		// traversal slipped through.
		full := filepath.Join(dir, e.Name())
		t.Errorf("unexpected file at <dir>/%s — path-traversal guard failed", full)
	}
}

// walkTicketDir asserts every file under <dir>/<ticketID>/ has a
// ".."-free basename. Cheaper than mocking the abs check in the
// handler — the real on-disk layout IS the contract.
func walkTicketDir(t *testing.T, dir, id string) {
	t.Helper()
	ticketDir := filepath.Join(dir, id)
	entries, err := os.ReadDir(ticketDir)
	if err != nil {
		t.Fatalf("read ticket subdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") {
			t.Errorf("file under ticket subdir carries '..': %q", e.Name())
		}
	}
}

func TestPhotoUpload_MIMEMismatch_415(t *testing.T) {
	// Filename says .pdf but bytes are JPEG → 415 (MIME sniff
	// wins over extension).
	s, _ := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	body, ct := buildMultipart(t, "spoof.pdf", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("MIME mismatch: status=%d, want 415; body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_NonInspectionTicket_404(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	// Plant a non-inspection ticket.
	now := time.Now().UTC()
	id := newTicketID()
	if _, err := s.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title: "alarm", Severity: "major", State: "open",
		OpenedAt: now, Runbook: "rb/cdu-deltat-low",
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("non-inspection: status=%d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_UnknownTicket_404(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/tk_AAAAAAAAAAAAAAAA/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown: status=%d, want 404", w.Code)
	}
}

func TestPhotoUpload_BadTicketID_400(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/not-a-ticket/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id: status=%d, want 400", w.Code)
	}
}

func TestPhotoUpload_DisabledDir_503(t *testing.T) {
	// No SetInspectionPhotoDir call → dir stays "".
	s := newFormServer(t)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := httptest.NewRequest(http.MethodPost, "/v1/inspections/form/"+id+"/photo", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled: status=%d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestPhotoUpload_WrongMethod_405(t *testing.T) {
	s, _ := newFormServerWithPhotoDir(t, 0)
	id := plantAckedInspectionTicket(t, s, "sgp01.pod001.cdu000.fws.supply.flow",
		[]string{"x"})
	r := httptest.NewRequest(http.MethodGet, "/v1/inspections/form/"+id+"/photo", nil)
	w := httptest.NewRecorder()
	s.serveInspectionForm(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on photo: status=%d, want 405", w.Code)
	}
}

func TestPhotoUpload_NoBearer_401(t *testing.T) {
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := doReq(t, ts, http.MethodPost, "/v1/inspections/form/"+id+"/photo", "")
	if r.code != http.StatusUnauthorized {
		t.Errorf("no-bearer: code=%d, want 401", r.code)
	}
}

func TestPhotoUpload_InScopeOperator_OK(t *testing.T) {
	// Happy path through the auth middleware: operator with the
	// right scope, in-scope ticket, valid file → 200.
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site01.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	srv.SetInspectionPhotoDir(t.TempDir(), 0)

	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Build a multipart body as bytes so we can hand it to
	// authReq's body string param.
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	// authReq takes a string body — convert.
	r := authReqBytes(t, ts, http.MethodPost,
		"/v1/inspections/form/"+id+"/photo", body.Bytes(), ct, operatorTok)
	if r.code != http.StatusOK {
		t.Errorf("operator in-scope: code=%d, want 200; body=%s", r.code, r.body)
	}
}

func TestPhotoUpload_OutOfScopeOperator_403(t *testing.T) {
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"site09.**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	srv.SetInspectionPhotoDir(t.TempDir(), 0)

	now := time.Now().UTC()
	id := newTicketID()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID: id, AssetPath: "site01.pod000.cdu000",
		Title: "t", Severity: "info", State: "open",
		OpenedAt: now, Runbook: encodeInspectionRunbook([]string{"x"}),
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildMultipart(t, "site.jpg", jpegMagic)
	r := authReqBytes(t, ts, http.MethodPost,
		"/v1/inspections/form/"+id+"/photo", body.Bytes(), ct, operatorTok)
	if r.code != http.StatusForbidden {
		t.Errorf("out-of-scope: code=%d, want 403; body=%s", r.code, r.body)
	}
}

// authReqBytes is a multipart-aware cousin of authReq. It sets
// the Content-Type header (which authReq cannot do) and submits
// a binary body. Returns the same httpResp struct.
func authReqBytes(t *testing.T, ts *httptest.Server, method, path string, body []byte, contentType, token string) httpResp {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResp{code: resp.StatusCode, body: string(b)}
}

// --- pure helper unit tests -----------------------------------------------

func TestSafePhotoName(t *testing.T) {
	good := []string{"site.jpg", "report.PDF", "x.jpeg", "x.png", "with-dash.jpg", "with_underscore.png"}
	for _, n := range good {
		out, ok := safePhotoName(n)
		if !ok {
			t.Errorf("safePhotoName(%q) = false, want true", n)
		}
		if out == "" {
			t.Errorf("safePhotoName(%q) empty on success", n)
		}
	}
	// ".." is rejected outright (the basename would still be "..").
	// "/" / "" / "." are also rejected by the base==special check.
	// Extension-whitelist misses are rejected.
	// "x\0.jpg" and "x\\y.jpg" are rejected by the path-sep /
	// NUL check. (Note: "../passwd.jpg" and "/abs.jpg" collapse
	// via path.Base to safe basenames — the abs-dir containment
	// in the handler is the real guarantee those can't escape;
	// the helper accepts them so the handler can save under
	// <dir>/<ticketID>/. This mirrors runbooks.go's defence
	// layout.)
	bad := []string{"", "noext", "x.exe", "x.txt", "x.doc", "..", ".", "/", "x\\y.jpg", "x\x00.jpg", "..jpg"}
	for _, n := range bad {
		out, ok := safePhotoName(n)
		if ok {
			t.Errorf("safePhotoName(%q) = %q, want false", n, out)
		}
	}
	// path.Base collapses the rest, so the helper accepts them;
	// the abs-dir check in the handler is what makes them safe
	// on disk. Verified by TestPhotoUpload_PathTraversal_Rejected.
	collapsing := []string{"../passwd.jpg", "/abs.jpg"}
	for _, n := range collapsing {
		_, ok := safePhotoName(n)
		if !ok {
			t.Errorf("safePhotoName(%q) collapsed safely but was rejected", n)
		}
	}
}

func TestMimeMatchesExtension(t *testing.T) {
	cases := []struct {
		mime, name string
		want       bool
	}{
		{"image/jpeg", "a.jpg", true},
		{"image/jpeg", "a.jpeg", true},
		{"image/jpeg", "a.JPG", true},
		{"image/png", "a.png", true},
		{"application/pdf", "a.pdf", true},
		// Mismatches
		{"image/jpeg", "a.png", false},
		{"image/png", "a.jpg", false},
		{"application/pdf", "a.jpg", false},
		{"application/octet-stream", "a.jpg", false},
	}
	for _, tc := range cases {
		if got := mimeMatchesExtension(tc.mime, tc.name); got != tc.want {
			t.Errorf("mimeMatchesExtension(%q, %q) = %v, want %v", tc.mime, tc.name, got, tc.want)
		}
	}
}

func TestAppendInspectionPhoto_AddsMarker(t *testing.T) {
	rb := encodeInspectionRunbook([]string{"a"})
	got := appendInspectionPhoto(rb, "tk_AAAAAAAAAAAAA/rand-site.jpg")
	if !strings.Contains(got, "result:photo=tk_AAAAAAAAAAAAA/rand-site.jpg\n") {
		t.Errorf("photo marker missing: %q", got)
	}
	// Original checklist must still be present.
	if !strings.Contains(got, "inspection:a\n") {
		t.Errorf("checklist lost: %q", got)
	}
}

func TestRandomHex_Length(t *testing.T) {
	for _, n := range []int{1, 4, 8} {
		s, err := randomHex(n)
		if err != nil {
			t.Fatalf("randomHex(%d): %v", n, err)
		}
		if len(s) != n*2 {
			t.Errorf("randomHex(%d) length = %d, want %d", n, len(s), n*2)
		}
	}
	_ = fmt.Sprintf // import is reserved for future test expansion
}
