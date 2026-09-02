// Package core — authmw.go: the HTTP middleware that turns a
// configured TokenVerifier + role/scope tables into 401/403/allow
// decisions on every /v1 request. The mapping from (method, URL)
// to (Action, path) is defined here per spec-004 §6 / PRMT-019 §4.4.
//
// Layering note: this middleware MUST sit INSIDE withRequestID so
// the audit line can include the request_id; Handler() wires it
// that way when Server.auth != nil.
//
// PRMT-019 §4.4 + §4.5.
package core

import (
	"context"
	"log"
	"net/http"
	"strings"
)

// authDecision is the result string used in the audit log; the set
// is closed (allow / deny-unauth / deny-forbidden) so audit consumers
// can grep without a glossary.
type authDecision string

const (
	authAllow         authDecision = "allow"
	authDenyUnauth    authDecision = "deny-unauth"
	authDenyForbidden authDecision = "deny-forbidden"
)

// ctxKeyPrincipal stores the authenticated Principal on a request's
// context. Handlers may pull it out with PrincipalFromContext (see
// auth.go-adjacent accessor below) but no handler does that in this
// prompt — Set endpoints will need it for the risk_class audit.
const ctxKeyPrincipal ctxKey = 2

// ctxKeyTenant stores the request-scoped tenant identity extracted
// from the X-CIOS-Tenant header. Next free ctxKey after
// ctxKeyRID(=1, server.go) and ctxKeyPrincipal(=2, this file).
const ctxKeyTenant ctxKey = 3

// tenantHeaderName is the request header the gateway attaches for
// tenant propagation (pkg/tenant.TenantHeaderName). It is redeclared
// here as a local literal rather than imported, because pkg/tenant
// transitively imports pkg/sts (pkg/tenant/claim.go, org.go) and R1
// forbids pulling pkg/sts into core. If spec-004 ever registers a
// header registry, BOTH this constant and pkg/tenant.TenantHeaderName
// move together (there are exactly two sites).
const tenantHeaderName = "X-CIOS-Tenant"

// PrincipalFromContext returns the Principal attached by
// authMiddleware. Falls back to (zero, false) when auth is disabled
// or the middleware was bypassed in a test.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal).(Principal)
	return p, ok
}

// TenantFromContext returns the tenant identity attached by
// authMiddleware from the X-CIOS-Tenant header. Returns ("", false)
// when the header was absent/empty or auth was bypassed. The bool is
// the presence signal — consumers (PRMT-185/190) branch on it:
// present ⇒ scope queries to this tenant; absent + RoleAdmin ⇒
// ops-realm platform-wide (R1).
func TenantFromContext(ctx context.Context) (string, bool) {
	tid, ok := ctx.Value(ctxKeyTenant).(string)
	return tid, ok
}

// authMiddleware wraps an inner handler with bearer-token auth and
// (action, path) RBAC. When the request does not match any /v1
// resource pattern the middleware lets it through to the inner mux
// without auth (so /metrics, /healthz and any future side-channel
// stay un-gated); the inner mux decides 404. /v1 requests with no
// or bad token get 401, allowed-role-but-out-of-scope get 403, and
// otherwise the Principal is attached to ctx and handed to inner.
//
// The verifier and auditLog are dependency-injected for tests; the
// production wiring uses log.Default()-style log.Printf via the
// auditLog default.
type authMW struct {
	verifier TokenVerifier
	inner    http.Handler
	// auditLog is the destination for the per-request audit line.
	// Defaults to log.Printf when nil; tests inject a bytes.Buffer
	// logger to assert the format and confirm token plaintext never
	// appears.
	auditLog func(format string, args ...any)
}

func newAuthMiddleware(verifier TokenVerifier, inner http.Handler) http.Handler {
	return &authMW{verifier: verifier, inner: inner, auditLog: log.Printf}
}

func (m *authMW) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// PRMT-066: /v1/health and /v1/health/ready are PUBLIC probe
	// endpoints — explicit carry. A kubelet / livenessProbe /
	// readinessProbe / load-balancer health check cannot carry
	// a bearer token; treating these as authenticated would
	// mark every pod down the moment the verifier changes
	// (key rotation, mTLS rollout, etc.). /v1/health/scanners
	// stays viewer-protected because it's a human-facing read.
	if r.URL.Path == "/v1/health" || r.URL.Path == "/v1/health/ready" {
		m.inner.ServeHTTP(w, r)
		return
	}
	// Map URL → (Action, path). Non-/v1 requests skip auth entirely.
	action, path, isAPI := mapRequest(r)
	if !isAPI {
		m.inner.ServeHTTP(w, r)
		return
	}

	rid := RequestIDFromContext(r.Context())

	// Step 1: extract bearer token.
	raw, ok := bearerFromHeader(r.Header.Get("Authorization"))
	if !ok {
		m.audit(Principal{}, action, path, authDenyUnauth, rid)
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"Unauthorized",
			"missing or malformed Bearer token",
			r.URL.Path, rid)
		return
	}

	// Step 2: verify token → Principal.
	principal, err := m.verifier.Verify(raw)
	if err != nil {
		m.audit(Principal{}, action, path, authDenyUnauth, rid)
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"Unauthorized",
			"token rejected",
			r.URL.Path, rid)
		return
	}

	// Step 3: gate. For list endpoints (GET /v1/assets, GET /v1/alarms)
	// the per-item scope check belongs in the handler — the middleware
	// only enforces a role floor via roleAllows. All other /v1 endpoints
	// (single-resource reads/writes + /v1/metrics/query*) keep the full
	// authorize(role × scope × action) decision; /v1/metrics/query* in
	// particular must stay fail-closed (M3 will add PromQL label
	// isolation; see PRMT-022 R2 §1). PRMT-022 R2 §4.0.
	var gateErr error
	if isListScopeEndpoint(r) {
		gateErr = roleAllows(principal, action)
	} else {
		gateErr = authorize(principal, action, path)
	}
	if gateErr != nil {
		m.audit(principal, action, path, authDenyForbidden, rid)
		writeProblem(w, http.StatusForbidden, "forbidden",
			"Forbidden",
			"principal not authorized for this action and path",
			r.URL.Path, rid)
		return
	}

	// Allow: attach Principal to ctx and pass through.
	m.audit(principal, action, path, authAllow, rid)
	// Trust posture (spec-006 §5 / P793 H3):
	//   - Lab (TenantHeaderRequiresMTLSPeer=false): header trusted as
	//     today (loopback + network isolation).
	//   - Cloud mTLS require: header accepted only from verified apigw
	//     peer cert (pkg/mtls.PeerComponent). Spoofed headers without
	//     client cert → 403.
	// Core does not verify the header against the bearer (independent
	// dimensions — pkg/tenant/propagate.go). Extracted on allow path
	// only so 401/403 carry no tenant on ctx.
	ctx := context.WithValue(r.Context(), ctxKeyPrincipal, principal)
	if tid := r.Header.Get(tenantHeaderName); tid != "" {
		if errMsg := gateTenantHeader(r); errMsg != "" {
			m.audit(principal, action, path, authDenyForbidden, rid)
			writeProblem(w, http.StatusForbidden, "forbidden",
				"Forbidden",
				errMsg,
				r.URL.Path, rid)
			return
		}
		ctx = context.WithValue(ctx, ctxKeyTenant, tid)
	}
	m.inner.ServeHTTP(w, r.WithContext(ctx))
}

// audit emits the one-line decision record. Fields are fixed-order
// key=value pairs (per §4.5); the raw token NEVER reaches this
// function and is therefore unreachable from any log line.
func (m *authMW) audit(p Principal, action Action, path string, decision authDecision, rid string) {
	m.auditLog("audit principal=%q role=%q action=%q path=%q decision=%q request_id=%q",
		p.Subject, string(p.Role), string(action), path, string(decision), rid)
}

// bearerFromHeader parses an Authorization header value. Returns the
// bearer token (the substring after "Bearer ") and ok=true iff the
// header begins with the case-insensitive scheme "Bearer" followed
// by exactly one space and a non-empty token. RFC 6750 actually
// permits multiple whitespace and "Bearer" with any case; we accept
// the canonical form only to keep parsing simple and the failure
// mode obvious in audit logs (deny-unauth without a Principal).
func bearerFromHeader(hdr string) (string, bool) {
	const prefix = "Bearer "
	if len(hdr) <= len(prefix) {
		return "", false
	}
	// Case-insensitive prefix match on the scheme name.
	if !strings.EqualFold(hdr[:len(prefix)-1], "Bearer") {
		return "", false
	}
	if hdr[len(prefix)-1] != ' ' {
		return "", false
	}
	tok := hdr[len(prefix):]
	if tok == "" {
		return "", false
	}
	return tok, true
}

// mapRequest decides whether an inbound request is an /v1 API call
// that needs auth, and if so what (Action, path) it represents.
//
// The mapping per §4.4:
//
//	GET  /v1/assets           → read,         path="**"
//	GET  /v1/assets/{p}       → read,         path={p}
//	PUT  /v1/assets/{p}       → apply,        path={p}
//	DELETE /v1/assets/{p}     → apply,        path={p}
//	GET  /v1/alarms           → read,         path="**"
//	GET  /v1/reports/ops      → read,         path="**"
//	GET  /v1/metrics/query*   → read,         path="**"
//	GET  /v1/points/{p}       → read,         path={p}
//	PUT  /v1/points/{p}:set   → control:write, path={p without ":set" suffix}
//
// Unknown methods on a known prefix fall through with Action="" so
// the inner handler can return 405 (the auth layer must not pre-empt
// the existing 405 logic for non-RBAC method errors).
//
// Returns isAPI=false for any URL not under /v1 — those bypass auth
// entirely (e.g. /metrics for Prometheus self-scrape, future /healthz).
func mapRequest(r *http.Request) (action Action, path string, isAPI bool) {
	u := r.URL.Path
	switch {
	case u == "/v1/assets":
		return mapMethodReadOnly(r.Method), "**", true
	case strings.HasPrefix(u, "/v1/assets/"):
		p := strings.TrimPrefix(u, "/v1/assets/")
		// PRMT-039: POST /v1/assets/{path}:lifecycle is the
		// lifecycle state-machine transition. Strip the suffix so
		// the authorize() scope check targets the asset path, not
		// the colon-tagged URL fragment.
		if r.Method == http.MethodPost && strings.HasSuffix(p, ":lifecycle") {
			return ActionApply, strings.TrimSuffix(p, ":lifecycle"), true
		}
		switch r.Method {
		case http.MethodGet:
			return ActionRead, p, true
		case http.MethodPut, http.MethodDelete:
			return ActionApply, p, true
		default:
			// Unknown method → pass through; inner returns 405.
			return ActionRead, p, true
		}
	case u == "/v1/alarms":
		return mapMethodReadOnly(r.Method), "**", true
	case strings.HasPrefix(u, "/v1/alarms/"):
		// PRMT-230: POST /v1/alarms/{id}:ack. The {id} is an alarm
		// id, not an asset path — the handler re-runs authorize()
		// against the stored alarm's Path (same shape as
		// /v1/tickets/{id}:transition; see isListScopeEndpoint).
		p := strings.TrimPrefix(u, "/v1/alarms/")
		if strings.HasSuffix(p, ":ack") {
			return ActionControlWrite, strings.TrimSuffix(p, ":ack"), true
		}
		return ActionRead, p, true
	case u == "/v1/reports/ops":
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/reports/reconcile":
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/maintenance/upcoming":
		// PRMT-058: merged PM + inspection "upcoming" view. Read-only
		// GET; per-item scope filter delegated to handler (same shape
		// as /v1/reports/ops).
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/maintenance/windows":
		// PRMT-096: explicit maintenance-window CRUD.
		// GET → list-scope (per-item authorize() in handler);
		// POST → ActionControlWrite with handler re-check on
		// asset_path (mirrors /v1/tickets / /v1/pm/schedules).
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/maintenance/windows/"):
		// DELETE /v1/maintenance/windows/{id}. The {id} segment is
		// a window id (not an asset path), so the handler re-runs
		// authorize() against the stored window's asset_path
		// (single-resource scope, not list-scope; mirror
		// /v1/tickets/{id}:transition).
		return ActionControlWrite, strings.TrimPrefix(u, "/v1/maintenance/windows/"), true
	case u == "/v1/sla":
		// PRMT-209: customer uptime SLA stub. ActionRead,
		// list-scope (constant response; tenant from header).
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/usage":
		// PRMT-193: usage list. ActionRead, list-scope (mirrors
		// /v1/capacity). Tenant filter forced in handler when
		// TenantFromContext is present.
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/usage:export":
		// PRMT-193: CSV export — same Read + list-scope as list.
		return mapMethodReadOnly(r.Method), "**", true
	case u == "/v1/usage:compute":
		// PRMT-193: admin compute+Upsert. ActionApply (admin role
		// floor); handler re-checks RoleAdmin. Path is not an
		// asset path so scope is "**".
		return ActionApply, "**", true
	case u == "/v1/control/audit":
		// PRMT-234: control-write audit list. Read-only GET at viewer
		// floor; per-item scope filter in the handler (audit.Path).
		return mapMethodReadOnly(r.Method), "**", true
	case strings.HasPrefix(u, "/v1/control/"):
		// PRMT-235: POST /v1/control/{id}:approve. The {id} is a
		// pending id, not an asset path — the middleware role-floors
		// ControlWrite (operator+) via isListScopeEndpoint and the
		// handler re-runs the FULL authorize() against the stored
		// pending's point path (mirror /v1/alarms/{id}:ack; a full
		// middleware authorize on the literal id would 403 every
		// scoped operator before the real check).
		p := strings.TrimPrefix(u, "/v1/control/")
		if strings.HasSuffix(p, ":approve") {
			return ActionControlWrite, strings.TrimSuffix(p, ":approve"), true
		}
		return ActionRead, p, true
	case u == "/v1/tickets":
		// GET → list (per-item scope filter delegated to handler);
		// POST → create (full authorize against the request body's
		// asset_path via the handler; middleware maps to "**" here
		// and the handler re-checks ActionControlWrite on the
		// resolved asset_path — same shape as PUT /v1/assets but
		// without the asset-path-in-URL).
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case u == "/v1/pm/schedules":
		// Same shape as /v1/tickets: GET → list-scope (handler
		// filters), POST → ActionControlWrite against "**" with
		// handler re-check on the request body's asset_path
		// (PRMT-043 §4).
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/pm/schedules/"):
		return ActionRead, strings.TrimPrefix(u, "/v1/pm/schedules/"), true
	case u == "/v1/inspections":
		// PRMT-049 §4: list-scope GET (handler filters per-item on
		// asset_path); POST create → ActionControlWrite against
		// "**" with handler re-check on the request body's
		// asset_path. Mirrors /v1/pm/schedules / /v1/tickets.
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/inspections/"):
		// PRMT-059: /v1/inspections/form/{id} is the mobile-web
		// checklist page. GET → read (viewer may render), POST →
		// control:write (operator resolves the ticket). The {id}
		// is a ticket id, not an asset path, so the handler re-runs
		// authorize() against the stored ticket.AssetPath
		// (single-resource scope, not list-scope).
		if strings.HasPrefix(u, "/v1/inspections/form/") {
			if r.Method == http.MethodPost {
				return ActionControlWrite, strings.TrimPrefix(u, "/v1/inspections/form/"), true
			}
			return ActionRead, strings.TrimPrefix(u, "/v1/inspections/form/"), true
		}
		return ActionRead, strings.TrimPrefix(u, "/v1/inspections/"), true
	case u == "/v1/cases":
		return ActionRead, "**", true
	case u == "/v1/spares":
		// Spare parts are not asset-path scoped (PRMT-048 §4:
		// spares have no asset_path). Same shape as /v1/alarms /
		// /v1/reports/ops: GET → list (role floor only, no per-item
		// scope filter); POST → ActionControlWrite with handler
		// accepting. Spec §16 follow-up will revisit per-item scope
		// once the spare domain picks up an asset binding.
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/spares/"):
		// GET /v1/spares/{id} and POST .../{id}:adjust.
		// GET is read; :adjust is control:write (state-changing).
		p := strings.TrimPrefix(u, "/v1/spares/")
		if strings.HasSuffix(p, ":adjust") {
			// PRMT-048 R1: collapse dead branch — both arms of the
			// previous if/return returned the same tuple, so the
			// method dispatch was a no-op. The :adjust endpoint is
			// control:write regardless of HTTP method on this URL;
			// wrong method → 405 from the inner handler.
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case u == "/v1/orgs":
		// PRMT-185: admin-only org CRUD list/create. Orgs are not
		// asset-path scoped (spec-001 §5bis.2: an org lives within a
		// tenant, not under an asset path), so the scope target is
		// "**"; the handler re-checks RoleAdmin (R1) and applies
		// tenant-scoping via TenantFromContext. Mirrors /v1/spares
		// above (also non-asset-path-scoped).
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/orgs/"):
		// PRMT-185: GET /v1/orgs/{id}, POST .../{id}:rename, DELETE
		// /v1/orgs/{id}. The {id} is an org id (not an asset path),
		// so scope is "**"; the handler parses the optional ":rename"
		// suffix and re-checks RoleAdmin + tenant-scoping (R1).
		// Mirrors /v1/spares/{id}:adjust above.
		return ActionControlWrite, "**", true
	case u == "/v1/tenants":
		// L109 P804: admin-only list/create tenants. Not asset-path
		// scoped; handler re-checks RoleAdmin. Mirrors /v1/orgs.
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/tenants/"):
		// PRMT-182: admin-only governed write path for isolation_tier.
		// The {id}:tier subresource is control:write regardless of
		// method on this URL; the handler re-checks RoleAdmin (the
		// tenant id is not an asset path, so scope is "**"). Mirrors
		// /v1/spares/{id}:adjust above.
		return ActionApply, "**", true
	case u == "/v1/site-orgs":
		// L109 P802: site→org registry. Not asset-path scoped.
		if r.Method == http.MethodPost {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case u == "/v1/role-bindings":
		// L109 P803: role binding CRUD. Not asset-path scoped.
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			return ActionControlWrite, "**", true
		}
		return ActionRead, "**", true
	case u == "/v1/model-packs" || strings.HasPrefix(u, "/v1/model-packs/"):
		// L109 P811–P814: Model Studio. Not asset-path scoped;
		// handler re-checks RoleAdmin (or org-admin gate).
		if r.Method == http.MethodGet {
			return ActionRead, "**", true
		}
		return ActionControlWrite, "**", true
	case u == "/v1/site-layouts" || strings.HasPrefix(u, "/v1/site-layouts/"):
		// L109 P821–P825: Site-Draw layout + writeback + scene kick.
		if r.Method == http.MethodGet {
			return ActionRead, "**", true
		}
		return ActionControlWrite, "**", true
	case strings.HasPrefix(u, "/v1/runbooks/"):
		return ActionRead, "**", true
	case strings.HasPrefix(u, "/v1/tickets/"):
		p := strings.TrimPrefix(u, "/v1/tickets/")
		// POST .../transition → state-machine write.
		// PRMT-060: POST .../note and POST .../assign are also
		// state-changing writes on the stored ticket; the
		// {id} segment is the ticket id (NOT an asset path),
		// so the handler re-runs authorize() against the
		// ticket.AssetPath (single-resource scope, not
		// list-scope; see isListScopeEndpoint below).
		// PRMT-061: GET .../history is the audit-trail reader
		// for the ticket (per-item scope check delegated to the
		// handler — the {id} is a ticket id, not an asset path).
		if strings.HasSuffix(p, ":transition") ||
			strings.HasSuffix(p, ":note") ||
			strings.HasSuffix(p, ":assign") {
			pathOnly := p
			switch {
			case strings.HasSuffix(p, ":transition"):
				pathOnly = strings.TrimSuffix(p, ":transition")
			case strings.HasSuffix(p, ":note"):
				pathOnly = strings.TrimSuffix(p, ":note")
			case strings.HasSuffix(p, ":assign"):
				pathOnly = strings.TrimSuffix(p, ":assign")
			}
			return ActionControlWrite, pathOnly, true
		}
		if r.Method == http.MethodGet && strings.HasSuffix(p, ":history") {
			return ActionRead, strings.TrimSuffix(p, ":history"), true
		}
		return ActionRead, p, true
	case u == "/v1/metrics/query" || u == "/v1/metrics/query_range":
		return mapMethodReadOnly(r.Method), "**", true
	case strings.HasPrefix(u, "/v1/points/"):
		p := strings.TrimPrefix(u, "/v1/points/")
		// Set is "PUT /v1/points/{path}:set". Strip the suffix before
		// scope matching so the operator's scope "site01.pod002.cdu000.fan000.rpm"
		// actually matches the path.
		if strings.HasSuffix(p, ":set") {
			pathOnly := strings.TrimSuffix(p, ":set")
			if r.Method == http.MethodPut {
				return ActionControlWrite, pathOnly, true
			}
			// Wrong method on :set → pass through; inner returns 405.
			return ActionControlWrite, pathOnly, true
		}
		return ActionRead, p, true
	case u == "/v1/health/scanners":
		// PRMT-066: per-scanner status snapshot. Read-only, no
		// asset-path concept — scanner names are a fixed
		// enumeration ("sla", "pm", "inspection", "spare",
		// "reconcile", "report"), so the per-item scope filter
		// has nothing to apply against. The middleware applies
		// the viewer role floor only; handler is the per-tick
		// mirror of /v1/alarms / /v1/cases / etc.
		return ActionRead, "**", true
	}
	// Any other URL is outside the /v1 API surface; let it through
	// un-gated. The inner ServeMux will 404.
	return "", "", false
}

// mapMethodReadOnly is a tiny helper for endpoints that only define
// GET semantics (/v1/assets list, /v1/alarms list, /v1/metrics/query*).
// The auth layer treats all methods on those URLs as ActionRead so
// the inner handler's own 405 path stays authoritative; we just need
// SOME action to log on the audit line.
func mapMethodReadOnly(_ string) Action { return ActionRead }

// isListScopeEndpoint reports whether the request targets a
// "collection" endpoint whose per-item scope filtering must happen
// in the handler (because only the handler holds the items). The
// middleware applies ONLY the role floor for these — the full
// scope match is delegated to the per-item handler filter.
//
// Currently: GET /v1/assets, GET /v1/alarms, GET /v1/reports/ops,
// GET /v1/tickets, POST /v1/tickets, GET /v1/tickets/{id}, and POST
// /v1/tickets/{id}:transition.
//
// The non-collection ticket endpoints are included because the
// scope-target lives in the request body (POST /v1/tickets), in
// the stored ticket (POST /v1/tickets/{id}:transition), or in the
// stored ticket's AssetPath (GET /v1/tickets/{id}). The {id} in
// the URL is a ticket ID, not an asset path — the handler reads
// the ticket from the store and re-runs authorize() against its
// AssetPath.
//
// NOT included: /v1/metrics/query* — those endpoints have no
// per-item handler filter and must stay fail-closed under full
// authorize until M3 adds PromQL label-level isolation. Treating
// metrics as a "list" endpoint would silently let scoped viewers
// read every metric on the edge VM, which is a real information
// leak (PRMT-019 §9 follow-up #2).
//
// PRMT-022 R2 §4.0; PRMT-033 extends with /v1/tickets.
func isListScopeEndpoint(r *http.Request) bool {
	u := r.URL.Path
	switch {
	case u == "/v1/assets" || u == "/v1/alarms":
		return r.Method == http.MethodGet
	case strings.HasPrefix(u, "/v1/alarms/"):
		// PRMT-230: {id}:ack — middleware role-floors ControlWrite
		// (operator+); handler re-checks authorize() against the
		// stored alarm's Path. Mirror of /v1/tickets/{id}:transition.
		return r.Method == http.MethodPost && strings.HasSuffix(u, ":ack")
	case u == "/v1/reports/ops":
		return r.Method == http.MethodGet
	case u == "/v1/reports/reconcile":
		// PRMT-050: list-scope (per-entry authorize() in handler);
		// orphans gate is role-floor-only at operator+ (handler).
		return r.Method == http.MethodGet
	case u == "/v1/maintenance/upcoming":
		// PRMT-058: list-scope GET (handler filters per-item on
		// authorize() against the caller's scope).
		return r.Method == http.MethodGet
	case u == "/v1/maintenance/windows":
		// PRMT-096: list-scope GET (handler filters per-item on
		// authorize() against the caller's scope); POST → ActionControlWrite
		// against "**" with handler re-check on the body's asset_path.
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/maintenance/windows/"):
		// PRMT-096 R2 F1: DELETE /v1/maintenance/windows/{id}. The
		// {id} segment is a window id (not an asset path), so the
		// middleware role-floors the call and the handler re-runs
		// authorize(ActionControlWrite, storedWindow.AssetPath) — the
		// same shape as /v1/tickets/{id}:transition above. Without
		// this case the middleware would call authorize() against
		// the literal window id and any operator token would 403
		// before reaching the handler (their scope pattern
		// `site01.pod001.**` cannot match the id `mw_...`).
		return r.Method == http.MethodDelete
	case u == "/v1/sla":
		// PRMT-209: list-scope GET (constant; role floor only).
		return r.Method == http.MethodGet
	case u == "/v1/usage":
		// PRMT-193: list-scope GET (filters in handler; no
		// per-asset authorize — usage is tenant/site keyed).
		return r.Method == http.MethodGet
	case u == "/v1/usage:export":
		// PRMT-193: same list-scope as GET /v1/usage.
		return r.Method == http.MethodGet
	case u == "/v1/usage:compute":
		// PRMT-193: role floor only (ActionApply → admin); not
		// an asset path. Handler re-checks RoleAdmin.
		return r.Method == http.MethodPost
	case u == "/v1/control/audit":
		// PRMT-234: list-scope GET (handler filters per-item on
		// authorize() against each audit's point path).
		return r.Method == http.MethodGet
	case strings.HasPrefix(u, "/v1/control/") && strings.HasSuffix(u, ":approve"):
		// PRMT-235: {id}:approve — role floor here; the handler
		// re-runs authorize() against pending.Path (see mapRequest).
		return r.Method == http.MethodPost
	case u == "/v1/tickets":
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/tickets/"):
		// {id} read + {id}:transition write — handler re-checks
		// against the stored ticket's asset_path. PRMT-060
		// extends this to {id}:note and {id}:assign (also
		// single-resource writes; the {id} is a ticket id, not
		// an asset path, so the handler re-runs authorize()
		// against the stored ticket). PRMT-061 adds {id}:history
		// as a list-scope read (per-item scope check delegated to
		// the handler against the stored ticket.AssetPath).
		if r.Method == http.MethodGet {
			return true
		}
		return r.Method == http.MethodPost && (strings.HasSuffix(u, ":transition") ||
			strings.HasSuffix(u, ":note") ||
			strings.HasSuffix(u, ":assign"))
	case u == "/v1/pm/schedules":
		// GET list → list-scope; POST create → ActionControlWrite
		// against "**" with handler re-check on asset_path
		// (mirrors /v1/tickets).
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/pm/schedules/"):
		return r.Method == http.MethodGet
	case u == "/v1/inspections":
		// PRMT-049 §4: GET list → list-scope (handler re-checks
		// per-item on asset_path); POST create → ActionControlWrite
		// against "**" with handler re-check on asset_path. Mirror
		// /v1/pm/schedules.
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/inspections/"):
		// PRMT-059: form GET/POST — handler re-runs authorize()
		// against the stored ticket.AssetPath; middleware only
		// enforces the role floor (mirrors /v1/tickets/{id} +
		// .../{id}:transition above). PRMT-063 extends the
		// same shape to /v1/inspections/form/{id}/photo: POST
		// to the photo sub-route is a control:write against the
		// stored ticket's asset_path, re-checked in the handler.
		if strings.HasPrefix(u, "/v1/inspections/form/") {
			return r.Method == http.MethodGet || r.Method == http.MethodPost
		}
		return r.Method == http.MethodGet
	case u == "/v1/cases":
		return r.Method == http.MethodGet
	case u == "/v1/spares":
		// Spare list/create: list-scope (role floor only, no per-item
		// scope filter — spares are not asset-path scoped, PRMT-048 §4).
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/spares/"):
		// {id} read + {id}:adjust write — both are role-floor-only
		// because the spare id is not an asset path.
		return r.Method == http.MethodGet ||
			(r.Method == http.MethodPost && strings.HasSuffix(u, ":adjust"))
	case u == "/v1/tenants":
		// L109 P804: list/create — role floor only (not asset-path scoped).
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/tenants/"):
		// PRMT-182: only the {id}:tier POST is a list-scope endpoint
		// (role floor only — the tenant id is not an asset path).
		// Handler re-checks RoleAdmin; non-:tier sub-paths return 404.
		return r.Method == http.MethodPost
	case u == "/v1/orgs":
		// PRMT-185: list/create role floor (handler re-checks RoleAdmin).
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case strings.HasPrefix(u, "/v1/orgs/"):
		return true
	case u == "/v1/site-orgs":
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case u == "/v1/role-bindings":
		return r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete
	case u == "/v1/model-packs" || strings.HasPrefix(u, "/v1/model-packs/"):
		return true
	case u == "/v1/site-layouts" || strings.HasPrefix(u, "/v1/site-layouts/"):
		return true
	case strings.HasPrefix(u, "/v1/runbooks/"):
		return r.Method == http.MethodGet
	case strings.HasPrefix(u, "/v1/assets/"):
		// PRMT-045: GET .../...:history is a list-scope read
		// (per-item scope filter delegated to handler — handler
		// re-checks authorize() on the path).
		if r.Method == http.MethodGet && strings.HasSuffix(u, ":history") {
			return true
		}
	case u == "/v1/health/scanners":
		// PRMT-066: list-scope read. The handler returns a
		// fixed-enumeration snapshot keyed by scanner name; no
		// per-item scope filter applies. Middleware enforces
		// only the viewer role floor.
		return r.Method == http.MethodGet
	}
	return false
}
