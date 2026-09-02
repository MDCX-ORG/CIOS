// Package core — reconcile.go: GET /v1/reports/reconcile (M2 E2.6
// P553). CMDB vs telemetry presence report.
//
// The endpoint pairs the CMDB asset registry (store.ListAssets,
// lifecycle-filtered to active|maintenance per PRMT-040 §2-bis(4))
// against what VictoriaMetrics actually has on disk over a
// trailing window. Three classifications emerge:
//
//   - registered_no_telemetry: CMDB knows it, VM has no samples in
//     the window. Suspect offline or not wired.
//   - telemetry_no_asset:      VM has a series under asset_path=…,
//     but the path is absent from the CMDB. Suspect unregistered.
//     gated to operator+ because the path string itself is
//     potentially topology-leaking (see §4 "orphan scope 管控").
//   - ok:                      both sides agree.
//
// Fail-soft contract (PRMT-050 §2): VM unreachable / partial
// failure degrades to `degraded=true` plus a per-asset
// "unknown" flag, NOT 500/502. Capacity proves the pattern works
// (PRMT-040 §4); we reuse the same `fetchVM` + per-asset caller
// shape but switch to a presence query
// (`count_over_time(...) > 0`) instead of a quantile value.
//
// The auto-ticket scanner (PRMT-057, M2 E2.6 close-loop) reuses
// computeReconcile — the same per-asset presence probe that
// serveReconcile runs — so a CMDB/VM drift opens exactly one
// ticket per asset path, deduplicated via the `reconcile:<path>`
// alarm_id namespace. Default-off (interval<=0); no behaviour
// change for existing deployments.
//
// spec-008 §11 records the endpoint contract.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. Constants + types         — window defaults, entry/orphan/response
//   2. serveReconcile            — HTTP entry, scope + fail-soft
//   3. computeReconcile          — CMDB-vs-VM classification core
//   4. VM presence probes        — single + batch fetchVMAssetPaths
//   5. Drift auto-ticket (PRMT-057) — scanner loop + fireDriftTicket
// ─────────────────────────────────────────────────────────────────────────────

// defaultReconcileWindow is the trailing window used when the
// client does not pass ?window=. 7d matches PRMT-040 §2's default.
const defaultReconcileWindow = "7d"

// maxReconcileWindowDays mirrors maxCapacityWindowDays (PRMT-040
// §4): the cap keeps the VM count_over_time query bounded and
// rejects typo'd "7y"-style input at the API boundary.
const maxReconcileWindowDays = 90

// reconcileEntry is one row of the registered-vs-telemetry matrix.
// Lifecycle echoes Spec["lifecycle"] so the client can tell
// "active with no telemetry" apart from "active under maintenance
// noise" without re-querying CMDB. State is the classification
// result; TelemetryUnknown=true means VM failed for this specific
// asset (only set when the response is degraded).
type reconcileEntry struct {
	Path             string `json:"path"`
	Lifecycle        string `json:"lifecycle"`
	State            string `json:"state"`                       // "ok" | "registered_no_telemetry"
	TelemetryPresent bool   `json:"telemetry_present"`           // false ⇒ no_telemetry
	TelemetryUnknown bool   `json:"telemetry_unknown,omitempty"` // true if VM failed for this row
}

// reconcileOrphan is one telemetry-only path (VM has a series, CMDB
// has no asset at that path). Gated to operator+ per §4 — viewers
// receive an empty Orphans slice plus OrphansRestricted=true.
type reconcileOrphan struct {
	Path string `json:"path"`
}

// reconcileResponse is the JSON envelope. Window is echoed back so
// the client can verify which trailing range the presence check
// covered. Degraded is the response-wide degradation flag (true if
// ANY per-asset VM query failed). OrphansRestricted=true means
// the caller lacks the operator+ role and the Orphans list was
// suppressed on purpose.
type reconcileResponse struct {
	Window            string            `json:"window"`
	Degraded          bool              `json:"degraded"`
	OrphansRestricted bool              `json:"orphans_restricted,omitempty"`
	Entries           []reconcileEntry  `json:"entries"`
	Orphans           []reconcileOrphan `json:"orphans"`
}

// serveReconcile handles GET /v1/reports/reconcile?window=7d. Non-
// GET → 405. Auth is handled by the middleware (list-scope, see
// authmw.go). Auth==nil ⇒ open path.
//
// Per-item scope filter: each visible Entry is filtered through
// authorize(ActionRead, entry.Path); out-of-scope entries are
// silently dropped (same rule as serveOpsReport / serveAlarms /
// serveCapacity). The Orphan list has no asset path to scope
// against, so the handler applies a role floor at operator+; if
// the caller is below that, Orphans is forced to empty and
// OrphansRestricted is set to true (caller signal). Recorded in
// §8 as a §11 follow-up for spec to ratify.
func (s *Server) serveReconcile(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	window := defaultReconcileWindow
	if v := r.URL.Query().Get("window"); v != "" {
		if !validCapacityWindow(v) {
			writeProblem(w, http.StatusBadRequest, "bad-request",
				"bad window", v, r.URL.Path, rid)
			return
		}
		window = v
	}

	principal, hasAuth := PrincipalFromContext(r.Context())

	// computeReconcile does the per-asset presence probe (Store +
	// VM); the handler adds the auth + orphan layer on top. Both
	// paths share the entries + degraded flag — no second probe.
	rep := s.computeReconcile(r.Context(), window)

	// Per-item scope filter (computeReconcile returns every
	// active|maintenance asset; the handler trims to the caller's
	// visible set).
	visible := make([]reconcileEntry, 0, len(rep.Entries))
	registeredSet := make(map[string]struct{}, len(rep.Entries))
	for _, e := range rep.Entries {
		if hasAuth && authorize(principal, ActionRead, e.Path) != nil {
			continue
		}
		visible = append(visible, e)
		registeredSet[e.Path] = struct{}{}
	}
	rep.Entries = visible

	// Orphans = telemetry-only paths not in CMDB. We pull every
	// asset_path label value currently present in VM, then subtract
	// the registered set. The role floor at operator+ suppresses the
	// list when the caller is below the threshold.
	if !hasAuth || roleAllows(principal, ActionControlWrite) == nil {
		// Either no auth (M0 legacy) or operator+ (roleAllows on
		// control:write is the operator|admin branch; viewers
		// fail at ActionControlWrite per rbac.go).
		vmPaths, vmDegraded := s.fetchVMAssetPaths(r.Context(), window)
		if vmDegraded {
			rep.Degraded = true
			// No telemetry side → no orphan computation possible;
			// surface the degradation but do not invent paths.
			rep.Orphans = []reconcileOrphan{}
		} else {
			rep.Orphans = make([]reconcileOrphan, 0)
			for _, p := range vmPaths {
				if _, ok := registeredSet[p]; ok {
					continue
				}
				rep.Orphans = append(rep.Orphans, reconcileOrphan{Path: p})
			}
		}
	} else {
		rep.OrphansRestricted = true
		// Force empty even if computeReconcile seeded something
		// (it shouldn't — computeReconcile never sets Orphans).
		rep.Orphans = []reconcileOrphan{}
	}

	writeJSON(w, http.StatusOK, rep)
}

// computeReconcile runs the registered-vs-telemetry probe over
// every active|maintenance asset and returns the entries plus the
// response-wide degraded flag. No auth/scope filter (the handler
// applies it on top) and no orphan computation (orphans require a
// role floor; the scanner does not need them).
//
// Reused by the auto-ticket scanner (PRMT-057) so the HTTP report
// and the scanner see identical drift classifications — the
// scanner never re-implements the per-asset probe.
//
// PRMT-086: a single VM instant query groups count_over_time by
// asset_path and returns the presence vector in one round trip.
// Per-asset classification is a local map lookup. Round trips
// collapse from O(N) to O(1) per call. Fail-soft semantics
// (degraded flag, per-asset unknown) unchanged.
func (s *Server) computeReconcile(ctx context.Context, window string) reconcileResponse {
	allAssets, err := s.st.ListAssets(ctx)
	if err != nil {
		// Store failure: report-wide degradation. Entries stays
		// empty so callers don't act on a half-populated matrix.
		return reconcileResponse{
			Window:   window,
			Degraded: true,
			Orphans:  []reconcileOrphan{},
		}
	}
	// Lifecycle filter (active|maintenance). Out-of-lifecycle
	// assets are silently dropped — same rule as the original
	// serveReconcile.
	visible := make([]Asset, 0, len(allAssets))
	for _, a := range allAssets {
		lc, _ := a.Spec["lifecycle"].(string)
		if lc != "active" && lc != "maintenance" {
			continue
		}
		visible = append(visible, a)
	}
	// Collect the visible paths up front so the batch query asks
	// VM for exactly the assets the report will classify. The
	// query still runs even on an empty slice (returns an empty
	// vector) so the no-assets branch is observable to the
	// caller as a clean "no entries, not degraded" response.
	visiblePaths := make([]string, 0, len(visible))
	for _, a := range visible {
		visiblePaths = append(visiblePaths, a.Path)
	}
	presentMap, vmDegraded := s.assetHasTelemetryBatch(ctx, visiblePaths, window)
	entries := make([]reconcileEntry, 0, len(visible))
	for _, a := range visible {
		lc, _ := a.Spec["lifecycle"].(string)
		entry := reconcileEntry{Path: a.Path, Lifecycle: lc}
		// VM-level failure: every per-asset field is unknown
		// (matches the legacy per-asset code's contract). The
		// response-wide Degraded flag mirrors this.
		if vmDegraded {
			entry.TelemetryUnknown = true
			entries = append(entries, entry)
			continue
		}
		// Map lookup. assetHasTelemetryBatch pre-seeds every
		// queried path with (false, known), so a missing key
		// (visible asset not in the query) is impossible here.
		// The only way to land in this loop is for the path
		// to have been requested, so the map entry exists.
		present := presentMap[a.Path]
		entry.TelemetryPresent = present
		if present {
			entry.State = "ok"
		} else {
			entry.State = "registered_no_telemetry"
		}
		entries = append(entries, entry)
	}
	return reconcileResponse{
		Window:   window,
		Degraded: vmDegraded,
		Orphans:  []reconcileOrphan{},
		Entries:  entries,
	}
}

// assetHasTelemetry issues a single VM instant query and returns
// (present, unknown). present=true means count_over_time over the
// window was > 0 (so the asset is in the live telemetry set);
// present=false means the query returned 0 or an empty vector.
// unknown=true signals any failure (network, status, parse, empty
// envelope) so the caller can flip degraded without distinguishing
// the cause.
//
// Retained as the per-asset wire-format sink for the PRMT-078
// escape regression test (TestPromqlEscape_AssetHasTelemetry_...).
// Production goes through assetHasTelemetryBatch, which collapses
// N round trips into one (PRMT-086). The query body is
// byte-identical when the batch input has one element.
//
// The per-asset signature is intentionally retained (no ctx param)
// because the PRMT-078 escape test pins the exact two-arg shape;
// the production path goes through assetHasTelemetryBatch, which
// carries the upstream ctx (PRMT-094).
func (s *Server) assetHasTelemetry(assetPath, window string) (bool, bool) {
	// PromQL: count_over_time(<metric>{asset_path="..."}[window]) > 0
	// We probe a generic name `cios_metric` because VM does not
	// pin a single metric for "this asset was ever scraped" — any
	// sample tagged with the asset_path counts. The selector
	// grammar is from promproj; here we just need a presence check,
	// not a value, so a generic metric name is sufficient.
	query := `count_over_time(cios_metric{asset_path="` + escapeLabelValue(assetPath) + `"}[` + window + `]) > 0`
	q := url.Values{}
	q.Set("query", query)
	body, err := s.fetchVM(context.Background(), s.vmURL+"/api/v1/query", q)
	if err != nil {
		return false, true
	}
	var vresp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		return false, true
	}
	if vresp.Status != "success" {
		return false, true
	}
	if len(vresp.Data.Result) == 0 {
		// No series at all for this asset over the window.
		return false, false
	}
	var series struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(vresp.Data.Result[0], &series); err != nil {
		return false, true
	}
	if len(series.Value) < 2 {
		return false, true
	}
	valStr, _ := series.Value[1].(string)
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return false, true
	}
	// The PromQL expression ends in `> 0`, so a 1.0 means present
	// and a 0.0 means absent. Anything else is a parse surprise
	// and we treat it as unknown (the dashboard can re-poll).
	if v >= 1.0 {
		return true, false
	}
	if v <= 0.0 {
		return false, false
	}
	return false, true
}

// assetHasTelemetryBatch issues ONE VM instant query that groups
// the presence of cios_metric by asset_path and returns the
// classification as a map. The map is keyed by asset_path; the
// boolean is "present" (true ⇒ VM saw samples, false ⇒ VM saw
// no samples in the window). Every path the caller passed in is
// pre-seeded with (false, known); the response fills in present
// assets. This mirrors the per-asset empty-vector contract: an
// asset that was queried but not returned is "absent, known",
// NOT "unknown" — same shape as the legacy per-asset code's
// "empty vector ⇒ not degraded" path. Round trips collapse from
// O(N) to O(1) per call (PRMT-086).
//
// Sink-side regexp.QuoteMeta per path (PRMT-086 regex-safe
// matcher); the whole alternation is anchored `^(...)$` for
// exact-match semantics. cpath forbids `" \ |` in real
// segments so QuoteMeta alone is sufficient; the per-asset
// sinks retain their escapeLabelValue wrap (PRMT-078 escape
// coverage stays load-bearing). Empty input returns (empty,
// false) so an empty asset set never fires a VM call.
//
// PRMT-094: upstream ctx is threaded through (caller passes scanner
// ctx or r.Context()); fetchVM still caps each call with
// vmUpstreamTimeout. The aggregate matcher that PRMT-086
// introduced is escaped per path here, keeping PRMT-078's wire-
// format sink closed for the batch path.
func (s *Server) assetHasTelemetryBatch(ctx context.Context, assetPaths []string, window string) (map[string]bool, bool) {
	out := make(map[string]bool, len(assetPaths))
	for _, p := range assetPaths {
		out[p] = false
	}
	if len(assetPaths) == 0 {
		return out, false
	}
	parts := make([]string, len(assetPaths))
	for i, p := range assetPaths {
		parts[i] = regexp.QuoteMeta(p)
	}
	// PromQL: count_over_time(cios_metric{asset_path=~"^(a|b|c)$"}[window]) > 0
	// Group-by yields one series per distinct asset_path, so the
	// result is a vector keyed by path.
	query := `count_over_time(cios_metric{asset_path=~"^(` + strings.Join(parts, "|") + `)$"}[` + window + `]) > 0 by (asset_path)`
	q := url.Values{}
	q.Set("query", query)
	body, err := s.fetchVM(ctx, s.vmURL+"/api/v1/query", q)
	if err != nil {
		return nil, true
	}
	var vresp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		return nil, true
	}
	if vresp.Status != "success" {
		return nil, true
	}
	if len(vresp.Data.Result) == 0 {
		// No series at all for any of the queried paths in the
		// window — every asset is "absent, not unknown". The
		// pre-seeded (false) values are the correct answer.
		return out, false
	}
	for _, raw := range vresp.Data.Result {
		var series struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(raw, &series); err != nil {
			continue
		}
		p := series.Metric["asset_path"]
		if p == "" {
			continue
		}
		if len(series.Value) < 2 {
			continue
		}
		valStr, _ := series.Value[1].(string)
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		// The PromQL expression ends in `> 0` so a 1.0 means
		// present and 0.0 means absent. Anything else is a
		// parse surprise; the pre-seeded (false) is the
		// conservative answer (we don't know it's present).
		if v >= 1.0 {
			out[p] = true
		} else if v <= 0.0 {
			out[p] = false
		}
	}
	return out, false
}

// fetchVMAssetPaths asks VM for every distinct asset_path label
// value currently present in any sample. Used to compute the
// orphan set (telemetry without CMDB). Returns the list and a
// degradation flag.
//
// Implementation note: VictoriaMetrics exposes `label_values`
// through /api/v1/label/<name>/values; the instant-query path
// also accepts a `label_values(...)` selector. We use the instant
// path so the helper composes with the same fetchVM seam. The
// response shape is vector (one series per distinct label value)
// and we extract asset_path from each series's metric map.
//
// PRMT-094: upstream ctx is threaded through so the orphan scan
// can be cancelled by client disconnect / server shutdown.
func (s *Server) fetchVMAssetPaths(ctx context.Context, window string) ([]string, bool) {
	// Any cios_* metric works; we just need the label_set. The
	// empty match `{}` returns every series the instance knows
	// about (subject to its retention / lookback).
	query := `label_values(cios_metric, asset_path)`
	q := url.Values{}
	q.Set("query", query)
	body, err := s.fetchVM(ctx, s.vmURL+"/api/v1/query", q)
	if err != nil {
		return nil, true
	}
	var vresp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		return nil, true
	}
	if vresp.Status != "success" {
		return nil, true
	}
	if len(vresp.Data.Result) == 0 {
		// Empty but not failed — distinct from failure mode.
		return []string{}, false
	}
	paths := make([]string, 0, len(vresp.Data.Result))
	for _, raw := range vresp.Data.Result {
		var series struct {
			Metric map[string]string `json:"metric"`
		}
		if err := json.Unmarshal(raw, &series); err != nil {
			// One bad row does not poison the whole list.
			continue
		}
		if p := series.Metric["asset_path"]; p != "" {
			paths = append(paths, p)
		}
	}
	return paths, false
}

// --- Drift auto-ticket scanner (PRMT-057 / M2 E2.6 闭环) -------
//
// The endpoint classify drift; the scanner closes the loop by
// opening one ticket per active asset that is registered but has
// no telemetry over the trailing window. Reuses computeReconcile
// so the HTTP report and the scanner see identical
// classifications — no second per-asset VM probe.
//
// Idempotency: the ticket's existing `alarm_id` field carries
// `reconcile:<assetPath>` — a namespace tag distinct from
// `spare:<id>` and `io.cios.alarm.<id>` (spec-008 §16/v0.4
// documents the convention). The dedup check is
// `alarm_id == "reconcile:<path>" AND state != "closed"`, mirroring
// the spare stock scanner.
//
// Restored telemetry does NOT auto-close the ticket (spec-008 Q5
// reserves close for human acknowledgement). The next tick
// simply finds the open ticket and skips; if it is then closed
// and telemetry drops again, a fresh ticket opens. One ticket
// per drift event, not one per forever-drift.
//
// Default-off: interval<=0 → no goroutine, no log spam (matches
// the report scheduler's "empty=off" convention).

// reconcileAlarmID returns the alarm_id dedup key for a given
// asset path. The "reconcile:" prefix marks this as a CMDB/VM
// drift source (vs the `io.cios.alarm.<id>` shape used by
// alarm-driven tickets and `spare:<id>` for low-stock). The
// prefix makes the namespace grep-able in /v1/tickets?alarm_id=…
// without colliding with existing dedup keys.
func reconcileAlarmID(assetPath string) string {
	return "reconcile:" + assetPath
}

// RunReconcileScanner is the long-lived drift auto-ticket
// background goroutine. Mirrors RunReportScheduler's "default-
// off" contract: interval<=0 → returns immediately (no goroutine,
// no startup tick). Otherwise startup tick + ticker + ctx.Done
// exit, fail-soft on per-asset errors.
//
// window is the trailing presence window used by computeReconcile
// (e.g. "7d"). The caller (-reconcile-window) owns the choice;
// the scanner doesn't second-guess.
func (s *Server) RunReconcileScanner(ctx context.Context, interval time.Duration, window string) {
	if interval <= 0 {
		// Default-off: matches the report scheduler convention so
		// existing deployments see no behaviour change when the
		// binary is upgraded. Logged once so an operator can tell
		// the flag was parsed but the scanner stayed parked.
		log.Printf("core: reconcile scanner: disabled (interval<=0)")
		return
	}
	if !validCapacityWindow(window) {
		// Misconfigured flag — fall back to the HTTP default so a
		// typo on the command line doesn't silently nuke the
		// scanner's classification logic.
		window = defaultReconcileWindow
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run one scan at startup so a freshly-booted cios-core picks
	// up any drift that accumulated while it was down — same
	// pattern as RunSLAScanner / RunPMScanner.
	// safeTick (PRMT-076) so a panic in scanReconcileTick can't kill
	// the long-lived goroutine.
	safeTick("reconcile", func() { s.scanReconcileTick(ctx, time.Now().UTC(), window) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("reconcile", func() { s.scanReconcileTick(ctx, now.UTC(), window) })
		}
	}
}

// scanReconcileTick is one iteration: computeReconcile over the
// supplied window, then for each `registered_no_telemetry` asset
// (and ONLY that state) dedup-check and fire. Pulled out of
// RunReconcileScanner so tests can drive a single tick
// deterministically (mirror of scanPMTick / scanSpareStockTick).
func (s *Server) scanReconcileTick(ctx context.Context, now time.Time, window string) {
	// PRMT-066: record the tick outcome for /v1/health/scanners.
	// Captured by the deferred closure so any return path
	// (lock failure, leader skip, degraded VM, per-asset error)
	// produces a registry entry.
	var tickErr error
	defer func() {
		s.recordScanner("reconcile", now, tickErr)
	}()
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the reconcile tick for
	// this tick window. The pg advisory lock is session-scoped
	// and released when the tick ends (release is deferred). On
	// error we log + skip (fail-soft, next tick will retry); on
	// !acquired we silently skip — another instance leads.
	ok, release, err := s.st.TryScannerLock(ctx, "reconcile")
	if err != nil {
		log.Printf("core: reconcile scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()
	rep := s.computeReconcile(ctx, window)
	if rep.Degraded {
		// VM unreachable / partial failure. Drift classification
		// is unreliable on a degraded read — skip opening tickets
		// rather than risk false positives. The next tick will
		// re-evaluate.
		log.Printf("core: reconcile scanner: degraded; skipping tick")
		tickErr = errors.New("vm degraded")
		return
	}
	for _, e := range rep.Entries {
		if e.State != "registered_no_telemetry" {
			continue
		}
		s.fireDriftTicket(ctx, e.Path, now)
	}
}

// fireDriftTicket opens one drift ticket for assetPath if no
// open ticket is already pinned to it via the
// `alarm_id="reconcile:<path>"` dedup key. Best-effort:
// dedup-check or PutTicket failures are logged, but we never
// propagate — the next tick will retry. There is no "advance"
// step (no NextDue / LastRun) because drift is a steady-state
// condition; once a ticket is open it stays open until a human
// closes it.
func (s *Server) fireDriftTicket(ctx context.Context, assetPath string, now time.Time) {
	if hasOpenDriftTicket(ctx, s.st, assetPath) {
		// Idempotent: an open ticket already pins this asset. Hot
		// path — log nothing so the scanner doesn't spam every tick.
		return
	}
	t := Ticket{
		ID:        newTicketID(),
		AlarmID:   reconcileAlarmID(assetPath),
		AssetPath: assetPath,
		Title:     "No telemetry: " + assetPath,
		Severity:  "major",
		State:     "open",
		OpenedAt:  now,
	}
	if _, err := s.st.PutTicket(ctx, t, 0); err != nil {
		log.Printf("core: reconcile scanner: put ticket for %s: %v", assetPath, err)
		return
	}
	s.emitTicketEventAsync(t, ticketEventTypeOpened)
}

// hasOpenDriftTicket reports whether an open (state != "closed")
// ticket is already pinned to assetPath via the
// `alarm_id="reconcile:<path>"` dedup key. Pure check; no side
// effects. Errors are logged and treated as "no open ticket" so
// a transient store failure does not block the scan loop.
func hasOpenDriftTicket(ctx context.Context, st Store, assetPath string) bool {
	all, err := st.ListTickets(ctx)
	if err != nil {
		log.Printf("core: reconcile scanner: list tickets for dedup: %v", err)
		return false
	}
	key := reconcileAlarmID(assetPath)
	for _, t := range all {
		if t.AlarmID == key && t.State != "closed" {
			return true
		}
	}
	return false
}
