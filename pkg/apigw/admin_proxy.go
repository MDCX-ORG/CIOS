// Platform Admin identity-forwarding proxies (L109 P802/P803).
// Unlike ops read routes these use GetV1As / PostV1As / DeleteV1As
// so platform-admin tokens without a tenant claim still reach core
// (core requireOrgAdmin is the authority).
package apigw

import (
	"io"
	"net/http"
	"strings"
)

const (
	upstreamPathSiteOrgs     = "/v1/site-orgs"
	upstreamPathRoleBindings = "/v1/role-bindings"
	upstreamPathModelPacks   = "/v1/model-packs"
	upstreamPathTenants      = "/v1/tenants"
	upstreamPathOrgs         = "/v1/orgs"
)

func (s *Server) handleSiteOrgs(w http.ResponseWriter, r *http.Request) {
	s.proxyAdminV1(w, r, upstreamPathSiteOrgs, true)
}

func (s *Server) handleRoleBindings(w http.ResponseWriter, r *http.Request) {
	s.proxyAdminV1(w, r, upstreamPathRoleBindings, true)
}

func (s *Server) handleModelPacks(w http.ResponseWriter, r *http.Request) {
	// Map /api/model-packs[…] → /v1/model-packs[…]
	up := upstreamPathModelPacks + strings.TrimPrefix(r.URL.Path, "/api/model-packs")
	if up == upstreamPathModelPacks+"/" {
		up = upstreamPathModelPacks
	}
	// Allow PUT for bindings.
	s.proxyAdminV1Methods(w, r, up, true, true)
}

func (s *Server) handleSiteLayouts(w http.ResponseWriter, r *http.Request) {
	up := "/v1/site-layouts" + strings.TrimPrefix(r.URL.Path, "/api/site-layouts")
	if up == "/v1/site-layouts/" {
		up = "/v1/site-layouts"
	}
	s.proxyAdminV1Methods(w, r, up, false, true)
}

// handleTenants L109 P804: list/create tenants (platform admin).
func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	up := upstreamPathTenants + strings.TrimPrefix(r.URL.Path, "/api/tenants")
	if up == upstreamPathTenants+"/" {
		up = upstreamPathTenants
	}
	s.proxyAdminV1Methods(w, r, up, false, false)
}

// handleOrgs L109 P804: list/create orgs (platform admin, thin over /v1/orgs).
func (s *Server) handleOrgs(w http.ResponseWriter, r *http.Request) {
	up := upstreamPathOrgs + strings.TrimPrefix(r.URL.Path, "/api/orgs")
	if up == upstreamPathOrgs+"/" {
		up = upstreamPathOrgs
	}
	// Orgs support DELETE (PRMT-185).
	s.proxyAdminV1Methods(w, r, up, true, false)
}

// proxyAdminV1 forwards GET/POST (and DELETE when allowDelete) to core path.
func (s *Server) proxyAdminV1(w http.ResponseWriter, r *http.Request, upstreamBase string, allowDelete bool) {
	s.proxyAdminV1Methods(w, r, upstreamBase, allowDelete, false)
}

// proxyAdminV1Methods adds optional PUT (bindings).
func (s *Server) proxyAdminV1Methods(w http.ResponseWriter, r *http.Request, upstreamBase string, allowDelete, allowPut bool) {
	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}
	rawToken, _ := RawTokenFrom(r.Context())
	upstream := upstreamBase
	if r.URL.RawQuery != "" {
		upstream = upstreamBase + "?" + r.URL.RawQuery
	}

	var (
		status      int
		body        []byte
		contentType string
		err         error
	)
	switch r.Method {
	case http.MethodGet:
		status, body, contentType, err = s.up.GetV1As(r.Context(), claims, rawToken, upstream)
	case http.MethodPost:
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if readErr != nil {
			WriteProblem(w, http.StatusBadRequest,
				"bad-request", "read body", readErr.Error(), r.URL.Path)
			return
		}
		status, body, contentType, err = s.up.PostV1As(r.Context(), claims, rawToken, upstream, raw)
	case http.MethodPut:
		if !allowPut {
			w.Header().Set("Allow", "GET, POST, DELETE")
			WriteProblem(w, http.StatusMethodNotAllowed,
				"bad-request", "method not allowed",
				"PUT not allowed on this path", r.URL.Path)
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if readErr != nil {
			WriteProblem(w, http.StatusBadRequest,
				"bad-request", "read body", readErr.Error(), r.URL.Path)
			return
		}
		// Reuse PostV1As transport but method PUT via temporary helper.
		status, body, contentType, err = s.up.PutV1As(r.Context(), claims, rawToken, upstream, raw)
	case http.MethodDelete:
		if !allowDelete {
			w.Header().Set("Allow", "GET, POST")
			WriteProblem(w, http.StatusMethodNotAllowed,
				"bad-request", "method not allowed",
				"DELETE not allowed on this path", r.URL.Path)
			return
		}
		status, body, contentType, err = s.up.DeleteV1As(r.Context(), claims, rawToken, upstream)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"only GET, POST, PUT, DELETE supported", r.URL.Path)
		return
	}
	if err != nil {
		WriteProblem(w, http.StatusBadGateway,
			"bad-gateway", "upstream error", err.Error(), r.URL.Path)
		return
	}
	if status >= 500 {
		WriteProblem(w, http.StatusBadGateway,
			"bad-gateway", "upstream 5xx",
			"core returned "+http.StatusText(status), r.URL.Path)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else if status != http.StatusNoContent {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}
