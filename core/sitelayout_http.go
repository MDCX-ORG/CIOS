// Package core — sitelayout_http.go: /v1/site-layouts admin API (L109 P821–P825).
//
//	GET    /v1/site-layouts
//	GET    /v1/site-layouts/{site}
//	PUT    /v1/site-layouts/{site}
//	POST   /v1/site-layouts/{site}:writeback[?rebuild_scene=1]
//	POST   /v1/site-layouts/{site}:rebuild-scene
//	GET    /v1/site-layouts/{site}/scene-job
package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type listLayoutsResponse struct {
	Sites     []string `json:"sites"`
	Relations []string `json:"relations"` // protocol types.yaml relations (PRMT-223)
}

// serveSiteLayoutsRoot GET /v1/site-layouts
func (s *Server) serveSiteLayoutsRoot(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	sites, err := ListSiteLayoutSites()
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "list layouts", err)
		return
	}
	rels := LayoutRelationNames(s.d)
	if rels == nil {
		rels = []string{}
	}
	writeJSON(w, http.StatusOK, listLayoutsResponse{Sites: sites, Relations: rels})
}

// serveSiteLayout /v1/site-layouts/{site}[…]
func (s *Server) serveSiteLayout(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/site-layouts/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing site", "", r.URL.Path, rid)
		return
	}
	if strings.HasSuffix(rest, ":writeback") {
		site := strings.TrimSuffix(rest, ":writeback")
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.serveSiteLayoutWriteback(w, r, rid, site)
		return
	}
	if strings.HasSuffix(rest, ":rebuild-scene") {
		site := strings.TrimSuffix(rest, ":rebuild-scene")
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.serveSiteLayoutRebuildScene(w, r, rid, site)
		return
	}
	// GET …/{site}/scene-job
	if strings.HasSuffix(rest, "/scene-job") {
		site := strings.TrimSuffix(rest, "/scene-job")
		site = strings.TrimSuffix(site, "/")
		if r.Method != http.MethodGet {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.serveSiteLayoutSceneJob(w, r, rid, site)
		return
	}
	site := sanitizeSeg(rest)
	if site == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad site", rest, r.URL.Path, rid)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		lay, err := LoadSiteLayout(site)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"load layout", err.Error(), r.URL.Path, rid)
			return
		}
		writeJSON(w, http.StatusOK, lay)
	case http.MethodPut:
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"read body", err.Error(), r.URL.Path, rid)
			return
		}
		var lay SiteLayout
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&lay); err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad json", err.Error(), r.URL.Path, rid)
			return
		}
		lay.Site = site
		p, _ := PrincipalFromContext(r.Context())
		saved, err := SaveSiteLayout(lay, p.Subject, s.d)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"save layout", err.Error(), r.URL.Path, rid)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

func (s *Server) serveSiteLayoutWriteback(w http.ResponseWriter, r *http.Request, rid, site string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	site = sanitizeSeg(site)
	lay, err := LoadSiteLayout(site)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"load layout", err.Error(), r.URL.Path, rid)
		return
	}
	// Optional body overrides layout before writeback (M1: honor empty
	// instances = clear layout; surface decode/save errors).
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		var override SiteLayout
		if err := json.Unmarshal(body, &override); err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad writeback body", err.Error(), r.URL.Path, rid)
			return
		}
		override.Site = site
		p, _ := PrincipalFromContext(r.Context())
		saved, err := SaveSiteLayout(override, p.Subject, s.d)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"save layout override", err.Error(), r.URL.Path, rid)
			return
		}
		lay = saved
	}
	p, _ := PrincipalFromContext(r.Context())
	saved, res, err := WritebackSiteLayout(r.Context(), s.st, lay, p.Subject, s.d)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"writeback failed", err.Error(), r.URL.Path, rid)
		return
	}
	out := map[string]any{
		"layout":    saved,
		"writeback": res,
	}
	// Optional async Scene Engine kick (P825): ?rebuild_scene=1|true
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("rebuild_scene")))
	if q == "1" || q == "true" || q == "yes" {
		job, jerr := KickSceneRebuild(site)
		if jerr != nil {
			out["scene_job_error"] = jerr.Error()
		} else {
			out["scene_job"] = job
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serveSiteLayoutRebuildScene(w http.ResponseWriter, r *http.Request, rid, site string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	site = sanitizeSeg(site)
	job, err := KickSceneRebuild(site)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"rebuild-scene", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) serveSiteLayoutSceneJob(w http.ResponseWriter, r *http.Request, rid, site string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	site = sanitizeSeg(site)
	job, err := LoadSceneJob(site)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"scene-job", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, job)
}
