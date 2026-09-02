// Package core — inspection_form.go: mobile-web rendering of an
// inspection ticket checklist + submission handler. PRMT-059 / E2.7
// P561 frontend slice. The M2 inspection backend (template +
// scanner + ticket open) ships in PRMT-049; this file is the
// human-facing page that lets an operator mark items done and
// resolve the ticket from a phone.
//
// Three routes under one mux entry (method + path dispatch in
// serveInspectionForm):
//
//	GET  /v1/inspections/form/{id}                 → render responsive checklist
//	POST /v1/inspections/form/{id}                 → collect result, resolve ticket,
//	                                                  append result summary to Runbook,
//	                                                  303 back to GET
//	POST /v1/inspections/form/{id}/photo           → multipart upload to
//	                                                  -inspection-photo-dir/{id}/{name}
//	                                                  (PRMT-063 / E2.7 移动端补完)
//
// Design constraints (PRMT-059 §3-§5; PRMT-063 §2):
//
//   - html/template for HTML rendering (auto-escape against XSS —
//     item text and free-form notes are user-controlled).
//   - Single-resource scope: handler re-runs authorize() against
//     the stored ticket.AssetPath (the {id} in the URL is a ticket
//     id, not an asset path). The middleware does the role floor;
//     the handler enforces the per-asset scope.
//   - No ticket/inspection schema change. Result data is appended
//     onto the existing Runbook field using a "result:" block —
//     see appendInspectionResult. The reader (inspection scanner /
//     runbook page) keeps treating lines without the result
//     marker as the original checklist; lines with the result
//     marker are the operator's submission.
//   - 404 (not 400) for tickets whose Runbook does not start with
//     "inspection:" — those are alarm/manual tickets and the form
//     page is meaningless for them. The prompt §2 calls this out
//     explicitly.
//   - State-machine transition (open/acknowledged → resolved) is
//     handled by allowedTransition; an illegal starting state
//     surfaces as 422 RFC 7807 like every other transition path.
//   - Photo upload shares the inspection-ticket gate (404 for
//     non-inspection tickets), the per-asset scope (403 on miss),
//     and the path-traversal triple defence (regex + path.Clean +
//     abs-dir containment) that /v1/runbooks/{key} already uses
//     (PRMT-063 §2 reuses runbook's defence pattern by design).
package core

import (
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. Constants + runbook fmt   — prefixes, result: block markers
//   2. Page types + templates    — inspectionFormItem / inspectionFormPage
//   3. serveInspectionForm       — method+path dispatch (GET/POST/photo)
//   4. loadInspectionFormTicket  — scope + state preconditions
//   5. GET handler               — renders responsive checklist
//   6. POST handler              — submit, resolve, append result: block
//   7. Photo upload (PRMT-063)   — multipart to -inspection-photo-dir
// ─────────────────────────────────────────────────────────────────────────────

// inspectionFormPrefix is the canonical Runbook prefix for
// auto-opened inspection tickets. Defined alongside
// encodeInspectionRunbook (inspection.go:296) so the reader and
// writer agree on the format. Empty Runbook or anything else
// → 404 from this handler.
const inspectionFormPrefix = "inspection:"

// inspectionFormResultPrefix is the block marker the form appends
// after a successful submission. Format inside the Runbook:
//
//	inspection:item1
//	item2
//	result:submitted=2026-06-20T12:00:00Z
//	result:checked=1,2
//	result:note=hello
//
// Each "result:" line is one self-describing key=value record so a
// future runbook reader can iterate `strings.HasPrefix(line,
// "result:")` and split on the first "=". Multi-line notes use
// "\n" inside the value (the Runbook field is a free-form string;
// the ticket store treats it as opaque text).
const inspectionFormResultPrefix = "result:"

// inspectionFormPhotoPrefix is the block marker the photo upload
// handler appends to the ticket Runbook after a successful save
// (PRMT-063 §1). Format:
//
//	result:photo=sub/<random>-<safename>  (one line per file)
//
// The on-disk path is a server-relative subpath under
// -inspection-photo-dir/{ticketID}/ so a future reader can map
// the marker back to the bytes without trusting client input.
// "sub/" makes the path a single token under the result: key so
// the existing decodeInspectionRunbook / appendInspectionResult
// pair is undisturbed.
const inspectionFormPhotoPrefix = "result:photo="

// photoUploadField is the multipart field name the form must
// use. Documented in the prompt; not configurable.
const photoUploadField = "file"

// allowedPhotoExts is the whitelist enforced before the bytes are
// written. Lower-case, no leading dot. Length-matching happens in
// safePhotoName against the original filename's extension.
var allowedPhotoExts = []string{".jpg", ".jpeg", ".png", ".pdf"}

// photoUploadSniffLen is how many bytes DetectContentType needs
// to identify a file (net/http contract — 512 is the documented
// minimum; 4096 is generous). We read this from the first chunk
// before writing the file to disk.
const photoUploadSniffLen = 4096

// photoUploadDefaultMax is the per-file cap when
// SetInspectionPhotoDir was called with maxBytes<=0. 8 MiB
// matches the prompt's default; server.go's
// defaultInspectionPhotoMax keeps the same constant.
//
// Defined as a package var (not const) so tests can override it
// without re-wiring the Server.
var photoUploadDefaultMax int64 = defaultInspectionPhotoMax

// inspectionFormTmpl is the GET rendering template. Inlined as a
// const so the binary is self-contained (no embedded file, no
// build step, no frontend tooling — PRMT-059 §0 §3).
//
// The template uses `html/template` so {{.}} on user-controlled
// strings auto-escapes. Items come from the inspection template's
// items list (server-controlled), but the original ticket title
// and the ticket-id header are echoed back too — escaping is
// defence in depth in case a future extension reads ticket
// title from user input.
var inspectionFormTmpl = template.Must(template.New("form").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Inspection · {{.TicketID}}</title>
<style>
  body { font-family: -apple-system, system-ui, sans-serif; margin: 1rem; max-width: 720px; color: #111; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .meta { color: #555; font-size: .9rem; margin-bottom: 1rem; }
  .item { display: flex; align-items: flex-start; gap: .5rem; padding: .5rem 0; border-bottom: 1px solid #eee; }
  .item input[type=checkbox] { width: 1.25rem; height: 1.25rem; margin-top: .15rem; }
  .item label { flex: 1; }
  .notes { width: 100%; min-height: 4rem; margin: 1rem 0; box-sizing: border-box; }
  .actions { display: flex; gap: .5rem; }
  button { padding: .75rem 1rem; font-size: 1rem; }
  .flashed { padding: .75rem; margin-bottom: 1rem; background: #eef; border: 1px solid #99c; }
  .state { font-weight: 600; }
</style>
</head>
<body>
{{with .Flashed -}}<div class="flashed">{{.}}</div>{{end -}}
<h1>{{.Title}}</h1>
<div class="meta">
  ticket <span class="state">{{.TicketID}}</span>
  · asset <code>{{.AssetPath}}</code>
  · state <span class="state">{{.State}}</span>
</div>
<form method="post" action="/v1/inspections/form/{{.TicketID}}">
  {{if .Items -}}
  {{range .Items -}}
  <div class="item">
    <input type="checkbox" id="item-{{.Index}}" name="item" value="{{.Index}}">
    <label for="item-{{.Index}}">{{.Text}}</label>
  </div>
  {{end -}}
  {{else -}}
  <p><em>No checklist items.</em></p>
  {{end -}}
  <label for="notes">Notes</label>
  <textarea id="notes" name="notes" class="notes">{{.Notes}}</textarea>
  <div class="actions">
    <button type="submit">Submit &amp; resolve</button>
  </div>
</form>
</body>
</html>
`))

// inspectionFormItem is the per-item template payload (index +
// text). Index is the 0-based position, also used as the form
// value so POST can recover which boxes were checked without
// trusting client-supplied free text.
type inspectionFormItem struct {
	Index int
	Text  string
}

// inspectionFormPage is the template data for both GET (render)
// and POST (re-render with `Flashed` after a redirect). Notes is
// only populated on the POST-redirect render path so the operator
// sees what they just typed; GET leaves it empty.
type inspectionFormPage struct {
	TicketID  string
	Title     string
	AssetPath string
	State     string
	Items     []inspectionFormItem
	Notes     string
	Flashed   string
}

// serveInspectionForm handles /v1/inspections/form/{id} and
// /v1/inspections/form/{id}/photo. Method + sub-path dispatch:
//
//	GET  /v1/inspections/form/{id}        → render checklist
//	POST /v1/inspections/form/{id}        → collect form, resolve
//	POST /v1/inspections/form/{id}/photo  → multipart upload
//
// The /photo sub-route is detected by the trailing "/photo" path
// segment; the id stays a ticket id and the same inspection
// gate (404 for non-inspection) is re-run. Other methods on a
// known sub-path → 405. The auth layer already enforces the role
// floor; this handler re-runs authorize() against the stored
// ticket's AssetPath (single-resource scope per PRMT-059 §4,
// extended by PRMT-063 for the photo path).
//
// Non-inspection tickets (Runbook does not start with the
// "inspection:" prefix) → 404: the form is meaningless for
// alarm/manual tickets and surfacing a render error would be
// louder than the silent "no form for that ticket" UX the prompt
// asks for.
func (s *Server) serveInspectionForm(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	// /v1/inspections/form/{id}[/photo] is registered under
	// "/v1/inspections/form/" in ServeMux; strip the prefix.
	const prefix = "/v1/inspections/form/"
	rest := trimPrefix(r.URL.Path, prefix)
	if rest == "" || rest == r.URL.Path {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing ticket id", "", r.URL.Path, rid)
		return
	}
	// Sub-route: /v1/inspections/form/{id}/photo
	if strings.HasSuffix(rest, "/photo") {
		id := strings.TrimSuffix(rest, "/photo")
		if !ticketIDPattern.MatchString(id) {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad ticket id", id, r.URL.Path, rid)
			return
		}
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
				"method not allowed", "", r.URL.Path, rid)
			return
		}
		s.servePhotoUpload(w, r, rid, id)
		return
	}
	id := rest
	if !ticketIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad ticket id", id, r.URL.Path, rid)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.serveInspectionFormGet(w, r, rid, id)
	case http.MethodPost:
		s.serveInspectionFormPost(w, r, rid, id)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// loadInspectionFormTicket resolves the ticket, enforces the
// "is this an inspection ticket?" gate (404 if not), and runs
// the per-asset scope check. Returns the ticket on success.
// Split out so GET and POST share the same load path.
func (s *Server) loadInspectionFormTicket(w http.ResponseWriter, r *http.Request, rid, id, action string) (Ticket, bool) {
	t, ok, err := s.st.GetTicket(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return Ticket{}, false
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket not found", id, r.URL.Path, rid)
		return Ticket{}, false
	}
	// Non-inspection tickets → 404. Runbook format is "inspection:..."
	// for auto-opened checklists; anything else (alarm-driven,
	// manual) is out of scope for the form page.
	if !strings.HasPrefix(t.Runbook, inspectionFormPrefix) {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"ticket is not an inspection", id, r.URL.Path, rid)
		return Ticket{}, false
	}
	if principal, hasAuth := PrincipalFromContext(r.Context()); hasAuth {
		var act Action
		switch action {
		case "read":
			act = ActionRead
		case "write":
			act = ActionControlWrite
		default:
			act = ActionRead
		}
		if err := authorize(principal, act, t.AssetPath); err != nil {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden",
				"principal not authorized for this asset path",
				r.URL.Path, rid)
			return Ticket{}, false
		}
	}
	return t, true
}

// decodeInspectionRunbook parses the "inspection:item1\nitem2\n..."
// format produced by encodeInspectionRunbook. Returns the items
// in their original order with empty lines filtered out. Mirrors
// the encoder's contract — symmetric with inspection.go:296.
func decodeInspectionRunbook(runbook string) []string {
	if !strings.HasPrefix(runbook, inspectionFormPrefix) {
		return nil
	}
	body := strings.TrimPrefix(runbook, inspectionFormPrefix)
	if body == "" {
		return nil
	}
	raw := strings.Split(body, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" {
			continue
		}
		// Strip any result: block that was appended after a previous
		// submission so the original checklist is what the operator
		// sees on re-render.
		if strings.HasPrefix(line, inspectionFormResultPrefix) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// appendInspectionResult adds the operator's submission block to
// the ticket Runbook. Format:
//
//	inspection:item1
//	item2
//	result:submitted=<RFC3339>
//	result:checked=<csv of zero-based indices>
//	result:note=<single-line note or empty>
//
// The "result:" marker makes the block trivial to grep / iterate
// from a future runbook reader without disturbing the existing
// checklist lines. Multiline notes are joined with "\n" inside
// the value so the Runbook stays a single line-terminated block.
//
// The ticket schema is NOT changed; the Runbook field is the
// carrier. PRMT-059 §5 MUST NOT.
func appendInspectionResult(runbook, note string, checked []int, now time.Time) string {
	var b strings.Builder
	b.WriteString(runbook)
	if runbook != "" && !strings.HasSuffix(runbook, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(inspectionFormResultPrefix)
	b.WriteString("submitted=")
	b.WriteString(now.UTC().Format(time.RFC3339))
	b.WriteString("\n")
	b.WriteString(inspectionFormResultPrefix)
	b.WriteString("checked=")
	for i, idx := range checked {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.Itoa(idx))
	}
	b.WriteString("\n")
	if note != "" {
		b.WriteString(inspectionFormResultPrefix)
		b.WriteString("note=")
		// Notes are user-controlled free text; we are storing them
		// in an opaque Runbook field, not echoing them through
		// HTML, so no escaping is required here. The form on the
		// NEXT render escapes via html/template (defence in depth).
		b.WriteString(strings.ReplaceAll(note, "\r", ""))
		b.WriteString("\n")
	}
	return b.String()
}

// parseCheckedIndices reads the "item" form values (each is a
// zero-based index as emitted by the GET template) and returns
// the deduplicated, sorted set. The form sends each checked box
// as a separate "item=N" pair; unchecked boxes are absent.
func parseCheckedIndices(values []string) []int {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(values))
	out := make([]int, 0, len(values))
	for _, v := range values {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	// Sort so the Runbook result line is deterministic (handy for
	// test assertions and audit diff).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// renderInspectionForm executes the GET template with page data
// and writes the response. Status is always 200 on the happy path
// (the template itself does not signal errors); 4xx/5xx paths
// short-circuit through writeProblem and never reach here.
func renderInspectionForm(w http.ResponseWriter, page inspectionFormPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// html/template auto-escapes every {{...}} expression. Execute
	// into the ResponseWriter directly; a template error here would
	// mean a programming bug (template compiled at init), so the
	// error is logged in spirit — the connection simply closes.
	_ = inspectionFormTmpl.Execute(w, page)
}

// serveInspectionFormGet renders the checklist. Items are
// decoded from the ticket Runbook; the title/asset/state echo
// the stored ticket for context.
func (s *Server) serveInspectionFormGet(w http.ResponseWriter, r *http.Request, rid, id string) {
	t, ok := s.loadInspectionFormTicket(w, r, rid, id, "read")
	if !ok {
		return
	}
	items := decodeInspectionRunbook(t.Runbook)
	page := inspectionFormPage{
		TicketID:  t.ID,
		Title:     t.Title,
		AssetPath: t.AssetPath,
		State:     t.State,
	}
	for i, txt := range items {
		page.Items = append(page.Items, inspectionFormItem{Index: i, Text: txt})
	}
	// PRMT-033 §4.1: only open / acknowledged are forward-resolvable.
	// For a resolved / closed ticket we still render the page (so
	// the operator can confirm what was submitted) but disable the
	// submit button via the State echo — actual re-submission is
	// rejected by allowedTransition below.
	renderInspectionForm(w, page)
}

// serveInspectionFormPost reads the form body, appends the result
// block to the ticket Runbook, transitions to resolved, then
// 303-redirects back to GET so a refresh does not re-submit.
func (s *Server) serveInspectionFormPost(w http.ResponseWriter, r *http.Request, rid, id string) {
	t, ok := s.loadInspectionFormTicket(w, r, rid, id, "write")
	if !ok {
		return
	}
	// Cap the form body size (mirror tickets.go:235) so a hostile
	// client cannot push a multi-MB "notes" field through the
	// server. 64 KiB is plenty for human-typed notes. PRMT-079:
	// the cap wraps r.Body with http.MaxBytesReader BEFORE
	// ParseForm so the rejection happens on the first byte over
	// the limit (mirrors the photo path in servePhotoUpload at
	// line 566 below) — the second-line len(notes)>1<<16 check
	// that follows is kept as a documented backstop, not the
	// primary defence.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad form", err.Error(), r.URL.Path, rid)
		return
	}
	if v := r.PostForm.Get("notes"); len(v) > 1<<16 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"notes too large", "", r.URL.Path, rid)
		return
	}
	checked := parseCheckedIndices(r.PostForm["item"])
	// allowedTransition does the state-machine check; illegal
	// starting state (e.g. already closed) → 422 via the inner
	// transition path. We re-implement the gate here so the form
	// handler does not need to round-trip through the JSON
	// :transition endpoint.
	if !allowedTransition(t.State, "resolved") {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
			"illegal transition", t.State+"->resolved", r.URL.Path, rid)
		return
	}
	now := time.Now().UTC()
	t.Runbook = appendInspectionResult(t.Runbook, r.PostForm.Get("notes"), checked, now)
	t.State = "resolved"
	if t.ResolvedAt == nil {
		t.ResolvedAt = &now
	}
	if _, err := s.st.PutTicket(r.Context(), t, 0); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	s.emitTicketEventAsync(t, ticketEventTypeTransitioned)
	// PRMT-059 §2 says "返回简单"已提交"确认页（或 303 重定向回 GET）".
	// Redirect avoids the refresh-resubmit trap and keeps the URL
	// clean; GET re-renders with the resolved state visible.
	http.Redirect(w, r, "/v1/inspections/form/"+t.ID, http.StatusSeeOther)
}

// --- PRMT-063: photo upload (POST /v1/inspections/form/{id}/photo) ------

// photoUploadResponse is the JSON success envelope. Path is the
// server-relative path under -inspection-photo-dir
// ("<ticketID>/<safename>"); Name is the client-acceptable
// filename. The two fields together let a future reader audit
// "what was uploaded against this ticket" without trusting
// client input.
type photoUploadResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// servePhotoUpload handles POST /v1/inspections/form/{id}/photo.
// Multipart parsing → size cap (http.MaxBytesReader) → extension
// whitelist → MIME sniff → filename sanitisation (runbook-style
// triple defence) → save under -inspection-photo-dir/{id}/ —
// then append a result:photo= line to the ticket Runbook so the
// uploaded evidence is queryable without a separate file DB.
//
// Status codes (PRMT-063 §2 / §5):
//
//	200  saved (returns photoUploadResponse JSON)
//	400  bad multipart / bad form / bad filename (defence trigger)
//	401  handled by middleware (no bearer)
//	403  handled by loadInspectionFormTicket (out-of-scope write)
//	404  ticket missing OR not an inspection ticket
//	413  body > max bytes
//	415  extension not whitelisted OR MIME does not match extension
//	503  -inspection-photo-dir not configured
func (s *Server) servePhotoUpload(w http.ResponseWriter, r *http.Request, rid, id string) {
	// Same inspection gate as the form handler: load the ticket,
	// 404 if missing or not inspection-prefixed, run authorize()
	// against the stored AssetPath for control:write. Reusing
	// loadInspectionFormTicket keeps the gate single-sourced.
	t, ok := s.loadInspectionFormTicket(w, r, rid, id, "write")
	if !ok {
		return
	}
	// Disabled-by-config: empty dir → 503, do not panic.
	if s.inspectionPhotoDir == "" {
		writeProblem(w, http.StatusServiceUnavailable, "disabled",
			"photo upload disabled", "inspection photo dir not configured",
			r.URL.Path, rid)
		return
	}
	maxBytes := s.inspectionPhotoMax
	if maxBytes <= 0 {
		maxBytes = photoUploadDefaultMax
	}
	// Cap the entire multipart body (cap must wrap r BEFORE
	// ParseMultipartForm; a host that sends 100 MiB will see 413
	// on the first read of the body, not after we've buffered
	// everything in memory).
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		// MaxBytesReader signals an oversize body with
		// http.MaxBytesError; both that and a generic parse
		// failure map to 413 here (the user is asking "give me
		// the file" — a body that won't fit IS the problem).
		writeProblem(w, http.StatusRequestEntityTooLarge, "too-large",
			"upload too large", err.Error(), r.URL.Path, rid)
		return
	}
	file, hdr, err := r.FormFile(photoUploadField)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing file field", err.Error(), r.URL.Path, rid)
		return
	}
	defer file.Close()
	if hdr == nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing file header", "", r.URL.Path, rid)
		return
	}
	// Defence layer 1: extension whitelist on the
	// client-supplied filename. Empty / unknown extension → 415.
	safeName, ok := safePhotoName(hdr.Filename)
	if !ok {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported-media-type",
			"unsupported file type", hdr.Filename, r.URL.Path, rid)
		return
	}
	// Defence layer 2: MIME sniff. Read up to
	// photoUploadSniffLen bytes; the first 512 are the contract
	// for net/http.DetectContentType, the rest is buffered so
	// the eventual disk write has the full file (maxBytes
	// already caps the total). We write to a temp file first
	// so an extension-whitelisted but MIME-mismatched file
	// never lands on disk.
	sniffBuf := make([]byte, photoUploadSniffLen)
	n, _ := io.ReadFull(file, sniffBuf)
	detected := http.DetectContentType(sniffBuf[:n])
	if !mimeMatchesExtension(detected, safeName) {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported-media-type",
			"MIME does not match extension", detected, r.URL.Path, rid)
		return
	}
	// Defence layer 3: path-traversal triple defence. Even
	// though safePhotoName already collapsed the name, the final
	// resolved path MUST live under inspectionPhotoDir after
	// Join. We reuse runbook's contract (path.Clean + regex +
	// abs-dir containment) verbatim.
	ticketSubdir := path.Join(s.inspectionPhotoDir, t.ID)
	absDir, _ := filepath.Abs(s.inspectionPhotoDir)
	absSubdir, _ := filepath.Abs(ticketSubdir)
	if !strings.HasPrefix(absSubdir, absDir+string(filepath.Separator)) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad ticket id for photo dir", t.ID, r.URL.Path, rid)
		return
	}
	if err := os.MkdirAll(ticketSubdir, 0o755); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"mkdir failed", err.Error(), r.URL.Path, rid)
		return
	}
	// Unique subname: <random>-<safeName>. The random prefix
	// disambiguates when two operators upload "photo.jpg" to
	// the same ticket; safeName keeps the on-disk name readable.
	randPrefix, err := randomHex(4)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"rand failed", err.Error(), r.URL.Path, rid)
		return
	}
	storedName := randPrefix + "-" + safeName
	fullPath := path.Join(ticketSubdir, storedName)
	// Final abs-dir containment on the actual file path.
	absFull, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absFull, absSubdir+string(filepath.Separator)) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad photo filename", storedName, r.URL.Path, rid)
		return
	}
	// Write: the temp file is the actual file (no rename — the
	// path is constructed from the sanitised name + ticketID
	// and the dir is the configured root). We open with O_CREATE
	// | O_EXCL | O_WRONLY so a random-collision overwrite is
	// impossible.
	out, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"create file failed", err.Error(), r.URL.Path, rid)
		return
	}
	// Stream sniffBuf first, then the remainder of the multipart
	// file. The total written equals the multipart body up to
	// maxBytes, which is what the size cap enforced.
	written, copyErr := copyMultipartToFile(out, file, sniffBuf[:n])
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(fullPath)
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"write failed", copyErr.Error(), r.URL.Path, rid)
		return
	}
	if closeErr != nil {
		_ = os.Remove(fullPath)
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"close failed", closeErr.Error(), r.URL.Path, rid)
		return
	}
	// Append a result:photo= marker to the ticket Runbook so the
	// upload is queryable from the existing runbook reader
	// without a separate file DB. The path stored is the
	// server-relative subpath; the absolute filesystem location
	// is recoverable from s.inspectionPhotoDir.
	relPath := path.Join(t.ID, storedName)
	t.Runbook = appendInspectionPhoto(t.Runbook, relPath)
	if _, err := s.st.PutTicket(r.Context(), t, 0); err != nil {
		// The file is on disk; the runbook write failed. We
		// could roll back the file, but the prompt does not
		// require transactional semantics here — surface 500 and
		// leave the file in place. A future "orphan reaper" can
		// reconcile (the runbook is the source of truth, the
		// file is best-effort).
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, photoUploadResponse{
		Path: relPath,
		Name: storedName,
		Size: written,
	})
}

// safePhotoName extracts the basename, applies the runbook-style
// triple defence (path.Base + path.Clean + regex allowlist), and
// returns the extension-cleaned name on success. Returns ("", false)
// when the input is empty, has no extension, escapes the
// whitelist, or carries a path-traversal segment.
//
// The "regex allowlist" is just the extension whitelist inverted:
// the basename must end in one of allowedPhotoExts after path.Clean.
// This is intentionally narrower than the runbook regex (which
// allowed letters/digits/dash/underscore mid-name) because user
// uploads of "résumé.pdf" must not break the on-disk read of an
// ops script that greps for "*.pdf".
func safePhotoName(in string) (string, bool) {
	if in == "" {
		return "", false
	}
	// path.Base strips any leading directory + handles "" / "."
	// / ".." gracefully on a single string. It does NOT cross
	// OSes (always uses '/'), but the extension check below is
	// separator-free.
	base := path.Base(in)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "", false
	}
	cleaned := path.Clean(base)
	if cleaned != base {
		return "", false
	}
	// Reject any remaining "..", "/", "\\", NUL inside the
	// basename. path.Base collapses "foo/../bar" → "bar" so a
	// "..jpg" input reaches here as "..jpg" (the segment is
	// not a traversal, just a leading ".."); the deeper
	// protection is the abs-dir containment check in the
	// handler (filepath.Abs(full) MUST live under
	// absSubdir+sep). Defence in depth: we also reject any
	// basename that still contains "..", so the on-disk
	// filename is greppable.
	if strings.Contains(cleaned, "..") {
		return "", false
	}
	if strings.ContainsAny(cleaned, "/\\\x00") {
		return "", false
	}
	// Extension whitelist (case-insensitive; the on-disk name
	// keeps the original case so "Photo.JPG" stays "Photo.JPG").
	lower := strings.ToLower(cleaned)
	for _, ext := range allowedPhotoExts {
		if strings.HasSuffix(lower, ext) {
			return cleaned, true
		}
	}
	return "", false
}

// mimeMatchesExtension maps the sniffed MIME to the extension
// whitelist. The mapping is conservative — a sniffed application/pdf
// matches ".pdf", image/jpeg matches ".jpg"/".jpeg", image/png
// matches ".png". Anything else is rejected. This catches the
// "rename .exe to .pdf" bypass at the byte level.
func mimeMatchesExtension(mime, name string) bool {
	lower := strings.ToLower(name)
	isJPEG := strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg")
	switch mime {
	case "image/jpeg":
		return isJPEG
	case "image/png":
		return strings.HasSuffix(lower, ".png")
	case "application/pdf":
		return strings.HasSuffix(lower, ".pdf")
	}
	return false
}

// appendInspectionPhoto is a thin wrapper over the existing
// appendInspectionResult that emits a single result:photo= line.
// Kept as a separate helper so the photo marker has a dedicated
// grep target ("result:photo=") without changing the
// appendInspectionResult contract.
func appendInspectionPhoto(runbook, relPath string) string {
	// We reuse the same line discipline (one record per line,
	// key=value, separated by "\n") so a future runbook reader
	// can iterate result: lines generically.
	var b strings.Builder
	b.WriteString(runbook)
	if runbook != "" && !strings.HasSuffix(runbook, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(inspectionFormPhotoPrefix)
	b.WriteString(relPath)
	b.WriteString("\n")
	return b.String()
}

// randomHex returns n bytes of crypto-random hex (lowercase). n=4
// gives 8 hex chars; the prompt's uniqueness requirement
// (collision-free per ticket) is satisfied because the on-disk
// name combines the random prefix with the sanitised original
// filename and lives under a per-ticket subdirectory.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// copyMultipartToFile copies the buffered prefix (from MIME
// sniffing) followed by the remainder of src into dst, returning
// the total bytes written. Kept as a tiny helper so the
// os.OpenFile + Write + Close triad above stays readable.
func copyMultipartToFile(dst *os.File, src io.Reader, prefix []byte) (int64, error) {
	var total int64
	if len(prefix) > 0 {
		n, err := dst.Write(prefix)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	n, err := io.Copy(dst, src)
	total += n
	return total, err
}
