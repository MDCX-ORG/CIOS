// Package core — sla_read.go: GET /v1/sla customer uptime SLA stub
// (PRMT-209 / E3.4 / P631 v0 / T37).
//
// This is NOT ticket-SLA (sla.go / PRMT-036). Customer uptime target
// is a display-only contract: target_pct=99.9, window=calendar_month,
// credit_note with no financial effect (Q4 PASS).
//
// v0: constant response — no DB table, no credit calculation, no ERP.
package core

import (
	"net/http"
)

// customerSLAResponse is the wire envelope for GET /v1/sla.
type customerSLAResponse struct {
	TargetPct  float64 `json:"target_pct"`
	Window     string  `json:"window"`
	CreditNote string  `json:"credit_note"`
}

// serveCustomerSLA handles GET /v1/sla. Other methods → 405.
// Authz: ActionRead + list-scope (middleware); tenant binding is
// request-scoped via TenantFromContext when the gateway attaches
// X-CIOS-Tenant (no per-row store filter in v0 constants).
func (s *Server) serveCustomerSLA(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	// Presence of principal is enforced by auth middleware when
	// auth is enabled. When auth is off (tests without AuthConfig),
	// the constant still returns so local smoke works.
	writeJSON(w, http.StatusOK, customerSLAResponse{
		TargetPct:  99.9,
		Window:     "calendar_month",
		CreditNote: "display-only; no financial effect",
	})
}
