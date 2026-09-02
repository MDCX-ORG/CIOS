// Package core — siteorg_http.go: /v1/site-orgs admin surface (L109 P802).
//
//	GET    /v1/site-orgs              → list site→org mappings (admin)
//	POST   /v1/site-orgs              → attach/re-home site to org (admin)
//	DELETE /v1/site-orgs?site=X       → detach site (PRMT-220)
//
// Body for POST: { "site": "sgp01", "org_id": "og_…" }
// City/timezone site metadata is not on SiteOrg yet — registry = slug + org only.
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

type siteOrgAttachRequest struct {
	Site  string `json:"site"`
	OrgID string `json:"org_id"`
}

// listSiteOrgsResponse is the wire envelope for GET /v1/site-orgs.
// NextPageToken uses omitempty for admin-list legacy clients (PRMT-218).
type listSiteOrgsResponse struct {
	Items         []SiteOrg `json:"items"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}

// serveSiteOrgs handles /v1/site-orgs (L109 P802 + PRMT-220 DELETE).
func (s *Server) serveSiteOrgs(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveSiteOrgsList(w, r, rid)
	case http.MethodPost:
		s.serveSiteOrgsAttach(w, r, rid)
	case http.MethodDelete:
		s.serveSiteOrgsDetach(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

func (s *Server) serveSiteOrgsList(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	all, err := s.st.ListSiteOrgs(r.Context())
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if all == nil {
		all = []SiteOrg{}
	}
	// Optional filters — BEFORE page_size slice (assets.go:389 / PRMT-220).
	if orgID := r.URL.Query().Get("org_id"); orgID != "" {
		filtered := make([]SiteOrg, 0, len(all))
		for _, so := range all {
			if so.OrgID == orgID {
				filtered = append(filtered, so)
			}
		}
		all = filtered
	}
	// q = case-insensitive substring match on site slug (PRMT-220 search).
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		ql := strings.ToLower(q)
		filtered := make([]SiteOrg, 0, len(all))
		for _, so := range all {
			if strings.Contains(strings.ToLower(so.Site), ql) {
				filtered = append(filtered, so)
			}
		}
		all = filtered
	}
	// Stable full order: site ASC (ListSiteOrgs already sorts; reaffirm).
	sort.Slice(all, func(i, j int) bool { return all[i].Site < all[j].Site })

	pageSize, err := parseAdminPageSize(r.URL.Query().Get("page_size"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad page_size", r.URL.Query().Get("page_size"), r.URL.Path, rid)
		return
	}
	var afterSite string
	if pt := r.URL.Query().Get("page_token"); pt != "" {
		p, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterSite = p
	}
	page := make([]SiteOrg, 0, pageSize+1)
	for _, so := range all {
		if afterSite != "" && so.Site <= afterSite {
			continue
		}
		page = append(page, so)
		if len(page) > pageSize {
			break
		}
	}
	var next string
	if len(page) > pageSize {
		next = encodePageToken(page[pageSize-1].Site)
		page = page[:pageSize]
	}
	if page == nil {
		page = []SiteOrg{}
	}
	writeJSON(w, http.StatusOK, listSiteOrgsResponse{Items: page, NextPageToken: next})
}

func (s *Server) serveSiteOrgsAttach(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req siteOrgAttachRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	req.Site = strings.TrimSpace(req.Site)
	req.OrgID = strings.TrimSpace(req.OrgID)
	if req.Site == "" || req.OrgID == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"site and org_id required", "", r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	if err := s.st.AttachSiteToOrg(r.Context(), req.Site, req.OrgID, p.Subject); err != nil {
		switch {
		case errors.Is(err, siteSlugError):
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"invalid site slug", req.Site, r.URL.Path, rid)
			return
		case errors.Is(err, siteOrgNotFoundError):
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"org not found", req.OrgID, r.URL.Path, rid)
			return
		default:
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
	}
	so, ok, err := s.st.GetSiteOrg(r.Context(), req.Site)
	if err != nil || !ok {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error after attach", err)
		return
	}
	writeJSON(w, http.StatusOK, so)
}

// serveSiteOrgsDetach implements DELETE /v1/site-orgs?site=X (PRMT-220).
func (s *Server) serveSiteOrgsDetach(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	site := strings.TrimSpace(r.URL.Query().Get("site"))
	if site == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"site query required", "", r.URL.Path, rid)
		return
	}
	p, _ := PrincipalFromContext(r.Context())
	if err := s.st.DetachSiteFromOrg(r.Context(), site, p.Subject); err != nil {
		if strings.Contains(err.Error(), "core: detach site from org: not found") {
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"site not found", site, r.URL.Path, rid)
			return
		}
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
