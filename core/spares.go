// Package core — spares.go: /v1/spares HTTP surface for M2 E2.5
// (P541 / PRMT-048). Minimal spare domain: catalog + current stock
// + append-only txn log. No procurement / supplier / price by
// prompt MUST NOT.
//
// Four routes:
//
//	GET    /v1/spares                          → list (paged, role ≥ viewer)
//	POST   /v1/spares                          → create catalog entry (operator+)
//	GET    /v1/spares/{id}                     → read one + low_stock + recent txns
//	POST   /v1/spares/{id}:adjust              → stock movement (operator+)
//
// Stock changes go exclusively through :adjust — there is no
// direct PUT qty. :adjust writes one spare_txns row AND updates
// spare_parts.qty inside one transaction (pgStore) or one mutex
// section (fileStore) so the cached aggregate can never disagree
// with the append-only log.
//
// Spare parts are not asset-path scoped (spec §16 follow-up: the
// /v1/spares list uses the role floor only, mirroring /v1/alarms
// and /v1/reports/ops — no per-item path filter).
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// spareIDPattern matches "sp_" + 16 uppercase base32 chars.
// Same shape as ticketIDPattern (10 random bytes → 16 chars in
// [A-Z2-7]); the prefix alone is the trust-boundary sentinel.
var spareIDPattern = regexp.MustCompile(`^sp_[A-Z2-7]{16}$`)

// spareTxnIDPattern matches "st_" + 16 uppercase base32 chars.
// Distinct prefix from tickets/spare_parts so logs grep cleanly.
var spareTxnIDPattern = regexp.MustCompile(`^st_[A-Z2-7]{16}$`)

// newSpareID produces "sp_" + 16 uppercase base32 chars from 10
// random bytes. Mirrors newTicketID / newRequestID's entropy and
// keeps the three ID families visually distinct in log lines.
func newSpareID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "sp_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// newSpareTxnID produces "st_" + 16 uppercase base32 chars.
// Mirrors newSpareID's shape; the prefix marks the row as a stock
// movement, not a catalog entry.
func newSpareTxnID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "st_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// sparePartWithDerived is the wire shape for /v1/spares/{id}: the
// stored SparePart plus the derived low_stock flag and the most
// recent txn log entries. low_stock is NOT persisted (PRMT-048 §2).
type sparePartWithDerived struct {
	SparePart
	LowStock bool       `json:"low_stock"`
	Recent   []SpareTxn `json:"recent_txns,omitempty"`
}

// listSparesResponse is the envelope for the list endpoint.
type listSparesResponse struct {
	Items         []SparePart `json:"items"`
	NextPageToken string      `json:"next_page_token"`
}

// spareCreateRequest is the JSON body for POST /v1/spares. All
// fields except Location are required. The server fills ID.
type spareCreateRequest struct {
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Qty      int    `json:"qty"`
	MinQty   int    `json:"min_qty"`
	Location string `json:"location"`
}

// spareAdjustRequest is the JSON body for POST /v1/spares/{id}:adjust.
// TicketID is optional; an outbound (delta<0) txn may carry it.
type spareAdjustRequest struct {
	Delta    int    `json:"delta"`
	TicketID string `json:"ticket_id"`
}

// serveSpares handles /v1/spares. Method dispatch:
//
//	GET  → list (page_size/page_token, role ≥ viewer)
//	POST → create (operator+)
func (s *Server) serveSpares(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		s.serveSparesList(w, r, rid)
	case http.MethodPost:
		s.serveSparesCreate(w, r, rid)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveSparesList implements GET /v1/spares. Role ≥ viewer
// (role floor enforced by middleware; the list endpoint is also
// in isListScopeEndpoint so the middleware applies only the floor
// — there is no per-item scope filter because spares are not
// asset-path scoped).
func (s *Server) serveSparesList(w http.ResponseWriter, r *http.Request, rid string) {
	q := r.URL.Query()
	pageSize := 100
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_size", ps, r.URL.Path, rid)
			return
		}
		if n > 1000 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"page_size > 1000", ps, r.URL.Path, rid)
			return
		}
		pageSize = n
	}
	var afterID string
	if pt := q.Get("page_token"); pt != "" {
		t, ok := decodePageToken(pt)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token", "", r.URL.Path, rid)
			return
		}
		afterID = t
	}
	all, err := s.st.ListSpares(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// Stable ID order — cursor pagination by index, same trick as
	// /v1/tickets (ListTickets returns OpenedAt-desc, but ListSpares
	// sorts by ID so a cursor over IDs is well-defined).
	var startIdx int
	if afterID != "" {
		idx, err := strconv.Atoi(afterID)
		if err != nil || idx < 0 {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad page_token (not an int)", afterID, r.URL.Path, rid)
			return
		}
		if idx > len(all) {
			idx = len(all)
		}
		startIdx = idx
	}
	items := all[startIdx:]
	var next string
	if len(items) > pageSize {
		next = encodePageToken(strconv.Itoa(startIdx + pageSize))
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, listSparesResponse{Items: items, NextPageToken: next})
}

// serveSparesCreate implements POST /v1/spares. Body validated;
// server fills ID. qty / min_qty must be ≥ 0.
func (s *Server) serveSparesCreate(w http.ResponseWriter, r *http.Request, rid string) {
	var req spareCreateRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.SKU == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing sku", "", r.URL.Path, rid)
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing name", "", r.URL.Path, rid)
		return
	}
	if req.Qty < 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"qty < 0", "", r.URL.Path, rid)
		return
	}
	if req.MinQty < 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"min_qty < 0", "", r.URL.Path, rid)
		return
	}
	sp := SparePart{
		ID:       newSpareID(),
		SKU:      req.SKU,
		Name:     req.Name,
		Qty:      req.Qty,
		MinQty:   req.MinQty,
		Location: req.Location,
	}
	if err := s.st.PutSpare(r.Context(), sp); err != nil {
		if err == ErrSKUExists {
			writeProblem(w, http.StatusUnprocessableEntity, "sku-exists",
				"sku already exists", req.SKU, r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

// serveSpare handles /v1/spares/{id} (GET) and
// /v1/spares/{id}:adjust (POST). Method dispatch:
//
//	GET   /{id}              → read with derived low_stock + recent txns
//	POST  /{id}:adjust       → atomic stock movement
func (s *Server) serveSpare(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/v1/spares/")
	if rest == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	id, adjust := rest, false
	if strings.HasSuffix(rest, ":adjust") {
		id = strings.TrimSuffix(rest, ":adjust")
		adjust = true
	}
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"missing id", "", r.URL.Path, rid)
		return
	}
	if !spareIDPattern.MatchString(id) {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad spare id", id, r.URL.Path, rid)
		return
	}
	switch {
	case adjust && r.Method == http.MethodPost:
		s.serveSpareAdjust(w, r, rid, id)
	case !adjust && r.Method == http.MethodGet:
		s.serveSpareGet(w, r, rid, id)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
	}
}

// serveSpareGet implements GET /v1/spares/{id}. Returns the stored
// SparePart augmented with the derived low_stock flag and the most
// recent 20 txns (newest-first) so the operator UI can show "what
// moved when" without a second round-trip.
func (s *Server) serveSpareGet(w http.ResponseWriter, r *http.Request, rid, id string) {
	sp, ok, err := s.st.GetSpare(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if !ok {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"spare not found", id, r.URL.Path, rid)
		return
	}
	txns, err := s.st.ListSpareTxns(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	if len(txns) > 20 {
		txns = txns[:20]
	}
	out := sparePartWithDerived{
		SparePart: sp,
		LowStock:  sp.Qty < sp.MinQty,
		Recent:    txns,
	}
	writeJSON(w, http.StatusOK, out)
}

// serveSpareAdjust implements POST /v1/spares/{id}:adjust. The
// ONLY legal stock-mutation path; no PUT qty exists by design.
// delta must be non-zero; qty+delta must stay ≥ 0 (else 422).
// Writes one SpareTxn AND updates spare_parts.qty atomically
// (single tx in pgStore, single mutex section in fileStore).
func (s *Server) serveSpareAdjust(w http.ResponseWriter, r *http.Request, rid, id string) {
	var req spareAdjustRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"read body", err.Error(), r.URL.Path, rid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad json", err.Error(), r.URL.Path, rid)
		return
	}
	if req.Delta == 0 {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"delta must be non-zero", "", r.URL.Path, rid)
		return
	}
	// ticket_id, when present, must look like a ticket id. We
	// deliberately do NOT require an existing ticket — the spec
	// (PRMT-048 §4) leaves ticket_id free-form — but a malformed
	// id is worth rejecting at the boundary so downstream
	// analytics never see "tk_bogus". If the operator wants to
	// record a consumption without a ticket they leave it empty.
	if req.TicketID != "" && !strings.HasPrefix(req.TicketID, "tk_") {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"bad ticket_id prefix", req.TicketID, r.URL.Path, rid)
		return
	}
	updated, txn, err := s.st.AdjustSpare(r.Context(), id, req.Delta, req.TicketID, time.Now().UTC())
	if err != nil {
		if err == ErrInsufficientStock {
			writeProblem(w, http.StatusUnprocessableEntity, "insufficient-stock",
				"insufficient stock", "", r.URL.Path, rid)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"store error", err.Error(), r.URL.Path, rid)
		return
	}
	// Echo back the updated spare (with low_stock derived for
	// caller convenience) plus the just-written txn id.
	out := sparePartWithDerived{
		SparePart: updated,
		LowStock:  updated.Qty < updated.MinQty,
		Recent:    []SpareTxn{txn},
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Spare low-stock scanner (PRMT-054 / M2 E2.5 P541 闭环) ----
//
// Mirrors core/pm.go:RunPMScanner — long-lived background
// goroutine, startup tick + ticker + ctx.Done exit + fail-soft
// per-spare errors. On each tick it walks every spare; for any
// SparePart where Qty < MinQty it opens a ticket (severity=minor)
// UNLESS an existing non-closed ticket is already pinned to that
// spare via the `alarm_id="spare:<id>"` dedup key.
//
// Dedup key: the ticket's existing `alarm_id` field carries
// `spare:<spareID>` — the "spare:" prefix is a namespace tag
// (alarm id space already coexists with manual-ticket id space,
// so this prefix marks "low-stock dedup key" without colliding
// with the `io.cios.alarm.<id>` convention). The dedup check is
// `alarm_id == "spare:<id>" AND state != "closed"` (mirrors the
// §4 alarm_id dedup contract used by the alarm→ticket path).
//
// Restock does NOT auto-close the open ticket — spec-008 Q5
// reserves close for human acknowledgement. The next tick after
// restock will simply find the open ticket and skip; if the
// ticket is then closed and qty drops below min again, a fresh
// ticket opens. (Idempotency is one-ticket-per-low-stock-event,
// not one-ticket-per-forever-low-stock.)

// RunSpareStockScanner is the long-lived spare low-stock
// background goroutine. Mirrors RunSLAScanner / RunPMScanner:
// interval<=0 → 60m default, startup tick, ticker, ctx.Done exit,
// fail-soft on per-spare errors.
//
// Single-instance assumption: spec-008 does not yet specify
// leader election for this scanner. Two cios-core instances
// ticking in parallel would race on the alarm_id dedup check
// (both see no open ticket, both create one) — the alarm_id
// uniqueness is the eventual-consistency gate but a duplicate
// window is possible. Logged for §8 review; M3 leader
// election (T43) is the durable fix.
func (s *Server) RunSpareStockScanner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run one scan at startup so a freshly-restored cios-core
	// picks up any spares that crossed min_qty while it was down.
	// safeTick (PRMT-076) so a panic in scanSpareStockTick can't
	// kill the long-lived goroutine.
	safeTick("spare", func() { s.scanSpareStockTick(ctx, time.Now().UTC()) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("spare", func() { s.scanSpareStockTick(ctx, now.UTC()) })
		}
	}
}

// scanSpareStockTick is one iteration: list every spare, for any
// with Qty<MinQty, dedup-check and fire. Pulled out of
// RunSpareStockScanner so tests can drive a single tick
// deterministically (mirror of scanPMTick).
func (s *Server) scanSpareStockTick(ctx context.Context, now time.Time) {
	// PRMT-066: record the tick outcome for /v1/health/scanners.
	// Captured by the deferred closure so any return path
	// (lock failure, leader skip, list error, per-spare
	// error) produces a registry entry.
	var tickErr error
	defer func() {
		s.recordScanner("spare", now, tickErr)
	}()
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the spare stock tick
	// for this tick window. The pg advisory lock is session-
	// scoped and released when the tick ends (release is
	// deferred). On error we log + skip (fail-soft, next tick
	// will retry); on !acquired we silently skip — another
	// instance leads. PRMT-054 noted this is the durable fix
	// for the alarm_id-dedup duplicate-window race.
	ok, release, err := s.st.TryScannerLock(ctx, "spare")
	if err != nil {
		log.Printf("core: spare stock scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()
	all, err := s.st.ListSpares(ctx)
	if err != nil {
		log.Printf("core: spare stock scanner: list: %v", err)
		tickErr = err
		return
	}
	for _, sp := range all {
		if sp.Qty >= sp.MinQty {
			continue
		}
		s.fireLowStock(ctx, sp, now)
	}
}

// fireLowStock opens one low-stock ticket for sp if no open
// ticket is already pinned to it via the alarm_id dedup key.
// Best-effort: dedup-check or PutTicket failures are logged, but
// we never propagate — the next tick will retry. There is no
// "advance" step (no NextDue / LastRun like PM) because low
// stock is a steady-state condition, not a deadline — once a
// ticket is open it stays open until a human closes it.
func (s *Server) fireLowStock(ctx context.Context, sp SparePart, now time.Time) {
	dedupKey := spareAlarmID(sp.ID)
	if hasOpenLowStockTicket(ctx, s.st, sp.ID) {
		// Idempotent: an open ticket already exists for this
		// spare. Log nothing — this is the hot path, every
		// tick would otherwise spam the log.
		return
	}
	t := Ticket{
		ID:        newTicketID(),
		AlarmID:   dedupKey,
		AssetPath: "", // spares are not asset-path scoped (PRMT-048 §1)
		Title:     fmt.Sprintf("Low stock: %s (qty %d < min %d)", sp.SKU, sp.Qty, sp.MinQty),
		Severity:  "minor",
		State:     "open",
		OpenedAt:  now,
	}
	if _, err := s.st.PutTicket(ctx, t, 0); err != nil {
		log.Printf("core: spare stock scanner: put ticket for %s: %v", sp.ID, err)
		return
	}
	s.emitTicketEventAsync(t, ticketEventTypeOpened)
}

// spareAlarmID returns the alarm_id dedup key for a given spare.
// The "spare:" prefix marks this as a low-stock source (vs the
// `io.cios.alarm.<id>` shape used by alarm-driven tickets);
// spec-008 §16/v0.4 documents the namespace convention.
func spareAlarmID(spareID string) string {
	return "spare:" + spareID
}

// hasOpenLowStockTicket reports whether an open (state !=
// "closed") ticket is already pinned to spareID via the
// `alarm_id="spare:<id>"` dedup key. Pure check; no side effects.
// Errors are logged and treated as "no open ticket" so a transient
// store failure does not block the scan loop.
func hasOpenLowStockTicket(ctx context.Context, st Store, spareID string) bool {
	all, err := st.ListTickets(ctx)
	if err != nil {
		log.Printf("core: spare stock scanner: list tickets for dedup: %v", err)
		return false
	}
	key := spareAlarmID(spareID)
	for _, t := range all {
		if t.AlarmID == key && t.State != "closed" {
			return true
		}
	}
	return false
}
