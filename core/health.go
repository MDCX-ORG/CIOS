// Package core — health.go: liveness, readiness, and scanner
// status endpoints (PRMT-066 / T34 meta-monitoring mechanism).
//
// Three surfaces, all under /v1/health:
//
//	GET /v1/health           → liveness: process is up. Always 200.
//	                          No dependency probing (a process that
//	                          cannot answer an in-memory ping is
//	                          dead — readiness is what tells us if
//	                          the deps are reachable).
//	GET /v1/health/ready     → readiness: probes PG (via Store) +
//	                          VM (via s.fetchVM). 200 when all
//	                          green; 503 + the list of down dep
//	                          names otherwise. Body never leaks
//	                          data or topology — just status +
//	                          dep names ("pg", "vm").
//	GET /v1/health/scanners  → per-scanner last-run + last-error
//	                          snapshot. Viewer-protected.
//
// Design note (PRMT-066 §0): /v1/health and /v1/health/ready
// are PUBLIC on purpose. Probe endpoints (k8s livenessProbe /
// readinessProbe, load-balancer health, monitoring uptime
// checks) cannot carry bearer tokens — exposing these as
// authenticated would mean a misconfigured probe marks every
// pod down. The authmw layer treats these two paths as
// explicit "no auth" carries; /v1/health/scanners is the
// human-facing read and stays viewer-gated.
//
// Scanner status: each long-lived scanner tick ends with
// s.recordScanner(name, err) so the registry always reflects
// "most recent tick outcome". recordScanner is concurrency-safe
// under sync.Mutex (the test runs 100 goroutines × 100 ticks
// each under go test -race to prove it).
//
// PRMT-066 §2 interface contract.
package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// --- scanner status registry ----------------------------------------------

// scannerStatusEntry is one scanner's most recent tick outcome.
// LastRun is the wall-clock time of the last completed tick
// (UTC). LastErr is "" when the last tick succeeded; otherwise
// it carries the human-readable error string. LastErr is NOT a
// stack trace and NOT a counter — the prompt keeps it simple so
// the JSON body stays a one-line "is this scanner healthy?"
// answer for on-call.
type scannerStatusEntry struct {
	LastRun time.Time `json:"last_run"`
	LastErr string    `json:"last_error,omitempty"`
}

// scannerStatusRegistry holds per-scanner status entries.
// Mutations are guarded by a single sync.Mutex — the write rate
// is "one tick per scanner per interval" (multi-second), so a
// coarse mutex has zero contention impact on the hot path. The
// /v1/health/scanners handler grabs the mutex once, copies the
// map, and releases before serializing so the write side never
// blocks on JSON encoding.
//
// Initialised in NewServer (alongside httpClient) so the
// registry is always non-nil — scanners can call recordScanner
// without a nil-check and tests can drive recordScanner without
// a NewServer round-trip (by constructing an entry directly).
type scannerStatusRegistry struct {
	mu      sync.Mutex
	entries map[string]scannerStatusEntry
}

func newScannerStatusRegistry() *scannerStatusRegistry {
	return &scannerStatusRegistry{entries: map[string]scannerStatusEntry{}}
}

// record stores the tick outcome for name. err == nil clears
// the last_error string; err != nil captures err.Error().
// Always updates LastRun to at so a failing scanner still has
// a fresh heartbeat (otherwise an "errored and silent" scanner
// would look like "never ran").
func (r *scannerStatusRegistry) record(name string, at time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := scannerStatusEntry{LastRun: at.UTC()}
	if err != nil {
		entry.LastErr = err.Error()
	}
	r.entries[name] = entry
}

// snapshot returns a copy of the registry sorted by name so the
// JSON output is deterministic (no map-iteration randomness in
// tests). The returned map is a fresh allocation — the caller
// can mutate it without affecting the registry.
func (r *scannerStatusRegistry) snapshot() map[string]scannerStatusEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]scannerStatusEntry, len(r.entries))
	keys := make([]string, 0, len(r.entries))
	for k := range r.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = r.entries[k]
	}
	return out
}

// --- Server wiring --------------------------------------------------------

// recordScanner is the public hook scanners call at the end of
// each tick. It delegates to the registry so the scanner files
// (sla.go / pm.go / etc.) do not import sync / mutex types. A
// nil registry is treated as "registry not initialised" — that
// cannot happen via NewServer (which always sets one) but is
// the safest behaviour if a future constructor skips it.
func (s *Server) recordScanner(name string, at time.Time, err error) {
	if s.scanners == nil {
		return
	}
	s.scanners.record(name, at, err)
}

// --- handlers --------------------------------------------------------------

// healthResponse is the liveness body. The shape is fixed
// (status: "ok") so a probe that pattern-matches on the body
// still works on a frozen JSON wire format.
type healthResponse struct {
	Status string `json:"status"`
}

// serveHealth handles GET /v1/health. Always 200 + {"status":"ok"}.
// No dependency probing by design: an in-memory handler can only
// fail to respond if the process is dead or wedged, and in
// either case the load balancer / kubelet marks the pod out
// without needing a body. See package doc for the auth decision.
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// readyResponse is the readiness body. Status is "ok" when all
// dependencies are reachable and "degraded" otherwise. Down is
// the sorted list of dependency names that failed the probe
// ("pg", "vm" — never anything more specific; see package doc).
type readyResponse struct {
	Status string   `json:"status"`
	Down   []string `json:"down,omitempty"`
}

// readinessTimeout caps the per-dependency probe so a hung
// backend cannot wedge the readiness endpoint. 2s is half of
// vmUpstreamTimeout (5s) so the endpoint can fail-fast even
// with both deps dead in sequence.
const readinessTimeout = 2 * time.Second

// serveReady handles GET /v1/health/ready. Probes Store
// (ListAlarms acts as a lightweight ping for both fileStore —
// always-fresh in-memory — and pgStore — a real SQL roundtrip)
// and VictoriaMetrics (s.fetchVM on /api/v1/query with a
// constant vector query). 200 when both green; 503 + the sorted
// list of down dep names otherwise.
//
// Public by design — see package doc. The dep-name list is the
// ONLY thing in the body; we never echo the VM URL, the DSN,
// the host, or anything else an attacker could use to pivot.
func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	var down []string
	if !s.probeStore() {
		down = append(down, "pg")
	}
	if !s.probeVM(r.Context()) {
		down = append(down, "vm")
	}
	if len(down) > 0 {
		sort.Strings(down)
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{
			Status: "degraded",
			Down:   down,
		})
		return
	}
	writeJSON(w, http.StatusOK, readyResponse{Status: "ok"})
}

// probeStore issues a cheap read against the Store. ListAlarms
// is the lightest existing call (no args, no pagination, no
// body parse) — it works for both backends without needing a
// Ping method on the Store interface (which the whitelist does
// not let us add). For pgStore the roundtrip exercises the pool
// (effective ping); for fileStore the in-memory map read is
// always fast (the readiness check stays useful as a process-
// liveness signal even though the file path is local).
func (s *Server) probeStore() bool {
	ctx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
	defer cancel()
	// ListAlarms is the cheapest existing probe and exercises the
	// store's read path. The returned slice is discarded — we
	// only care about err.
	done := make(chan error, 1)
	var err error
	go func() {
		_, err = s.st.ListAlarms(ctx)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return false
	case e := <-done:
		return e == nil
	}
}

// probeVM issues a one-line vector query against VM. We reuse
// s.fetchVM (the same seam capacity / reconcile use) so the
// timeout / body-cap / status-code behaviour matches the rest
// of the codebase. The query is "vector(1)" — a constant that
// returns success on any healthy VM without depending on
// tenant data being present.
func (s *Server) probeVM(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, readinessTimeout)
	defer cancel()
	q := url.Values{}
	q.Set("query", "vector(1)")
	body, err := s.fetchVM(ctx, s.vmURL+"/api/v1/query", q)
	if err != nil {
		return false
	}
	var vresp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		return false
	}
	return vresp.Status == "success"
}

// scannersResponse is the /v1/health/scanners envelope. The
// "scanners" map is keyed by scanner name ("sla", "pm",
// "inspection", "spare", "reconcile", "report") and matches
// the per-scanner entry shape. An empty map renders as `{}`
// so the JSON stays a valid object even before the first tick.
type scannersResponse struct {
	Scanners map[string]scannerStatusEntry `json:"scanners"`
}

// serveScanners handles GET /v1/health/scanners. Returns the
// full registry snapshot. Viewer+ — the auth middleware applies
// the role floor; per-item scope filter does not apply (the
// scanner status is site-wide metadata, not asset-scoped data).
func (s *Server) serveScanners(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request",
			"method not allowed", "", r.URL.Path, rid)
		return
	}
	// Auth middleware attaches the Principal when auth is enabled
	// and the caller is in role. The handler does NOT re-check
	// scope — the per-scanner name is a fixed enumeration, not
	// an asset path, and "viewer floor" is enforced by the
	// middleware's list-scope branch.
	_, _ = PrincipalFromContext(r.Context())
	var snap map[string]scannerStatusEntry
	if s.scanners != nil {
		snap = s.scanners.snapshot()
	} else {
		snap = map[string]scannerStatusEntry{}
	}
	writeJSON(w, http.StatusOK, scannersResponse{Scanners: snap})
}
