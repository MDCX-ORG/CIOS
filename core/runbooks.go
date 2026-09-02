// Package core — runbooks.go: KB seed endpoints (M2 E2.8 P571/P572
// / PRMT-044).
//
// Two surfaces:
//
//  1. GET /v1/runbooks/{key} — read-only static runbook content
//     from deploy/edge/runbooks/<key>.md. Path traversal is
//     blocked: the key is run through path.Clean and rejected
//     if it escapes the runbooks directory.
//
//  2. GET /v1/cases — closed ticket list (KB seed for M4 AI
//     training). Filtered by the caller's auth scope.
//
// runbookDir is a Server field so tests can swap a tempdir in
// without touching cmd/cios-core's wiring.
package core

import (
	"encoding/csv"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/alarm"
)

// runbookIDPattern is intentionally permissive: keys are
// free-form labels like "rb/cdu-deltat-low". We block the
// dangerous chars (., \, leading/trailing slash) at the trust
// boundary, then run path.Clean as a belt-and-braces defence.
// Letters/digits/dash/underscore, single slash between segments,
// anchored so an empty key or leading/trailing slash is rejected.
var runbookIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]+(/[A-Za-z0-9_\-]+)*$`)

// serveRunbook handles GET /v1/runbooks/{key}. Reads
// runbookDir + key + ".md" after sanitising the key. 404 on
// missing file; 400 on a malformed key. role ≥ viewer.
func (s *Server) serveRunbook(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	const prefix = "/v1/runbooks/"
	key := strings.TrimPrefix(r.URL.Path, prefix)
	if key == "" || key == r.URL.Path {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing key", "", r.URL.Path, rid)
		return
	}
	// Strip a trailing .md if the caller passed it (canonical
	// form is the key alone, but tolerate both).
	key = strings.TrimSuffix(key, ".md")
	// Belt-and-braces path-traversal defence: clean, then
	// reject any key that still contains "..", an absolute path,
	// or fails the regex.
	cleaned := path.Clean(key)
	if cleaned != key || !runbookIDPattern.MatchString(key) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad runbook key", key, r.URL.Path, rid)
		return
	}
	if s.runbookDir == "" {
		writeProblem(w, http.StatusNotFound, "not-found",
			"runbook dir not configured", key, r.URL.Path, rid)
		return
	}
	full := filepath.Join(s.runbookDir, key+".md")
	// Final filesystem-level defence: ensure the resolved path is
	// under runbookDir. filepath.Join + the regex should make
	// this unreachable; the check exists for defense in depth.
	absDir, _ := filepath.Abs(s.runbookDir)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absDir+string(filepath.Separator)) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad runbook key", key, r.URL.Path, rid)
		return
	}
	body, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeProblem(w, http.StatusNotFound, "not-found",
				"runbook not found", key, r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"read error", err.Error(), r.URL.Path, rid)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// listCasesResponse is the envelope shape for /v1/cases.
// Mirrors listTicketsResponse. The cases endpoint is read-only;
// M2 closes the KB seed loop, M4 adds search/AI.
type listCasesResponse struct {
	Items []Ticket `json:"items"`
}

// casesHardLimit is the ceiling enforced for ?limit on /v1/cases.
// ?limit > this is silently capped at the hard limit (PRMT-053 §2).
const casesHardLimit = 1000

// casesDefaultLimit is the default page size when ?limit is absent.
const casesDefaultLimit = 100

// serveCases handles GET /v1/cases. Returns every closed ticket
// in scope (viewer+, per-item scope filter like alarms). Empty
// store / no closed tickets → 200 with an empty items list.
//
// Query parameters (PRMT-053 §2, all optional, AND-combined):
//
//	severity     critical|major|minor|info (exact match, bad value → 400)
//	asset_prefix asset_path prefix match
//	since/until  RFC3339; filter by closed_at (parse error → 400)
//	limit        cap on returned items (default 100, hard cap 1000)
//	format       json (default) | csv (text/csv)
//
// Filter order: store → closed-only → field filters (severity,
// asset_prefix, since/until) → per-item scope filter → limit.
func (s *Server) serveCases(w http.ResponseWriter, r *http.Request) {
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
	severity := q.Get("severity")
	assetPrefix := q.Get("asset_prefix")

	var sincePtr, untilPtr *time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad since (need RFC3339)", v, r.URL.Path, rid)
			return
		}
		t = t.UTC()
		sincePtr = &t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad until (need RFC3339)", v, r.URL.Path, rid)
			return
		}
		t = t.UTC()
		untilPtr = &t
	}

	// limit (default 100; > 1000 silently capped at 1000)
	limit := casesDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad limit", v, r.URL.Path, rid)
			return
		}
		if n > casesHardLimit {
			n = casesHardLimit
		}
		limit = n
	}

	format := strings.ToLower(q.Get("format"))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad format (need json|csv)", q.Get("format"), r.URL.Path, rid)
		return
	}

	all, err := s.st.ListTickets(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]Ticket, 0, len(all))
	for _, t := range all {
		if t.State != "closed" {
			continue
		}
		if severity != "" && t.Severity != severity {
			continue
		}
		if assetPrefix != "" && !strings.HasPrefix(t.AssetPath, assetPrefix) {
			continue
		}
		if sincePtr != nil {
			if t.ClosedAt == nil || t.ClosedAt.Before(*sincePtr) {
				continue
			}
		}
		if untilPtr != nil {
			if t.ClosedAt == nil || t.ClosedAt.After(*untilPtr) {
				continue
			}
		}
		if hasAuth && authorize(principal, ActionRead, t.AssetPath) != nil {
			continue
		}
		items = append(items, t)
	}
	if len(items) > limit {
		items = items[:limit]
	}

	if format == "csv" {
		writeCasesCSV(w, items)
		return
	}
	writeJSON(w, http.StatusOK, listCasesResponse{Items: items})
}

// writeCasesCSV emits a text/csv body. The column order is fixed
// (PRMT-053 §4): id, severity, asset_path, title, opened_at,
// closed_at, runbook. RFC3339 for timestamps. Empty store → header
// row only (no data rows).
func writeCasesCSV(w http.ResponseWriter, items []Ticket) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "severity", "asset_path", "title",
		"opened_at", "closed_at", "runbook",
	})
	for _, t := range items {
		var closedAt string
		if t.ClosedAt != nil {
			closedAt = t.ClosedAt.UTC().Format(time.RFC3339)
		}
		_ = cw.Write([]string{
			t.ID,
			t.Severity,
			t.AssetPath,
			t.Title,
			t.OpenedAt.UTC().Format(time.RFC3339),
			closedAt,
			t.Runbook,
		})
	}
	cw.Flush()
}
