// Package core — reports.go: GET /v1/reports/ops (M2 E2.6 P551).
//
// Read-only aggregation over ListTickets + ListAlarms. Produces
// MTTR / mean response time / MTBF / ticket counts / alarm Top per
// spec-008 §3 (L69). Pure compute: no writes, no state changes, no
// new infra. Ticket/alarm endpoints are untouched.
//
// Permission: role ≥ viewer (the middleware applies the role floor
// for /v1/reports/ops as a list-scope endpoint — see authmw.go's
// isListScopeEndpoint table; the per-item scope filter lives here).
//
// Error contract: RFC 7807 via writeProblem. Empty store → 200 with
// zero/null values, never an error.
package core

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// opsReportResponse is the JSON body of GET /v1/reports/ops.
//
// JSON tags are the wire contract; do not rename without bumping
// spec-008. The pointer fields render as `null` when empty (so a
// client can tell "no resolved tickets" from "MTTR=0"), matching
// the §2-bis "0 / null" rule.
type opsReportResponse struct {
	MTTRSeconds         *float64                `json:"mttr_seconds"`
	MeanResponseSeconds *float64                `json:"mean_response_seconds"`
	MTBFSeconds         *float64                `json:"mtbf_seconds"`
	TicketCounts        opsReportTicketCounts   `json:"ticket_counts"`
	AlarmTop            []opsReportAlarmTopItem `json:"alarm_top"`
	Window              *opsReportWindow        `json:"window"`
}

// opsReportTicketCounts is the breakdown by state + severity.
// `open`/`acknowledged`/`resolved`/`closed` mirror Ticket.State;
// `critical`/`major`/`minor`/`info` mirror Ticket.Severity.
type opsReportTicketCounts struct {
	ByState    map[string]int `json:"by_state"`
	BySeverity map[string]int `json:"by_severity"`
}

// opsReportAlarmTopItem is one row of the alarm Top-N listing.
type opsReportAlarmTopItem struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// opsReportWindow echoes the optional ?since filter so the client
// can confirm what window the numbers were computed over.
type opsReportWindow struct {
	Since *time.Time `json:"since,omitempty"`
}

// serveOpsReport handles GET /v1/reports/ops. Non-GET → 405.
// Query params:
//
//	since  RFC3339 timestamp; tickets with OpenedAt < since excluded
//	top    alarm Top-N cap (default 10, max 100)
//	filter cpath glob applied to ticket.AssetPath + alarm.Path
//
// All other behaviour is governed by the per-item scope filter
// (authorize ActionRead) and the empty-store contract.
func (s *Server) serveOpsReport(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	q := r.URL.Query()

	var since *time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad since (need RFC3339)", v, r.URL.Path, rid)
			return
		}
		t = t.UTC()
		since = &t
	}

	topN := 10
	if v := q.Get("top"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad top (need positive int)", v, r.URL.Path, rid)
			return
		}
		if n > 100 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"top > 100", v, r.URL.Path, rid)
			return
		}
		topN = n
	}

	glob, err := cpath.CompileGlob("**")
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"default glob", err.Error(), r.URL.Path, rid)
		return
	}
	if f := q.Get("filter"); f != "" {
		g, err := cpath.CompileGlob(f)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad filter", err.Error(), r.URL.Path, rid)
			return
		}
		glob = g
	}

	allTickets, err := s.st.ListTickets(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	allAlarms, err := s.st.ListAlarms(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}

	principal, hasAuth := PrincipalFromContext(r.Context())

	// Per-item scope filter on tickets + optional since/glob. Out-of-
	// scope items are silently dropped (same rule as serveAlarms /
	// serveTicketsList).
	visibleTickets := make([]Ticket, 0, len(allTickets))
	for _, t := range allTickets {
		if since != nil && t.OpenedAt.Before(*since) {
			continue
		}
		if !glob.Match(t.AssetPath) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, t.AssetPath) != nil {
			continue
		}
		visibleTickets = append(visibleTickets, t)
	}

	resp := computeOpsReport(visibleTickets)

	// Per-item scope filter on alarms + Top-N aggregation. The
	// "snapshot not cumulative" caveat from §4 is recorded in the
	// report via the alarm_top field name (no extra flag in v1).
	visibleAlarms := make([]Alarm, 0, len(allAlarms))
	for _, a := range allAlarms {
		if !glob.Match(a.Path) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, a.Path) != nil {
			continue
		}
		visibleAlarms = append(visibleAlarms, a)
	}
	resp.AlarmTop = topAlarmsByPath(visibleAlarms, topN)
	if since != nil {
		resp.Window = &opsReportWindow{Since: since}
	}

	writeJSON(w, http.StatusOK, resp)
}

// computeOpsReport does the pure math: MTTR / mean response / MTBF
// + ticket counts. Exposed (lowercase) to the test file via the
// same package. The function takes the already-scope-filtered ticket
// slice so the authz concerns stay in serveOpsReport.
func computeOpsReport(tickets []Ticket) opsReportResponse {
	resp := opsReportResponse{
		TicketCounts: opsReportTicketCounts{
			ByState:    map[string]int{},
			BySeverity: map[string]int{},
		},
	}
	for _, t := range tickets {
		resp.TicketCounts.ByState[t.State]++
		resp.TicketCounts.BySeverity[t.Severity]++
	}
	if v := meanMTTR(tickets); v != nil {
		resp.MTTRSeconds = v
	}
	if v := meanResponse(tickets); v != nil {
		resp.MeanResponseSeconds = v
	}
	if v := meanMTBF(tickets); v != nil {
		resp.MTBFSeconds = v
	}
	return resp
}

// meanMTTR is the mean of (ResolvedAt - OpenedAt) over tickets
// that have reached resolution. nil → no resolvable tickets in
// the window. Returns seconds (float, sub-second precision).
func meanMTTR(tickets []Ticket) *float64 {
	var total float64
	var n int
	for _, t := range tickets {
		if t.ResolvedAt == nil {
			continue
		}
		d := t.ResolvedAt.Sub(t.OpenedAt).Seconds()
		if d < 0 {
			continue // clock skew guard; spec-008 doesn't define, skip
		}
		total += d
		n++
	}
	if n == 0 {
		return nil
	}
	v := total / float64(n)
	return &v
}

// meanResponse is the mean of (AckedAt - OpenedAt) over tickets
// that have been acknowledged. nil → no acked tickets in the
// window. Returns seconds.
func meanResponse(tickets []Ticket) *float64 {
	var total float64
	var n int
	for _, t := range tickets {
		if t.AckedAt == nil {
			continue
		}
		d := t.AckedAt.Sub(t.OpenedAt).Seconds()
		if d < 0 {
			continue
		}
		total += d
		n++
	}
	if n == 0 {
		return nil
	}
	v := total / float64(n)
	return &v
}

// meanMTBF is the mean of per-asset adjacent open intervals. For
// each asset, sort tickets by OpenedAt; gaps between consecutive
// opens in the same asset contribute one sample. The overall MTBF
// is the mean of all samples. Assets with < 2 opens contribute
// nothing (per §4 contract). Returns seconds, or nil when no asset
// has ≥ 2 opens in the window.
func meanMTBF(tickets []Ticket) *float64 {
	byAsset := map[string][]Ticket{}
	for _, t := range tickets {
		byAsset[t.AssetPath] = append(byAsset[t.AssetPath], t)
	}
	var total float64
	var n int
	for _, group := range byAsset {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			return group[i].OpenedAt.Before(group[j].OpenedAt)
		})
		for i := 1; i < len(group); i++ {
			d := group[i].OpenedAt.Sub(group[i-1].OpenedAt).Seconds()
			if d < 0 {
				continue
			}
			total += d
			n++
		}
	}
	if n == 0 {
		return nil
	}
	v := total / float64(n)
	return &v
}

// topAlarmsByPath counts currently-firing alarms per path and
// returns the top-N. "Snapshot not cumulative" per spec-008 §3 /
// §4 — M3 alarm history table will replace this with accumulated
// counts. Only firing rows contribute (acked/resolved alarms are
// not in the live problem set, per spec-003 §4).
func topAlarmsByPath(alarms []Alarm, topN int) []opsReportAlarmTopItem {
	counts := map[string]int{}
	for _, a := range alarms {
		if a.State != "firing" {
			continue
		}
		counts[a.Path]++
	}
	if len(counts) == 0 {
		return []opsReportAlarmTopItem{}
	}
	rows := make([]opsReportAlarmTopItem, 0, len(counts))
	for p, c := range counts {
		rows = append(rows, opsReportAlarmTopItem{Path: p, Count: c})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Path < rows[j].Path
	})
	if len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}
