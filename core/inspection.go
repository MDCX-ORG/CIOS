// Package core — inspection.go: InspectionTemplate CRUD +
// background scanner (M2 E2.7 P561 / PRMT-049).
//
// Two surfaces:
//
//  1. HTTP — /v1/inspections (GET list, POST create) and
//     /v1/inspections/{id} (GET). POST mirrors /v1/tickets
//     create: ActionControlWrite + handler re-check on the
//     request body's asset_path. PUT/DELETE are out of scope
//     (mirrors PM).
//  2. Scanner — RunInspectionScanner mirrors RunPMScanner
//     (startup + ticker + ctx.Done + fail-soft). On each tick
//     it walks every enabled template, opens a ticket when
//     now >= NextDue, and advances NextDue by Interval
//     (advance-then-fire).
//
// Idempotency: after a successful open the scanner advances
// NextDue=now+Interval BEFORE the next tick can observe the
// same template. The advance is the in-memory gate; a second
// tick in the same interval sees NextDue > now and skips.
//
// Mobile web is a separate prompt; PRMT-049 ships the backend
// only (template + scanner + ticket open).
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

// inspectionIDPattern mirrors pmScheduleIDPattern: "ins_" + 16
// uppercase base32 chars. Defends the URL id segment at the
// trust boundary (PRMT-049 §4).
var inspectionIDPattern = regexp.MustCompile(`^ins_[A-Z2-7]{16}$`)

// newInspectionID produces a "ins_" + 16 base32 id. Same shape
// as newTicketID / newPMScheduleID; uses a separate prefix so
// the three namespaces don't collide and the URL id-pattern is
// unambiguous.
func newInspectionID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "ins_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// listInspectionsResponse is the envelope shape (mirror of
// listPMSchedulesResponse).
type listInspectionsResponse struct {
	Items []InspectionTemplate `json:"items"`
}

// serveInspections handles /v1/inspections. Method dispatch:
//
//	GET  → list (per-item scope filter)
//	POST → create (operator+, handler re-checks asset_path)
//
// All other methods → 405.
func (s *Server) serveInspections(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveInspectionsList(w, r, rid)
	case http.MethodPost:
		s.serveInspectionsCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveInspectionsList mirrors servePMSchedulesList: list every
// template, per-item scope filter, JSON envelope. No pagination
// (M2 inspection scale: small — operator-set).
func (s *Server) serveInspectionsList(w http.ResponseWriter, r *http.Request, rid string) {
	all, err := s.st.ListInspectionTemplates(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]InspectionTemplate, 0, len(all))
	for _, it := range all {
		if hasAuth && authorize(principal, ActionRead, it.AssetPath) != nil {
			continue
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, listInspectionsResponse{Items: items})
}

// createInspectionRequest is the POST body. AssetPath / Title
// are required; Interval must be > 0; Items is optional
// (defaults to empty); Enabled defaults to true; NextDue is
// computed server-side at now + Interval so two clients creating
// the same plan do not race on the initial schedule time.
type createInspectionRequest struct {
	AssetPath string        `json:"asset_path"`
	Title     string        `json:"title"`
	Items     []string      `json:"items,omitempty"`
	Interval  time.Duration `json:"interval"`
	Enabled   *bool         `json:"enabled,omitempty"`
}

// serveInspectionsCreate implements POST /v1/inspections. role ≥
// operator (authmw already gates the role floor). The handler
// re-checks ActionControlWrite against the request body's
// asset_path — same shape as POST /v1/tickets / /v1/pm/schedules.
func (s *Server) serveInspectionsCreate(w http.ResponseWriter, r *http.Request, rid string) {
	var req createInspectionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.AssetPath == "" || req.Title == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"asset_path, title required", "", r.URL.Path, rid)
		return
	}
	// PRMT-049 §2 calls for a duration. The handler accepts any
	// non-zero, non-negative duration; the schema CHECK on
	// interval_ns > 0 mirrors this.
	if req.Interval <= 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"interval must be > 0", "", r.URL.Path, rid)
		return
	}
	if req.Items == nil {
		req.Items = []string{}
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
	tpl := InspectionTemplate{
		ID:        newInspectionID(),
		AssetPath: req.AssetPath,
		Title:     req.Title,
		Items:     req.Items,
		Interval:  req.Interval,
		NextDue:   now.Add(req.Interval),
		Enabled:   enabled,
	}
	if err := s.st.PutInspectionTemplate(r.Context(), tpl); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusCreated, tpl)
}

// serveInspection handles /v1/inspections/{id}. GET only. The id
// is validated against inspectionIDPattern before lookup so a
// bogus id returns 400 (not 404) and the auth audit line gets a
// real path.
func (s *Server) serveInspection(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	id := r.URL.Path
	// /v1/inspections/{id} is registered under "/v1/inspections/"
	// in ServeMux; strip the prefix to get the id.
	const prefix = "/v1/inspections/"
	id = trimPrefix(id, prefix)
	if !inspectionIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad inspection id", id, r.URL.Path, rid)
		return
	}
	tpl, ok, err := s.st.GetInspectionTemplate(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "not-found",
			"inspection template not found", id, r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionRead, tpl.AssetPath) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", tpl.AssetPath, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, tpl)
}

// RunInspectionScanner is the long-lived inspection background
// goroutine. Mirrors RunPMScanner's contract: interval≤0 → 60m
// default, startup run, ticker, ctx.Done exit, fail-soft on
// per-template errors.
func (s *Server) RunInspectionScanner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// safeTick (PRMT-076) so a panic in scanInspectionTick can't
	// kill the long-lived goroutine.
	safeTick("inspection", func() { s.scanInspectionTick(ctx, time.Now().UTC()) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("inspection", func() { s.scanInspectionTick(ctx, now.UTC()) })
		}
	}
}

// scanInspectionTick is one iteration: list every enabled
// template whose NextDue has arrived, open a ticket per
// template, advance NextDue. Pulled out so tests can drive a
// single tick deterministically.
func (s *Server) scanInspectionTick(ctx context.Context, now time.Time) {
	// PRMT-066: record the tick outcome for /v1/health/scanners.
	// Captured by the deferred closure so any return path
	// (lock failure, leader skip, list error, per-template
	// error) produces a registry entry.
	var tickErr error
	defer func() {
		s.recordScanner("inspection", now, tickErr)
	}()
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the inspection tick
	// for this tick window. The pg advisory lock is session-
	// scoped and released when the tick ends (release is
	// deferred). On error we log + skip (fail-soft, next tick
	// will retry); on !acquired we silently skip — another
	// instance leads.
	ok, release, err := s.st.TryScannerLock(ctx, "inspection")
	if err != nil {
		log.Printf("core: inspection scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()
	all, err := s.st.ListInspectionTemplates(ctx)
	if err != nil {
		log.Printf("core: inspection scanner: list: %v", err)
		tickErr = err
		return
	}
	for _, it := range all {
		if !it.Enabled {
			continue
		}
		if now.Before(it.NextDue) {
			continue
		}
		s.fireInspection(ctx, it, now)
	}
}

// fireInspection opens one inspection ticket for template it
// and advances NextDue. Best-effort: ticket creation failure is
// logged, but NextDue is NOT advanced (otherwise a
// persistently-broken ticket path would silently stop firing
// forever).
//
// items are serialised into the ticket Runbook field (a
// newline-joined, prefix-tagged string). Per PRMT-049 §4
// MUST NOT, no ticket schema change — the existing Runbook
// field is the carrier.
func (s *Server) fireInspection(ctx context.Context, it InspectionTemplate, now time.Time) {
	t := Ticket{
		ID:        newTicketID(),
		AlarmID:   "", // Inspection tickets are not alarm-driven
		AssetPath: it.AssetPath,
		Title:     it.Title,
		Severity:  "info", // PRMT-049 §4: info by default
		State:     "open",
		OpenedAt:  now,
		Runbook:   encodeInspectionRunbook(it.Items),
	}
	if _, err := s.st.PutTicket(ctx, t, 0); err != nil {
		log.Printf("core: inspection scanner: put ticket for %s: %v", it.ID, err)
		return
	}
	// Advance the template. NextDue is the in-memory idempotency
	// gate for the next tick.
	it.NextDue = now.Add(it.Interval)
	if err := s.st.PutInspectionTemplate(ctx, it); err != nil {
		log.Printf("core: inspection scanner: advance %s: %v", it.ID, err)
		// Best-effort: the ticket is open; the template may fire
		// again next tick. Same minor inconsistency as PM
		// (logged for §8 review) — not a data-loss bug.
	}
	s.emitTicketEventAsync(t, ticketEventTypeOpened)
}

// encodeInspectionRunbook turns a checklist into the existing
// ticket Runbook field. Format: "inspection:item1\nitem2\n...".
// The "inspection:" prefix lets the future runbook reader know
// this is an auto-generated checklist (vs. a human-named
// runbook key like "rb/cdu-deltat-low"). An empty checklist
// yields an empty string.
func encodeInspectionRunbook(items []string) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("inspection:")
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(it)
	}
	return b.String()
}
