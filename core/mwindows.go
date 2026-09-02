// Package core — maintenance_windows.go: /v1/maintenance/windows HTTP
// surface for the explicit-window table added in PRMT-096
// (E2.4 P532 / T42). Three routes:
//
//	GET    /v1/maintenance/windows              → list (role ≥ viewer, per-item scope)
//	POST   /v1/maintenance/windows              → create (operator+)
//	DELETE /v1/maintenance/windows/{id}         → delete (operator+, ends the window)
//
// The handler is the write-side of the table; pkg/alarm is the
// read-side (via Store.ActiveWindowFor on every OpenTicket). The
// table is shared between cios-core and cios-alarm via the same PG
// DSN — no HTTP coupling.
//
// Per-item scope on GET is delegated to the list-scope handler
// pattern (mirror /v1/tickets, /v1/pm/schedules): the middleware
// enforces a role floor and authorize() runs against each item's
// AssetPath inside the handler.
package core

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maintenanceWindowIDPattern matches "mw_" + 16 uppercase base32
// chars. Same shape as ticketIDPattern / pmScheduleIDPattern; the
// prefix alone distinguishes the namespace.
var maintenanceWindowIDPattern = regexp.MustCompile(`^mw_[A-Z2-7]{16}$`)

// listMaintenanceWindowsResponse is the envelope for the list
// endpoint. Mirrors listTicketsResponse: a non-nil "items" slice
// (never null) so clients can iterate without a nil-guard, and a
// page_token for cursor pagination.
type listMaintenanceWindowsResponse struct {
	Items         []MaintenanceWindow `json:"items"`
	NextPageToken string              `json:"page_token"`
}

// maintenanceWindowCreateRequest is the POST body. ID is server-
// generated; StartsAt / EndsAt must satisfy EndsAt > StartsAt
// (the SQL CHECK enforces the same invariant).
type maintenanceWindowCreateRequest struct {
	AssetPath string `json:"asset_path"`
	StartsAt  string `json:"starts_at"`
	EndsAt    string `json:"ends_at"`
	Reason    string `json:"reason"`
}

// serveMaintenanceWindowsRoot routes /v1/maintenance/windows.
//
//	GET  → list (role ≥ viewer; per-item scope inside handler)
//	POST → create (operator+)
func (s *Server) serveMaintenanceWindowsRoot(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveMaintenanceWindowsList(w, r, rid)
	case http.MethodPost:
		s.serveMaintenanceWindowsCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveMaintenanceWindowsList implements GET /v1/maintenance/windows
// with page_size / page_token + per-item scope filter. Mirrors the
// /v1/tickets list (asset_path-scoped; admin bypasses; viewer/operator
// per scope).
//
// PRMT-096 R2 F2: the sort is (StartsAt asc, ID asc) and the cursor
// is a (sortKey, id) tuple so the cursor filter matches the sort key.
// Pre-R2 the cursor only encoded the id and the filter used
// `m.ID <= afterID`, which decoupled sort from filter and could
// skip or duplicate rows across pages (same bug class as
// core/tickets.go, deferred to its own PRMT per §8-arch R2 MUST NOT).
func (s *Server) serveMaintenanceWindowsList(w http.ResponseWriter, r *http.Request, rid string) {
	q := r.URL.Query()
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
	var cursorKey, cursorID string
	if pt := q.Get("page_token"); pt != "" {
		k, id, ok := decodePageTokenPair(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		cursorKey, cursorID = k, id
	}
	all, err := s.st.ListMaintenanceWindows(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// PRMT-022 R2 §4.1 style per-item scope: the middleware has
	// already enforced the role floor; this filter is the per-item
	// scope (admin bypass, viewer/operator per scope, L50
	// read-implies-subtree handled by authorize). The list-scope
	// delegation lives in core/authmw.go::isListScopeEndpoint
	// (the middleware applies role floor only on this URL).
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]MaintenanceWindow, 0, len(all))
	for _, m := range all {
		if cursorKey != "" && !maintenanceWindowAfterCursor(m, cursorKey, cursorID) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, m.AssetPath) != nil {
			continue
		}
		items = append(items, m)
	}
	var next string
	if len(items) > pageSize {
		last := items[pageSize-1]
		next = encodePageTokenPair(strconv.FormatInt(last.StartsAt.UnixNano(), 10), last.ID)
		items = items[:pageSize]
	}
	if items == nil {
		items = []MaintenanceWindow{}
	}
	writeJSON(w, http.StatusOK, listMaintenanceWindowsResponse{
		Items:         items,
		NextPageToken: next,
	})
}

// maintenanceWindowAfterCursor is the (StartsAt, ID) cursor filter
// used by serveMaintenanceWindowsList. Returns true iff `m` is
// strictly after the (cursorKey, cursorID) tuple under the
// (StartsAt asc, ID asc) lexicographic order. cursorKey is the
// last emitted row's StartsAt encoded as UnixNano (string). If the
// parse fails the row is treated as after (defensive: an
// unparseable cursor on an in-spec row means we made a mistake
// generating the token, not the caller's fault; surfacing that
// row is better than silently dropping it).
func maintenanceWindowAfterCursor(m MaintenanceWindow, cursorKey, cursorID string) bool {
	curNS, err := strconv.ParseInt(cursorKey, 10, 64)
	if err != nil {
		return true
	}
	thisNS := m.StartsAt.UnixNano()
	if thisNS != curNS {
		return thisNS > curNS
	}
	return m.ID > cursorID
}

// serveMaintenanceWindowsCreate implements POST /v1/maintenance/windows.
// Body validated; server fills ID. The handler also re-runs
// authorize(ActionControlWrite, asset_path) on the request body's
// asset_path — the middleware maps POST /v1/maintenance/windows to
// ActionControlWrite against "**" with handler re-check, mirroring
// /v1/tickets / /v1/pm/schedules / /v1/inspections. This stops a
// token with broad scope from writing windows outside its subtree
// (matches L50 "写显式": writes do NOT imply subtree).
func (s *Server) serveMaintenanceWindowsCreate(w http.ResponseWriter, r *http.Request, rid string) {
	var req maintenanceWindowCreateRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad json", err.Error(), r.URL.Path, rid)
			return
		}
	}
	if req.AssetPath == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing asset_path", "", r.URL.Path, rid)
		return
	}
	if _, err := s.d.ParseAssetPath(req.AssetPath); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset_path", err.Error(), r.URL.Path, rid)
		return
	}
	startsAt, err := parseMaintenanceWindowTime(req.StartsAt, "starts_at")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			err.Error(), req.StartsAt, r.URL.Path, rid)
		return
	}
	endsAt, err := parseMaintenanceWindowTime(req.EndsAt, "ends_at")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			err.Error(), req.EndsAt, r.URL.Path, rid)
		return
	}
	if !endsAt.After(startsAt) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"ends_at must be after starts_at", "", r.URL.Path, rid)
		return
	}
	// Per-item scope re-check on the body's asset_path: an operator
	// token whose scope is e.g. "site01.pod001.**" cannot open a
	// window on "site01.pod002.**". Mirrors /v1/tickets create path.
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionControlWrite, req.AssetPath) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", req.AssetPath, r.URL.Path, rid)
		return
	}
	mw := MaintenanceWindow{
		ID:        newMaintenanceWindowID(),
		AssetPath: req.AssetPath,
		StartsAt:  startsAt,
		EndsAt:    endsAt,
		Reason:    req.Reason,
	}
	if err := s.st.PutMaintenanceWindow(r.Context(), mw); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusCreated, mw)
}

// parseMaintenanceWindowTime accepts an RFC3339 string and returns
// the parsed UTC time. Bad input returns a non-nil error whose text
// the caller can drop into a 400 RFC 7807 detail.
func parseMaintenanceWindowTime(s, field string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("missing " + field + " (need RFC3339)")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("bad " + field + " (need RFC3339): " + err.Error())
	}
	return t.UTC(), nil
}

// newMaintenanceWindowID produces "mw_" + 16 uppercase base32
// chars from 10 random bytes. Mirror of newTicketID /
// newPMScheduleID; the prefix distinguishes the namespace in psql
// listings.
func newMaintenanceWindowID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "mw_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// ServeMaintenanceWindow handles /v1/maintenance/windows/{id} (DELETE
// only; GET-by-id is intentionally not exposed — windows are
// managed via the list/create/delete surface). Method dispatch:
//
//	DELETE /{id} → remove (operator+; handler re-runs authorize()
//	              against the stored window's asset_path because
//	              the {id} in the URL is a window id, not an asset
//	              path)
func (s *Server) ServeMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/maintenance/windows/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	if r.Method != http.MethodDelete {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	s.serveMaintenanceWindowDelete(w, r, rid, rest)
}

// serveMaintenanceWindowDelete implements DELETE
// /v1/maintenance/windows/{id}. The middleware maps DELETE to
// ActionControlWrite against the {id} segment (which is the
// window id, not an asset path), so the handler re-runs
// authorize() against the stored window's asset_path (mirror
// POST /v1/tickets/{id}:transition).
func (s *Server) serveMaintenanceWindowDelete(w http.ResponseWriter, r *http.Request, rid, id string) {
	if !maintenanceWindowIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad maintenance window id", id, r.URL.Path, rid)
		return
	}
	cur, ok, err := s.st.GetMaintenanceWindow(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"maintenance window not found", id, r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionControlWrite, cur.AssetPath) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", cur.AssetPath, r.URL.Path, rid)
		return
	}
	deleted, err := s.st.DeleteMaintenanceWindow(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !deleted {
		// Racing delete won; surface a 404 so the operator retries.
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"maintenance window not found", id, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}
