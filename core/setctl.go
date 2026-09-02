// Package core — setctl.go: Set / risk_class policy (PRMT-215 / P722 / L108).
//
// Enforces spec-006 §5.4 + spec-002 §8bis on PUT /v1/points/{path}:set:
//   - default access is ro (points not on the L108 allow-list reject writes)
//   - class A: dual approval headers + value + audit
//   - class B: single actor + value + audit (readback flag required)
//   - class C: single actor + value + audit
//
// This is the core gate only — gateway southbound command dispatch and
// full pointmap YAML annotation remain follow-on work.
package core

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RiskClass is A/B/C per spec-006 §5.4 (stored lower-case).
type RiskClass string

const (
	RiskClassA RiskClass = "a"
	RiskClassB RiskClass = "b"
	RiskClassC RiskClass = "c"
)

// setAllowEntry is one L108-listed controllable point stem.
// Matching is by quantity/path suffix (case-sensitive cpath segments).
type setAllowEntry struct {
	// Suffix is matched against the full point path (contains).
	// Prefer concrete trailing segments where possible.
	Suffix string
	Class  RiskClass
}

// l108AllowList is the first-batch L108 table (spec-002 §8bis).
// Transformer reserved rows that are typically hardware-ro are omitted
// (N/A) per L108 scope note.
var l108AllowList = []setAllowEntry{
	{Suffix: "chiller.compressor.status", Class: RiskClassA},
	{Suffix: "fws.supply.temp", Class: RiskClassA}, // chiller supply setpoint
	{Suffix: "cdu.pump.rpm", Class: RiskClassA},
	{Suffix: "chiller.valve.opening", Class: RiskClassA},
	{Suffix: "switchgear.breaker.status", Class: RiskClassA},
	{Suffix: "bess.pcs.power", Class: RiskClassA},
	{Suffix: "genset.status", Class: RiskClassA},
	{Suffix: "ups.status", Class: RiskClassA},
	{Suffix: "drycooler.fan.rpm", Class: RiskClassB},
	{Suffix: "cdu.valve.opening", Class: RiskClassB},
	// Demo/sim alias for secondary-side valve (deploy/edge cdu-sim tcs.opening).
	{Suffix: "tcs.opening", Class: RiskClassB},
	{Suffix: "tou.status", Class: RiskClassB},
	{Suffix: "pdu.status", Class: RiskClassB},
	{Suffix: "node.status", Class: RiskClassB},
	{Suffix: "gpu.clock", Class: RiskClassC},
	{Suffix: ".status", Class: RiskClassC}, // maintenance mode mark (narrowed in lookup)
}

// LookupRiskClass returns (class, ok) for a point path under L108.
// Default is not ok → treat as ro (reject write).
// Matching strips numeric instance pads (cdu000 → cdu) so site.podNNN.typeNNN...
// paths align with the type.subpath.quantity forms in §8bis.
func LookupRiskClass(pointPath string) (RiskClass, bool) {
	p := strings.ToLower(strings.TrimSpace(pointPath))
	if p == "" {
		return "", false
	}
	norm := stripInstanceDigits(p)
	// Prefer longest suffix match (except the broad maintenance status row).
	bestLen := -1
	var best RiskClass
	found := false
	for _, e := range l108AllowList {
		suf := strings.ToLower(e.Suffix)
		if suf == ".status" {
			if strings.HasSuffix(norm, ".status") && strings.Contains(norm, "maintenance") {
				if len(suf) > bestLen {
					bestLen = len(suf)
					best = e.Class
					found = true
				}
			}
			continue
		}
		if strings.HasSuffix(norm, suf) || strings.Contains(norm, "."+suf) || strings.HasSuffix(p, suf) {
			if len(suf) > bestLen {
				bestLen = len(suf)
				best = e.Class
				found = true
			}
		}
	}
	return best, found
}

// stripInstanceDigits turns "sgp01.pod000.cdu000.pump.rpm" into
// "sgp.pod.cdu.pump.rpm" so L108 stems like "cdu.pump.rpm" match.
func stripInstanceDigits(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	i := 0
	for i < len(p) {
		c := p[i]
		if c >= '0' && c <= '9' {
			// skip run of digits
			for i < len(p) && p[i] >= '0' && p[i] <= '9' {
				i++
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// SetRequest is the JSON body for PUT ...:set.
type SetRequest struct {
	Value           float64 `json:"value"`
	TTLSeconds      int     `json:"ttl_seconds"`
	SecondApprover  string  `json:"second_approver,omitempty"`  // advisory since PRMT-235 (real approver = second bearer on :approve)
	RequireReadback bool    `json:"require_readback,omitempty"` // class B should set true
	Note            string  `json:"note,omitempty"`
}

// SetAudit is one append-only control-write audit record (PRMT-234; was in-memory MVP).
type SetAudit struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Class     RiskClass `json:"risk_class"`
	Value     float64   `json:"value"`
	Actor     string    `json:"actor"`
	Second    string    `json:"second_approver,omitempty"`
	At        time.Time `json:"at"`
	Readback  bool      `json:"readback_required"`
	Note      string    `json:"note,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
}

// EvaluateSetPolicy applies L108/spec-006 gates. Returns HTTP status + problem detail.
func EvaluateSetPolicy(path string, actor string, req SetRequest) (RiskClass, int, string) {
	if actor == "" {
		return "", http.StatusUnauthorized, "missing actor identity"
	}
	if req.TTLSeconds <= 0 {
		return "", http.StatusBadRequest, "ttl_seconds must be > 0"
	}
	class, ok := LookupRiskClass(path)
	if !ok {
		return "", http.StatusForbidden, "point is not on L108 rw allow-list (default ro)"
	}
	switch class {
	case RiskClassA:
		// PRMT-235: dual approval is enforced by the two-phase pending
		// flow (servePointSet → pendingControl → POST :approve with a
		// SECOND bearer). The body second_approver string proved
		// nothing — the requester could type any name — and is no
		// longer checked; it is retained in SetRequest for wire
		// compatibility only.
	case RiskClassB:
		if !req.RequireReadback {
			return class, http.StatusBadRequest, "risk_class=b requires require_readback=true"
		}
	case RiskClassC:
		// single actor + audit only
	default:
		return "", http.StatusInternalServerError, "unknown risk class"
	}
	return class, 0, ""
}

// servePoint dispatches GET (read) and PUT ...:set (control write).
func (s *Server) servePoint(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	raw := r.URL.Path[len("/v1/points/"):]
	if raw == "" {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"missing point path", "", r.URL.Path, rid)
		return
	}

	if strings.HasSuffix(raw, ":set") {
		if r.Method != http.MethodPut {
			writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed",
				"use PUT for :set", "", r.URL.Path, rid)
			return
		}
		s.servePointSet(w, r, strings.TrimSuffix(raw, ":set"), rid)
		return
	}

	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed",
			"use GET for point read", "", r.URL.Path, rid)
		return
	}
	s.servePointGet(w, r, raw, rid)
}

func (s *Server) servePointSet(w http.ResponseWriter, r *http.Request, pt string, rid string) {
	// Path grammar check (same as GET).
	if _, err := s.d.ParsePoint(pt); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad point path", err.Error(), r.URL.Path, rid)
		return
	}
	var req SetRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"invalid set body", err.Error(), r.URL.Path, rid)
		return
	}
	actor := ""
	if p, ok := PrincipalFromContext(r.Context()); ok {
		actor = p.Subject
	}
	// Dev / test / lab fallback — class B/C ONLY. Class A refuses
	// header actors in serveSetPending (PRMT-235): the header is
	// fully client-controlled and cannot open an approval chain.
	if actor == "" {
		actor = strings.TrimSpace(r.Header.Get("X-CIOS-Actor"))
	}
	class, code, detail := EvaluateSetPolicy(pt, actor, req)
	if code != 0 {
		title := "set-denied"
		if code == http.StatusBadRequest {
			title = "bad-request"
		} else if code == http.StatusUnauthorized {
			title = "unauthorized"
		}
		writeProblem(w, code, title, detail, "", r.URL.Path, rid)
		return
	}
	if class == RiskClassA {
		s.serveSetPending(w, r, pt, req, rid)
		return
	}
	s.executeSet(w, r, pt, class, req, actor, req.SecondApprover, rid)
}

// executeSet is the execution tail shared by the single-phase path
// (class B/C — servePointSet) and the approved two-phase path
// (class A — serveControlApprove). Writes the execution audit row
// (fail-closed), dispatches southbound when a sink is wired,
// answers 202 "accepted".
func (s *Server) executeSet(w http.ResponseWriter, r *http.Request, pt string, class RiskClass, req SetRequest, actor, second, rid string) {
	audit := SetAudit{
		ID:        newUsageID(), // reuse entropy helper; prefix is fine for MVP
		Path:      pt,
		Class:     class,
		Value:     req.Value,
		Actor:     actor,
		Second:    second,
		At:        time.Now().UTC(),
		Readback:  req.RequireReadback || class == RiskClassA || class == RiskClassB,
		Note:      req.Note,
		RequestID: rid,
	}
	if err := s.st.AppendSetAudit(r.Context(), audit); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}

	// Southbound dispatch (P722): optional ControlSink. Nil sink keeps
	// policy-only Accepted (lab / no gateway wired).
	dispatched := false
	note := "policy gate only; no control sink wired"
	var readbackVal any
	if s.controlSink != nil {
		ttl := time.Duration(req.TTLSeconds) * time.Second
		res, err := s.controlSink.DispatchControl(r.Context(), ControlDispatch{
			Path:            pt,
			AuditID:         audit.ID,
			Actor:           actor,
			Class:           class,
			Value:           req.Value,
			TTL:             ttl,
			RequireReadback: audit.Readback,
		})
		if err != nil {
			writeProblem(w, http.StatusBadGateway, "upstream-unavailable",
				"control dispatch failed", err.Error(), r.URL.Path, rid)
			return
		}
		dispatched = res.Accepted
		if res.Detail != "" {
			note = res.Detail
		} else if dispatched {
			note = "dispatched southbound"
		} else {
			note = "sink refused write"
		}
		// Surface readback when sink provided one (zero ts still OK if accepted).
		if res.Accepted && (res.Readback != 0 || !res.ReadbackTs.IsZero()) {
			readbackVal = res.Readback
		}
	}

	out := map[string]any{
		"status":     "accepted",
		"path":       pt,
		"risk_class": class,
		"audit_id":   audit.ID,
		"readback":   audit.Readback,
		"dispatched": dispatched,
		"note":       note,
	}
	if readbackVal != nil {
		out["readback_value"] = readbackVal
	}
	writeJSON(w, http.StatusAccepted, out)
}

// listSetAuditsResponse is the envelope for GET /v1/control/audit.
// Mirrors listMaintenanceWindowsResponse: non-nil "items", pair
// cursor in "page_token".
type listSetAuditsResponse struct {
	Items         []SetAudit `json:"items"`
	NextPageToken string     `json:"page_token"`
}

// setAuditAfterCursor reports whether a sorts strictly after the
// (At DESC, ID DESC) cursor position — i.e. belongs on a later page.
// Cursor key and sort key are the same pair, so pages cannot skip
// or duplicate rows (the T54 defect was sorting on created_at while
// filtering the cursor on id).
func setAuditAfterCursor(a SetAudit, cursorKey, cursorID string) bool {
	curNS, err := strconv.ParseInt(cursorKey, 10, 64)
	if err != nil {
		return true
	}
	thisNS := a.At.UnixNano()
	if thisNS != curNS {
		return thisNS < curNS // DESC: later pages hold older rows
	}
	return a.ID < cursorID
}

// serveControlAudit implements GET /v1/control/audit (PRMT-234).
// Read-only, viewer floor (authmw), per-item scope filter on the
// audited point path, (At,ID) pair-cursor pagination newest-first.
func (s *Server) serveControlAudit(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed",
			"use GET for control audit", "", r.URL.Path, rid)
		return
	}
	q := r.URL.Query()
	pageSize := DefaultPageSize
	if ps := q.Get("page_size"); ps != "" {
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
	var cursorKey, cursorID string
	if pt := q.Get("page_token"); pt != "" {
		k, id, ok := decodePageTokenPair(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		cursorKey, cursorID = k, id
	}
	all, err := s.st.ListSetAudits(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	principal, hasAuth := PrincipalFromContext(r.Context())
	items := make([]SetAudit, 0, len(all))
	for _, a := range all {
		if cursorKey != "" && !setAuditAfterCursor(a, cursorKey, cursorID) {
			continue
		}
		if hasAuth && authorize(principal, ActionRead, a.Path) != nil {
			continue
		}
		items = append(items, a)
	}
	var next string
	if len(items) > pageSize {
		last := items[pageSize-1]
		next = encodePageTokenPair(strconv.FormatInt(last.At.UnixNano(), 10), last.ID)
		items = items[:pageSize]
	}
	if items == nil {
		items = []SetAudit{}
	}
	writeJSON(w, http.StatusOK, listSetAuditsResponse{
		Items:         items,
		NextPageToken: next,
	})
}

// pendingControl is one class-A control write awaiting a second
// bearer's approval (PRMT-235; L108 approval chain). In-memory
// only: entries are TTL-bounded and a core restart voids them
// (the requester re-submits); the durable trail is set_audit.
// ponytail: in-memory map; move into Store if HA core ever ships.
type pendingControl struct {
	ID        string
	Path      string
	Class     RiskClass
	Req       SetRequest
	Requester string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// serveSetPending is phase 1 of the class-A two-man rule
// (PRMT-235): no immediate execution — park the request under a
// TTL (reuses ttl_seconds) and return a pending id for a SECOND
// bearer to POST /v1/control/{id}:approve. The requester must be
// a real authenticated principal: X-CIOS-Actor is client-
// controlled and cannot open an A-class approval chain.
func (s *Server) serveSetPending(w http.ResponseWriter, r *http.Request, pt string, req SetRequest, rid string) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"risk_class=a requires an authenticated principal (header actor not accepted)",
			"", r.URL.Path, rid)
		return
	}
	now := s.now().UTC()
	pend := pendingControl{
		ID:        newUsageID(),
		Path:      pt,
		Class:     RiskClassA,
		Req:       req,
		Requester: p.Subject,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(req.TTLSeconds) * time.Second),
	}
	audit := SetAudit{
		ID:        pend.ID,
		Path:      pt,
		Class:     RiskClassA,
		Value:     req.Value,
		Actor:     p.Subject,
		At:        now,
		Readback:  true,
		Note:      "pending dual approval",
		RequestID: rid,
	}
	if err := s.st.AppendSetAudit(r.Context(), audit); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	s.pendingMu.Lock()
	if s.pendingSets == nil {
		s.pendingSets = map[string]pendingControl{}
	}
	for id, pc := range s.pendingSets { // lazy sweep of expired entries
		if now.After(pc.ExpiresAt) {
			delete(s.pendingSets, id)
		}
	}
	s.pendingSets[pend.ID] = pend
	s.pendingMu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "pending",
		"path":       pt,
		"risk_class": RiskClassA,
		"pending_id": pend.ID,
		"expires_at": pend.ExpiresAt,
	})
}

// serveControlApprove implements POST /v1/control/{id}:approve —
// phase 2 of the class-A two-man rule (PRMT-235). The approver must
// be an authenticated principal, must differ from the requester,
// and must independently hold control:write on the pending point
// path (full authorize; the middleware only role-floors because
// {id} is a pending id, not an asset path — mirror /v1/alarms/{id}:ack).
func (s *Server) serveControlApprove(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	raw := strings.TrimPrefix(r.URL.Path, "/v1/control/")
	if !strings.HasSuffix(raw, ":approve") {
		writeProblem(w, http.StatusNotFound, "not-found",
			"unknown control resource", "", r.URL.Path, rid)
		return
	}
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed",
			"use POST for :approve", "", r.URL.Path, rid)
		return
	}
	id := strings.TrimSuffix(raw, ":approve")
	approver, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"approve requires an authenticated principal", "", r.URL.Path, rid)
		return
	}
	now := s.now().UTC()
	s.pendingMu.Lock()
	pend, found := s.pendingSets[id]
	if found && now.After(pend.ExpiresAt) {
		delete(s.pendingSets, id)
		s.pendingMu.Unlock()
		writeProblem(w, http.StatusGone, "pending-expired",
			"pending control write expired", id, r.URL.Path, rid)
		return
	}
	s.pendingMu.Unlock()
	if !found {
		writeProblem(w, http.StatusNotFound, "not-found",
			"no such pending control write", id, r.URL.Path, rid)
		return
	}
	if strings.EqualFold(approver.Subject, pend.Requester) {
		writeProblem(w, http.StatusForbidden, "set-denied",
			"approver must differ from requester (two-man rule)", "", r.URL.Path, rid)
		return
	}
	if err := authorize(approver, ActionControlWrite, pend.Path); err != nil {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"approver lacks control:write on the target path", err.Error(), r.URL.Path, rid)
		return
	}
	// Consume the pending BEFORE dispatch — single-shot token. A
	// failed dispatch (502) burns it; the requester re-submits.
	s.pendingMu.Lock()
	if _, still := s.pendingSets[id]; !still {
		s.pendingMu.Unlock()
		writeProblem(w, http.StatusNotFound, "not-found",
			"no such pending control write", id, r.URL.Path, rid)
		return
	}
	delete(s.pendingSets, id)
	s.pendingMu.Unlock()
	s.executeSet(w, r, pend.Path, pend.Class, pend.Req, pend.Requester, approver.Subject, rid)
}
