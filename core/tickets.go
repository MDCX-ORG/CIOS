// Package core — tickets.go: /v1/tickets HTTP surface for M2 E2.3.
//
// Three routes:
//
//	GET    /v1/tickets                  → list (page + per-item scope filter)
//	POST   /v1/tickets                  → create (operator+)
//	GET    /v1/tickets/{id}             → read one (viewer+, scope on AssetPath)
//	POST   /v1/tickets/{id}:transition  → state-machine transition (operator+)
//	POST   /v1/tickets/{id}:note        → append note (operator+; PRMT-060)
//	POST   /v1/tickets/{id}:assign      → update assignee (operator+; PRMT-060)
//
// State machine (spec-008 draft, PRMT-033 §4.1):
//
//	open          → acknowledged | closed
//	acknowledged  → resolved     | closed
//	resolved      → closed
//	(any other) → 422 RFC 7807
//
// Timestamps are written on each transition (AckedAt/ResolvedAt/ClosedAt).
// All writes go through Store.PutTicket (idempotent upsert by ID). State
// transitions and assignee updates are read-modify-write with optimistic
// locking (PRMT-082): the handler reads the ticket (with its
// ResourceVersion), mutates locally, and writes back via
// PutTicket(t, t.ResourceVersion). A concurrent writer loses with
// ErrVersionConflict, mapped to 409 RFC 7807 (problem type
// "version-conflict"). The auto-opener / scanner paths still use
// PutTicket(t, 0) (create-or-force-overwrite) per their dedup semantics.
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. Enums + ID pattern         — allowed states, severities, ticketIDPattern
//   2. Request/response types     — list/create/transition/note/assign
//   3. Collection handlers        — serveTickets (LIST + CREATE)
//   4. Item handler               — serveTicket (GET, dispatch)
//   5. State machine              — serveTicketTransition (PRMT-033 §4.1)
//   6. Notes (PRMT-060)           — serveTicketNote
//   7. Assignee (PRMT-060)        — serveTicketAssign
//   8. Audit trail (PRMT-061)     — appendTicketAudit + history handler
// ─────────────────────────────────────────────────────────────────────────────

// allowedTicketStates mirrors the state machine. PRMT-033 §4.1 —
// spec-008 (draft) is the authoritative source.
var allowedTicketStates = map[string]struct{}{
	"open":         {},
	"acknowledged": {},
	"resolved":     {},
	"closed":       {},
}

// allowedTicketSeverities mirrors Alarm severity enum (spec-003 §2).
// PRMT-033 §4.2 reuses the same set; the prompt says "mirror Alarm".
var allowedTicketSeverities = map[string]struct{}{
	"critical": {},
	"major":    {},
	"minor":    {},
	"info":     {},
}

// ticketIDPattern matches the output shape of newTicketID(): "tk_"
// + 16 uppercase base32 chars (RFC 4648, no padding). 10 random
// bytes → 16 chars in the base32 alphabet [A-Z2-7]. PRMT-033 §5.1
// R2-1 — defends the URL id segment at the trust boundary.
var ticketIDPattern = regexp.MustCompile(`^tk_[A-Z2-7]{16}$`)

// allowedTransition reports whether from→to is legal per PRMT-033 §4.1.
// closed is reachable from any state; otherwise only one-step forward.
func allowedTransition(from, to string) bool {
	switch from {
	case "open":
		return to == "acknowledged" || to == "closed"
	case "acknowledged":
		return to == "resolved" || to == "closed"
	case "resolved":
		return to == "closed"
	}
	return false
}

// listTicketsResponse is the envelope shape (mirrors alarms).
type listTicketsResponse struct {
	Items         []Ticket `json:"items"`
	NextPageToken string   `json:"next_page_token"`
}

// ticketCreateRequest is the JSON body for POST /v1/tickets.
// All fields except Assignee are required; the server fills ID/State/
// OpenedAt.
type ticketCreateRequest struct {
	AssetPath string `json:"asset_path"`
	Title     string `json:"title"`
	Severity  string `json:"severity"`
	Assignee  string `json:"assignee"`
	AlarmID   string `json:"alarm_id"`
}

// ticketTransitionRequest is the JSON body for POST /v1/tickets/{id}:transition.
type ticketTransitionRequest struct {
	To string `json:"to"`
}

// ticketNoteRequest is the JSON body for POST /v1/tickets/{id}:note.
// Body is the operator's text (≤ 8 KiB; PRMT-060 §2).
type ticketNoteRequest struct {
	Body string `json:"body"`
}

// ticketAssignRequest is the JSON body for POST /v1/tickets/{id}:assign.
// Empty string is allowed and means "unassign" (PRMT-060 §3).
type ticketAssignRequest struct {
	Assignee string `json:"assignee"`
}

// serveTickets handles /v1/tickets. Method dispatch:
//
//	GET  → list (page + scope filter)
//	POST → create
//	anything else → 405
func (s *Server) serveTickets(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveTicketsList(w, r, rid)
	case http.MethodPost:
		s.serveTicketsCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveTicketsList implements GET /v1/tickets. Mirrors serveAlarms
// (page_size/page_token, per-item scope filter on AssetPath, severity
// + state whitelists). role ≥ viewer (gated by middleware role floor
// for list endpoints via isListScopeEndpoint).
func (s *Server) serveTicketsList(w http.ResponseWriter, r *http.Request, rid string) {
	q := r.URL.Query()
	if v := q.Get("severity"); v != "" {
		if _, ok := allowedTicketSeverities[v]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad severity", v, r.URL.Path, rid)
			return
		}
	}
	if v := q.Get("state"); v != "" {
		if _, ok := allowedTicketStates[v]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad state", v, r.URL.Path, rid)
			return
		}
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
	pageSize := 100
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_size", ps, r.URL.Path, rid)
			return
		}
		if n > 1000 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"page_size > 1000", ps, r.URL.Path, rid)
			return
		}
		pageSize = n
	}
	var afterID string
	if pt := q.Get("page_token"); pt != "" {
		t, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterID = t
	}
	all, err := s.st.ListTickets(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	wantSeverity := q.Get("severity")
	wantState := q.Get("state")
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]Ticket, 0, len(all))
	for _, t := range all {
		if wantSeverity != "" && t.Severity != wantSeverity {
			continue
		}
		if wantState != "" && t.State != wantState {
			continue
		}
		if !glob.Match(t.AssetPath) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, t.AssetPath) != nil {
			continue
		}
		items = append(items, t)
	}
	// Page-token is a 0-based index into the deterministic
	// OpenedAt-desc order, same trick as serveAlarms.
	var startIdx int
	if afterID != "" {
		idx, err := strconv.Atoi(afterID)
		if err != nil || idx < 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token (not an int)", afterID, r.URL.Path, rid)
			return
		}
		if idx > len(items) {
			idx = len(items)
		}
		startIdx = idx
	}
	items = items[startIdx:]
	var next string
	if len(items) > pageSize {
		next = encodePageToken(strconv.Itoa(startIdx + pageSize))
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, listTicketsResponse{Items: items, NextPageToken: next})
}

// serveTicketsCreate implements POST /v1/tickets. role ≥ operator
// (middleware applies only the role floor; this handler re-runs
// the full authorize against the body-parsed asset_path). Body
// fields validated; server fills ID/State/OpenedAt.
func (s *Server) serveTicketsCreate(w http.ResponseWriter, r *http.Request, rid string) {
	var req ticketCreateRequest
	// PRMT-033 §5.1 R2-3: cap body size (mirror assets.go:71) and
	// reject unknown fields so spec drift is surfaced loudly.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.AssetPath == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing asset_path", "", r.URL.Path, rid)
		return
	}
	// PRMT-033 §5.1 R2-2: validate the body asset_path through the
	// cpath dictionary (mirror core/assets.go:97). Bad path → 400
	// bad-path; the body is also the scope target for the role-floor
	// middleware, so an unparsed path would otherwise leak into
	// authorize() and the stored ticket.
	if _, err := s.d.ParseAssetPath(req.AssetPath); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	if req.Title == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing title", "", r.URL.Path, rid)
		return
	}
	if _, ok := allowedTicketSeverities[req.Severity]; !ok {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad severity", req.Severity, r.URL.Path, rid)
		return
	}
	// Per-asset scope re-check (middleware only enforced role floor
	// because the asset_path lives in the body, not the URL).
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionControlWrite, req.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	now := time.Now().UTC()
	t := Ticket{
		ID:        newTicketID(),
		AlarmID:   req.AlarmID,
		AssetPath: req.AssetPath,
		Title:     req.Title,
		Severity:  req.Severity,
		State:     "open",
		Assignee:  req.Assignee,
		OpenedAt:  now,
	}
	if _, err := s.st.PutTicket(r.Context(), t, 0); err != nil {
		// PRMT-231: a racing/duplicate open ticket for the same
		// non-empty alarm_id trips tickets_alarm_id_active_uniq
		// (migration 011); surface it as 409 conflict, not 500.
		if errors.Is(err, ErrDuplicateActiveTicket) {
			writeProblem(w, http.StatusConflict, "conflict",
				"duplicate active ticket",
				"an open ticket already exists for alarm_id "+req.AlarmID,
				r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// PRMT-061: append "created" audit entry. Best-effort — a
	// failure is logged but does not change the response.
	s.appendTicketAudit(r.Context(), t.ID, "created", "", t.State)
	// M4 F3: async notify (webhook + SMTP) so an unreachable mail host
	// cannot stall the ticket HTTP response (smtp.SendMail has no dial deadline).
	s.emitTicketEventAsync(t, ticketEventTypeOpened)
	writeJSON(w, http.StatusCreated, t)
}

// serveTicket handles /v1/tickets/{id} and the /{id}:{action}
// sub-resources (transition / note / assign).
// Method dispatch:
//
//	GET    /{id}              → read (viewer+, inlines notes)
//	POST   /{id}:transition   → state-machine transition
//	POST   /{id}:note         → append note (operator+; PRMT-060)
//	POST   /{id}:assign       → update assignee (operator+; PRMT-060)
//	anything else → 405
func (s *Server) serveTicket(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	// URL path is /v1/tickets/{id} or /v1/tickets/{id}:{suffix} —
	// strip the prefix to get {id}[ :suffix ].
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tickets/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	id, suffix := rest, ""
	switch {
	case strings.HasSuffix(rest, ":transition"):
		id = strings.TrimSuffix(rest, ":transition")
		suffix = "transition"
	case strings.HasSuffix(rest, ":note"):
		id = strings.TrimSuffix(rest, ":note")
		suffix = "note"
	case strings.HasSuffix(rest, ":assign"):
		id = strings.TrimSuffix(rest, ":assign")
		suffix = "assign"
	case strings.HasSuffix(rest, ":history"):
		id = strings.TrimSuffix(rest, ":history")
		suffix = "history"
	}
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	// PRMT-033 §5.1 R2-1: validate id shape before handing to the
	// store. A bad id (e.g. "tk_..", "tk_abc%00def") would otherwise
	// reach s.st.GetTicket and surface as a generic 404; this gates
	// the format mismatch at 400.
	if !ticketIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad ticket id", id, r.URL.Path, rid)
		return
	}
	switch {
	case suffix == "" && r.Method == http.MethodGet:
		s.serveTicketGet(w, r, rid, id)
	case suffix == "transition" && r.Method == http.MethodPost:
		s.serveTicketTransition(w, r, rid, id)
	case suffix == "note" && r.Method == http.MethodPost:
		s.serveTicketNote(w, r, rid, id)
	case suffix == "assign" && r.Method == http.MethodPost:
		s.serveTicketAssign(w, r, rid, id)
	case suffix == "history" && r.Method == http.MethodGet:
		s.serveTicketHistory(w, r, rid, id)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveTicketGet implements GET /v1/tickets/{id}. role ≥ viewer;
// per-item scope check on the ticket's AssetPath. 404 RFC 7807 on miss.
// The response inlines `notes:[]` (At ASC, oldest first) per
// PRMT-060 §3 — the notes timeline is part of the read shape, not
// a separate endpoint.
func (s *Server) serveTicketGet(w http.ResponseWriter, r *http.Request, rid, id string) {
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return
	}
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionRead, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	notes, err := s.st.ListTicketNotes(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if notes == nil {
		notes = []TicketNote{}
	}
	// Embed via a struct so the JSON shape is the canonical Ticket
	// fields PLUS a `notes` array (PRMT-060 §3). We use a
	// distinct response type rather than the Ticket struct
	// because adding `Notes` to the canonical Ticket would
	// leak the field into POST /v1/tickets response and the
	// list-scope response, which the spec doesn't ask for.
	type ticketWithNotes struct {
		Ticket
		Notes []TicketNote `json:"notes"`
	}
	writeJSON(w, http.StatusOK, ticketWithNotes{Ticket: t, Notes: notes})
}

// serveTicketTransition implements POST /v1/tickets/{id}:transition.
// role ≥ operator; body {"to": "<state>"}; read-modify-write;
// legal transitions write the corresponding timestamp; illegal → 422.
func (s *Server) serveTicketTransition(w http.ResponseWriter, r *http.Request, rid, id string) {
	var req ticketTransitionRequest
	// PRMT-033 §5.1 R2-3: cap body size + reject unknown fields
	// (mirror serveTicketsCreate).
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if _, ok := allowedTicketStates[req.To]; !ok {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
			"invalid transition target", req.To, r.URL.Path, rid)
		return
	}
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return
	}
	if !allowedTransition(t.State, req.To) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
			"illegal transition", t.State+"->"+req.To, r.URL.Path, rid)
		return
	}
	// Per-asset scope re-check (middleware only enforced role floor
	// because the scope target lives on the stored ticket, not in
	// the URL — the {id} is a ticket ID, not an asset path).
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionControlWrite, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	// Write the timestamp corresponding to the target state.
	// ClosedAt is written by every →closed transition; AckedAt
	// and ResolvedAt are written only on the canonical "first
	// arrival" at that state (so re-closing does not overwrite
	// the original ack time, etc.).
	now := time.Now().UTC()
	switch req.To {
	case "acknowledged":
		if t.AckedAt == nil {
			t.AckedAt = &now
		}
	case "resolved":
		if t.ResolvedAt == nil {
			t.ResolvedAt = &now
		}
	case "closed":
		if t.ClosedAt == nil {
			t.ClosedAt = &now
		}
	}
	// PRMT-061: snapshot pre-transition state for the audit row.
	fromState := t.State
	t.State = req.To
	// PRMT-082 R2: write back with the version we observed so a
	// concurrent transition loses with ErrVersionConflict (mirrors
	// PutAsset; PRMT-016b). The store's CAS is otherwise dead at
	// the API surface — see PRMT-082 §9-quater R1 review.
	out, err := s.st.PutTicket(r.Context(), t, t.ResourceVersion)
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			writeProblem(w, http.StatusConflict, "version-conflict",
				"ticket was updated concurrently; refetch and retry",
				err.Error(), r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	s.appendTicketAudit(r.Context(), id, "transitioned", fromState, out.State)
	// M4 F3: async notify — same contract as create (never block response).
	s.emitTicketEventAsync(out, ticketEventTypeTransitioned)
	writeJSON(w, http.StatusOK, out)
}

// serveTicketNote implements POST /v1/tickets/{id}:note. role ≥
// operator (authmw enforces the role floor; handler re-runs the
// per-asset scope check on the stored ticket). body must be ≤ 8 KiB
// (PRMT-060 §2). The note is append-only; 404 if the ticket does
// not exist.
func (s *Server) serveTicketNote(w http.ResponseWriter, r *http.Request, rid, id string) {
	// PRMT-060 §5 (body size cap mirrors tickets.go:75) — 8 KiB
	// is a hard server-side limit on note body length; a request
	// larger than 1<<13 + envelope is rejected at 400 before we
	// touch the store. We also reject unknown JSON fields so spec
	// drift is surfaced loudly.
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<13)+1024))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req ticketNoteRequest
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.Body == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing body", "", r.URL.Path, rid)
		return
	}
	if len(req.Body) > 1<<13 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"body too large", "", r.URL.Path, rid)
		return
	}
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return
	}
	// Per-asset scope re-check (middleware only enforced role
	// floor because the scope target lives on the stored ticket,
	// not in the URL — the {id} is a ticket ID, not an asset
	// path). Mirrors the same re-check in serveTicketTransition.
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionControlWrite, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	author := "anonymous"
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		author = principal.Subject
	}
	note := TicketNote{
		ID:       newTicketNoteID(),
		TicketID: id,
		Author:   author,
		Body:     req.Body,
		At:       time.Now().UTC(),
	}
	if err := s.st.AppendTicketNote(r.Context(), note); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

// serveTicketAssign implements POST /v1/tickets/{id}:assign. role ≥
// operator (authmw enforces the role floor; handler re-runs the
// per-asset scope check on the stored ticket). empty string means
// "unassign" (PRMT-060 §3). 404 if the ticket does not exist.
func (s *Server) serveTicketAssign(w http.ResponseWriter, r *http.Request, rid, id string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req ticketAssignRequest
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	// Look up the ticket first so the scope re-check has a real
	// asset_path; missing ticket → 404 before we touch the
	// assignee column.
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return
	}
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionControlWrite, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	// PRMT-082 R2: read-mutate-CAS — same shape as serveTicketTransition
	// so the version is bumped consistently with the state machine and
	// a racing assignee write loses with ErrVersionConflict. The
	// dedicated UpdateTicketAssignee SQL path is kept on the Store
	// interface for backward compatibility but is no longer called
	// from any handler (see PRMT-082 §9-quater R1 review).
	t.Assignee = req.Assignee
	out, err := s.st.PutTicket(r.Context(), t, t.ResourceVersion)
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			writeProblem(w, http.StatusConflict, "version-conflict",
				"ticket was updated concurrently; refetch and retry",
				err.Error(), r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// PRMT-061: append "assigned" audit entry. From/To state are
	// empty (assignee change is not a state-machine transition).
	s.appendTicketAudit(r.Context(), id, "assigned", "", "")
	writeJSON(w, http.StatusOK, out)
}

// newTicketID produces a "tk_" + 16 uppercase base32 chars from
// 10 random bytes. Prefix makes tickets easy to spot in logs and
// distinguishes them from any other UUID-flavoured identifier in
// the system. Mirrors newRequestID's entropy (10 bytes → ~80 bits).
// PRMT-033 §4.2 says "reuse UUIDv7 if available; else same shape
// as alarm's UUIDv7" — there is no pkg/id yet, so we mint locally.
func newTicketID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "tk_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// newTicketNoteID produces a "tn_" + 16 uppercase base32 chars
// from 10 random bytes (mirror newTicketID's shape, distinct
// prefix). PRMT-060 §2.
func newTicketNoteID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "tn_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// --- PRMT-061: ticket audit trail --------------------------------------

// appendTicketAudit writes one audit row. Best-effort by contract
// (PRMT-061 §1): a save failure is logged and the request response
// is NOT modified — the ticket write already succeeded and rolling
// back the ticket for an audit-write failure would be worse than a
// missing audit entry (mirrors core/assets.go appendAudit's pattern
// for asset_audit; PRMT-045 §Q12 / 061 §Q12 alignment).
func (s *Server) appendTicketAudit(ctx context.Context, ticketID, op, fromState, toState string) {
	principal := "anonymous"
	if p, ok := PrincipalFromContext(ctx); ok && p.Subject != "" {
		principal = p.Subject
	}
	entry := TicketAudit{
		ID:        newTicketAuditID(),
		TicketID:  ticketID,
		Op:        op,
		FromState: fromState,
		ToState:   toState,
		Who:       principal,
		At:        time.Now().UTC(),
	}
	if err := s.st.AppendTicketAudit(ctx, entry); err != nil {
		log.Printf("core: ticket audit append (%s/%s): %v", op, ticketID, err)
	}
}

// newTicketAuditID produces "ta_" + 16 base32 chars. Mirror of
// newTicketID / newAuditID so the audit namespace is
// distinguishable in logs.
func newTicketAuditID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "ta_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// listTicketAuditsResponse is the envelope shape for the
// per-ticket history endpoint. Matches listAssetAuditsResponse's
// "items" naming so callers have one shape to parse across audit
// surfaces.
type listTicketAuditsResponse struct {
	Items []TicketAudit `json:"items"`
}

// serveTicketHistory handles GET /v1/tickets/{id}:history. role
// ≥ viewer; per-item scope check on the stored ticket's
// AssetPath mirrors GET /v1/tickets/{id} behaviour. The
// middleware has already enforced the role floor for this
// list-scope endpoint (authmw.isListScopeEndpoint).
func (s *Server) serveTicketHistory(w http.ResponseWriter, r *http.Request, rid, id string) {
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return
	}
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		if err := authorize(principal, ActionRead, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden", "principal not authorized for this asset path",
				r.URL.Path, rid)
			return
		}
	}
	entries, err := s.st.ListTicketAudits(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, listTicketAuditsResponse{Items: entries})
}
