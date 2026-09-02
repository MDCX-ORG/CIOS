// Package core — maintenance.go: GET /v1/maintenance/upcoming
// (M2 E2.4 + E2.7 / PRMT-058).
//
// Read-only aggregation over ListPMSchedules + ListInspectionTemplates.
// Produces a merged "todo" list sorted by NextDue asc; per-item
// authorize(ActionRead, asset_path) drops out-of-scope entries
// silently (same shape as /v1/pm/schedules, /v1/inspections,
// /v1/reports/ops).
//
// Query params:
//
//	before  RFC3339; include only items with NextDue <= before
//	overdue "true" | "false"; if true, include only items with
//	        NextDue < now
//
// Either may be present alone or together; both apply as filters
// (intersection). Bad parse → 400 (RFC 7807).
//
// Pure compute: no writes, no state changes, no new infra. PM /
// inspection schemas are untouched.
package core

import (
	"net/http"
	"sort"
	"strconv"
	"time"
)

// maintenanceUpcomingItem is one row of the upcoming view. Kind
// distinguishes the source ("pm" vs "inspection"); ID is the
// underlying schedule/template id; overdue is computed against
// time.Now().UTC() at request time.
type maintenanceUpcomingItem struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	AssetPath string    `json:"asset_path"`
	Title     string    `json:"title"`
	NextDue   time.Time `json:"next_due"`
	Overdue   bool      `json:"overdue"`
}

// maintenanceUpcomingResponse is the JSON envelope. Always emits
// "items": [] — never null — so clients can iterate without a
// nil-guard.
type maintenanceUpcomingResponse struct {
	Items []maintenanceUpcomingItem `json:"items"`
}

// serveMaintenanceUpcoming handles GET /v1/maintenance/upcoming.
// Non-GET → 405. Bad before/overdue → 400. Per-item authorize()
// drops out-of-scope silently.
func (s *Server) serveMaintenanceUpcoming(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	q := r.URL.Query()

	var before *time.Time
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad before (need RFC3339)", v, r.URL.Path, rid)
			return
		}
		t = t.UTC()
		before = &t
	}

	var overdueOnly *bool
	if v := q.Get("overdue"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad overdue (need bool)", v, r.URL.Path, rid)
			return
		}
		overdueOnly = &b
	}

	pm, err := s.st.ListPMSchedules(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	insp, err := s.st.ListInspectionTemplates(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}

	now := time.Now().UTC()
	principal, hasAuth := PrincipalFromContext(r.Context())

	items := make([]maintenanceUpcomingItem, 0, len(pm)+len(insp))
	for _, p := range pm {
		if !p.Enabled {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, p.AssetPath) != nil {
			continue
		}
		items = append(items, maintenanceUpcomingItem{
			Kind:      "pm",
			ID:        p.ID,
			AssetPath: p.AssetPath,
			Title:     p.Title,
			NextDue:   p.NextDue,
			Overdue:   p.NextDue.Before(now),
		})
	}
	for _, it := range insp {
		if !it.Enabled {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, it.AssetPath) != nil {
			continue
		}
		items = append(items, maintenanceUpcomingItem{
			Kind:      "inspection",
			ID:        it.ID,
			AssetPath: it.AssetPath,
			Title:     it.Title,
			NextDue:   it.NextDue,
			Overdue:   it.NextDue.Before(now),
		})
	}

	if before != nil || overdueOnly != nil {
		filtered := items[:0]
		for _, it := range items {
			if before != nil && it.NextDue.After(*before) {
				continue
			}
			if overdueOnly != nil {
				if *overdueOnly && !it.Overdue {
					continue
				}
				if !*overdueOnly && it.Overdue {
					continue
				}
			}
			filtered = append(filtered, it)
		}
		items = filtered
	}

	sort.Slice(items, func(i, j int) bool {
		if !items[i].NextDue.Equal(items[j].NextDue) {
			return items[i].NextDue.Before(items[j].NextDue)
		}
		// Stable tie-breaker: kind asc, then id asc. Keeps the
		// output deterministic when two items share a NextDue.
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].ID < items[j].ID
	})

	if items == nil {
		items = []maintenanceUpcomingItem{}
	}
	writeJSON(w, http.StatusOK, maintenanceUpcomingResponse{Items: items})
}
