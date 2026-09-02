// Package core — usage_http.go: HTTP surface for Usage list, CSV
// export, and admin compute (PRMT-193 / spec-010 §3 / L102).
//
// Routes (registered in core/server.go):
//
//	GET  /v1/usage          → filtered list + pagination
//	GET  /v1/usage:export   → same filters, text/csv body
//	POST /v1/usage:compute  → admin-only compute + Upsert
//
// Auth mirrors /v1/capacity for reads (ActionRead, list-scope).
// TenantFromContext present forces TenantID filter; conflicting
// query tenant_id → 403 tenant-scope-mismatch (orgs R1 shape).
package core

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// listUsageResponse is the wire envelope for GET /v1/usage.
type listUsageResponse struct {
	Items         []UsageRecord `json:"items"`
	NextPageToken string        `json:"next_page_token"`
}

// usageComputeRequest is the JSON body for POST /v1/usage:compute.
// Measurement fields are inlined (json tags) so we do not alter the
// PRMT-192 Measurement type which lives outside this whitelist.
type usageComputeRequest struct {
	PeriodStart  time.Time        `json:"period_start"`
	PeriodEnd    time.Time        `json:"period_end"`
	Granularity  UsageGranularity `json:"granularity"`
	Kinds        []UsageKind      `json:"kinds"`
	Measurements []struct {
		AssetPath string    `json:"asset_path"`
		Time      time.Time `json:"time"`
		Quantity  float64   `json:"quantity"`
		Unit      string    `json:"unit"`
	} `json:"measurements"`
}

// usageComputeResponse is the wire envelope for POST /v1/usage:compute.
type usageComputeResponse struct {
	Items []UsageRecord `json:"items"`
}

// serveUsage handles GET /v1/usage. Other methods → 405.
func (s *Server) serveUsage(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	filter, pageSize, startIdx, ok := s.parseUsageListQuery(w, r, rid)
	if !ok {
		return
	}
	items, err := s.st.ListUsage(r.Context(), filter)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if startIdx > len(items) {
		startIdx = len(items)
	}
	page := items[startIdx:]
	var next string
	if len(page) > pageSize {
		next = strconv.Itoa(startIdx + pageSize)
		page = page[:pageSize]
	}
	// Always non-nil slice for stable JSON "items":[].
	if page == nil {
		page = []UsageRecord{}
	}
	writeJSON(w, http.StatusOK, listUsageResponse{Items: page, NextPageToken: next})
}

// serveUsageExport handles GET /v1/usage:export → text/csv.
func (s *Server) serveUsageExport(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	// Export ignores pagination: emit every matching record.
	filter, _, _, ok := s.parseUsageListQuery(w, r, rid)
	if !ok {
		return
	}
	items, err := s.st.ListUsage(r.Context(), filter)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "kind", "tenant_id", "site_id", "asset_path",
		"period_start", "period_end", "granularity", "quantity", "unit",
	})
	for _, rec := range items {
		_ = cw.Write([]string{
			rec.ID,
			string(rec.Kind),
			rec.TenantID,
			rec.SiteID,
			rec.AssetPath,
			rec.PeriodStart.UTC().Format(time.RFC3339),
			rec.PeriodEnd.UTC().Format(time.RFC3339),
			string(rec.Granularity),
			strconv.FormatFloat(rec.Quantity, 'f', -1, 64),
			rec.Unit,
		})
	}
	cw.Flush()
}

// serveUsageCompute handles POST /v1/usage:compute (admin-only).
func (s *Server) serveUsageCompute(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Role != RoleAdmin {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"forbidden", "admin required", r.URL.Path, rid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req usageComputeRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.PeriodStart.IsZero() || req.PeriodEnd.IsZero() {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing period", "period_start and period_end required", r.URL.Path, rid)
		return
	}
	if !req.PeriodEnd.After(req.PeriodStart) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad period", "period_end must be after period_start", r.URL.Path, rid)
		return
	}
	g := req.Granularity
	if g == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing granularity", "", r.URL.Path, rid)
		return
	}
	if g != UsageDaily && g != UsageMonthly {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad granularity", string(g), r.URL.Path, rid)
		return
	}
	// kinds default both when empty/omitted.
	wantEnergy, wantRack := false, false
	if len(req.Kinds) == 0 {
		wantEnergy, wantRack = true, true
	} else {
		for _, k := range req.Kinds {
			switch k {
			case UsageKindEnergy:
				wantEnergy = true
			case UsageKindRackHour:
				wantRack = true
			default:
				writeProblem(w, http.StatusBadRequest, "bad-request",
					"bad kind", string(k), r.URL.Path, rid)
				return
			}
		}
	}

	// Tenant force from context (if present) applied to emitted records.
	forceTenant, hasTenant := TenantFromContext(r.Context())

	var computed []UsageRecord
	if wantRack {
		assets, err := s.st.ListAssets(r.Context())
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
		computed = append(computed, ComputeRackHourUsage(assets, req.PeriodStart, req.PeriodEnd, g)...)
	}
	if wantEnergy && len(req.Measurements) > 0 {
		ms := make([]Measurement, 0, len(req.Measurements))
		for _, m := range req.Measurements {
			ms = append(ms, Measurement{
				AssetPath: m.AssetPath,
				Time:      m.Time,
				Quantity:  m.Quantity,
				Unit:      m.Unit,
			})
		}
		computed = append(computed, ComputeEnergyUsage(ms, req.PeriodStart, req.PeriodEnd, g)...)
	}

	out := make([]UsageRecord, 0, len(computed))
	for _, rec := range computed {
		if hasTenant {
			rec.TenantID = forceTenant
		}
		enriched, err := EnrichUsageIdentity(r.Context(), s.st, rec)
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "identity enrich error", err)
			return
		}
		saved, err := s.st.UpsertUsage(r.Context(), enriched)
		if err != nil {
			if errors.Is(err, ErrUsagePGNotImplemented) {
				writeProblem(w, http.StatusNotImplemented, "not-implemented",
					"usage postgres backend not implemented", "", r.URL.Path, rid)
				return
			}
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
		sink := s.usageSink
		if sink == nil {
			sink = NoopUsageEventSink{}
		}
		sink.OnUsageUpserted(r.Context(), saved)
		out = append(out, saved)
	}
	if out == nil {
		out = []UsageRecord{}
	}
	writeJSON(w, http.StatusOK, usageComputeResponse{Items: out})
}

// parseUsageListQuery parses shared list/export filters + pagination.
// On validation failure it writes an RFC 7807 problem and returns ok=false.
// For export, pageSize/startIdx are still parsed (and validated) but
// the caller may ignore them.
func (s *Server) parseUsageListQuery(w http.ResponseWriter, r *http.Request, rid string) (UsageListFilter, int, int, bool) {
	q := r.URL.Query()
	var f UsageListFilter

	f.TenantID = q.Get("tenant_id")
	if tid, ok := TenantFromContext(r.Context()); ok {
		if f.TenantID != "" && f.TenantID != tid {
			writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
				"tenant scope mismatch", f.TenantID, r.URL.Path, rid)
			return f, 0, 0, false
		}
		f.TenantID = tid
	}
	f.SiteID = q.Get("site_id")

	if k := q.Get("kind"); k != "" {
		switch UsageKind(k) {
		case UsageKindEnergy, UsageKindRackHour:
			f.Kind = UsageKind(k)
		default:
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad kind", k, r.URL.Path, rid)
			return f, 0, 0, false
		}
	}
	if g := q.Get("granularity"); g != "" {
		switch UsageGranularity(g) {
		case UsageDaily, UsageMonthly:
			f.Granularity = UsageGranularity(g)
		default:
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad granularity", g, r.URL.Path, rid)
			return f, 0, 0, false
		}
	}
	if v := q.Get("period_start"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad period_start", v, r.URL.Path, rid)
			return f, 0, 0, false
		}
		f.PeriodStart = t
	}
	if v := q.Get("period_end"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad period_end", v, r.URL.Path, rid)
			return f, 0, 0, false
		}
		f.PeriodEnd = t
	}

	pageSize := DefaultPageSize
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_size", ps, r.URL.Path, rid)
			return f, 0, 0, false
		}
		if n > MaxPageSize {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"page_size > 1000", ps, r.URL.Path, rid)
			return f, 0, 0, false
		}
		pageSize = n
	}
	startIdx := 0
	if pt := q.Get("page_token"); pt != "" {
		// MVP: opaque offset is a decimal index string (PRMT-193 §4.1).
		n, err := strconv.Atoi(pt)
		if err != nil || n < 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", pt, r.URL.Path, rid)
			return f, 0, 0, false
		}
		startIdx = n
	}
	return f, pageSize, startIdx, true
}
