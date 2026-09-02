// Package core — modelpacks_http.go: /v1/model-packs admin API (L109 P811–P813).
//
//	GET  /v1/model-packs
//	POST /v1/model-packs:import          (multipart USD import + usdlint gate)
//	GET  /v1/model-packs/{type}/{model}
//	GET  /v1/model-packs/{type}/{model}:export
//	POST /v1/model-packs/{type}/{model}:lint
//	GET|PUT /v1/model-packs/{type}/{model}/bindings
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type listModelPacksResponse struct {
	Items []ModelPackSummary `json:"items"`
}

type modelPackDetailResponse struct {
	Pack     ModelPackSummary `json:"pack"`
	Bindings ModelBindingsDoc `json:"bindings"`
	Lint     *ModelLintReport `json:"lint,omitempty"`
}

type putBindingsRequest struct {
	Notes    string         `json:"notes"`
	Bindings []ModelBinding `json:"bindings"`
}

// serveModelPacks handles GET /v1/model-packs and POST /v1/model-packs:import
// (import is also accepted as POST /v1/model-packs with multipart).
func (s *Server) serveModelPacks(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	// Import: POST /v1/model-packs:import (routed via trailing path) is on serveModelPack.
	if r.Method == http.MethodGet {
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		items, err := ListModelPacks()
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "list packs", err)
			return
		}
		writeJSON(w, http.StatusOK, listModelPacksResponse{Items: items})
		return
	}
	if r.Method == http.MethodPost {
		// Allow POST /v1/model-packs as import shorthand (Content-Type multipart).
		s.serveModelPackImport(w, r, rid)
		return
	}
	writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
		"method not allowed", "", r.URL.Path, rid)
}

// serveModelPack handles /v1/model-packs/{type}/{model}[…]
func (s *Server) serveModelPack(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/model-packs/")
	if rest == "" || rest == r.URL.Path {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"not found", "", r.URL.Path, rid)
		return
	}
	// bindings subpath
	if strings.HasSuffix(rest, "/bindings") {
		core := strings.TrimSuffix(rest, "/bindings")
		core = strings.TrimSuffix(core, "/")
		typ, model, ok := splitTypeModel(core)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad pack path", rest, r.URL.Path, rid)
			return
		}
		s.serveModelPackBindings(w, r, rid, typ, model)
		return
	}
	// import at /v1/model-packs/:import or "import"
	if rest == "import" || rest == ":import" || rest == "import/" {
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.serveModelPackImport(w, r, rid)
		return
	}
	// :export verb — stream validated vendor USD out for handoff.
	if strings.HasSuffix(rest, ":export") {
		core := strings.TrimSuffix(rest, ":export")
		typ, model, ok := splitTypeModel(core)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad pack path", rest, r.URL.Path, rid)
			return
		}
		if r.Method != http.MethodGet {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		path, err := packFilePath(typ, model)
		if err != nil {
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"pack not found", typ+"/"+model, r.URL.Path, rid)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "open pack", err)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		name := filepath.Base(path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		if st != nil {
			w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		return
	}
	// :lint verb
	if strings.HasSuffix(rest, ":lint") {
		core := strings.TrimSuffix(rest, ":lint")
		typ, model, ok := splitTypeModel(core)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad pack path", rest, r.URL.Path, rid)
			return
		}
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		rep, err := RunModelPackLint(typ, model)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeProblem(w, http.StatusNotFound, "path-not-found",
					"pack not found", typ+"/"+model, r.URL.Path, rid)
				return
			}
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "lint error", err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}
	// :conform verb (P814 G8 S-first)
	if strings.HasSuffix(rest, ":conform") {
		core := strings.TrimSuffix(rest, ":conform")
		typ, model, ok := splitTypeModel(core)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad pack path", rest, r.URL.Path, rid)
			return
		}
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var req struct {
			DryRun   bool              `json:"dry_run"`
			Mode     string            `json:"mode"`     // optional; "g8" is the only supported mode (H2)
			Mappings map[string]string `json:"mappings"` // prim → to_type overrides
		}
		if len(body) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				writeProblem(w, http.StatusBadRequest, "bad-request",
					"bad json", err.Error(), r.URL.Path, rid)
				return
			}
		}
		if req.Mode != "" && !strings.EqualFold(req.Mode, "g8") {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"unsupported conform mode", req.Mode, r.URL.Path, rid)
			return
		}
		// query dry_run=1 also works
		if r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true" {
			req.DryRun = true
		}
		p, _ := PrincipalFromContext(r.Context())
		rep, err := RunG8Conform(s.d, typ, model, p.Subject, req.DryRun, req.Mappings)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeProblem(w, http.StatusNotFound, "path-not-found",
					"pack not found", typ+"/"+model, r.URL.Path, rid)
				return
			}
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"conform failed", err.Error(), r.URL.Path, rid)
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}
	// GET vocab helper for UI
	if rest == "vocab" || rest == "vocab/" {
		if r.Method != http.MethodGet {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		if !requireOrgAdmin(w, r, rid) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"types": LoadTypeVocabList(s.d)})
		return
	}
	// detail GET
	typ, model, ok := splitTypeModel(rest)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad pack path", rest, r.URL.Path, rid)
		return
	}
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	pack, bindings, lint, err := GetModelPackDetail(typ, model)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeProblem(w, http.StatusNotFound, "path-not-found",
				"pack not found", typ+"/"+model, r.URL.Path, rid)
			return
		}
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
			"bad-request", "store error", err)
		return
	}
	writeJSON(w, http.StatusOK, modelPackDetailResponse{
		Pack: pack, Bindings: bindings, Lint: lint,
	})
}

// serveModelPackImport accepts multipart form:
//
//	type   — asset type segment (e.g. cdu)
//	model  — model name segment (e.g. DC45)
//	file   — .usda / .usdc binary
//	force  — optional "1"/"true" to overwrite
//
// Validates with usdlint before the pack is kept (hard gate).
func (s *Server) serveModelPackImport(w http.ResponseWriter, r *http.Request, rid string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	// Bound body (file + fields).
	if err := r.ParseMultipartForm(MaxUSDImportBytes + 1<<20); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad multipart", err.Error(), r.URL.Path, rid)
		return
	}
	typ := strings.TrimSpace(r.FormValue("type"))
	model := strings.TrimSpace(r.FormValue("model"))
	force := r.FormValue("force") == "1" || strings.EqualFold(r.FormValue("force"), "true")
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"file required", err.Error(), r.URL.Path, rid)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxUSDImportBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read file", err.Error(), r.URL.Path, rid)
		return
	}
	if len(data) > MaxUSDImportBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "bad-request",
			"file too large", strconv.Itoa(MaxUSDImportBytes), r.URL.Path, rid)
		return
	}
	name := "pack.usda"
	if hdr != nil && hdr.Filename != "" {
		name = hdr.Filename
	}
	res, err := ImportUSDPack(typ, model, name, data, force)
	if err != nil {
		switch {
		case errors.Is(err, ErrUSDImportExists):
			writeProblem(w, http.StatusConflict, "conflict",
				"pack exists", typ+"/"+model+" (pass force=1 to overwrite)", r.URL.Path, rid)
		case errors.Is(err, ErrUSDImportValidation):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"type":   "about:blank",
				"title":  "validation-failed",
				"status": http.StatusUnprocessableEntity,
				"detail": err.Error(),
				"lint":   res.Lint,
			})
		default:
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"import failed", err.Error(), r.URL.Path, rid)
		}
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) serveModelPackBindings(w http.ResponseWriter, r *http.Request, rid, typ, model string) {
	if !requireOrgAdmin(w, r, rid) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		_, doc, _, err := GetModelPackDetail(typ, model)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeProblem(w, http.StatusNotFound, "path-not-found",
					"pack not found", typ+"/"+model, r.URL.Path, rid)
				return
			}
			writeInternalProblem(w, r.Context(), http.StatusInternalServerError,
				"bad-request", "store error", err)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"read body", err.Error(), r.URL.Path, rid)
			return
		}
		var req putBindingsRequest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad json", err.Error(), r.URL.Path, rid)
			return
		}
		p, _ := PrincipalFromContext(r.Context())
		doc, err := SaveBindings(typ, model, p.Subject, req.Notes, req.Bindings)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeProblem(w, http.StatusNotFound, "path-not-found",
					"pack not found", typ+"/"+model, r.URL.Path, rid)
				return
			}
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"save bindings", err.Error(), r.URL.Path, rid)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

func splitTypeModel(rest string) (typ, model string, ok bool) {
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	typ, model = sanitizeSeg(parts[0]), sanitizeSeg(parts[1])
	if typ == "" || model == "" {
		return "", "", false
	}
	return typ, model, true
}
