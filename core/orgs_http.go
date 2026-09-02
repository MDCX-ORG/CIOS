// Package core — orgs_http.go: /v1/orgs admin CRUD surface
// (PRMT-185 / spec-004 v1.1 §1 / spec-001 v1.1 §5bis.2). The Org
// record is the middle scope axis tenant → Org → site; this file
// ships list/create/get/rename/delete. Every operation is
// admin-only (RoleAdmin), tenant-scoped via TenantFromContext
// (PRMT-188, R1), and audit-appended via tenant_audit (PRMT-184).
//
// Routes (registered in core/server.go):
//
//	GET    /v1/orgs                      → list by tenant
//	POST   /v1/orgs                      → create one
//	GET    /v1/orgs/{id}                 → read one
//	POST   /v1/orgs/{id}:rename          → rename
//	DELETE /v1/orgs/{id}                 → delete (R5 guarded)
//
// Contract pinned by PRMT-185 §4:
//   - Methods outside the dispatch return 405.
//   - Non-admin caller → 403, no side effects (no audit row, no Store call).
//   - Tenant scoping (R1): if TenantFromContext returns present,
//     every query/mutation is constrained to that tenant; mismatch
//     against the path or body → 403 "tenant-scope-mismatch". If
//     absent + RoleAdmin, this is an ops-realm platform admin and
//     platform-wide access is correct (tenant_id comes from the
//     query param or body).
//   - Delete uses 189's CountSitesByOrg in-tx: 0 → delete + one
//     org_delete audit row; ≥1 → 409 "org-owns-resources" with no
//     delete and no audit row. Applies uniformly to `default`.
//   - All non-2xx responses go through writeProblem / writeInternalProblem
//     (RFC 7807) with the request id from context.
//   - Audit ops are pinned to PRMT-184 vocabulary: org_create /
//     org_rename / org_delete. No new tokens introduced.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// orgIDPattern matches "og_" + 16 uppercase base32 chars. Same
// shape as spareIDPattern / newTicketID's family; the prefix alone
// is the trust-boundary sentinel.
var orgIDPattern = regexp.MustCompile(`^og_[A-Z2-7]{16}$`)

// orgRenameSuffix is the subresource verb for the rename write
// path. MUST be the trailing path segment exactly — anything else
// returns 404 (no /v1/orgs/{id} PUT, no /v1/orgs/{id}/rename).
const orgRenameSuffix = ":rename"

// orgCreateRequest is the JSON body for POST /v1/orgs. Both fields
// required. tenant_id may be omitted only when the caller is an
// ops-realm platform admin (TenantFromContext absent + RoleAdmin),
// in which case the body tenant_id is honoured as-is.
type orgCreateRequest struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// orgRenameRequest is the JSON body for POST /v1/orgs/{id}:rename.
// Name is required (slug-validated).
type orgRenameRequest struct {
	Name string `json:"name"`
}

// listOrgsResponse is the wire envelope for GET /v1/orgs.
// NextPageToken uses omitempty for admin-list legacy clients (PRMT-218).
type listOrgsResponse struct {
	Items         []Org  `json:"items"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

// serveOrgs handles /v1/orgs. Method dispatch:
//
//	GET  → list by tenant (admin only)
//	POST → create one (admin only)
func (s *Server) serveOrgs(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveOrgsList(w, r, rid)
	case http.MethodPost:
		s.serveOrgsCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveOrgsList implements GET /v1/orgs. Admin-gated, tenant-scoped
// (R1). Order: name ASC (ListOrgs contract, PRMT-184 §4).
func (s *Server) serveOrgsList(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	if tid, ok := TenantFromContext(r.Context()); ok {
		if tenantID != "" && tenantID != tid {
			writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
				"tenant scope mismatch", tenantID, r.URL.Path, rid)
			return
		}
		tenantID = tid
	}
	if tenantID == "" {
		// Absent TenantFromContext AND no query param → caller is a
		// platform admin listing across all tenants; refuse rather
		// than silently return everything.
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing tenant_id", "", r.URL.Path, rid)
		return
	}
	if _, ok, err := s.st.GetTenant(r.Context(), tenantID); err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	} else if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"tenant not found", tenantID, r.URL.Path, rid)
		return
	}
	items, err := s.st.ListOrgs(r.Context(), tenantID)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	// ListOrgs contract: name ASC within tenant (stable full order).
	pageSize, err := parseAdminPageSize(r.URL.Query().Get("page_size"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad page_size", r.URL.Query().Get("page_size"), r.URL.Path, rid)
		return
	}
	var afterName string
	if pt := r.URL.Query().Get("page_token"); pt != "" {
		p, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterName = p
	}
	// Cursor filter BEFORE page_size slice (assets.go:389 / PRMT-218).
	page := make([]Org, 0, pageSize+1)
	for _, o := range items {
		if afterName != "" && o.Name <= afterName {
			continue
		}
		page = append(page, o)
		if len(page) > pageSize {
			break
		}
	}
	var next string
	if len(page) > pageSize {
		next = encodePageToken(page[pageSize-1].Name)
		page = page[:pageSize]
	}
	if page == nil {
		page = []Org{}
	}
	writeJSON(w, http.StatusOK, listOrgsResponse{Items: page, NextPageToken: next})
}

// serveOrgsCreate implements POST /v1/orgs. Admin-gated. Validates
// slug, RFC 7807 errors for the boundary failures, ErrOrgNameConflict → 409.
func (s *Server) serveOrgsCreate(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req orgCreateRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.TenantID == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing tenant_id", "", r.URL.Path, rid)
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing name", "", r.URL.Path, rid)
		return
	}
	if !validTenantSlug(req.Name) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad slug", req.Name, r.URL.Path, rid)
		return
	}
	if tid, ok := TenantFromContext(r.Context()); ok {
		if req.TenantID != tid {
			writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
				"tenant scope mismatch", req.TenantID, r.URL.Path, rid)
			return
		}
	}
	p, _ := PrincipalFromContext(r.Context())
	o, err := s.st.CreateOrg(r.Context(), req.TenantID, req.Name, p.Subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrOrgNameConflict):
			writeProblem(w, http.StatusConflict, "org-name-conflict",
				"org name already exists in tenant", req.Name, r.URL.Path, rid)
			return
		case orgIsCreateTenantNotFound(err):
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"tenant not found", req.TenantID, r.URL.Path, rid)
			return
		case orgIsBadSlug(err):
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad slug", req.Name, r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, o)
}

// serveOrg handles /v1/orgs/{id} (GET) and /v1/orgs/{id}:rename
// (POST). Method dispatch:
//
//	GET    /{id}              → read
//	POST   /{id}:rename       → rename
//	DELETE /{id}              → delete (R5 guarded)
func (s *Server) serveOrg(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/orgs/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	id, isRename := rest, false
	if strings.HasSuffix(rest, orgRenameSuffix) {
		id = strings.TrimSuffix(rest, orgRenameSuffix)
		isRename = true
	}
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	if !orgIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad org id", id, r.URL.Path, rid)
		return
	}
	switch {
	case isRename && r.Method == http.MethodPost:
		s.serveOrgRename(w, r, rid, id)
	case !isRename && r.Method == http.MethodGet:
		s.serveOrgGet(w, r, rid, id)
	case !isRename && r.Method == http.MethodDelete:
		s.serveOrgDelete(w, r, rid, id)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveOrgGet implements GET /v1/orgs/{id}. Tenant-scoped (R1).
func (s *Server) serveOrgGet(w http.ResponseWriter, r *http.Request, rid, id string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	o, ok, err := s.st.GetOrg(r.Context(), id)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"org not found", id, r.URL.Path, rid)
		return
	}
	if tid, ok := TenantFromContext(r.Context()); ok && o.TenantID != tid {
		writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
			"tenant scope mismatch", o.TenantID, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// serveOrgRename implements POST /v1/orgs/{id}:rename. Tenant-scoped
// (R1): the loaded org's TenantID is checked against the request tenant.
func (s *Server) serveOrgRename(w http.ResponseWriter, r *http.Request, rid, id string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	o, ok, err := s.st.GetOrg(r.Context(), id)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"org not found", id, r.URL.Path, rid)
		return
	}
	if tid, ok := TenantFromContext(r.Context()); ok && o.TenantID != tid {
		writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
			"tenant scope mismatch", o.TenantID, r.URL.Path, rid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req orgRenameRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing name", "", r.URL.Path, rid)
		return
	}
	if !validTenantSlug(req.Name) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad slug", req.Name, r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	if err := s.st.RenameOrg(r.Context(), id, req.Name, p.Subject); err != nil {
		switch {
		case errors.Is(err, ErrOrgNameConflict):
			writeProblem(w, http.StatusConflict, "org-name-conflict",
				"org name already exists in tenant", req.Name, r.URL.Path, rid)
			return
		case orgIsNotFound(err):
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"org not found", id, r.URL.Path, rid)
			return
		case orgIsBadSlug(err):
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad slug", req.Name, r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	updated, _, gerr := s.st.GetOrg(r.Context(), id)
	if gerr != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", gerr)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// serveOrgDelete implements DELETE /v1/orgs/{id}. Tenant-scoped (R1).
// R5: org owns ≥1 site mapping → 409 org-owns-resources, no delete,
// no audit. Applies uniformly to `default`.
func (s *Server) serveOrgDelete(w http.ResponseWriter, r *http.Request, rid, id string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	o, ok, err := s.st.GetOrg(r.Context(), id)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"org not found", id, r.URL.Path, rid)
		return
	}
	if tid, ok := TenantFromContext(r.Context()); ok && o.TenantID != tid {
		writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
			"tenant scope mismatch", o.TenantID, r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	if err := s.st.DeleteOrg(r.Context(), id, p.Subject); err != nil {
		switch {
		case errors.Is(err, ErrOrgOwnsResources):
			writeProblem(w, http.StatusConflict, "org-owns-resources",
				"org owns resources", id, r.URL.Path, rid)
			return
		case orgIsNotFound(err):
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"org not found", id, r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireOrgAdmin returns false after writing a 403 RFC 7807
// response when the caller is not RoleAdmin. Mirrors the inline
// pattern in core/tenants_http.go and avoids duplicating the gate
// in every handler. Anonymous callers fail the PrincipalFromContext
// check (no Principal at all → not admin).
func requireOrgAdmin(w http.ResponseWriter, r *http.Request, rid string) bool {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Role != RoleAdmin {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"forbidden", "", r.URL.Path, rid)
		return false
	}
	return true
}

// orgIsCreateTenantNotFound matches the wrapped "tenant not found"
// error the fileStore CreateOrg emits when the body tenant_id does
// not resolve (PRMT-185 §4.1: tenant_id must reference an existing
// tenant). The pgStore path maps FK violations to the same shape.
func orgIsCreateTenantNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "core: create org: tenant not found")
}

// orgIsNotFound matches the wrapped not-found error shape the
// fileStore rename/delete mutators emit on id-vanished-mid-call
// races (mirrors isTenantTierNotFound from core/tenants_http.go).
func orgIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "core: rename org: not found") ||
		strings.Contains(s, "core: delete org: not found")
}

// orgIsBadSlug matches the wrapped "invalid slug" error the
// fileStore CreateOrg / RenameOrg emit when the slug fails
// validTenantSlug. The HTTP boundary re-validates so this branch
// is unreachable in practice; it survives as a defensive surface in
// case a future Store impl surfaces the error differently.
func orgIsBadSlug(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "core: create org: invalid slug") ||
		strings.Contains(s, "core: rename org: invalid slug")
}
