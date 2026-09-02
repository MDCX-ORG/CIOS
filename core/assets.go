// Package core — assets.go: /v1/assets (PUT/GET/LIST/DELETE) plus
// the dedup-aware replay for PUT. Path validation is delegated to
// cpath.Dict.ParseAssetPath (V1) so we never re-implement path
// grammar; spec.type is checked against the leaf node type.
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. Lifecycle state machine   — allowedLifecycle + transition table
//   2. Collection + path routes  — serveAssetsRoot / serveAssetPath
//   3. PUT / GET / LIST          — asset CRUD with dedup-aware replay
//   4. DELETE                    — cascade vs refuse
//   5. Lifecycle sub-endpoint    — serveAssetLifecycle (PRMT-039)
//   6. Audit trail (PRMT-045)    — appendAudit + history handler
// ─────────────────────────────────────────────────────────────────────────────

// allowedLifecycle is the closed set of legal asset lifecycle
// values per spec-001 (line 313: "planned→installed→active→
// maintenance→retired"). Storage lives in Asset.Spec["lifecycle"]
// as a free-form map[string]any — no Asset schema migration needed
// (PRMT-039 §1).
var allowedLifecycle = map[string]struct{}{
	"planned":     {},
	"installed":   {},
	"active":      {},
	"maintenance": {},
	"retired":     {},
}

// defaultLifecycle is what an asset gets if Spec["lifecycle"] is
// absent on PUT (PRMT-039 §4: "PUT 落库：若 Spec['lifecycle']
// 缺省→设 'planned'").
const defaultLifecycle = "planned"

// allowedLifecycleTransition reports whether from→to is legal per
// PRMT-039 §4. planned→installed→active; active↔maintenance;
// active|maintenance→retired. Every other pair (including
// same-state) is illegal → 422 invalid-transition.
func allowedLifecycleTransition(from, to string) bool {
	switch from {
	case "planned":
		return to == "installed"
	case "installed":
		return to == "active"
	case "active":
		return to == "maintenance" || to == "retired"
	case "maintenance":
		return to == "active" || to == "retired"
	}
	return false
}

// serveAssetsRoot routes /v1/assets (no path suffix) — LIST only.
// Anything else (e.g. /v1/assets/foo) is handled by the asset
// path-handler below via a fallback ServeMux pattern.
func (s *Server) serveAssetsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listAssets(w, r)
		return
	}
	rid := RequestIDFromContext(r.Context())
	writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
		"method not allowed", "", r.URL.Path, rid)
}

// serveAssetPath routes /v1/assets/{path} on PUT/GET/DELETE, and
// also dispatches the PRMT-039 lifecycle state-machine sub-endpoint
// POST /v1/assets/{path}:lifecycle (registered through the same
// mux pattern since Go's http.ServeMux only routes by prefix, not
// by suffix — a second HandleFunc for "/v1/assets/" would silently
// overwrite this one).
//
// PRMT-045: GET /v1/assets/{path}:history is the audit-trail
// reader for the asset.
func (s *Server) serveAssetPath(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	// The mux prefix is "/v1/assets/"; everything after is the path.
	path := strings.TrimPrefix(r.URL.Path, "/v1/assets/")
	if path == "" || strings.Contains(path, "/") {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", path, r.URL.Path, rid)
		return
	}
	// PRMT-045: GET ...:history is the per-path audit list.
	if r.Method == http.MethodGet && strings.HasSuffix(path, ":history") {
		s.serveAssetHistory(w, r, strings.TrimSuffix(path, ":history"))
		return
	}
	// PRMT-039: POST ...:lifecycle is the lifecycle state-machine
	// transition (apply-role gated by the middleware).
	if r.Method == http.MethodPost && strings.HasSuffix(path, ":lifecycle") {
		assetPath := strings.TrimSuffix(path, ":lifecycle")
		s.serveAssetLifecycle(w, r, assetPath)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.putAsset(w, r, path)
	case http.MethodGet:
		s.getAsset(w, r, path)
	case http.MethodDelete:
		s.deleteAsset(w, r, path)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", path, r.URL.Path, rid)
	}
}

// putAssetRequest is the PUT body shape. All fields are optional
// from the client's perspective; missing request_id skips dedup
// (spec-004 §5: "no request_id → no dedup").
type putAssetRequest struct {
	Spec            map[string]any `json:"spec"`
	ResourceVersion int64          `json:"resource_version"`
	RequestID       string         `json:"request_id"`
}

// putAsset applies a declarative asset spec. Steps:
//  1. dedup: same (method,path,body.request_id) within 24h → replay
//  2. parse path via cpath; bad path → 400 bad-path
//  3. validate spec.type == path leaf type; mismatch → 400 bad-request
//  4. PutAsset with optimistic lock; conflict → 409 conflict
//  5. emit 201 (create) or 200 (update) Asset JSON; remember in dedup
func (s *Server) putAsset(w http.ResponseWriter, r *http.Request, path string) {
	rid := RequestIDFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	var req putAssetRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad json", err.Error(), r.URL.Path, rid)
			return
		}
	}
	// Dedup: spec-004 §5 says apply (PUT /v1/assets) replays on
	// (body.request_id) repeat. We do not require auth in M0 so we
	// key on (method,path,body.request_id) only.
	clientRID := req.RequestID
	if clientRID != "" {
		key := "PUT " + path + " " + clientRID
		if e, ok := s.dedup.lookup(key); ok {
			replayDedup(w, e)
			return
		}
	}
	// Path validation.
	ap, err := s.d.ParseAssetPath(path)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	// spec.type check.
	specType, _ := req.Spec["type"].(string)
	leaf := ap.Nodes[len(ap.Nodes)-1].Type
	if specType == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"spec.type is required", "leaf type="+leaf, r.URL.Path, rid)
		return
	}
	if specType != leaf {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"spec.type does not match path leaf",
			"spec.type="+specType+" leaf="+leaf, r.URL.Path, rid)
		return
	}
	// PRMT-039 §4: validate lifecycle membership; default when absent.
	if req.Spec == nil {
		req.Spec = map[string]any{}
	}
	if v, present := req.Spec["lifecycle"]; present {
		lc, ok := v.(string)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"spec.lifecycle must be a string", "", r.URL.Path, rid)
			return
		}
		if _, ok := allowedLifecycle[lc]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad lifecycle", lc, r.URL.Path, rid)
			return
		}
	} else {
		req.Spec["lifecycle"] = defaultLifecycle
	}
	// Existence check → status code (201 new, 200 update).
	_, existed, _ := s.st.GetAsset(r.Context(), path)
	a := Asset{Path: path, Spec: req.Spec}
	out, err := s.st.PutAsset(r.Context(), a, req.ResourceVersion)
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			writeProblem(w, http.StatusConflict, "conflict",
				"resource version conflict", out.Path, r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	status := http.StatusOK
	if !existed {
		status = http.StatusCreated
	}
	// PRMT-045: append an audit entry for the successful PUT.
	// Best-effort: a failure is logged but does not change the
	// response (asset write already succeeded; rolling back the
	// asset for an audit-write failure would be worse — §8).
	s.appendAudit(r.Context(), path, "put", auditPutDetail(existed, out.ResourceVersion))
	// Remember for dedup ONLY if the client supplied a request_id.
	if clientRID != "" {
		s.dedup.remember("PUT "+path+" "+clientRID, captureResponse(status, out))
	}
	writeJSON(w, status, out)
}

// replayDedup writes a previously-cached response back. Used for
// apply idempotency.
func replayDedup(w http.ResponseWriter, e dedupEntry) {
	if e.ct != "" {
		w.Header().Set("Content-Type", e.ct)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

// captureResponse snapshots the response so a later dedup hit can
// replay it byte-for-byte. The body bytes here must match exactly
// what writeJSON writes — both marshal with json.Marshal and emit
// a trailing "\n" so the replays are byte-equal (spec-004 §5).
func captureResponse(status int, v any) dedupEntry {
	b, _ := json.Marshal(v)
	body := append(b, '\n')
	return dedupEntry{status: status, body: body, ct: "application/json"}
}

// getAsset returns one asset or 404 path-not-found.
func (s *Server) getAsset(w http.ResponseWriter, r *http.Request, path string) {
	rid := RequestIDFromContext(r.Context())
	if _, err := s.d.ParseAssetPath(path); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	a, ok, err := s.st.GetAsset(r.Context(), path)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"asset path not found", path, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// listAssets handles GET /v1/assets with filter / type / lifecycle /
// prefix / limit / page_size / page_token / order_by. order_by only
// accepts "path" (the only field the store can sort on) and is
// silently ignored if absent. PRMT-067 adds the ops-search quartet
// (type, lifecycle, prefix, limit) on top of the existing pagination
// surface; they stack with AND.
func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	q := r.URL.Query()
	// PRMT-067: limit takes precedence over page_size when set; if
	// neither is supplied the default is DefaultPageSize (preserves
	// M0 behaviour). Cap mirrors page_size's MaxPageSize.
	pageSize := DefaultPageSize
	switch {
	case q.Get("limit") != "":
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad limit", q.Get("limit"), r.URL.Path, rid)
			return
		}
		if n > MaxPageSize {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"limit > 1000", q.Get("limit"), r.URL.Path, rid)
			return
		}
		pageSize = n
	case q.Get("page_size") != "":
		ps := q.Get("page_size")
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_size", ps, r.URL.Path, rid)
			return
		}
		if n > MaxPageSize {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"page_size > 1000", ps, r.URL.Path, rid)
			return
		}
		pageSize = n
	}
	// order_by (whitelist)
	if ob := q.Get("order_by"); ob != "" && ob != "path" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"order_by must be path", ob, r.URL.Path, rid)
		return
	}
	// filter (cpath glob). We default to a Glob that matches every
	// path (the empty pattern is rejected by CompileGlob, so use "**"
	// which is the documented "match all" form per spec-001 §2).
	glob, err := cpath.CompileGlob("**")
	if err != nil {
		// CompileGlob("**") is a constant; if it ever fails, the
		// spec is broken. Surface as 500 (the request itself is fine).
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"default glob", err.Error(), r.URL.Path, rid)
		return
	}
	if f := q.Get("filter"); f != "" {
		g, err := cpath.CompileGlob(f)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad filter", err.Error(), r.URL.Path, rid)
			return
		}
		glob = g
	}
	// page_token
	var afterPath string
	if pt := q.Get("page_token"); pt != "" {
		p, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterPath = p
	}
	all, err := s.st.ListAssets(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// PRMT-067: field filters (type / lifecycle / prefix) take the
	// all-set and narrow it BEFORE the per-item scope check so the
	// per-item scope can never be bypassed (the must-not-bypass
	// invariant). type: Asset.Type exact match — invalid/unknown
	// value simply yields the empty set. lifecycle: must be a
	// member of allowedLifecycle else 400 (matches the PUT
	// validation policy). prefix: crn path prefix (e.g.
	// "sgp01.pod002" matches everything under that subtree).
	wantType := q.Get("type")
	wantLifecycle := q.Get("lifecycle")
	if wantLifecycle != "" {
		if _, ok := allowedLifecycle[wantLifecycle]; !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad lifecycle", wantLifecycle, r.URL.Path, rid)
			return
		}
	}
	wantPrefix := q.Get("prefix")
	// PRMT-022 R2 §4.1: when auth is enabled the request carries a
	// Principal; each list item is additionally gated by authorize()
	// against the item's own path. The middleware has already let the
	// request through the role-floor gate; this filter is the per-item
	// scope check (admin bypass, viewer/operator per scope, L50
	// read-implies-subtree handled by authorize). Auth disabled
	// (hasAuth==false) → no per-item filter, M0 behaviour preserved.
	// The filter sits BEFORE the page_size slice so next_page_token
	// reflects the post-filter set.
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]Asset, 0, len(all))
	for _, a := range all {
		if afterPath != "" && a.Path <= afterPath {
			continue
		}
		if wantType != "" {
			ap, err := s.d.ParseAssetPath(a.Path)
			if err != nil {
				continue
			}
			if ap.Nodes[len(ap.Nodes)-1].Type != wantType {
				continue
			}
		}
		if wantLifecycle != "" {
			lc, _ := a.Spec["lifecycle"].(string)
			if lc == "" {
				lc = defaultLifecycle
			}
			if lc != wantLifecycle {
				continue
			}
		}
		if wantPrefix != "" && !strings.HasPrefix(a.Path, wantPrefix) {
			continue
		}
		if !glob.Match(a.Path) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, a.Path) != nil {
			continue
		}
		items = append(items, a)
	}
	// page_size slice.
	var next string
	if len(items) > pageSize {
		next = encodePageToken(items[pageSize-1].Path)
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, listAssetsResponse{Items: items, NextPageToken: next})
}

// listAssetsResponse is the envelope for /v1/assets GET.
type listAssetsResponse struct {
	Items         []Asset `json:"items"`
	NextPageToken string  `json:"next_page_token"`
}

// deleteAsset removes one asset, optionally cascading to children.
func (s *Server) deleteAsset(w http.ResponseWriter, r *http.Request, path string) {
	rid := RequestIDFromContext(r.Context())
	if _, err := s.d.ParseAssetPath(path); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	cascade := r.URL.Query().Get("cascade") == "true"
	n, err := s.st.DeleteAsset(r.Context(), path, cascade)
	if err != nil {
		if errors.Is(err, ErrHasChildren) {
			detail := "asset has " + strconv.Itoa(n) + " children; use ?cascade=true"
			writeProblem(w, http.StatusConflict, "conflict",
				"asset has children", detail, r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if n == 0 {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"asset path not found", path, r.URL.Path, rid)
		return
	}
	// PRMT-045: append audit for the successful DELETE.
	s.appendAudit(r.Context(), path, "delete", auditDeleteDetail(cascade, n))
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

// lifecycleTransitionRequest is the body of POST
// /v1/assets/{path}:lifecycle. Spec-008 v0.3 Q9 (PRMT-039 §4):
// the only field is the target state.
type lifecycleTransitionRequest struct {
	To string `json:"to"`
}

// serveAssetLifecycle handles POST /v1/assets/{path}:lifecycle. The
// route strips the ":lifecycle" suffix before reaching this handler
// (see core/server.go Handler()). Steps (PRMT-039 §4):
//
//  1. validate path (400 bad-path if cpath rejects)
//  2. parse body {to}; reject unknown fields (mirror tickets.go)
//  3. reject if `to` ∉ allowedLifecycle (422 invalid-transition)
//  4. read asset (404 if absent)
//  5. reject if allowedLifecycleTransition(cur, to) is false
//  6. write Spec["lifecycle"] = to + PutAsset with expectVersion
//     (409 conflict on race; the only writer outside this handler
//     is PUT /v1/assets/{path}, which uses the same path)
//  7. respond 200 with the new Asset JSON
//
// The middleware has already enforced apply-role (PRMT-039 §5:
// "防 037 类 RBAC 漏接") by mapping POST :lifecycle to ActionApply
// in core/authmw.go mapRequest. This handler does NOT need to
// re-run the role floor; the per-asset scope check is the same
// one PUT /v1/assets/{path} goes through (admin bypasses; operator
// needs explicit subtree scope per L50 "写显式").
func (s *Server) serveAssetLifecycle(w http.ResponseWriter, r *http.Request, path string) {
	rid := RequestIDFromContext(r.Context())
	if _, err := s.d.ParseAssetPath(path); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	var req lifecycleTransitionRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
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
	if _, ok := allowedLifecycle[req.To]; !ok {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
			"invalid lifecycle value", req.To, r.URL.Path, rid)
		return
	}
	a, ok, err := s.st.GetAsset(r.Context(), path)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"asset path not found", path, r.URL.Path, rid)
		return
	}
	cur, _ := a.Spec["lifecycle"].(string)
	if cur == "" {
		cur = defaultLifecycle
	}
	if !allowedLifecycleTransition(cur, req.To) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
			"illegal lifecycle transition", cur+"->"+req.To, r.URL.Path, rid)
		return
	}
	// Mutate a local copy so we don't poison the store's struct on
	// the write-failure path (fileStore.save restores the index on
	// error, but in-memory the get-fetched copy is ours).
	if a.Spec == nil {
		a.Spec = map[string]any{}
	}
	a.Spec["lifecycle"] = req.To
	out, err := s.st.PutAsset(r.Context(), a, a.ResourceVersion)
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			writeProblem(w, http.StatusConflict, "conflict",
				"resource version conflict", out.Path, r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// PRMT-045: append audit for the successful lifecycle transition.
	s.appendAudit(r.Context(), path, "lifecycle", auditLifecycleDetail(cur, req.To))
	writeJSON(w, http.StatusOK, out)
}

// --- PRMT-045: audit trail ----------------------------------------------

// appendAudit writes one audit row. Best-effort by contract
// (PRMT-045 §5 MUST): a save failure is logged and the request
// response is NOT modified — the asset write already succeeded
// and rolling back the asset for an audit-write failure would be
// worse than a missing audit entry. The trade-off is logged for
// §8 review; the prompt records that strict consistency can be
// restored via a write-ahead transaction in M3.
func (s *Server) appendAudit(ctx context.Context, path, op, detail string) {
	principal := "anonymous"
	if p, ok := PrincipalFromContext(ctx); ok && p.Subject != "" {
		principal = p.Subject
	}
	entry := AssetAudit{
		ID:        newAuditID(),
		TS:        time.Now().UTC(),
		Principal: principal,
		Path:      path,
		Op:        op,
		Detail:    detail,
	}
	if err := s.st.AppendAssetAudit(ctx, entry); err != nil {
		log.Printf("core: asset audit append (%s/%s): %v", op, path, err)
	}
}

// auditPutDetail formats the "version N→M" detail string.
// existed=false (new asset) → "0→1".
func auditPutDetail(existed bool, newVersion int64) string {
	if !existed {
		return "0→1"
	}
	// Update path: the previous version is newVersion-1.
	return strconv.FormatInt(newVersion-1, 10) + "→" + strconv.FormatInt(newVersion, 10)
}

// auditDeleteDetail formats "cascade=true,n=3" or "cascade=false,n=1".
func auditDeleteDetail(cascade bool, n int) string {
	return "cascade=" + strconv.FormatBool(cascade) + ",n=" + strconv.Itoa(n)
}

// auditLifecycleDetail formats "from→to" lifecycle transition.
func auditLifecycleDetail(from, to string) string {
	return from + "→" + to
}

// newAuditID produces "au_" + 16 base32 chars. Mirror of
// newTicketID / newPMScheduleID so the audit namespace is
// distinguishable in logs.
func newAuditID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "au_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// listAssetAuditsResponse is the envelope shape for the
// per-path history endpoint. Matches listTicketsResponse's
// "items" naming.
type listAssetAuditsResponse struct {
	Items []AssetAudit `json:"items"`
}

// serveAssetHistory handles GET /v1/assets/{path}:history. role
// ≥ viewer; per-item scope check on path mirrors GET /v1/assets
// behaviour.
func (s *Server) serveAssetHistory(w http.ResponseWriter, r *http.Request, path string) {
	rid := RequestIDFromContext(r.Context())
	if _, err := s.d.ParseAssetPath(path); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad asset path", err.Error(), r.URL.Path, rid)
		return
	}
	entries, err := s.st.ListAssetAudits(r.Context(), path)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	if hasAuth && authorize(principal, ActionRead, path) != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"out of scope", path, r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, listAssetAuditsResponse{Items: entries})
}
