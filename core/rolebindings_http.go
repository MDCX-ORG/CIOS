// Package core — rolebindings_http.go: /v1/role-bindings admin surface (L109 P803).
//
//	GET    /v1/role-bindings                 → list (all or ?subject=)
//	POST   /v1/role-bindings                 → put/upsert one grant
//	DELETE /v1/role-bindings?subject=&scope= → delete grant (admin)
//
// Body for POST: { "subject": "svc:…", "scope": "sgp01.**", "origin": "legacy"|"crn" }
// origin defaults to "legacy" when empty (lab path globs).
package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type roleBindingWriteRequest struct {
	Subject string `json:"subject"`
	Scope   string `json:"scope"`
	Origin  string `json:"origin"`
}

// listRoleBindingsResponse is the wire envelope for GET /v1/role-bindings.
// NextPageToken uses omitempty for admin-list legacy clients (PRMT-218).
type listRoleBindingsResponse struct {
	Items         []RoleBinding `json:"items"`
	NextPageToken string        `json:"next_page_token,omitempty"`
}

// serveRoleBindings handles /v1/role-bindings (L109 P803).
func (s *Server) serveRoleBindings(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveRoleBindingsList(w, r, rid)
	case http.MethodPost:
		s.serveRoleBindingsPut(w, r, rid)
	case http.MethodDelete:
		s.serveRoleBindingsDelete(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

func (s *Server) serveRoleBindingsList(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	// q = case-insensitive substring on subject when exact subject is empty
	// (PRMT-220 list search; reuses subject axis, filter-before-slice).
	qSearch := strings.TrimSpace(r.URL.Query().Get("q"))
	var (
		items []RoleBinding
		err   error
	)
	if subject != "" {
		items, err = s.st.ListRoleBindings(r.Context(), subject)
	} else {
		items, err = s.st.ListAllRoleBindings(r.Context())
	}
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	if items == nil {
		items = []RoleBinding{}
	}
	if subject == "" && qSearch != "" {
		ql := strings.ToLower(qSearch)
		filtered := make([]RoleBinding, 0, len(items))
		for _, rb := range items {
			if strings.Contains(strings.ToLower(rb.Subject), ql) {
				filtered = append(filtered, rb)
			}
		}
		items = filtered
	}
	// Store already returns (subject ASC, scope ASC) / scope ASC; full order stable.

	pageSize, err := parseAdminPageSize(r.URL.Query().Get("page_size"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad page_size", r.URL.Query().Get("page_size"), r.URL.Path, rid)
		return
	}
	// subject / q filters already applied above (before slice).
	// Cursor uses single-scope when exact subject is set; pair otherwise
	// (including q-only search, which can span subjects).
	exactSubject := subject != ""
	var (
		afterSub, afterScope string
		afterSingle          string
		usePair              bool
	)
	if pt := r.URL.Query().Get("page_token"); pt != "" {
		if exactSubject {
			p, ok := decodePageToken(pt)
			if !ok {
				writeProblem(w, http.StatusBadRequest, "bad-request",
					"bad page_token", "", r.URL.Path, rid)
				return
			}
			afterSingle = p
		} else {
			sk, id, ok := decodePageTokenPair(pt)
			if !ok {
				writeProblem(w, http.StatusBadRequest, "bad-request",
					"bad page_token", "", r.URL.Path, rid)
				return
			}
			afterSub, afterScope = sk, id
			usePair = true
		}
	}
	page := make([]RoleBinding, 0, pageSize+1)
	for _, rb := range items {
		if exactSubject {
			if afterSingle != "" && rb.Scope <= afterSingle {
				continue
			}
		} else if usePair {
			if rb.Subject < afterSub || (rb.Subject == afterSub && rb.Scope <= afterScope) {
				continue
			}
		}
		page = append(page, rb)
		if len(page) > pageSize {
			break
		}
	}
	var next string
	if len(page) > pageSize {
		last := page[pageSize-1]
		if exactSubject {
			next = encodePageToken(last.Scope)
		} else {
			next = encodePageTokenPair(last.Subject, last.Scope)
		}
		page = page[:pageSize]
	}
	if page == nil {
		page = []RoleBinding{}
	}
	writeJSON(w, http.StatusOK, listRoleBindingsResponse{Items: page, NextPageToken: next})
}

func (s *Server) serveRoleBindingsPut(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req roleBindingWriteRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	req.Scope = strings.TrimSpace(req.Scope)
	req.Origin = strings.TrimSpace(req.Origin)
	if req.Subject == "" || req.Scope == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"subject and scope required", "", r.URL.Path, rid)
		return
	}
	if req.Origin == "" {
		req.Origin = "legacy"
	}
	if req.Origin != "legacy" && req.Origin != "crn" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"origin must be legacy or crn", req.Origin, r.URL.Path, rid)
		return
	}
	now := time.Now().UTC()
	rb := RoleBinding{
		ID:        newRoleBindingID(),
		Subject:   req.Subject,
		Scope:     req.Scope,
		Origin:    req.Origin,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.st.PutRoleBinding(r.Context(), rb); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"put role binding", err.Error(), r.URL.Path, rid)
		return
	}
	// Return stored row (id may be existing on upsert).
	rows, err := s.st.ListRoleBindings(r.Context(), req.Subject)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	for _, row := range rows {
		if row.Scope == req.Scope {
			writeJSON(w, http.StatusOK, row)
			return
		}
	}
	writeJSON(w, http.StatusOK, rb)
}

func (s *Server) serveRoleBindingsDelete(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if subject == "" || scope == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"subject and scope query required", "", r.URL.Path, rid)
		return
	}
	if err := s.st.DeleteRoleBinding(r.Context(), subject, scope); err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
