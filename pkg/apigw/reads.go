// Read-only experience-layer aggregation routes (PRMT-141, PRMT-151, PRMT-153).
// PRMT-101 introduced the pattern with /api/sites → /v1/sites.
// PRMT-141 mirrors that pattern for the three read routes the
// Ops Portal needs to bootstrap (spec-009 §8 Phase A):
//
//	GET /api/assets         → core /v1/assets
//	GET /api/alarms         → core /v1/alarms
//	GET /api/metrics/query  → core /v1/metrics/query?<rawQuery>
//
// PRMT-151 adds a fourth read route for the spec-001 §7
// relationship graph (feeds / cools / connects) that R3
// root-cause/impact (spec-009 §5.2) consumes via PRMT-147:
//
//	GET /api/topology       → core /v1/topology
//
// PRMT-153 adds the four E3.5 ops-portal read routes
// (PRMT-156 tickets + PRMT-157 capacity, P642 unblocker):
//
//	GET /api/tickets            → core /v1/tickets?<rawQuery>
//	POST /api/tickets           → core /v1/tickets (create; PRMT-231)
//	GET /api/tickets/{id}       → core /v1/tickets/{id}
//	GET /api/capacity           → core /v1/capacity?<rawQuery>
//	GET /api/capacity/metrics   → core /v1/capacity/metrics?<rawQuery>
//	GET /api/capacity/forecast  → core /v1/capacity/forecast?<rawQuery>  (P741)
//
// PRMT-193 adds the Commercial Platform usage read route
// (spec-010 / L102):
//
//	GET /api/usage              → core /v1/usage?<rawQuery>
//
// PRMT-154 adds the seven E3.5 ops-portal read routes for
// maintenance, PM, spares, and inspections (P643 unblocker;
// feeds portal pages PRMT-158/159/160):
//
//	GET /api/maintenance/upcoming  → core /v1/maintenance/upcoming?<rawQuery>
//	GET /api/pm/schedules          → core /v1/pm/schedules?<rawQuery>
//	GET /api/pm/schedules/{id}     → core /v1/pm/schedules/{id}
//	GET /api/spares                → core /v1/spares?<rawQuery>
//	GET /api/spares/{id}           → core /v1/spares/{id}
//	GET /api/inspections           → core /v1/inspections?<rawQuery>
//	GET /api/inspections/{id}      → core /v1/inspections/{id}
//
// PRMT-155 adds the four E3.5 ops-portal read routes for
// runbooks, cases, and ops/reconcile reports (P643 + P642
// unblocker; feeds portal pages PRMT-161/162):
//
//	GET /api/runbooks/{key}      → core /v1/runbooks/{key}
//	GET /api/cases               → core /v1/cases?<rawQuery>
//	GET /api/reports/ops         → core /v1/reports/ops?<rawQuery>
//	GET /api/reports/reconcile   → core /v1/reports/reconcile?<rawQuery>
//
// All handlers are thin identity-forwarding proxies: they recover
// the verified claims + raw token that AuthMiddleware injected
// into r.Context(), fail closed if the (tenant, tier) pair is
// missing, and then call Upstream.GetV1AsTenant against the
// corresponding /v1 path. Error mapping is identical to
// handleSites (5xx → 502, 4xx verbatim, transport error → 502).
//
// Per L81 (spec-009 §7.1) the Gateway carries identity, never
// enforces visibility — these handlers do not inspect crn / path
// patterns, do not transform the response, and do not implement
// pagination or role-based access checks. Core /v1 is the
// authority on what each verified caller is allowed to see.
package apigw

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/yurimeng/cios/pkg/tenant"
)

// upstream path constants. These are the only literals each
// handler hardcodes — the per-route mapping /api/<r> → /v1/<r>
// is fixed by spec-009 §8 Phase A.
const (
	upstreamPathAssets            = "/v1/assets"
	upstreamPathAlarms            = "/v1/alarms"
	upstreamPathAlarmsByIDPrefix  = "/v1/alarms/"
	upstreamPathMetricsQuery      = "/v1/metrics/query"
	upstreamPathMetricsQueryRange = "/v1/metrics/query_range"
	upstreamPathTopology          = "/v1/topology"
	upstreamPathTickets           = "/v1/tickets"
	upstreamPathTicketsByIDPrefix = "/v1/tickets/"
	upstreamPathCapacity          = "/v1/capacity"
	upstreamPathCapacityMetrics   = "/v1/capacity/metrics"
	upstreamPathCapacityForecast  = "/v1/capacity/forecast"
	// PRMT-193 §4.5: Commercial Platform usage list.
	upstreamPathUsage = "/v1/usage"
	// PRMT-154 §4: E3.5 maintenance/PM/spares/inspections reads.
	upstreamPathMaintenanceUpcoming = "/v1/maintenance/upcoming"
	upstreamPathPMSchedules         = "/v1/pm/schedules"
	upstreamPathPMSchedulesByIDPref = "/v1/pm/schedules/"
	upstreamPathSpares              = "/v1/spares"
	upstreamPathSparesByIDPrefix    = "/v1/spares/"
	upstreamPathInspections         = "/v1/inspections"
	upstreamPathInspectionsByIDPref = "/v1/inspections/"
	// PRMT-155 §4: E3.5 runbook/case/report reads.
	upstreamPathRunbooksByKeyPrefix = "/v1/runbooks/"
	upstreamPathCases               = "/v1/cases"
	upstreamPathReportsOps          = "/v1/reports/ops"
	upstreamPathReportsReconcile    = "/v1/reports/reconcile"
)

// extractTicketID returns the trailing {id} segment of
// /api/tickets/{id}. Mirrors the prefix-parsing pattern used by
// parseSiteFromStreamPath (sse.go L432) so the stdlib mux's
// exact-match dispatch in Routes() keeps working without
// introducing a prefix pattern that would shadow the bare
// /api/tickets case. Rejects empty / nested ids so the handler
// can 404 path-not-found on malformed input.
func extractTicketID(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/tickets/")
	if rest == p {
		return "", false
	}
	// Allow /api/tickets/{id}:transition (PRMT-199 write path).
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// extractTicketTransition returns (id, true) when path is
// /api/tickets/{id}:transition.
func extractTicketTransition(p string) (string, bool) {
	const suffix = ":transition"
	if !strings.HasSuffix(p, suffix) {
		return "", false
	}
	base := strings.TrimSuffix(p, suffix)
	return extractTicketID(base)
}

// extractAlarmAck returns (id, true) when path is /api/alarms/{id}:ack.
// Mirrors extractTicketTransition: empty / nested ids rejected.
func extractAlarmAck(p string) (string, bool) {
	const suffix = ":ack"
	if !strings.HasSuffix(p, suffix) {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimSuffix(p, suffix), "/api/alarms/")
	if rest == "" || strings.Contains(rest, "/") || strings.Contains(rest, ":") {
		return "", false
	}
	return rest, true
}

// handleAssets serves GET /api/assets by reverse-calling
// core /v1/assets. Behaviour matrix matches handleSites exactly
// (PRMT-101 §4 + §5 + PRMT-109 §5 + PRMT-141 §2-bis).
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/assets only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstreamPathAssets)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/assets is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/assets returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleAlarms serves GET /api/alarms by reverse-calling
// core /v1/alarms. Behaviour matrix matches handleSites exactly
// (PRMT-101 §4 + §5 + PRMT-109 §5 + PRMT-141 §2-bis).
func (s *Server) handleAlarms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/alarms only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstreamPathAlarms)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/alarms is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/alarms returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleMetricsQuery serves GET /api/metrics/query by
// reverse-calling core /v1/metrics/query and forwarding the
// inbound raw query string verbatim (PRMT-141 §2 + §4).
// joinURL (upstream.go) concatenates the path argument with
// a single "/" join and does not parse or re-encode any query
// string present in the argument; net/http.NewRequestWithContext
// then splits on the first "?" to separate path from query.
// Behaviour matrix matches handleSites for the parts that are
// not query-specific.
func (s *Server) handleMetricsQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/metrics/query only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// PRMT-141 §4: forward the inbound raw query string verbatim.
	// joinURL (upstream.go L293–305) concatenates the path arg
	// with a single "/" join; it does not parse, re-encode, or
	// strip any query string. net/http.NewRequestWithContext
	// (in GetV1AsTenant) splits on the first "?" to separate
	// path from query. The raw query bytes are therefore carried
	// end-to-end without URL-mangling.
	// PRMT-228: /api/metrics/query_range shares this handler;
	// core routes both /v1 paths to the same serveMetricsQuery.
	base := upstreamPathMetricsQuery
	if r.URL.Path == "/api/metrics/query_range" {
		base = upstreamPathMetricsQueryRange
	}
	upstream := base
	if r.URL.RawQuery != "" {
		upstream = base + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/metrics/query is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/metrics/query returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleTopology serves GET /api/topology by reverse-calling
// core /v1/topology (the spec-001 §7 relationship graph:
// feeds / cools / connects edges). Behaviour matrix matches
// handleAssets exactly (PRMT-141 §2 + §4 + PRMT-151 §2-bis).
// Per L81 (spec-009 §7.1) the Gateway carries identity, never
// enforces visibility — pure identity-forward proxy.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/topology only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstreamPathTopology)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/topology is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/topology returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleTickets serves GET /api/tickets by reverse-calling
// core /v1/tickets and forwarding the inbound raw query string
// verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour matrix
// matches handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153
// §2-bis). Per L81 (spec-009 §7.1) the Gateway carries identity,
// never enforces visibility — pure identity-forward proxy.
func (s *Server) handleTickets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/tickets only supports GET and POST", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// PRMT-231: POST /api/tickets → create. Body forwarded as-is
	// (JSON per core /v1/tickets contract); identity-forward only
	// (L81) — core validates and scope-checks the body asset_path.
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			WriteProblem(w, http.StatusBadRequest,
				"bad-request", "read body", err.Error(), r.URL.Path)
			return
		}
		rawToken, _ := RawTokenFrom(r.Context())
		status, respBody, contentType, err := s.up.PostV1AsTenant(r.Context(), claims, rawToken, upstreamPathTickets, body)
		if err != nil {
			if errors.Is(err, ErrTenantMissing) {
				WriteProblem(w, http.StatusForbidden,
					"forbidden", "tenant or isolation tier missing",
					"the verified token did not carry a usable tenant identity",
					r.URL.Path)
				return
			}
			WriteProblem(w, http.StatusBadGateway,
				"upstream-unavailable", "upstream unavailable",
				"core /v1/tickets is not reachable", r.URL.Path)
			return
		}
		switch {
		case status >= 500:
			WriteProblem(w, http.StatusBadGateway,
				"upstream-unavailable", "upstream unavailable",
				"core /v1/tickets returned "+http.StatusText(status),
				r.URL.Path)
		default:
			ct := contentType
			if ct == "" {
				if status >= 400 {
					ct = "application/problem+json"
				} else {
					ct = "application/json"
				}
			}
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = w.Write(respBody)
		}
		return
	}

	// PRMT-141 §4 + PRMT-153 §4: forward the inbound raw query
	// string verbatim. joinURL (upstream.go) concatenates the
	// path arg with a single "/" join; it does not parse,
	// re-encode, or strip any query string.
	upstream := upstreamPathTickets
	if r.URL.RawQuery != "" {
		upstream = upstreamPathTickets + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleTicketByID serves GET /api/tickets/{id} and
// POST /api/tickets/{id}:transition (PRMT-199). The {id} segment is
// the trailing path component of the inbound request
// (extractTicketID / extractTicketTransition). Malformed id → 404.
// Per L81 the Gateway carries identity, never enforces visibility.
func (s *Server) handleTicketByID(w http.ResponseWriter, r *http.Request) {
	// Write path: POST …:transition (PRMT-199).
	if id, ok := extractTicketTransition(r.URL.Path); ok {
		s.handleTicketTransition(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/tickets/{id} only supports GET (POST allowed on :transition)", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	id, ok := extractTicketID(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path,
			r.URL.Path)
		return
	}

	upstream := upstreamPathTicketsByIDPrefix + id

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets/{id} is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets/{id} returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleTicketTransition proxies POST /api/tickets/{id}:transition
// to core /v1/tickets/{id}:transition (PRMT-199 §2). Body forwarded
// as-is (JSON {"to":"..."}). Identity-forward only (L81).
func (s *Server) handleTicketTransition(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/tickets/{id}:transition only supports POST", r.URL.Path)
		return
	}
	if id == "" {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path, r.URL.Path)
		return
	}
	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}
	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "read body", err.Error(), r.URL.Path)
		return
	}
	upstream := upstreamPathTicketsByIDPrefix + id + ":transition"
	rawToken, _ := RawTokenFrom(r.Context())
	status, respBody, contentType, err := s.up.PostV1AsTenant(r.Context(), claims, rawToken, upstream, body)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets/{id}:transition is not reachable", r.URL.Path)
		return
	}
	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/tickets/{id}:transition returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}
}

// handleCapacity serves GET /api/capacity by reverse-calling
// core /v1/capacity and forwarding the inbound raw query string
// verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour matrix
// matches handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153
// §2-bis). Per L81 (spec-009 §7.1) the Gateway carries identity,
// never enforces visibility — pure identity-forward proxy.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/capacity only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// PRMT-141 §4 + PRMT-153 §4: forward the inbound raw query
	// string verbatim. joinURL (upstream.go) concatenates the
	// path arg with a single "/" join; it does not parse,
	// re-encode, or strip any query string.
	upstream := upstreamPathCapacity
	if r.URL.RawQuery != "" {
		upstream = upstreamPathCapacity + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleCapacityForecast serves GET /api/capacity/forecast (P741).
// Same identity-forward matrix as handleCapacity.
func (s *Server) handleCapacityForecast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/capacity/forecast only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathCapacityForecast
	if r.URL.RawQuery != "" {
		upstream = upstreamPathCapacityForecast + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity/forecast is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity/forecast returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleUsage serves GET /api/usage by reverse-calling core
// /v1/usage and forwarding the inbound raw query string
// verbatim (PRMT-193 §4.5). Behaviour matrix matches
// handleCapacity exactly. Per L81 (spec-009 §7.1) the Gateway
// carries identity, never enforces visibility — pure identity-
// forward proxy.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/usage only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathUsage
	if r.URL.RawQuery != "" {
		upstream = upstreamPathUsage + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/usage is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/usage returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleCapacityMetrics serves GET /api/capacity/metrics by
// reverse-calling core /v1/capacity/metrics and forwarding the
// inbound raw query string verbatim (PRMT-141 §4 + PRMT-153 §4).
// Behaviour matrix matches handleAssets exactly (PRMT-141 §2 +
// §4 + PRMT-153 §2-bis). Per L81 (spec-009 §7.1) the Gateway
// carries identity, never enforces visibility — pure identity-
// forward proxy.
func (s *Server) handleCapacityMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/capacity/metrics only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// PRMT-141 §4 + PRMT-153 §4: forward the inbound raw query
	// string verbatim. joinURL (upstream.go) concatenates the
	// path arg with a single "/" join; it does not parse,
	// re-encode, or strip any query string.
	upstream := upstreamPathCapacityMetrics
	if r.URL.RawQuery != "" {
		upstream = upstreamPathCapacityMetrics + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity/metrics is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/capacity/metrics returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// extractPMScheduleID returns the trailing {id} segment of
// /api/pm/schedules/{id}. Same prefix-parsing pattern as
// extractTicketID (PRMT-153) — stdlib mux dispatch in Routes()
// keeps working without introducing a prefix pattern that would
// shadow the bare /api/pm/schedules case. Rejects empty /
// nested ids so the handler can 404 path-not-found on malformed
// input.
func extractPMScheduleID(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/pm/schedules/")
	if rest == p {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// extractSpareID returns the trailing {id} segment of
// /api/spares/{id}. Same prefix-parsing pattern as
// extractTicketID (PRMT-153).
func extractSpareID(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/spares/")
	if rest == p {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// extractInspectionID returns the trailing {id} segment of
// /api/inspections/{id}. Same prefix-parsing pattern as
// extractTicketID (PRMT-153).
func extractInspectionID(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/inspections/")
	if rest == p {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// handleMaintenanceUpcoming serves GET /api/maintenance/upcoming
// by reverse-calling core /v1/maintenance/upcoming and forwarding
// the inbound raw query string verbatim (PRMT-141 §4 + PRMT-153
// §4). Behaviour matrix matches handleAssets exactly (PRMT-141
// §2 + §4 + PRMT-153 §2-bis + PRMT-154 §2-bis). Per L81
// (spec-009 §7.1) the Gateway carries identity, never enforces
// visibility — pure identity-forward proxy.
func (s *Server) handleMaintenanceUpcoming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/maintenance/upcoming only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathMaintenanceUpcoming
	if r.URL.RawQuery != "" {
		upstream = upstreamPathMaintenanceUpcoming + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/maintenance/upcoming is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/maintenance/upcoming returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handlePMSchedules serves GET /api/pm/schedules by reverse-
// calling core /v1/pm/schedules and forwarding the inbound raw
// query string verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour
// matrix matches handleAssets exactly (PRMT-141 §2 + §4 +
// PRMT-153 §2-bis + PRMT-154 §2-bis). Per L81 (spec-009 §7.1)
// the Gateway carries identity, never enforces visibility — pure
// identity-forward proxy.
func (s *Server) handlePMSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/pm/schedules only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathPMSchedules
	if r.URL.RawQuery != "" {
		upstream = upstreamPathPMSchedules + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/pm/schedules is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/pm/schedules returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handlePMScheduleByID serves GET /api/pm/schedules/{id} by
// reverse-calling core /v1/pm/schedules/{id}. The {id} segment
// is the trailing path component of the inbound request
// (extractPMScheduleID does the parsing); a malformed id is
// rejected with 404 path-not-found. Behaviour matrix matches
// handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153 §2-bis +
// PRMT-154 §2-bis). Per L81 (spec-009 §7.1) the Gateway carries
// identity, never enforces visibility — pure identity-forward
// proxy.
func (s *Server) handlePMScheduleByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/pm/schedules/{id} only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	id, ok := extractPMScheduleID(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path,
			r.URL.Path)
		return
	}

	upstream := upstreamPathPMSchedulesByIDPref + id

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/pm/schedules/{id} is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/pm/schedules/{id} returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleSpares serves GET /api/spares by reverse-calling core
// /v1/spares and forwarding the inbound raw query string
// verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour matrix matches
// handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153 §2-bis +
// PRMT-154 §2-bis). Per L81 (spec-009 §7.1) the Gateway carries
// identity, never enforces visibility — pure identity-forward
// proxy.
func (s *Server) handleSpares(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/spares only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathSpares
	if r.URL.RawQuery != "" {
		upstream = upstreamPathSpares + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/spares is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/spares returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleSpareByID serves GET /api/spares/{id} by reverse-calling
// core /v1/spares/{id}. The {id} segment is the trailing path
// component of the inbound request (extractSpareID does the
// parsing); a malformed id is rejected with 404 path-not-found.
// Behaviour matrix matches handleAssets exactly (PRMT-141 §2 +
// §4 + PRMT-153 §2-bis + PRMT-154 §2-bis). Per L81 (spec-009
// §7.1) the Gateway carries identity, never enforces visibility
// — pure identity-forward proxy.
func (s *Server) handleSpareByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/spares/{id} only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	id, ok := extractSpareID(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path,
			r.URL.Path)
		return
	}

	upstream := upstreamPathSparesByIDPrefix + id

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/spares/{id} is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/spares/{id} returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleInspections serves GET /api/inspections by reverse-
// calling core /v1/inspections and forwarding the inbound raw
// query string verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour
// matrix matches handleAssets exactly (PRMT-141 §2 + §4 +
// PRMT-153 §2-bis + PRMT-154 §2-bis). Per L81 (spec-009 §7.1)
// the Gateway carries identity, never enforces visibility — pure
// identity-forward proxy.
func (s *Server) handleInspections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/inspections only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathInspections
	if r.URL.RawQuery != "" {
		upstream = upstreamPathInspections + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/inspections is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/inspections returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleInspectionByID serves GET /api/inspections/{id} by
// reverse-calling core /v1/inspections/{id}. The {id} segment is
// the trailing path component of the inbound request
// (extractInspectionID does the parsing); a malformed id is
// rejected with 404 path-not-found. Behaviour matrix matches
// handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153 §2-bis +
// PRMT-154 §2-bis). Per L81 (spec-009 §7.1) the Gateway carries
// identity, never enforces visibility — pure identity-forward
// proxy.
func (s *Server) handleInspectionByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/inspections/{id} only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	id, ok := extractInspectionID(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path,
			r.URL.Path)
		return
	}

	upstream := upstreamPathInspectionsByIDPref + id

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/inspections/{id} is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/inspections/{id} returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// extractRunbookKey returns the trailing {key} segment of
// /api/runbooks/{key}. Same prefix-parsing pattern as
// extractTicketID (PRMT-153) / extractPMScheduleID /
// extractSpareID / extractInspectionID (PRMT-154). Rejects
// empty / nested keys so the handler can 404 path-not-found
// on malformed input.
func extractRunbookKey(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/runbooks/")
	if rest == p {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// handleRunbookByKey serves GET /api/runbooks/{key} by reverse-
// calling core /v1/runbooks/{key}. The {key} segment is the
// trailing path component of the inbound request (extractRunbookKey
// does the parsing); a malformed key is rejected with 404
// path-not-found. Behaviour matrix matches handleAssets exactly
// (PRMT-141 §2 + §4 + PRMT-153 §2-bis + PRMT-155 §2-bis). Per
// L81 (spec-009 §7.1) the Gateway carries identity, never
// enforces visibility — pure identity-forward proxy.
func (s *Server) handleRunbookByKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/runbooks/{key} only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	key, ok := extractRunbookKey(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path,
			r.URL.Path)
		return
	}

	upstream := upstreamPathRunbooksByKeyPrefix + key

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/runbooks/{key} is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/runbooks/{key} returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleCases serves GET /api/cases by reverse-calling core
// /v1/cases and forwarding the inbound raw query string verbatim
// (PRMT-141 §4 + PRMT-153 §4). Behaviour matrix matches
// handleAssets exactly (PRMT-141 §2 + §4 + PRMT-153 §2-bis +
// PRMT-155 §2-bis). Per L81 (spec-009 §7.1) the Gateway carries
// identity, never enforces visibility — pure identity-forward
// proxy.
func (s *Server) handleCases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/cases only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// PRMT-141 §4 + PRMT-153 §4: forward the inbound raw query
	// string verbatim. joinURL (upstream.go) concatenates the
	// path arg with a single "/" join; it does not parse,
	// re-encode, or strip any query string.
	upstream := upstreamPathCases
	if r.URL.RawQuery != "" {
		upstream = upstreamPathCases + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/cases is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/cases returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleReportOps serves GET /api/reports/ops by reverse-
// calling core /v1/reports/ops and forwarding the inbound raw
// query string verbatim (PRMT-141 §4 + PRMT-153 §4). Behaviour
// matrix matches handleAssets exactly (PRMT-141 §2 + §4 +
// PRMT-153 §2-bis + PRMT-155 §2-bis). Per L81 (spec-009 §7.1)
// the Gateway carries identity, never enforces visibility —
// pure identity-forward proxy.
func (s *Server) handleReportOps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/reports/ops only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathReportsOps
	if r.URL.RawQuery != "" {
		upstream = upstreamPathReportsOps + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/reports/ops is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/reports/ops returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleReportReconcile serves GET /api/reports/reconcile by
// reverse-calling core /v1/reports/reconcile and forwarding
// the inbound raw query string verbatim (PRMT-141 §4 +
// PRMT-153 §4). Behaviour matrix matches handleAssets exactly
// (PRMT-141 §2 + §4 + PRMT-153 §2-bis + PRMT-155 §2-bis).
// Per L81 (spec-009 §7.1) the Gateway carries identity, never
// enforces visibility — pure identity-forward proxy.
func (s *Server) handleReportReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/reports/reconcile only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	upstream := upstreamPathReportsReconcile
	if r.URL.RawQuery != "" {
		upstream = upstreamPathReportsReconcile + "?" + r.URL.RawQuery
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, upstream)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/reports/reconcile is not reachable", r.URL.Path)
		return
	}

	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/reports/reconcile returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// handleAlarmAck proxies POST /api/alarms/{id}:ack to core
// /v1/alarms/{id}:ack (PRMT-230). Body forwarded as-is (core
// ignores it). Identity-forward only (L81).
func (s *Server) handleAlarmAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/alarms/{id}:ack only supports POST", r.URL.Path)
		return
	}
	id, ok := extractAlarmAck(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "API path not found",
			"no handler registered for "+r.URL.Path, r.URL.Path)
		return
	}
	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}
	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "read body", err.Error(), r.URL.Path)
		return
	}
	upstream := upstreamPathAlarmsByIDPrefix + id + ":ack"
	rawToken, _ := RawTokenFrom(r.Context())
	status, respBody, contentType, err := s.up.PostV1AsTenant(r.Context(), claims, rawToken, upstream, body)
	if err != nil {
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/alarms/{id}:ack is not reachable", r.URL.Path)
		return
	}
	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/alarms/{id}:ack returned "+http.StatusText(status),
			r.URL.Path)
	default:
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}
}
