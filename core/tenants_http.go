// Package core — tenants_http.go: POST /v1/tenants/{id}:tier
// (PRMT-182) + DELETE /v1/tenants/{id} (PRMT-220 hard delete).
//
// Contract (PRMT-182 §4.2 + PRMT-220):
//   - POST /v1/tenants/{id}:tier → tier upgrade (body isolation_tier).
//   - DELETE /v1/tenants/{id} → hard delete when zero orgs (409 if orgs remain).
//   - Other methods → 405; unknown sub-paths → 404.
//   - Auth: RoleAdmin else 403. p.Subject is the audit principal.
//   - Body for :tier: {"isolation_tier":"label|row|db"}; malformed/empty → 400.
//   - Call UpdateTenantTier; nil → 200; ErrTierDowngrade → 409;
//     DeleteTenant: nil → 204; ErrTenantOwnsOrgs → 409 "tenant-owns-orgs".
//
// No change to pkg/tenant or claim emission; no downgrade override.
package core

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// tenantTierSuffix is the subresource verb for the tier write path
// (PRMT-182 §4.2). It MUST appear as the trailing path segment
// exactly — anything else returns 404.
const tenantTierSuffix = ":tier"

// tenantTierRequest is the body shape for POST /v1/tenants/{id}:tier.
// IsolationTier is one of {label,row,db}; any other value (including
// the empty string) → 400.
type tenantTierRequest struct {
	IsolationTier string `json:"isolation_tier"`
}

// serveTenantTier handles /v1/tenants/{id}:tier (PRMT-182) and
// DELETE /v1/tenants/{id} (PRMT-220). Registered as the prefix
// handler for /v1/tenants/.
func (s *Server) serveTenantTier(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tenants/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}

	// DELETE /v1/tenants/{id} — hard delete (PRMT-220). No :tier suffix.
	if r.Method == http.MethodDelete {
		if strings.Contains(rest, ":") {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.serveTenantDelete(w, r, rid, rest)
		return
	}

	// POST /v1/tenants/{id}:tier only beyond DELETE.
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	if !strings.HasSuffix(rest, tenantTierSuffix) {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"tenant subresource not found", rest, r.URL.Path, rid)
		return
	}
	id := strings.TrimSuffix(rest, tenantTierSuffix)
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}

	// Admin gate (PRMT-182 §5: only admin may write). Non-admin or
	// anonymous → 403 RFC 7807 tail "forbidden", no side effects.
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Role != RoleAdmin {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"forbidden", "", r.URL.Path, rid)
		return
	}

	// Body parse. Reject empty bodies and unknown fields so a future
	// spec bump (PRMT-185/186) doesn't quietly inherit a typo'd
	// extra field.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	if len(body) == 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"empty body", "", r.URL.Path, rid)
		return
	}
	var req tenantTierRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.IsolationTier == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"isolation_tier required", "", r.URL.Path, rid)
		return
	}

	// Existence pre-check (PRMT-182 §4.2). GetTenant is the read
	// path shipped by PRMT-184; we use the (T,bool,error) idiom.
	if _, ok, err := s.st.GetTenant(r.Context(), id); err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	} else if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"tenant not found", id, r.URL.Path, rid)
		return
	}

	// Mutate. principal = p.Subject is the actor stored on the
	// tenant_audit row.
	err = s.st.UpdateTenantTier(r.Context(), id, req.IsolationTier, p.Subject)
	switch {
	case err == nil:
		// Upgrade success and equal-tier no-op both reach here;
		// the mutator decided whether an audit row was needed.
		// Echo the current tenant record so the caller can confirm
		// the new tier without a follow-up GET (mirrors serveSpareAdjust
		// which returns the updated SparePart).
		cur, _, gerr := s.st.GetTenant(r.Context(), id)
		if gerr != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", gerr)
			return
		}
		writeJSON(w, http.StatusOK, cur)
		return
	case errors.Is(err, ErrTierDowngrade):
		writeProblem(w, http.StatusConflict, "tier-downgrade",
			"isolation_tier downgrade refused", "", r.URL.Path, rid)
		return
	case errors.Is(err, tenantTierValidationError):
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad isolation_tier", req.IsolationTier, r.URL.Path, rid)
		return
	case isTenantTierNotFound(err):
		// Mid-call race: row vanished between GetTenant and
		// UpdateTenantTier. Mirror AdjustSpare's wrapped not-found
		// idiom — surface as 404.
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"tenant not found", id, r.URL.Path, rid)
		return
	default:
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
}

// serveTenantDelete implements DELETE /v1/tenants/{id} (PRMT-220).
// Platform-admin only (tenant-scoped tokens cannot delete tenants).
// Zero orgs required; else 409 tenant-owns-orgs.
func (s *Server) serveTenantDelete(w http.ResponseWriter, r *http.Request, rid, id string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	if tid, ok := TenantFromContext(r.Context()); ok {
		writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
			"platform admin required to delete tenants", tid, r.URL.Path, rid)
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	if _, ok, err := s.st.GetTenant(r.Context(), id); err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	} else if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"tenant not found", id, r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	if err := s.st.DeleteTenant(r.Context(), id, p.Subject); err != nil {
		switch {
		case errors.Is(err, ErrTenantOwnsOrgs):
			writeProblem(w, http.StatusConflict, "tenant-owns-orgs",
				"tenant still has orgs; delete orgs first", id, r.URL.Path, rid)
			return
		case strings.Contains(err.Error(), "core: delete tenant: not found"):
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"tenant not found", id, r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// isTenantTierNotFound reports whether err is the wrapped
// not-found the mutators (fileStore + pgStore) emit on an
// id-vanished-mid-call race (PRMT-182 §4.1 final bullet). Kept
// here so the handler has a single, narrow predicate instead of
// string-matching each implementation.
func isTenantTierNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "core: update tenant tier: not found")
}
