// Package core — reports_test.go: end-to-end + unit tests for the
// /v1/reports/ops handler (PRMT-037, M2 E2.6 P551).
//
// Coverage:
//   - MTTR/mean response/MTBF math (unit, constructed timestamps)
//   - ticket counts (by state + severity)
//   - alarm Top (counting, sort, top-N cap, "firing only" filter)
//   - per-item scope filter (authz) and glob filter
//   - empty store → 200 with zero/null values
//   - HTTP method guard + bad query params
//   - end-to-end happy path through newTestServer
package core

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- pure-math unit tests -------------------------------------------------

func TestReports_MTTR_ResolvesAndIgnoresUnresolved(t *testing.T) {
	o1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	o2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	r1 := o1.Add(30 * time.Minute)
	r2 := o2.Add(90 * time.Minute)
	// Third ticket still open: ResolvedAt nil → must not contribute.
	o3 := o2.Add(time.Hour)
	tickets := []Ticket{
		{ID: "tk_a", AssetPath: "site01.pod000.cdu000", State: "resolved", OpenedAt: o1, ResolvedAt: &r1},
		{ID: "tk_b", AssetPath: "site01.pod000.cdu000", State: "resolved", OpenedAt: o2, ResolvedAt: &r2},
		{ID: "tk_c", AssetPath: "site01.pod000.cdu000", State: "open", OpenedAt: o3},
	}
	got := meanMTTR(tickets)
	if got == nil {
		t.Fatalf("MTTR is nil, want (1800+5400)/2=3600s")
	}
	want := 3600.0
	if math.Abs(*got-want) > 1e-6 {
		t.Errorf("MTTR = %v, want %v", *got, want)
	}
}

func TestReports_MTTR_NilWhenNoResolved(t *testing.T) {
	o := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	tickets := []Ticket{
		{ID: "tk_a", State: "open", OpenedAt: o},
		{ID: "tk_b", State: "acknowledged", OpenedAt: o, AckedAt: ptrTime(o.Add(5 * time.Minute))},
	}
	if got := meanMTTR(tickets); got != nil {
		t.Errorf("MTTR = %v, want nil", *got)
	}
}

func TestReports_Response_OnlyAckedTickets(t *testing.T) {
	o1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	o2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	a1 := o1.Add(2 * time.Minute)
	a2 := o2.Add(8 * time.Minute)
	tickets := []Ticket{
		{ID: "tk_a", State: "acknowledged", OpenedAt: o1, AckedAt: &a1},
		{ID: "tk_b", State: "acknowledged", OpenedAt: o2, AckedAt: &a2},
		{ID: "tk_c", State: "open", OpenedAt: o2}, // unacked → ignored
	}
	got := meanResponse(tickets)
	if got == nil {
		t.Fatalf("response is nil, want (120+480)/2=300s")
	}
	if want := 300.0; math.Abs(*got-want) > 1e-6 {
		t.Errorf("response = %v, want %v", *got, want)
	}
}

func TestReports_MTBF_PerAssetAdjacentGaps(t *testing.T) {
	// Asset A: 3 opens, gaps 100s + 200s → contributes 150 (mean)
	// Asset B: 2 opens, gap 400s → contributes 400
	// Asset C: 1 open → contributes nothing
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tickets := []Ticket{
		{ID: "1", AssetPath: "site01.pod000.cdu000", OpenedAt: base},
		{ID: "2", AssetPath: "site01.pod000.cdu000", OpenedAt: base.Add(100 * time.Second)},
		{ID: "3", AssetPath: "site01.pod000.cdu000", OpenedAt: base.Add(300 * time.Second)},
		{ID: "4", AssetPath: "site01.pod000.cdu001", OpenedAt: base},
		{ID: "5", AssetPath: "site01.pod000.cdu001", OpenedAt: base.Add(400 * time.Second)},
		{ID: "6", AssetPath: "site01.pod000.cdu002", OpenedAt: base},
	}
	got := meanMTBF(tickets)
	if got == nil {
		t.Fatalf("MTBF is nil, want non-nil")
	}
	// (100+200+400) / 3 = 233.333...
	want := 700.0 / 3.0
	if math.Abs(*got-want) > 1e-6 {
		t.Errorf("MTBF = %v, want %v", *got, want)
	}
}

func TestReports_MTBF_NilWhenNoAssetHasTwoOpens(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tickets := []Ticket{
		{ID: "1", AssetPath: "site01.pod000.cdu000", OpenedAt: base},
		{ID: "2", AssetPath: "site01.pod000.cdu001", OpenedAt: base},
	}
	if got := meanMTBF(tickets); got != nil {
		t.Errorf("MTBF = %v, want nil", *got)
	}
}

func TestReports_CountsByStateAndSeverity(t *testing.T) {
	o := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	r := o.Add(time.Minute)
	tickets := []Ticket{
		{ID: "1", State: "open", Severity: "critical", OpenedAt: o},
		{ID: "2", State: "open", Severity: "major", OpenedAt: o},
		{ID: "3", State: "acknowledged", Severity: "critical", OpenedAt: o, AckedAt: ptrTime(o)},
		{ID: "4", State: "resolved", Severity: "minor", OpenedAt: o, ResolvedAt: &r},
	}
	got := computeOpsReport(tickets)
	if got.TicketCounts.ByState["open"] != 2 {
		t.Errorf("open = %d, want 2", got.TicketCounts.ByState["open"])
	}
	if got.TicketCounts.ByState["acknowledged"] != 1 {
		t.Errorf("acknowledged = %d, want 1", got.TicketCounts.ByState["acknowledged"])
	}
	if got.TicketCounts.ByState["resolved"] != 1 {
		t.Errorf("resolved = %d, want 1", got.TicketCounts.ByState["resolved"])
	}
	if got.TicketCounts.BySeverity["critical"] != 2 {
		t.Errorf("critical = %d, want 2", got.TicketCounts.BySeverity["critical"])
	}
	if got.TicketCounts.BySeverity["major"] != 1 {
		t.Errorf("major = %d, want 1", got.TicketCounts.BySeverity["major"])
	}
	if got.TicketCounts.BySeverity["minor"] != 1 {
		t.Errorf("minor = %d, want 1", got.TicketCounts.BySeverity["minor"])
	}
}

func TestReports_EmptyStoreReturnsZeroOrNull(t *testing.T) {
	got := computeOpsReport(nil)
	if got.MTTRSeconds != nil {
		t.Errorf("MTTR = %v, want nil", *got.MTTRSeconds)
	}
	if got.MeanResponseSeconds != nil {
		t.Errorf("response = %v, want nil", *got.MeanResponseSeconds)
	}
	if got.MTBFSeconds != nil {
		t.Errorf("MTBF = %v, want nil", *got.MTBFSeconds)
	}
	if got.TicketCounts.ByState["open"] != 0 {
		t.Errorf("by_state[open] = %d, want 0", got.TicketCounts.ByState["open"])
	}
}

func TestReports_AlarmTop_OnlyFiringAndSortsByCount(t *testing.T) {
	alarms := []Alarm{
		{ID: "a1", Path: "site01.pod000.cdu000.fws.supply.flow", State: "firing"},
		{ID: "a2", Path: "site01.pod000.cdu000.fws.supply.flow", State: "firing"},
		{ID: "a3", Path: "site01.pod000.cdu000.fws.supply.flow", State: "acked"},
		{ID: "a4", Path: "site01.pod000.cdu001.fws.supply.flow", State: "firing"},
		{ID: "a5", Path: "site01.pod000.cdu002.fws.supply.flow", State: "firing"},
		{ID: "a6", Path: "site01.pod000.cdu002.fws.supply.flow", State: "firing"},
		{ID: "a7", Path: "site01.pod000.cdu002.fws.supply.flow", State: "firing"},
	}
	got := topAlarmsByPath(alarms, 10)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 distinct paths", len(got))
	}
	if got[0].Path != "site01.pod000.cdu002.fws.supply.flow" || got[0].Count != 3 {
		t.Errorf("top1 = %+v, want cdu002 count=3", got[0])
	}
	if got[1].Path != "site01.pod000.cdu000.fws.supply.flow" || got[1].Count != 2 {
		t.Errorf("top2 = %+v, want cdu000 count=2 (acked row excluded)", got[1])
	}
	if got[2].Path != "site01.pod000.cdu001.fws.supply.flow" || got[2].Count != 1 {
		t.Errorf("top3 = %+v, want cdu001 count=1", got[2])
	}
}

func TestReports_AlarmTop_RespectsTopN(t *testing.T) {
	alarms := []Alarm{
		{ID: "a1", Path: "p1", State: "firing"},
		{ID: "a2", Path: "p2", State: "firing"},
		{ID: "a3", Path: "p3", State: "firing"},
	}
	got := topAlarmsByPath(alarms, 2)
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2 (top-N=2)", len(got))
	}
}

// --- HTTP handler tests ---------------------------------------------------

// opsReport is the local mirror of opsReportResponse for json
// decoding in the handler tests. Field tags must stay in sync.
type opsReport struct {
	MTTRSeconds         *float64 `json:"mttr_seconds"`
	MeanResponseSeconds *float64 `json:"mean_response_seconds"`
	MTBFSeconds         *float64 `json:"mtbf_seconds"`
	TicketCounts        struct {
		ByState    map[string]int `json:"by_state"`
		BySeverity map[string]int `json:"by_severity"`
	} `json:"ticket_counts"`
	AlarmTop []struct {
		Path  string `json:"path"`
		Count int    `json:"count"`
	} `json:"alarm_top"`
	Window *struct {
		Since string `json:"since"`
	} `json:"window"`
}

func TestReports_EmptyStore_200(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET /v1/reports/ops on empty store: %d %s", r.code, r.body)
	}
	var got opsReport
	mustJSON(t, r.body, &got)
	if got.MTTRSeconds != nil {
		t.Errorf("MTTR = %v, want null on empty store", *got.MTTRSeconds)
	}
	if got.MeanResponseSeconds != nil {
		t.Errorf("response = %v, want null on empty store", *got.MeanResponseSeconds)
	}
	if got.MTBFSeconds != nil {
		t.Errorf("MTBF = %v, want null on empty store", *got.MTBFSeconds)
	}
	if got.TicketCounts.ByState["open"] != 0 {
		t.Errorf("by_state[open] = %d, want 0", got.TicketCounts.ByState["open"])
	}
	if got.AlarmTop == nil {
		t.Errorf("alarm_top = nil, want []")
	}
	if len(got.AlarmTop) != 0 {
		t.Errorf("alarm_top len = %d, want 0", len(got.AlarmTop))
	}
}

func TestReports_HappyPath(t *testing.T) {
	_, ts := newTestServer(t)
	// Seed two tickets via the public store + run them through the
	// state machine so timestamps are real.
	seedResolvedTicket(t, ts, "site01.pod000.cdu000", "major")
	seedAckedOnlyTicket(t, ts, "site01.pod000.cdu001", "critical")

	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got opsReport
	mustJSON(t, r.body, &got)
	if got.MTTRSeconds == nil {
		t.Errorf("MTTR is nil, want >=0 from resolved ticket")
	} else if *got.MTTRSeconds < 0 {
		t.Errorf("MTTR = %v, want >=0", *got.MTTRSeconds)
	}
	if got.MeanResponseSeconds == nil {
		t.Errorf("response is nil, want >=0 from acked ticket")
	} else if *got.MeanResponseSeconds < 0 {
		t.Errorf("response = %v, want >=0", *got.MeanResponseSeconds)
	}
	if got.TicketCounts.ByState["resolved"] < 1 {
		t.Errorf("by_state[resolved] = %d, want >=1", got.TicketCounts.ByState["resolved"])
	}
	if got.TicketCounts.ByState["acknowledged"] < 1 {
		t.Errorf("by_state[acknowledged] = %d, want >=1", got.TicketCounts.ByState["acknowledged"])
	}
	if got.TicketCounts.BySeverity["major"] < 1 {
		t.Errorf("by_severity[major] = %d, want >=1", got.TicketCounts.BySeverity["major"])
	}
}

func TestReports_SinceFilter(t *testing.T) {
	_, ts := newTestServer(t)
	seed := func(path, sev string) string {
		r := doReq(t, ts, http.MethodPost, "/v1/tickets",
			`{"asset_path":"`+path+`","title":"x","severity":"`+sev+`"}`)
		if r.code != http.StatusCreated {
			t.Fatalf("create: %d %s", r.code, r.body)
		}
		var tk Ticket
		mustJSON(t, r.body, &tk)
		return tk.ID
	}
	seed("site01.pod000.cdu000", "minor")
	// Future since → ticket should be filtered out.
	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops?since=2099-01-01T00:00:00Z", "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got opsReport
	mustJSON(t, r.body, &got)
	if got.Window == nil {
		t.Errorf("window = nil, want echoed since")
	}
	if got.TicketCounts.ByState["open"] != 0 {
		t.Errorf("by_state[open] = %d, want 0 (filtered by future since)", got.TicketCounts.ByState["open"])
	}
}

func TestReports_MethodNotAllowed(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/reports/ops", `{}`)
	if r.code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d %s, want 405", r.code, r.body)
	}
}

func TestReports_BadSince(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops?since=not-a-time", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("GET: %d %s, want 400", r.code, r.body)
	}
	mustProblem(t, r.body, "bad-request")
}

func TestReports_BadTop(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops?top=0", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("top=0: %d %s, want 400", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/reports/ops?top=banana", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("top=banana: %d %s, want 400", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/reports/ops?top=500", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("top=500: %d %s, want 400", r.code, r.body)
	}
}

func TestReports_BadFilter(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/reports/ops?filter=[bad", "")
	if r.code != http.StatusBadRequest {
		t.Fatalf("GET: %d %s, want 400", r.code, r.body)
	}
}

// --- scope filter end-to-end (auth enabled) ------------------------------

func TestReports_ScopeFilter_OutOfScopeDropped(t *testing.T) {
	// Build a viewer-scoped verifier via the existing helper, then
	// wire a Server with auth enabled. We hand-seed the store (the
	// viewer token cannot POST to /v1/tickets — role floor; the
	// scope filter on the GET path is what we want to test).
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"site01.**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	now := time.Now().UTC()
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID:        "tk_in",
		AssetPath: "site01.pod000.cdu000",
		State:     "open",
		Severity:  "minor",
		OpenedAt:  now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.st.PutTicket(context.Background(), Ticket{
		ID:        "tk_out",
		AssetPath: "site99.pod000.cdu000",
		State:     "open",
		Severity:  "minor",
		OpenedAt:  now,
	}, 0); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/reports/ops", nil)
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
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got opsReport
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.TicketCounts.ByState["open"] != 1 {
		t.Errorf("by_state[open] = %d, want 1 (out-of-scope dropped)", got.TicketCounts.ByState["open"])
	}
}

// --- helpers --------------------------------------------------------------

// ptrTime returns *time.Time for a value. Cheaper than time.Time
// literal in test setup.
func ptrTime(t time.Time) *time.Time { return &t }

// seedResolvedTicket creates a ticket and walks it open→ack→resolve.
// Returns the ticket id.
func seedResolvedTicket(t *testing.T, ts *httptest.Server, asset, sev string) []string {
	t.Helper()
	r := doReq(t, ts, http.MethodPost, "/v1/tickets",
		`{"asset_path":"`+asset+`","title":"x","severity":"`+sev+`"}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var tk Ticket
	mustJSON(t, r.body, &tk)
	// Walk: open → acknowledged → resolved. Wall clock controls
	// (ResolvedAt - OpenedAt); the happy-path test asserts a sane
	// non-negative value rather than a target duration.
	walk := []string{"acknowledged", "resolved"}
	for _, to := range walk {
		r = doReq(t, ts, http.MethodPost, "/v1/tickets/"+tk.ID+":transition",
			`{"to":"`+to+`"}`)
		if r.code != http.StatusOK {
			t.Fatalf("transition to %s: %d %s", to, r.code, r.body)
		}
	}
	return []string{tk.ID}
}

// seedAckedOnlyTicket creates a ticket and acknowledges it (no resolve).
func seedAckedOnlyTicket(t *testing.T, ts *httptest.Server, asset, sev string) []string {
	t.Helper()
	r := doReq(t, ts, http.MethodPost, "/v1/tickets",
		`{"asset_path":"`+asset+`","title":"x","severity":"`+sev+`"}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var tk Ticket
	mustJSON(t, r.body, &tk)
	r = doReq(t, ts, http.MethodPost, "/v1/tickets/"+tk.ID+":transition",
		`{"to":"acknowledged"}`)
	if r.code != http.StatusOK {
		t.Fatalf("ack: %d %s", r.code, r.body)
	}
	return []string{tk.ID}
}
