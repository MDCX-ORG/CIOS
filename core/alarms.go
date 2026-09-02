// Package core — alarms.go: GET /v1/alarms. M0 has no rule engine,
// so this is a read-only listing of whatever SeedAlarms put into
// the store. Filters: severity (enum), state (enum), filter (cpath
// glob on Path). Pagination: page_size + page_token.
package core

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/yurimeng/cios/pkg/alarm"
	"github.com/yurimeng/cios/pkg/cpath"
)

// allowedStates is the whitelist for the `state` filter parameter
// (spec-003 §4). It has no second source of truth, so it stays
// local; the severity set moved to pkg/alarm.AllowedSeverities in
// PRMT-030 §A.
var allowedStates = map[string]struct{}{
	"firing":   {},
	"acked":    {},
	"resolved": {},
}

// listAlarmsResponse is the envelope shape (same as assets).
type listAlarmsResponse struct {
	Items         []Alarm `json:"items"`
	NextPageToken string  `json:"next_page_token"`
}

// serveAlarms handles GET /v1/alarms. Other methods → 405.
func (s *Server) serveAlarms(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	q := r.URL.Query()
	// severity
	if v := q.Get("severity"); v != "" {
		if _, ok := alarm.AllowedSeverities[v]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad severity", v, r.URL.Path, rid)
			return
		}
	}
	// state
	if v := q.Get("state"); v != "" {
		if _, ok := allowedStates[v]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad state", v, r.URL.Path, rid)
			return
		}
	}
	// filter (cpath glob) — default "**" matches everything.
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
	// page_size
	pageSize := DefaultPageSize
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_size", ps, r.URL.Path, rid)
			return
		}
		if n > MaxPageSize {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"page_size > 1000", ps, r.URL.Path, rid)
			return
		}
		pageSize = n
	}
	// page_token
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
	// Filter + sort.
	all, err := s.st.ListAlarms(r.Context())
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError, "bad-request",
			"store error", err)
		return
	}
	wantSeverity := q.Get("severity")
	wantState := q.Get("state")
	// PRMT-022 R2 §4.2: per-item scope filter on Alarm.Path, same
	// semantics as listAssets. Sits AFTER severity/state/glob
	// filters and BEFORE the index-based page slice, so
	// next_page_token is computed on the post-filter set (admin
	// bypasses via authorize). Auth disabled (hasAuth==false) →
	// no scope filter, M0 behaviour preserved.
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]Alarm, 0, len(all))
	for _, a := range all {
		if wantSeverity != "" && a.Severity != wantSeverity {
			continue
		}
		if wantState != "" && a.State != wantState {
			continue
		}
		if !glob.Match(a.Path) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, a.Path) != nil {
			continue
		}
		items = append(items, a)
	}
	// M0 pagination: page_token is a 0-based index into the
	// already-sorted list. We never decode it as an ID — only as
	// an integer offset, which is safe because the sort is
	// deterministic (severity rank → Since desc).
	var startIdx int
	if afterID != "" {
		idx, err := strconv.Atoi(afterID)
		if err != nil || idx < 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token (not an int)", afterID, r.URL.Path, rid)
			return
		}
		if idx > len(items) {
			idx = len(items) // empty page
		}
		startIdx = idx
	}
	items = items[startIdx:]
	var next string
	if len(items) > pageSize {
		next = encodePageToken(strconv.Itoa(startIdx + pageSize))
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, listAlarmsResponse{Items: items, NextPageToken: next})
}

// serveAlarmAck handles POST /v1/alarms/{id}:ack (PRMT-230).
// State machine: firing→acked one-way (spec-003 §4); acked/resolved
// re-ack → 409 `conflict` (registered slug, spec-004 §5 family).
// The {id} is an alarm id, NOT an asset path — the middleware only
// role-floors (isListScopeEndpoint); this handler re-runs
// authorize() against the stored alarm's Path (L50: explicit scope,
// same shape as serveTicketTransition).
func (s *Server) serveAlarmAck(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/alarms/")
	if !strings.HasSuffix(rest, ":ack") {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"alarm sub-resource not found", rest, r.URL.Path, rid)
		return
	}
	id := strings.TrimSuffix(rest, ":ack")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	// Actor = principal only (CI identity discipline — this handler
	// MUST NOT read any actor-override request header). With auth
	// configured the middleware
	// guarantees a principal; in lab no-auth builds
	// labNoAuthAdminPrincipal injects one. This branch is the
	// fail-closed guard for a production binary running with auth
	// disabled — it must 401, never fall back to a header.
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Subject == "" {
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"Unauthorized", "alarm ack requires an authenticated principal",
			r.URL.Path, rid)
		return
	}
	// Locate the alarm so the scope check runs against the real
	// asset path (L50: explicit scope, no implied subtree).
	// ponytail: O(n) scan — Store has no GetAlarm; add one when alarm cardinality hurts.
	alarms, err := s.st.ListAlarms(r.Context())
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError, "bad-request",
			"store error", err)
		return
	}
	var cur Alarm
	var hit bool
	for _, a := range alarms {
		if a.ID == id {
			cur, hit = a, true
			break
		}
	}
	if !hit {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"alarm not found", id, r.URL.Path, rid)
		return
	}
	if err := authorize(principal, ActionControlWrite, cur.Path); err != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"Forbidden", "principal not authorized for this asset path",
			r.URL.Path, rid)
		return
	}
	out, found, err := s.st.AckAlarm(r.Context(), id, principal.Subject)
	switch {
	case errors.Is(err, ErrAlarmNotAckable):
		writeProblem(w, http.StatusConflict, "conflict",
			"alarm already acknowledged or resolved", out.State, r.URL.Path, rid)
		return
	case err != nil:
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError, "bad-request",
			"store error", err)
		return
	case !found:
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"alarm not found", id, r.URL.Path, rid)
		return
	}
	log.Printf("core: alarm ack id=%s path=%s by=%s", out.ID, out.Path, principal.Subject)
	writeJSON(w, http.StatusOK, out)
}
