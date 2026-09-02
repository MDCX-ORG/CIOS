// Package core — server.go assembles the HTTP handler. It owns
// three cross-cutting concerns: RFC 7807 problem responses,
// request_id generation/propagation, and the in-memory request
// dedup table for apply-idempotency. Per-resource handlers live in
// assets.go, metrics.go, alarms.go.
package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/reqid"
)

// internalLog is the sink writeProblem (5xx branch) and
// writeInternalProblem use to record the *real* error before the
// public detail is scrubbed. Default is the same log.Printf the
// access-log middleware uses (see middleware.go) so operators see
// one consistent format. Tests inject a sink via SetInternalLogForTest
// (mirrors SetAccessLogForTest).
//
// PRMT-083 §2.
var internalLog = log.Printf

// SetInternalLogForTest replaces the 5xx internal-error sink.
// Returns a restore func the test must defer to undo the override.
func SetInternalLogForTest(fn func(format string, args ...any)) (restore func()) {
	prev := internalLog
	internalLog = fn
	return func() { internalLog = prev }
}

// maxInternalDetail is the upper bound on the text we copy into
// the server-side log line for a 5xx scrub. The prompt allows
// "reasonable length" truncation; 8 KiB is well past anything a
// human reads in a log viewer while staying clear of Go's default
// 1 MiB logger cap.
//
// PRMT-083 §2 (truncate to reasonable length).
const maxInternalDetail = 8 << 10

// truncateDetail clamps a free-form error/detail string to
// maxInternalDetail bytes. UTF-8 boundary-safe: cut on the last
// valid boundary at or below the cap so we never split a rune.
func truncateDetail(s string) string {
	if len(s) <= maxInternalDetail {
		return s
	}
	cut := maxInternalDetail
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...(truncated)"
}

// utf8RuneStart reports whether s[i] is the start of a UTF-8
// rune (i.e. s[i]&0xC0 != 0x80, meaning it's not a continuation
// byte). Equivalent to utf8.RuneStart but inlined to avoid
// importing unicode/utf8 for a 4-line helper.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Server is the HTTP front of cios-core. St is the persistence
// seam; Dict is the protocol dictionary (for path validation);
// vmURL is the upstream VictoriaMetrics base URL (e.g.
// http://127.0.0.1:8428). All three are immutable after NewServer.
type Server struct {
	st    Store
	d     *cpath.Dict
	vmURL string
	dedup *dedupTable
	// now is overridable for tests; production uses time.Now.
	now func() time.Time
	// auth gates /v1 requests when non-nil. nil → M0 unauthenticated
	// behaviour (existing server_test.go must not regress). Set via
	// ServerConfig.Auth on NewServerFromConfig; the legacy 3-arg
	// NewServer leaves it nil. PRMT-019 §4.6.
	auth *AuthConfig
	// ticketWebhookURLs and httpClient back emitTicketEvent (PRMT-035
	// + PRMT-200 fan-out). Set via SetTicketWebhookURL(s); httpClient
	// defaults to a timeout-bounded client in the constructors so
	// emitTicketEvent cannot nil-deref when URLs are configured
	// without a client. Empty slice → no-op emit.
	ticketWebhookURLs []string
	httpClient        *http.Client
	// ticketSMTP is the optional email channel (P783 / L105).
	// Set via SetTicketSMTP; nil → no-op.
	ticketSMTP *TicketSMTPConfig
	// vmHTTP is the client used for VictoriaMetrics upstream calls
	// (P793 Phase 3 TLS). nil → http.DefaultClient.
	vmHTTP *http.Client
	// runbookDir is the on-disk root for /v1/runbooks/{key}
	// (PRMT-044). Empty → runbook reads return 404. Set via
	// SetRunbookDir from cmd/cios-core's -runbook-dir flag.
	runbookDir string
	// inspectionPhotoDir is the on-disk root for
	// /v1/inspections/form/{id}/photo (PRMT-063). Empty → upload
	// returns 503 disabled. Set via SetInspectionPhotoDir from
	// cmd/cios-core's -inspection-photo-dir flag.
	inspectionPhotoDir string
	// inspectionPhotoMax is the per-file size cap for photo
	// uploads (PRMT-063). Defaults to 8 MiB; cmd/cios-core's
	// -inspection-photo-max overrides. Read at request time.
	inspectionPhotoMax int64
	// scanners is the in-memory per-scanner status registry
	// backing GET /v1/health/scanners (PRMT-066). Mutex-
	// protected internally; scanners call s.recordScanner at the
	// end of each tick. Initialised in NewServer so scanners
	// never need a nil-check; nil is the "registry not wired"
	// safety belt for tests that build a Server by hand.
	scanners *scannerStatusRegistry
	// usageSink receives OnUsageUpserted after compute upserts
	// (PRMT-195). Nil → NoopUsageEventSink at call sites.
	usageSink UsageEventSink
	// controlSink receives policy-passed Sets (P722). Nil →
	// policy-only Accepted with dispatched=false.
	controlSink ControlSink
	// pendingSets holds class-A control writes awaiting second-token
	// approval (PRMT-235 two-man rule), keyed by pending id. Lazily
	// initialized under pendingMu (no constructor changes).
	pendingMu   sync.Mutex
	pendingSets map[string]pendingControl
}

// AuthConfig enables the auth middleware on Handler(). nil ⇒ M0
// behaviour (no auth). Verifier is required when enabling; the
// future *oidcVerifier slot lives at the same seam.
//
// PRMT-019 §4.6.
type AuthConfig struct {
	Verifier TokenVerifier
}

// ServerConfig is the structured input for NewServerFromConfig. It
// adds the optional DSN switch (L55② → pgStore) without breaking
// the legacy NewServer(st, dict, vmURL) entry point that the
// existing server_test.go and cmd/cios-core call. Empty DSN keeps
// the M0 fileStore path bit-for-bit identical; non-empty DSN
// boots a pgxpool, runs migrations/001_init.sql, and returns a
// Store backed by PostgreSQL.
//
// PRMT-016 §4.9, PRMT-019 §4.6 (Auth seam).
type ServerConfig struct {
	StorePath     string // path to the JSON fileStore (used when DSN is empty)
	DSN           string // PostgreSQL DSN; empty → fileStore
	MigrationsDir string // path to migrations/; default "./migrations" relative to binary
	VMURL         string
	Dict          *cpath.Dict
	// Auth, when non-nil, enables bearer-token + RBAC on Handler().
	// nil keeps the M0 unauthenticated default — existing M0 callers
	// (cmd/cios-core boot, server_test.go) leave this nil and observe
	// no behaviour change.
	Auth *AuthConfig
}

// NewServer builds a Server. Returns a ready-to-use *Server; the
// caller wraps Handler() in an http.Server with timeouts.
func NewServer(st Store, d *cpath.Dict, vmURL string) *Server {
	return &Server{
		st:         st,
		d:          d,
		vmURL:      strings.TrimRight(vmURL, "/"),
		dedup:      newDedupTable(24 * time.Hour),
		now:        time.Now,
		httpClient: &http.Client{Timeout: ticketWebhookTimeout},
		scanners:   newScannerStatusRegistry(),
	}
}

// NewServerWithStore builds a Server from an already-constructed
// Store plus an optional AuthConfig. It exists so a caller that must
// touch the Store before serving (e.g. cmd/cios-core's seed-alarms)
// can do so and still enable auth — NewServer(st, d, vmURL) cannot
// set auth (its 3-arg signature is frozen, M0-CHECKPOINT §3.8) and
// NewServerFromConfig builds the Store internally (no seam to seed).
// auth==nil ⇒ no auth (M0).
//
// PRMT-M1-Checkpoint-Fix-R1 §4.1.
func NewServerWithStore(st Store, d *cpath.Dict, vmURL string, auth *AuthConfig) *Server {
	s := NewServer(st, d, vmURL)
	s.auth = auth
	return s
}

// NewServerFromConfig selects the persistence backend by config:
//   - DSN == ""   → NewFileStore(StorePath)   (M0 behaviour preserved)
//   - DSN != ""  → NewPGStore(ctx, DSN, MigrationsDir) (L55② replacement)
//
// and then hands the Store to the same NewServer that the M0
// callers use. The MigrationsDir default ("./migrations") is
// applied here so callers do not have to plumb a constant. The
// context is forwarded to the pgxpool so the boot can be
// cancelled cleanly.
func NewServerFromConfig(ctx context.Context, cfg ServerConfig) (*Server, error) {
	if cfg.Dict == nil {
		return nil, fmt.Errorf("core: server config: nil dict")
	}
	if cfg.VMURL == "" {
		return nil, fmt.Errorf("core: server config: empty vm url")
	}
	migDir := cfg.MigrationsDir
	if migDir == "" && cfg.DSN != "" {
		migDir = "./migrations"
	}

	var (
		st  Store
		err error
	)
	if cfg.DSN != "" {
		st, err = NewPGStore(ctx, cfg.DSN, migDir)
		if err != nil {
			return nil, fmt.Errorf("core: pg store: %w", err)
		}
	} else {
		st, err = NewFileStore(cfg.StorePath)
		if err != nil {
			return nil, fmt.Errorf("core: file store: %w", err)
		}
	}
	srv := NewServer(st, cfg.Dict, cfg.VMURL)
	srv.auth = cfg.Auth
	return srv, nil
}

// SetVMHTTPClient sets the HTTP client used for VictoriaMetrics
// (P793 Phase 3). nil resets to http.DefaultClient.
func (s *Server) SetVMHTTPClient(c *http.Client) {
	s.vmHTTP = c
}

// vmClient returns the VM upstream client (DefaultClient when unset).
func (s *Server) vmClient() *http.Client {
	if s.vmHTTP != nil {
		return s.vmHTTP
	}
	return http.DefaultClient
}

// SetTicketWebhookURL configures a single ticket lifecycle webhook
// (PRMT-035). url=="" disables emission. client==nil falls back to
// the default timeout-bounded client built by NewServer.
// Prefer SetTicketWebhookURLs for multi-channel fan-out (PRMT-200).
func (s *Server) SetTicketWebhookURL(url string, client *http.Client) {
	if strings.TrimSpace(url) == "" {
		s.ticketWebhookURLs = nil
	} else {
		s.ticketWebhookURLs = []string{strings.TrimSpace(url)}
	}
	if client != nil {
		s.httpClient = client
	} else if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: ticketWebhookTimeout}
	}
}

// SetTicketWebhookURLs configures N webhook channels (PRMT-200 /
// P644 v0). Empty / whitespace entries dropped; order preserved;
// duplicates removed. client==nil uses default timeout client.
func (s *Server) SetTicketWebhookURLs(urls []string, client *http.Client) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	s.ticketWebhookURLs = out
	if client != nil {
		s.httpClient = client
	} else if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: ticketWebhookTimeout}
	}
}

// SetRunbookDir configures the on-disk root for /v1/runbooks/{key}
// (PRMT-044). Empty dir disables the endpoint (returns 404). The
// path is read at request time, so changes after boot are
// reflected without restart.
func (s *Server) SetRunbookDir(dir string) {
	s.runbookDir = dir
}

// defaultInspectionPhotoMax is the per-file cap applied when
// SetInspectionPhotoDir is called without a max (or before
// SetInspectionPhotoMax is invoked). 8 MiB matches the prompt's
// default; raising it later is a config change, not a code change.
const defaultInspectionPhotoMax int64 = 8 << 20

// SetInspectionPhotoDir configures the on-disk root for
// /v1/inspections/form/{id}/photo (PRMT-063). Empty dir disables
// the endpoint (returns 503). maxBytes==0 falls back to
// defaultInspectionPhotoMax. The fields are read at request time
// so changes after boot are reflected without restart.
func (s *Server) SetInspectionPhotoDir(dir string, maxBytes int64) {
	s.inspectionPhotoDir = dir
	if maxBytes > 0 {
		s.inspectionPhotoMax = maxBytes
	} else if s.inspectionPhotoMax == 0 {
		s.inspectionPhotoMax = defaultInspectionPhotoMax
	}
}

// SetUsageEventSink configures the UsageEventSink (PRMT-195/198).
// nil → call sites use NoopUsageEventSink. Typical wire:
// NATSUsageEventSink{Pub: natsConn} from cmd/cios-core -nats-url.
func (s *Server) SetUsageEventSink(sink UsageEventSink) {
	s.usageSink = sink
}

// SetControlSink configures southbound Set dispatch (P722).
// nil → policy-only Accepted with dispatched=false.
func (s *Server) SetControlSink(sink ControlSink) {
	s.controlSink = sink
}

// Handler returns the http.Handler for the whole M0 API surface.
// The ServeMux matches exact paths; we route by method+prefix in
// each handler because the resources share path prefixes
// (/v1/assets/{path} vs /v1/assets for LIST).
//
// When Server.auth is non-nil, the auth middleware is inserted
// BETWEEN withRequestID and the mux so the audit line can read the
// request_id from ctx. PRMT-019 §4.4 / §4.6.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/assets", s.serveAssetsRoot)
	// PRMT-039: serveAssetPath dispatches /v1/assets/{path} on
	// PUT/GET/DELETE and POST ...:{suffix} (currently :lifecycle)
	// to serveAssetLifecycle. Registering once avoids the second
	// mux.HandleFunc("/v1/assets/", ...) silently overwriting the
	// first (Go's ServeMux does NOT chain handlers per prefix).
	mux.HandleFunc("/v1/assets/", s.serveAssetPath)
	mux.HandleFunc("/v1/alarms", s.serveAlarms)
	mux.HandleFunc("/v1/alarms/", s.serveAlarmAck) // PRMT-230: {id}:ack only
	mux.HandleFunc("/v1/tickets", s.serveTickets)
	mux.HandleFunc("/v1/tickets/", s.serveTicket)
	mux.HandleFunc("/v1/reports/ops", s.serveOpsReport)
	mux.HandleFunc("/v1/reports/reconcile", s.serveReconcile)
	mux.HandleFunc("/v1/pm/schedules", s.servePMSchedules)
	mux.HandleFunc("/v1/pm/schedules/", s.servePMSchedule)
	mux.HandleFunc("/v1/control/audit", s.serveControlAudit) // PRMT-234
	mux.HandleFunc("/v1/control/", s.serveControlApprove)    // PRMT-235: {id}:approve only
	// PRMT-209: customer uptime SLA stub (E3.4 / P631). Constant
	// read — not ticket-SLA (sla.go). Display-only credit note.
	mux.HandleFunc("/v1/sla", s.serveCustomerSLA)
	// PRMT-193: Commercial Platform usage list/export/compute
	// (spec-010 §3 / L102). Exact paths — colon verbs are not
	// prefixes under /v1/usage/.
	mux.HandleFunc("/v1/usage", s.serveUsage)
	mux.HandleFunc("/v1/usage:export", s.serveUsageExport)
	mux.HandleFunc("/v1/usage:compute", s.serveUsageCompute)
	mux.HandleFunc("/v1/runbooks/", s.serveRunbook)
	mux.HandleFunc("/v1/cases", s.serveCases)
	mux.HandleFunc("/v1/spares", s.serveSpares)
	mux.HandleFunc("/v1/spares/", s.serveSpare)
	mux.HandleFunc("/v1/inspections", s.serveInspections)
	mux.HandleFunc("/v1/inspections/", s.serveInspection)
	// PRMT-059: mobile-web inspection form. The /form/ sub-tree
	// is a separate handler from serveInspection because the form
	// serves text/html (not JSON) and uses POST for submission —
	// it cannot share serveInspection's GET-only contract.
	mux.HandleFunc("/v1/inspections/form/", s.serveInspectionForm)
	// PRMT-058: merged PM + inspection "upcoming" view. Read-only
	// aggregation; the per-item authorize(ActionRead, asset_path)
	// filter lives in the handler (same shape as /v1/pm/schedules,
	// /v1/inspections, /v1/reports/ops).
	mux.HandleFunc("/v1/maintenance/upcoming", s.serveMaintenanceUpcoming)
	// PRMT-096: explicit maintenance-window CRUD. List/create on the
	// root, delete on the {id} sub-path. Read by cios-alarm via the
	// shared PG table (Store.ActiveWindowFor on every OpenTicket).
	mux.HandleFunc("/v1/maintenance/windows", s.serveMaintenanceWindowsRoot)
	mux.HandleFunc("/v1/maintenance/windows/", s.ServeMaintenanceWindow)
	// PRMT-182: admin-only governed write path for isolation_tier.
	// L109 P804: GET/POST /v1/tenants list+create (platform admin).
	// Prefixed /v1/tenants/ keeps PRMT-182 POST {id}:tier.
	mux.HandleFunc("/v1/tenants", s.serveTenants)
	mux.HandleFunc("/v1/tenants/", s.serveTenantTier)
	// PRMT-185: /v1/orgs admin CRUD (list/create on root, get/
	// rename/delete on the {id} sub-path). Admin-gated; tenant-scoped
	// (R1) via TenantFromContext; R5 delete-guard owned by Store.
	mux.HandleFunc("/v1/orgs", s.serveOrgs)
	mux.HandleFunc("/v1/orgs/", s.serveOrg)
	// L109 P802/P803: site→org registry + role_bindings admin surfaces.
	mux.HandleFunc("/v1/site-orgs", s.serveSiteOrgs)
	mux.HandleFunc("/v1/role-bindings", s.serveRoleBindings)
	// L109 P811–P813: Model Studio catalog / soft lint / S-layer bindings.
	mux.HandleFunc("/v1/model-packs", s.serveModelPacks)
	mux.HandleFunc("/v1/model-packs/", s.serveModelPack)
	// L109 P821–P825: Site-Draw layout + CMDB writeback.
	mux.HandleFunc("/v1/site-layouts", s.serveSiteLayoutsRoot)
	mux.HandleFunc("/v1/site-layouts/", s.serveSiteLayout)
	mux.HandleFunc("/v1/metrics/query", s.serveMetricsQuery)
	mux.HandleFunc("/v1/metrics/query_range", s.serveMetricsQuery)
	mux.HandleFunc("/v1/points/", s.servePoint)
	// PRMT-066: liveness + readiness + scanner status. The auth
	// middleware marks /v1/health and /v1/health/ready as public
	// (probe endpoints cannot carry bearer tokens — see
	// core/authmw.go) and treats /v1/health/scanners as
	// viewer-protected (list-scope role floor).
	mux.HandleFunc("/v1/health", s.serveHealth)
	mux.HandleFunc("/v1/health/ready", s.serveReady)
	mux.HandleFunc("/v1/health/scanners", s.serveScanners)
	var h http.Handler = mux
	if s.auth != nil && s.auth.Verifier != nil {
		h = newAuthMiddleware(s.auth.Verifier, h)
	} else {
		// Lab-only (-allow-no-auth / no -rbac): L109 admin handlers
		// call requireOrgAdmin → PrincipalFromContext. Without a
		// principal, GET/POST /v1/tenants|orgs|site-orgs always 403
		// even though the rest of /v1 is open. Inject a synthetic
		// platform-admin principal so portal-live / edge demos can
		// exercise admin surfaces. Never active when RBAC is on.
		h = labNoAuthAdminPrincipal(h)
	}
	// P793 H3: tenant-header peer gate is independent of RBAC so
	// -allow-no-auth lab still enforces mTLS peer rules when
	// CIOS_MTLS_MODE=require (authMW alone would skip).
	h = newTenantMTLSGate(h)
	// PRMT-074: access-log middleware sits INSIDE request-id so
	// the request_id it logs is the one the response header
	// carries. auth (if enabled) sits INSIDE access-log so the
	// access line records the final status (401/403 included),
	// not a 200 from an inner short-circuit.
	h = accessLogMiddleware(h)
	return withRequestID(h)
}

// LabBypassAvailable reports whether this binary was built with
// -tags lab and therefore includes the lab auth bypass
// (labNoAuthAdminPrincipal injects a synthetic admin when RBAC is
// off). Production builds (default tags) return false; cmd/cios-core
// refuses -allow-no-auth in that case. PRMT-217 (report S-1).
func LabBypassAvailable() bool { return labBypassAvailable }

// withRequestID wraps the mux so every request gets a request_id
// (X-Request-Id header in, generated if absent, echoed in the
// response). The id also flows into request.Context so handler
// bodies can grab it via RequestIDFromContext for problem responses.
// PRMT-030 §A: id generation moved to pkg/reqid.
func withRequestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = reqid.New()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRID, rid)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- request_id in context ----------------------------------------------

type ctxKey int

const ctxKeyRID ctxKey = 1

// RequestIDFromContext extracts the per-request id. Falls back to ""
// if the wrapper was bypassed (e.g. in a test that calls the handler
// directly without withRequestID).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRID).(string); ok {
		return v
	}
	return ""
}

// --- problem response (RFC 7807) ----------------------------------------

// problemTypeBase is the only place the error registry URL is built.
// Keep the tail registry in sync with spec-004 §4 (and §4.4
// upstream-unavailable added in this prompt — see §8).
const problemTypeBase = "https://cios.dev/errors/"

// writeProblem emits an RFC 7807 response with the standard fields.
// The type URL is built from problemTypeBase + typeTail. Empty
// detail / instance are omitted (not emitted as "").
//
// 4xx and 5xx both pass detail through verbatim. 5xx responses
// that need to hide store / VM / OS internals from the caller
// must go through writeInternalProblem, which scrubs the public
// detail to a generic string and logs the real error server-side.
//
// PRMT-083.
func writeProblem(w http.ResponseWriter, status int, typeTail, title, detail, instance, requestID string) {
	type problem struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Detail    string `json:"detail,omitempty"`
		Instance  string `json:"instance,omitempty"`
		RequestID string `json:"request_id"`
	}
	body := problem{
		Type:      problemTypeBase + typeTail,
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  instance,
		RequestID: requestID,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeInternalProblem is the canonical call for a 5xx response
// whose public detail we do NOT want to expose (e.g. raw store /
// VM / OS error text). It:
//   - logs the real error server-side (truncated to maxInternalDetail)
//     at "core: <title>: <internalErr>" with the request id so an
//     operator can grep the log by rid;
//   - emits the response with a generic public detail
//     ("internal error; see request_id <rid>") so the caller never
//     sees store / VM / OS internals.
//
// Callers MUST pass status >= 500; for 4xx the caller-facing detail
// stays verbatim via writeProblem. Status below 500 is passed
// through without scrub (defensive — the wrapper is documented as
// "internal problem" but does not enforce).
//
// PRMT-083 §2.
func writeInternalProblem(w http.ResponseWriter, ctx context.Context, status int, typeTail, title string, internalErr error) {
	rid := RequestIDFromContext(ctx)
	var realDetail string
	if internalErr != nil {
		realDetail = internalErr.Error()
	}
	if status >= 500 {
		internalLog("core: %s: %s request_id=%q", title, truncateDetail(realDetail), rid)
	}
	publicDetail := realDetail
	if status >= 500 {
		publicDetail = "internal error; see request_id " + rid
	}
	writeProblem(w, status, typeTail, title, publicDetail, "", rid)
}

// writeJSON emits a 2xx JSON response with the given status. Used
// by success paths; problem responses always go through writeProblem.
// The body is json.Marshal'd and emitted with a single trailing
// newline so the wire bytes match what captureResponse (used by
// the dedup table) stores — that's what makes the dedup contract
// "replay byte-for-byte" actually byte-equal, per spec-004 §5.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
}

// --- dedup table --------------------------------------------------------

// dedupEntry is the cached response of a previously-seen apply.
type dedupEntry struct {
	status  int
	body    []byte
	ct      string
	expires time.Time
}

// dedupTable is the in-memory (method, path, request_id) → response
// cache. Spec-004 §5 says: same request_id within 24h replays the
// first response byte-for-byte. The table is mutex-protected; the
// GC runs lazily on every Get.
type dedupTable struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	data map[string]dedupEntry
}

func newDedupTable(ttl time.Duration) *dedupTable {
	return &dedupTable{ttl: ttl, now: time.Now, data: map[string]dedupEntry{}}
}

// lookup returns the cached response for key, or (zero, false) if
// absent/expired. Expired entries are GC'd on access so the table
// does not grow without bound under steady load.
func (t *dedupTable) lookup(key string) (dedupEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gcLocked()
	e, ok := t.data[key]
	if !ok {
		return dedupEntry{}, false
	}
	if e.expires.Before(t.now()) {
		delete(t.data, key)
		return dedupEntry{}, false
	}
	return e, true
}

// remember stores e under key with a fresh expiry. Overwrites any
// prior entry (the spec does not define what to do if a second
// request arrives with the same key but a different body; we keep
// the first, matching the "replay the first response" wording).
func (t *dedupTable) remember(key string, e dedupEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.expires = t.now().Add(t.ttl)
	t.data[key] = e
}

func (t *dedupTable) gcLocked() {
	now := t.now()
	for k, e := range t.data {
		if e.expires.Before(now) {
			delete(t.data, k)
		}
	}
}

// --- page-token helpers -------------------------------------------------

// encodePageToken produces a base64("v1:" + lastPath). The "v1:"
// prefix is a forward-compat hook: a future v2 cursor format can
// dispatch on it without breaking old clients.
func encodePageToken(lastPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + lastPath))
}

// decodePageToken parses a token previously produced by encodePageToken.
// On any error it returns ("", false); the caller turns that into 400.
func decodePageToken(tok string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", false
	}
	const p = "v1:"
	if !strings.HasPrefix(string(raw), p) {
		return "", false
	}
	return string(raw)[len(p):], true
}

// encodePageTokenPair produces a cursor that encodes a (sortKey,
// id) tuple. Used by list endpoints whose sort is (sortKey asc, id
// asc) and whose cursor filter needs to match the sort key (PRMT-
// 096 R2 F2). The pair format is "v2:<sortKey><US><id>" where US
// (Unit Separator, 0x1F) is a non-printable control byte that
// cannot appear in crn asset paths or in our window ids (which
// are base32). The "v2:" prefix keeps the format forward-compat
// with v1 tokens (so an old client that sends a v1 token gets
// rejected by decodePageTokenPair instead of silently mis-parsing).
func encodePageTokenPair(sortKey string, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v2:" + sortKey + "\x1f" + id))
}

// decodePageTokenPair parses a pair cursor produced by
// encodePageTokenPair. Returns (sortKey, id, true) on success;
// ("", "", false) on any parse error so the caller can 400.
func decodePageTokenPair(tok string) (string, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", "", false
	}
	const p = "v2:"
	if !strings.HasPrefix(string(raw), p) {
		return "", "", false
	}
	body := string(raw)[len(p):]
	idx := strings.IndexByte(body, 0x1f)
	if idx < 0 {
		return "", "", false
	}
	return body[:idx], body[idx+1:], true
}
