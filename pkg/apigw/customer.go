// Package apigw — customer.go: customer-portal read proxies (PRMT-208 / E3.4).
//
//	GET /api/customer/status  → aggregate tenant site health from core
//	                           /v1/alarms (+ /v1/sites when available)
//	GET /api/customer/sla     → Q4 default constants (optional forward to
//	                           core /v1/sla when PRMT-209 is live)
//	GET /api/customer/usage   → core /v1/usage with tenant_id forced (对量)
//
// Auth: same AuthMiddleware as every /api/* route. Missing tenant → 403.
// No write paths. Credit has no financial effect.
//
// Health rule (v0), documented for customer-portal parity:
//
//	any open alarm with severity == critical → red
//	else open_alarms > 0                    → yellow
//	else                                    → green
//
// "open" = state is firing or acked (not resolved).
package apigw

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/sts"
	"github.com/yurimeng/cios/pkg/tenant"
)

type customerSiteStatus struct {
	ID         string `json:"id"`
	Health     string `json:"health"` // green|yellow|red
	OpenAlarms int    `json:"open_alarms"`
}

type customerStatusResponse struct {
	TenantID string               `json:"tenant_id"`
	Sites    []customerSiteStatus `json:"sites"`
	AsOf     string               `json:"as_of"`
	// Degraded is true when core upstream could not be fully read
	// (M4 F4). Sites may be empty or partial; clients must not treat
	// this as an all-green facility.
	Degraded bool   `json:"degraded,omitempty"`
	Note     string `json:"note,omitempty"`
}

type customerSLAResponse struct {
	TargetPct  float64 `json:"target_pct"`
	Window     string  `json:"window"`
	CreditNote string  `json:"credit_note"`
}

// defaultCustomerSLA is the Q4 PASS contract (PRMT-208 / PRMT-209).
var defaultCustomerSLA = customerSLAResponse{
	TargetPct:  99.9,
	Window:     "calendar_month",
	CreditNote: "display-only; no financial effect",
}

// handleCustomerStatus serves GET /api/customer/status.
func (s *Server) handleCustomerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/customer/status only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	tenantID, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	rawToken, _ := RawTokenFrom(r.Context())
	siteIDs, sitesOK := s.fetchCustomerSiteIDs(r, claims, rawToken)
	alarms, alarmsOK := s.fetchCustomerAlarms(r, claims, rawToken)
	degraded := !sitesOK || !alarmsOK

	bySite := map[string]*customerSiteStatus{}
	for _, id := range siteIDs {
		bySite[id] = &customerSiteStatus{ID: id, Health: "green", OpenAlarms: 0}
	}

	for _, a := range alarms {
		if !isCustomerOpenAlarm(a.State) {
			continue
		}
		siteID := siteIDFromPath(a.Path)
		if siteID == "" {
			continue
		}
		st, ok := bySite[siteID]
		if !ok {
			st = &customerSiteStatus{ID: siteID, Health: "green", OpenAlarms: 0}
			bySite[siteID] = st
		}
		st.OpenAlarms++
		if a.Severity == "critical" {
			st.Health = "red"
		} else if st.Health != "red" {
			st.Health = "yellow"
		}
	}

	sites := make([]customerSiteStatus, 0, len(bySite))
	seen := map[string]bool{}
	for _, id := range siteIDs {
		if st, ok := bySite[id]; ok {
			sites = append(sites, *st)
			seen[id] = true
		}
	}
	var extra []string
	for id := range bySite {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		sites = append(sites, *bySite[id])
	}
	if sites == nil {
		sites = []customerSiteStatus{}
	}

	resp := customerStatusResponse{
		TenantID: tenantID,
		Sites:    sites,
		AsOf:     time.Now().UTC().Format(time.RFC3339),
		Degraded: degraded,
	}
	if degraded {
		resp.Note = "status upstream degraded (sites or alarms unavailable)"
	}
	writeCustomerJSON(w, http.StatusOK, resp)
}

// handleCustomerSLA serves GET /api/customer/sla.
// Prefer core /v1/sla when reachable; otherwise return Q4 constants.
func (s *Server) handleCustomerSLA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/customer/sla only supports GET", r.URL.Path)
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
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, "/v1/sla")
	if err == nil && status >= 200 && status < 300 && len(body) > 0 {
		ct := contentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	if err != nil && errors.Is(err, ErrTenantMissing) {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	// v0 constant fallback (PRMT-208: pure gateway OK when core absent).
	writeCustomerJSON(w, http.StatusOK, defaultCustomerSLA)
}

// handleCustomerUsage serves GET /api/customer/usage → /v1/usage.
// Forces tenant_id from claims (cannot read another tenant).
// Forwards kind/granularity/period_* query params only.
func (s *Server) handleCustomerUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/customer/usage only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	tenantID, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK || strings.TrimSpace(tenantID) == "" {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	q := url.Values{}
	q.Set("tenant_id", tenantID)
	for _, k := range []string{"kind", "granularity", "period_start", "period_end", "page_size", "page_token"} {
		if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" {
			q.Set(k, v)
		}
	}
	upstream := "/v1/usage?" + q.Encode()

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
		// Degraded empty list (M4 F4) — explicit flag so UI does not
		// treat "no rows" as healthy zero usage.
		writeCustomerJSON(w, http.StatusOK, map[string]any{
			"items":     []any{},
			"tenant_id": tenantID,
			"degraded":  true,
			"note":      "usage upstream unavailable",
		})
		return
	}

	switch {
	case status >= 500:
		writeCustomerJSON(w, http.StatusOK, map[string]any{
			"items":     []any{},
			"tenant_id": tenantID,
			"degraded":  true,
			"note":      "usage upstream error",
		})
	case status >= 400:
		// Auth/upstream rejection — still mark degraded when we choose
		// not to forward problem details (customer-facing shell).
		writeCustomerJSON(w, http.StatusOK, map[string]any{
			"items":     []any{},
			"tenant_id": tenantID,
			"degraded":  true,
			"note":      "usage upstream rejected",
		})
	default:
		ct := contentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// --- helpers ----------------------------------------------------------------

type customerAlarmWire struct {
	Path     string `json:"path"`
	Severity string `json:"severity"`
	State    string `json:"state"`
}

type customerAlarmsEnvelope struct {
	Items []customerAlarmWire `json:"items"`
}

type customerSitesEnvelope struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
}

// fetchCustomerAlarms returns items and ok=false when upstream is
// unavailable (transport error or non-2xx). ok=true with empty items
// means a healthy empty alarm set.
func (s *Server) fetchCustomerAlarms(r *http.Request, claims sts.TokenClaims, rawToken string) ([]customerAlarmWire, bool) {
	if s.up == nil {
		return nil, false
	}
	status, body, _, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, "/v1/alarms")
	if err != nil || status < 200 || status >= 300 {
		return nil, false
	}
	var env customerAlarmsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	if env.Items == nil {
		env.Items = []customerAlarmWire{}
	}
	return env.Items, true
}

// fetchCustomerSiteIDs returns site ids and ok=false when upstream fails.
func (s *Server) fetchCustomerSiteIDs(r *http.Request, claims sts.TokenClaims, rawToken string) ([]string, bool) {
	if s.up == nil {
		return nil, false
	}
	status, body, _, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, "/v1/sites")
	if err != nil || status < 200 || status >= 300 {
		return nil, false
	}
	var env customerSitesEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(env.Items))
	for _, it := range env.Items {
		if id := strings.TrimSpace(it.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, true
}

func isCustomerOpenAlarm(state string) bool {
	switch state {
	case "firing", "acked":
		return true
	default:
		return false
	}
}

// siteIDFromPath takes the first CRN segment (e.g. sgp01.pod002.cdu000 → sgp01).
func siteIDFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '.'); i > 0 {
		return path[:i]
	}
	return path
}

func writeCustomerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}
