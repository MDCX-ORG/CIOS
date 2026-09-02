// Package core — tenants_admin_http.go: GET/POST /v1/tenants (L109 P804).
//
// Complements PRMT-182's POST /v1/tenants/{id}:tier (tenants_http.go).
// Platform-admin list + create with auto `default` org (spec-001 §5bis.2).
//
//	GET  /v1/tenants  → list tenants + orgs (default_org highlighted)
//	POST /v1/tenants  → create {id, display_name}; isolation_tier=label
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

type tenantCreateRequest struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// tenantListItem is one list row: tenant + orgs + default_org pointer.
type tenantListItem struct {
	Tenant
	Orgs       []Org `json:"orgs"`
	DefaultOrg *Org  `json:"default_org,omitempty"`
}

// listTenantsResponse is the wire envelope for GET /v1/tenants.
// NextPageToken uses omitempty so clients that predate PRMT-218 never
// see an empty next_page_token key (assets list deliberately omits
// omitempty — admin lists have more legacy consumers).
type listTenantsResponse struct {
	Items         []tenantListItem `json:"items"`
	NextPageToken string           `json:"next_page_token,omitempty"`
}

type tenantCreateResponse struct {
	Tenant
	DefaultOrg Org `json:"default_org"`
}

// serveTenants handles GET/POST /v1/tenants (exact path).
func (s *Server) serveTenants(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveTenantsList(w, r, rid)
	case http.MethodPost:
		s.serveTenantsCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

func (s *Server) serveTenantsList(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	// Tenant-scoped tokens may only list their own tenant.
	if tid, ok := TenantFromContext(r.Context()); ok {
		t, found, err := s.st.GetTenant(r.Context(), tid)
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, listTenantsResponse{Items: []tenantListItem{}})
			return
		}
		item, err := s.tenantListItem(r, t)
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
		writeJSON(w, http.StatusOK, listTenantsResponse{Items: []tenantListItem{item}})
		return
	}
	all, err := s.st.ListTenants(r.Context())
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	// ListTenants is already ID ASC; re-sort for a stable full order
	// before the page slice (PRMT-218).
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	// q = case-insensitive substring on id OR display_name (PRMT-220).
	// Filter BEFORE page_token slice (assets.go:389).
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		ql := strings.ToLower(q)
		filtered := make([]Tenant, 0, len(all))
		for _, t := range all {
			if strings.Contains(strings.ToLower(t.ID), ql) ||
				strings.Contains(strings.ToLower(t.DisplayName), ql) {
				filtered = append(filtered, t)
			}
		}
		all = filtered
	}

	pageSize, err := parseAdminPageSize(r.URL.Query().Get("page_size"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad page_size", r.URL.Query().Get("page_size"), r.URL.Path, rid)
		return
	}
	var afterID string
	if pt := r.URL.Query().Get("page_token"); pt != "" {
		p, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterID = p
	}
	// Filter (cursor) BEFORE page_size slice — same order as
	// assets.go listAssets (filter before slice) so next_page_token
	// reflects the post-filter set (PRMT-218 / assets.go:389).
	pageTenants := make([]Tenant, 0, pageSize+1)
	for _, t := range all {
		if afterID != "" && t.ID <= afterID {
			continue
		}
		pageTenants = append(pageTenants, t)
		if len(pageTenants) > pageSize {
			break
		}
	}
	var next string
	if len(pageTenants) > pageSize {
		next = encodePageToken(pageTenants[pageSize-1].ID)
		pageTenants = pageTenants[:pageSize]
	}

	// One bulk fetch: removes the per-tenant ListOrgs N+1 (PRMT-214).
	// Orgs are attached only for this page of tenants (PRMT-218).
	byTenant, err := s.st.ListOrgsAll(r.Context())
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	items := make([]tenantListItem, 0, len(pageTenants))
	for _, t := range pageTenants {
		items = append(items, buildTenantListItem(t, byTenant[t.ID]))
	}
	if items == nil {
		items = []tenantListItem{}
	}
	writeJSON(w, http.StatusOK, listTenantsResponse{Items: items, NextPageToken: next})
}

// buildTenantListItem assembles one row from an already-fetched org
// slice. Pure -- no store access -- so the batch path can reuse the
// exact DefaultOrg semantics of tenantListItem.
func buildTenantListItem(t Tenant, orgs []Org) tenantListItem {
	if orgs == nil {
		orgs = []Org{}
	}
	item := tenantListItem{Tenant: t, Orgs: orgs}
	for i := range orgs {
		if orgs[i].Name == DefaultOrgName {
			o := orgs[i]
			item.DefaultOrg = &o
			break
		}
	}
	return item
}

func (s *Server) tenantListItem(r *http.Request, t Tenant) (tenantListItem, error) {
	orgs, err := s.st.ListOrgs(r.Context(), t.ID)
	if err != nil {
		return tenantListItem{}, err
	}
	return buildTenantListItem(t, orgs), nil
}

func (s *Server) serveTenantsCreate(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	// Platform admin only: tenant-scoped callers cannot mint new tenants.
	if tid, ok := TenantFromContext(r.Context()); ok {
		writeProblem(w, http.StatusForbidden, "tenant-scope-mismatch",
			"platform admin required to create tenants", tid, r.URL.Path, rid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req tenantCreateRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.ID == "" || req.DisplayName == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"id and display_name required", "", r.URL.Path, rid)
		return
	}
	if !validTenantSlug(req.ID) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad slug", req.ID, r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	t, defOrg, err := s.st.CreateTenant(r.Context(), req.ID, req.DisplayName, p.Subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrTenantExists):
			writeProblem(w, http.StatusConflict, "tenant-exists",
				"tenant already exists", req.ID, r.URL.Path, rid)
			return
		case strings.Contains(err.Error(), "invalid slug"):
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad slug", req.ID, r.URL.Path, rid)
			return
		case strings.Contains(err.Error(), "display_name required"):
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"display_name required", "", r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, tenantCreateResponse{
		Tenant:     t,
		DefaultOrg: defOrg,
	})
}
