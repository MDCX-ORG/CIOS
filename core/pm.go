// Package core — pm.go: PMSchedule CRUD + background scanner
// (M2 E2.4 P531 / PRMT-043).
//
// Two surfaces:
//
//  1. HTTP — /v1/pm/schedules (GET list, POST create) and
//     /v1/pm/schedules/{id} (GET). POST mirrors /v1/tickets
//     create: ActionControlWrite + handler re-check on the
//     request body's asset_path. PUT/DELETE are out of scope
//     (delete endpoint is explicitly out per §4).
//  2. Scanner — RunPMScanner mirrors RunSLAScanner (startup +
//     ticker + ctx.Done + fail-soft). On each tick it walks
//     every enabled schedule, opens a ticket when now >=
//     NextDue, and advances NextDue by IntervalDays.
//
// Idempotency: after a successful open the scanner sets
// LastRun=now and NextDue=now+IntervalDays BEFORE the next tick
// can observe the same schedule. The (LastRun, NextDue) update
// is the in-memory gate; a second tick in the same interval
// sees NextDue > now and skips.
//
// Scope: M2 ships calendar triggers only. Meter (runhours) is
// stubbed (no impl, no flag) per spec-008 v0.3 Q12 — see §8 in
// the prompt for the deferred work.
package core

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// pmScheduleIDPattern mirrors ticketIDPattern: "pm_" + 16
// uppercase base32 chars. Defends the URL id segment at the
// trust boundary (PRMT-043 §4).
var pmScheduleIDPattern = regexp.MustCompile(`^pm_[A-Z2-7]{16}$`)

// newPMScheduleID produces a "pm_" + 16 base32 id. Same shape as
// newTicketID; uses a separate prefix so the two namespaces don't
// collide and the URL id-pattern is unambiguous.
func newPMScheduleID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "pm_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// listPMSchedulesResponse is the envelope shape (mirror of
// listTicketsResponse).
type listPMSchedulesResponse struct {
	Items []PMSchedule `json:"items"`
}

// servePMSchedules handles /v1/pm/schedules. Method dispatch:
//
//	GET → list (per-item scope filter)
//	POST → create (operator+, handler re-checks asset_path)
//
// All other methods → 405.
func (s *Server) servePMSchedules(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.servePMSchedulesList(w, r, rid)
	case http.MethodPost:
		s.servePMSchedulesCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// servePMSchedulesList mirrors serveTicketsList: list every
// schedule, per-item scope filter, JSON envelope. No pagination
// (M2 PM scale: small — operator-set).
func (s *Server) servePMSchedulesList(w http.ResponseWriter, r *http.Request, rid string) {
	all, err := s.st.ListPMSchedules(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]PMSchedule, 0, len(all))
	for _, p := range all {
		if hasAuth && authorize(principal, ActionRead, p.AssetPath) != nil {
			continue
		}
		items = append(items, p)
	}
	writeJSON(w, http.StatusOK, listPMSchedulesResponse{Items: items})
}

// createPMScheduleRequest is the POST body. AssetPath / Title /
// Severity are required; Kind defaults to "calendar" (only
// allowed value in M2); IntervalDays must be > 0; Enabled
// defaults to true; NextDue is computed server-side at now +
// IntervalDays so two clients creating the same plan do not
// race on the initial schedule time.
type createPMScheduleRequest struct {
	AssetPath    string `json:"asset_path"`
	Kind         string `json:"kind"`
	IntervalDays int    `json:"interval_days"`
	Title        string `json:"title"`
	Severity     string `json:"severity"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

// servePMSchedulesCreate implements POST /v1/pm/schedules.
// role ≥ operator (authmw already gates the role floor). The
// handler re-checks ActionControlWrite against the request
// body's asset_path — same shape as POST /v1/tickets.
func (s *Server) servePMSchedulesCreate(w http.ResponseWriter, r *http.Request, rid string) {
	var req createPMScheduleRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.AssetPath == "" || req.Title == "" || req.Severity == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"asset_path, title, severity required", "", r.URL.Path, rid)
		return
	}
	if req.Kind == "" {
		req.Kind = "calendar"
	}
	if req.Kind != "calendar" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"only kind=calendar is supported in M2", req.Kind, r.URL.Path, rid)
		return
	}
	if req.IntervalDays <= 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"interval_days must be > 0", "", r.URL.Path, rid)
		return
	}
	if _, ok := allowedTicketSeverities[req.Severity]; !ok {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"unknown severity", req.Severity, r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionControlWrite, req.AssetPath) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", req.AssetPath, r.URL.Path, rid)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := time.Now().UTC()
	sched := PMSchedule{
		ID:           newPMScheduleID(),
		AssetPath:    req.AssetPath,
		Kind:         req.Kind,
		IntervalDays: req.IntervalDays,
		NextDue:      now.AddDate(0, 0, req.IntervalDays),
		Title:        req.Title,
		Severity:     req.Severity,
		Enabled:      enabled,
	}
	if err := s.st.PutPMSchedule(r.Context(), sched); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusCreated, sched)
}

// servePMSchedule handles /v1/pm/schedules/{id}. GET only. The id
// is validated against pmScheduleIDPattern before lookup so a
// bogus id returns 400 (not 404) and the auth audit line gets a
// real path.
func (s *Server) servePMSchedule(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	id := r.URL.Path
	// /v1/pm/schedules/{id} is registered under "/v1/pm/schedules/"
	// in ServeMux; strip the prefix to get the id.
	const prefix = "/v1/pm/schedules/"
	id = trimPrefix(id, prefix)
	if !pmScheduleIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad pm id", id, r.URL.Path, rid)
		return
	}
	sched, ok, err := s.st.GetPMSchedule(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "not-found",
			"pm schedule not found", id, r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionRead, sched.AssetPath) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", sched.AssetPath, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

// trimPrefix is a tiny helper that mirrors strings.TrimPrefix
// but lives in this file so we don't grow the authmw dependency
// surface. (Existing code uses strings.TrimPrefix; this is the
// same shape but localised.)
func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// RunPMScanner is the long-lived PM background goroutine. Mirrors
// RunSLAScanner's contract: interval≤0 → 60m default, startup
// run, ticker, ctx.Done exit, fail-soft on per-schedule errors.
func (s *Server) RunPMScanner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// safeTick (PRMT-076) so a panic in scanPMTick can't kill
	// the long-lived goroutine.
	safeTick("pm", func() { s.scanPMTick(ctx, time.Now().UTC()) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("pm", func() { s.scanPMTick(ctx, now.UTC()) })
		}
	}
}

// scanPMTick is one iteration: list every enabled schedule whose
// NextDue has arrived, open a ticket per schedule, advance
// NextDue. Pulled out so tests can drive a single tick
// deterministically.
func (s *Server) scanPMTick(ctx context.Context, now time.Time) {
	// PRMT-066: record the tick outcome for /v1/health/scanners.
	// Captured by the deferred closure so any return path
	// (lock failure, leader skip, list error, per-schedule
	// error) produces a registry entry.
	var tickErr error
	defer func() {
		s.recordScanner("pm", now, tickErr)
	}()
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the PM tick for this
	// tick window. The pg advisory lock is session-scoped and
	// released when the tick ends (release is deferred). On
	// error we log + skip (fail-soft, next tick will retry); on
	// !acquired we silently skip — another instance leads.
	ok, release, err := s.st.TryScannerLock(ctx, "pm")
	if err != nil {
		log.Printf("core: pm scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()
	all, err := s.st.ListPMSchedules(ctx)
	if err != nil {
		log.Printf("core: pm scanner: list: %v", err)
		tickErr = err
		return
	}
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		if now.Before(p.NextDue) {
			continue
		}
		s.firePMSchedule(ctx, p, now)
	}
}

// firePMSchedule opens one PM ticket for schedule p and advances
// NextDue. Best-effort: ticket creation failure is logged, but
// NextDue is NOT advanced (otherwise a persistently-broken ticket
// path would silently stop firing forever).
func (s *Server) firePMSchedule(ctx context.Context, p PMSchedule, now time.Time) {
	t := Ticket{
		ID:        newTicketID(),
		AlarmID:   "", // PM tickets are not alarm-driven
		AssetPath: p.AssetPath,
		Title:     p.Title,
		Severity:  p.Severity,
		State:     "open",
		OpenedAt:  now,
	}
	if _, err := s.st.PutTicket(ctx, t, 0); err != nil {
		log.Printf("core: pm scanner: put ticket for %s: %v", p.ID, err)
		return
	}
	// Advance the schedule. LastRun + NextDue are the in-memory
	// idempotency gate for the next tick.
	nowCopy := now
	p.LastRun = &nowCopy
	p.NextDue = now.AddDate(0, 0, p.IntervalDays)
	if err := s.st.PutPMSchedule(ctx, p); err != nil {
		log.Printf("core: pm scanner: advance %s: %v", p.ID, err)
		// Best-effort: the ticket is open; the schedule may fire
		// again next tick. That's a known minor inconsistency
		// (logged here for §8 review) — not a data-loss bug.
	}
	s.emitTicketEventAsync(t, ticketEventTypeOpened)
}
