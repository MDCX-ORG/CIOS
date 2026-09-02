// Server: HTTP wiring for the experience-layer API Gateway.
//
// PRMT-101 §4 pins the public surface; subsequent PRMTs
// (PRMT-102..104, 106, 107) layer authn/authz/SSE on top WITHOUT
// changing these signatures. In particular AuthMiddleware is a
// placeholder today — downstream PRMTs will replace its body
// while keeping the (http.Handler) -> http.Handler shape.
//
// PRMT-103 adds POST /auth/{realm}/token, which exchanges a
// verified session cookie (pkg/authn) for a scoped API token
// (pkg/sts). PRMT-103 §6 forbids changing AuthMiddleware /
// Upstream / sites handlers; the token route is mounted as a
// separate mux entry so /auth/* keeps its existing login/
// callback/logout surface intact.
package apigw

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yurimeng/cios/pkg/policy"
	"github.com/yurimeng/cios/pkg/sts"
)

// Server is the HTTP front-door for the Gateway. It holds the
// resolved config, a reverse-HTTP client (Upstream), and a
// stdlib ServeMux for routing. All three fields are set once at
// construction; Server is safe for concurrent use after that
// (ServeMux is goroutine-safe and Upstream's *http.Client is too).
//
// PRMT-108: Server also holds an AccountStore for service-account
// client_credentials grants on POST /auth/token. NewServer wires
// one from env (CIOS_STS_SERVICE_ACCOUNTS — file or inline JSON)
// when present; tests can inject via SetServiceAccounts.
type Server struct {
	cfg Config
	up  *Upstream
	mux *http.ServeMux

	// authHandler is the optional /auth/{realm}/* surface from
	// pkg/authn (PRMT-102 §3). Set via SetAuthHandler; nil means
	// /auth/* returns 404. We store it as http.Handler so pkg/apigw
	// does not depend on pkg/authn (authn depends on apigw, not
	// the other way round).
	authHandler http.Handler

	// sessionKey is the HMAC key used to verify the session
	// cookie at /auth/{realm}/token (PRMT-103 §2-bis). Sourced
	// from env CIOS_APIGW_SESSION_KEY in NewServer (must be the
	// same byte sequence authn.Handler was given; main.go is
	// responsible for supplying one key to both). nil means the
	// /auth/{realm}/token route is disabled (returns 404).
	sessionKey []byte

	// sessionDecoder verifies and decodes the session cookie
	// (PRMT-117 R2). Set by main.go via SetSessionDecoder; nil
	// means /auth/{realm}/token returns 500 with a generic
	// "session decoder not configured" problem. Keeping this as
	// a function-typed seam — rather than depending on
	// authn.DecodeSession directly — lets production pkg/apigw
	// avoid importing pkg/authn (spec-006 §5 — apigw ← authn).
	sessionDecoder SessionDecoder

	// sts is the Security Token Service used to mint scoped API
	// tokens (PRMT-103 §4). Constructed in NewServer from env
	// CIOS_STS_SIGNING_KEY + CIOS_STS_TTL (with sensible defaults
	// when the env is empty for non-prod); nil disables the token
	// route.
	sts *sts.STS

	// pdp is the experience-layer Policy Decision Point (PRMT-104).
	// Optional; when nil AuthMiddleware falls back to a pass-through
	// (preserves the PRMT-101 placeholder contract for tests that
	// don't wire an STS / OPA). When set, AuthMiddleware consults
	// pdp.Decision after sts.Verify and fails closed on any
	// non-nil error from the PDP.
	pdp policy.PDP

	// Source is the telemetry delta producer used by
	// handleSiteStream (PRMT-106). nil means the SSE route
	// returns 500 (it must be wired before /api/sites/{site}/
	// stream is exercised). NewServer installs a default
	// source that polls core /v1 — main.go can override via
	// SetSource to swap in a NATS-backed implementation
	// (per spec-009 §6 / D39) without touching Routes.
	Source TelemetrySource

	// omniverseURL is the base URL for the Omniverse/Nucleus
	// upstream (PRMT-107). Sourced from env CIOS_OMNIVERSE_URL
	// in NewServer via loadOmniverse. Empty means the route
	// returns 502 — operator misconfiguration surfaces as a
	// visible failure rather than a silent pass-through.
	omniverseURL string

	// omniverseToken is the ServiceTokenSource used to attach a
	// machine-identity bearer to outbound Omniverse calls
	// (PRMT-107 §4). NewServer installs an env-backed source by
	// default; tests inject deterministic sources via
	// SetOmniverseTokenSource.
	omniverseToken ServiceTokenSource

	// omniverseHTTP is the *http.Client used for outbound
	// Omniverse calls. NewServer leaves it nil so
	// omniverseHTTPClient() falls back to http.DefaultClient;
	// main.go injects a tuned client (mTLS, timeout) via
	// SetOmniverseHTTPClient per spec-006 §5.
	omniverseHTTP *http.Client

	// serviceAccounts is the lookup surface for client_credentials
	// grants on POST /auth/token (PRMT-108 §2). nil means the
	// route returns 503 so a misconfiguration surfaces visibly
	// rather than as silent 500s. NewServer installs one from env
	// when present; tests inject via SetServiceAccounts.
	serviceAccounts sts.AccountStore
}

// NewServer constructs a Server with its routes pre-wired. The
// returned *Server is the value the main goroutine hands to
// http.ListenAndServe (via Handler()).
//
// PRMT-103: NewServer also reads CIOS_APIGW_SESSION_KEY (must
// match the key authn.Handler was given so the cookie here is
// verifiable), CIOS_STS_SIGNING_KEY (mandatory, hard error if
// empty per PRMT-103 §3), and CIOS_STS_TTL (default 15m).
// Missing STS keys degrade to "token route returns 404" rather
// than abort startup so PRMT-101's "no auth required" mode still
// boots cleanly.
//
// PRMT-106: NewServer also installs a default TelemetrySource
// (newDefaultTelemetrySource) so the SSE route is functional
// out of the box. The default polls core /v1 — it does NOT
// connect to NATS (spec-009 §7.1 red line). Callers can swap
// the source via SetSource before serving traffic.
func NewServer(cfg Config, up *Upstream) *Server {
	s := &Server{
		cfg: cfg,
		up:  up,
		mux: http.NewServeMux(),
	}
	s.loadTokenKeys()
	s.loadPDP()
	s.loadOmniverse()
	s.loadServiceAccounts()
	s.Source = newDefaultTelemetrySource(up)
	s.Routes()
	return s
}

// SetSource installs a TelemetrySource. Callers (e.g. main.go
// wiring a NATS-backed source per spec-009 §6 / D39) can use
// this to override the default polling source wired by
// NewServer. Must be called before Routes() takes effect, but
// in practice the call is the same shape as SetSTS /
// SetSessionKey / SetAuthHandler — init-time only, no
// concurrent swap during request handling.
func (s *Server) SetSource(src TelemetrySource) { s.Source = src }

// isSiteStreamPath returns true for /api/sites/{site}/stream
// with a non-empty {site} segment. PRMT-106 §2 pins the shape;
// the check lives here so Routes() can dispatch without pulling
// in the handler module's full path parser. The underlying
// parser (parseSiteFromStreamPath) does the same job plus
// extracts the site name; we keep a thin predicate so the
// switch in Routes() reads cleanly.
func isSiteStreamPath(p string) bool {
	_, ok := parseSiteFromStreamPath(p)
	return ok
}

// envSessionKey / envSTSSigningKey / envSTSTTL are the env var
// names PRMT-103 reads in NewServer. CIOS_APIGW_SESSION_KEY is
// already used by authn.Handler (PRMT-102 §3); we re-use the
// same name so main.go only needs to set one variable to feed
// both packages.
const (
	envSessionKey    = "CIOS_APIGW_SESSION_KEY"
	envSTSSigningKey = "CIOS_STS_SIGNING_KEY"
	envSTSTTL        = "CIOS_STS_TTL"
	// envOPAURL is the OPA endpoint PRMT-104 reads. Empty is
	// allowed and yields a fail-closed PDP (every request
	// denies) rather than a startup error — operators see the
	// broken wire immediately via 403s in their logs rather than
	// after a deployment.
	envOPAURL = "CIOS_OPA_URL"
	// envServiceAccounts points at a JSON file describing the
	// service-account directory consulted by POST /auth/token
	// (PRMT-108 §2). The file format is intentionally minimal —
	// see loadServiceAccounts — and secrets inside are stored
	// hashed (PRMT-108 §5 secret 比对走哈希).
	envServiceAccounts = "CIOS_STS_SERVICE_ACCOUNTS"

	// PRMT-173: opt-in dev flag. When truthy AND AuthMiddleware
	// is in pass-through branch (sts==nil && pdp==nil), inject
	// fixed dev claims so handler-layer ClaimsFrom returns ok.
	// LoadConfig runs the STS/OPA sanity check that forces this
	// back to false when production auth is configured.
	envDevNoAuth = "CIOS_APIGW_DEV_NO_AUTH"
	// devNoAuthSubject is the Subject stamped onto dev claims
	// (informative only — fail-closed is the DevNoAuth gate,
	// not the subject).
	devNoAuthSubject = "dev-no-auth"
	// devNoAuthTenantID is the tenant id stamped onto dev
	// claims; non-empty is what tenant.TenantFromClaims
	// (PRMT-109 §5) needs to return ok=true.
	devNoAuthTenantID = "dev"
)

// loadTokenKeys populates s.sessionKey / s.sts from env. Errors
// (empty key, bad TTL) are silent on the boot path: a missing
// STS key disables the token route (404), a missing session key
// disables cookie verification (the route then 500s because
// every request becomes "no session" — this is intentional so
// the operator sees the broken wire immediately rather than
// after a deployment).
func (s *Server) loadTokenKeys() {
	if raw := os.Getenv(envSessionKey); raw != "" {
		s.sessionKey = []byte(raw)
	}
	if raw := os.Getenv(envSTSSigningKey); raw != "" {
		ttl := sts.DefaultTTL
		if rawTTL := os.Getenv(envSTSTTL); rawTTL != "" {
			if d, err := time.ParseDuration(rawTTL); err == nil && d > 0 {
				ttl = d
			}
		}
		s.sts = sts.New([]byte(raw), ttl, sts.NewMemRevoker())
	}
}

// loadPDP wires the optional OPA PDP from CIOS_OPA_URL (PRMT-104
// §3). Empty env → s.pdp stays nil, which causes AuthMiddleware
// to fall back to a pass-through (preserving PRMT-101's
// placeholder contract for tests that never set up an STS /
// OPA). The PDP itself fails closed when given an empty URL (see
// policy.NewOPAPDP), so this is the safe default.
func (s *Server) loadPDP() {
	raw := os.Getenv(envOPAURL)
	if raw == "" {
		return
	}
	s.pdp = policy.NewOPAPDP(raw, http.DefaultClient)
}

// SetPDP lets a caller inject a pre-built PDP (used by tests to
// point at a httptest mock OPA). When set, NewServer's
// env-driven construction is bypassed.
func (s *Server) SetPDP(p policy.PDP) { s.pdp = p }

// SetSTS lets a caller inject a pre-built STS (e.g. a test that
// wants a deterministic clock). When set, NewServer's
// env-driven construction is bypassed; the caller is
// responsible for the key source.
func (s *Server) SetSTS(svc *sts.STS) { s.sts = svc }

// SetSessionKey lets a caller inject the session HMAC key used
// to verify the session cookie at /auth/{realm}/token. Mirrors
// SetSTS for test injection. Production code reads the same
// value from env in NewServer.
func (s *Server) SetSessionKey(key []byte) { s.sessionKey = key }

// SetSessionDecoder installs the function used to verify and
// decode session cookies at /auth/{realm}/token (PRMT-117 R2).
// main.go wraps authn.DecodeSession with the SessionInfo
// projection and calls this unconditionally so a misconfiguration
// (forget-to-wire) fails loudly at request time with a 500
// "session decoder not configured" problem rather than
// silently mis-handling the route.
//
// Mirrors SetSessionKey / SetSTS for test injection. Init-time
// only — Server is not safe to reconfigure after Handler() is
// called.
func (s *Server) SetSessionDecoder(d SessionDecoder) { s.sessionDecoder = d }

// SetServiceAccounts installs the AccountStore consulted by
// POST /auth/token (PRMT-108 §2). Tests use this to inject a
// deterministic store without going through the env-loader path;
// production code reads CIOS_STS_SERVICE_ACCOUNTS via
// loadServiceAccounts. The store is consulted read-only by the
// handler, so concurrent safe-for-read implementations are fine.
func (s *Server) SetServiceAccounts(store sts.AccountStore) { s.serviceAccounts = store }

// loadServiceAccounts wires the service-account directory used
// by POST /auth/token (PRMT-108 §2 / §3). The env value is
// interpreted as a file path holding JSON of the shape:
//
//	{
//	  "accounts": [
//	    {
//	      "client_id":   "cli-bot",
//	      "secret_hash": "<hex of HMAC-SHA256(sts_key, plain_secret)>",
//	      "max_scope":   ["viewer", "editor"],
//	      "realm":       "ops"
//	    }
//	  ]
//	}
//
// We never read plaintext secrets from this file — the loader
// expects hashes so a stolen config file alone cannot impersonate
// a client. If env is empty, s.serviceAccounts stays nil and the
// /auth/token route returns 503 (visible misconfig, not silent
// pass).
func (s *Server) loadServiceAccounts() {
	raw := os.Getenv(envServiceAccounts)
	if raw == "" {
		return
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		// Misconfiguration surfaces as "no store" — the route
		// will 503 and operators see the broken wire immediately.
		return
	}
	store, err := parseServiceAccountsJSON(data, s.sts)
	if err != nil {
		return
	}
	s.serviceAccounts = store
}

// parseServiceAccountsJSON decodes the JSON directory format
// documented on loadServiceAccounts. Extracted so tests can drive
// the parser with hand-built byte slices without touching the
// filesystem. Returns an in-memory store keyed by client_id.
func parseServiceAccountsJSON(data []byte, svc *sts.STS) (sts.AccountStore, error) {
	var wire struct {
		Accounts []struct {
			ClientID   string   `json:"client_id"`
			SecretHash string   `json:"secret_hash"` // hex
			MaxScope   []string `json:"max_scope"`
			Realm      string   `json:"realm"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if svc == nil {
		// Hashing requires an STS key — without one we cannot
		// verify the loaded hashes against incoming requests.
		return nil, errors.New("apigw: sts not configured; cannot load service accounts")
	}
	store := &memAccountStore{byID: make(map[string]sts.ServiceAccount, len(wire.Accounts))}
	for _, a := range wire.Accounts {
		if a.ClientID == "" || a.SecretHash == "" {
			continue
		}
		hash, err := decodeHex(a.SecretHash)
		if err != nil {
			continue
		}
		store.byID[a.ClientID] = sts.ServiceAccount{
			ClientID:   a.ClientID,
			SecretHash: hash,
			MaxScope:   a.MaxScope,
			Realm:      a.Realm,
		}
	}
	return store, nil
}

// memAccountStore is the in-memory AccountStore used by the
// loader and by tests. Lookups are read-only after construction
// so no lock is needed.
type memAccountStore struct {
	byID map[string]sts.ServiceAccount
}

func (m *memAccountStore) Lookup(clientID string) (sts.ServiceAccount, bool) {
	a, ok := m.byID[clientID]
	return a, ok
}

// decodeHex parses a hex-encoded byte string. Lowercase or
// uppercase both work; we don't accept mixed-case-insensitive
// shortcuts because a misconfiguration should fail visibly.
func decodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("apigw: hex string has odd length")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexNibble(s[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := hexNibble(s[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, errors.New("apigw: invalid hex character")
}

// Handler returns the composed http.Handler. The composition order
// is fixed by PRMT-101 §5: /healthz bypasses AuthMiddleware so a
// load balancer / orchestrator can probe liveness without a
// token, while every /api/* handler is wrapped.
//
// Note: http.ServeMux's pattern matching already distinguishes
// /healthz from /api/* (no overlap), so the two paths can share
// the same mux without re-ordering — we just register them
// differently.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Routes registers /healthz and /api/sites on the mux. /healthz is
// registered bare (no AuthMiddleware); /api/sites is registered
// under a prefix pattern so the AuthMiddleware can be applied to
// the whole /api/* surface uniformly, which makes it easy for
// future PRMTs (e.g. /api/twins, /api/omniverse) to inherit
// authn/authz without re-wiring.
//
// PRMT-101 explicitly forbids /api/twins, /api/omniverse, SSE,
// and write paths in this batch; do not add them here.
//
// PRMT-102 §3 also registers /auth/{realm}/* if SetAuthHandler
// has been called. The auth surface sits OUTSIDE /api/* (and
// outside AuthMiddleware) because /auth/* itself is the login
// flow — applying AuthMiddleware to it would deadlock. Only
// /auth/ is mounted; /api/ still wraps every /api/* request.
func (s *Server) Routes() {
	// /healthz: bypass AuthMiddleware (PRMT-101 §5).
	s.mux.HandleFunc("/healthz", s.handleHealthz)

	// /api/*: wrap with AuthMiddleware. Use the "/api/" prefix
	// pattern so any future /api/<x> added later inherits the
	// middleware by virtue of being registered through the same
	// helper.
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Dispatch by exact path. Anything we don't know about
		// returns 404 + RFC7807 so it doesn't fall through to a
		// different handler or a misleading 200 from a wildcard.
		switch {
		case r.URL.Path == "/api/sites":
			s.handleSites(w, r)
		// L109 P802/P803: platform admin site-org + role-bindings (no tenant claim required).
		case r.URL.Path == "/api/site-orgs":
			s.handleSiteOrgs(w, r)
		case r.URL.Path == "/api/role-bindings":
			s.handleRoleBindings(w, r)
		case r.URL.Path == "/api/model-packs" || strings.HasPrefix(r.URL.Path, "/api/model-packs/"):
			s.handleModelPacks(w, r)
		case r.URL.Path == "/api/site-layouts" || strings.HasPrefix(r.URL.Path, "/api/site-layouts/"):
			s.handleSiteLayouts(w, r)
		// L109 P804: tenants + orgs admin (platform-admin identity forward).
		case r.URL.Path == "/api/tenants" || strings.HasPrefix(r.URL.Path, "/api/tenants/"):
			s.handleTenants(w, r)
		case r.URL.Path == "/api/orgs" || strings.HasPrefix(r.URL.Path, "/api/orgs/"):
			s.handleOrgs(w, r)
		// PRMT-141 §4: /api/assets, /api/alarms, and
		// /api/metrics/query are thin identity-forwarding proxies
		// over core /v1/{assets,alarms,metrics/query} — they
		// carry the verified claims + raw token through
		// AuthMiddleware and GetV1AsTenant, holding no
		// resource-scope logic of their own (L81).
		// PRMT-151 adds /api/topology → /v1/topology for the
		// spec-001 §7 relationship graph (feeds/cools/connects)
		// consumed by R3 root-cause/impact (spec-009 §5.2).
		case r.URL.Path == "/api/assets":
			s.handleAssets(w, r)
		case r.URL.Path == "/api/alarms":
			s.handleAlarms(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/alarms/"):
			s.handleAlarmAck(w, r) // PRMT-230
		case r.URL.Path == "/api/metrics/query",
			r.URL.Path == "/api/metrics/query_range": // PRMT-228
			s.handleMetricsQuery(w, r)
		case r.URL.Path == "/api/topology":
			s.handleTopology(w, r)
		// PRMT-153 §4: /api/tickets, /api/tickets/{id},
		// /api/capacity, and /api/capacity/metrics are thin
		// identity-forwarding proxies over core /v1/{tickets,
		// tickets/{id}, capacity, capacity/metrics} — same
		// identity + tenant check + GetV1AsTenant contract as
		// the PRMT-141/151 reads above (L81: Gateway carries
		// identity, holds no resource-scope).
		case r.URL.Path == "/api/tickets":
			s.handleTickets(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/tickets/"):
			s.handleTicketByID(w, r)
		case r.URL.Path == "/api/capacity":
			s.handleCapacity(w, r)
		case r.URL.Path == "/api/capacity/metrics":
			s.handleCapacityMetrics(w, r)
		case r.URL.Path == "/api/capacity/forecast":
			s.handleCapacityForecast(w, r) // P741
		// PRMT-193 §4.5: /api/usage → /v1/usage (identity-forward).
		case r.URL.Path == "/api/usage":
			s.handleUsage(w, r)
		// PRMT-154 §4: /api/maintenance/upcoming, /api/pm/schedules
		// (+/{id}), /api/spares (+/{id}), and /api/inspections
		// (+/{id}) are thin identity-forwarding proxies over core
		// /v1/{maintenance/upcoming, pm/schedules[/...],
		// spares[/...], inspections[/...]} — same identity +
		// tenant check + GetV1AsTenant contract as the PRMT-141/
		// 151/153 reads above (L81: Gateway carries identity,
		// holds no resource-scope).
		case r.URL.Path == "/api/maintenance/upcoming":
			s.handleMaintenanceUpcoming(w, r)
		case r.URL.Path == "/api/pm/schedules":
			s.handlePMSchedules(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/pm/schedules/"):
			s.handlePMScheduleByID(w, r)
		case r.URL.Path == "/api/spares":
			s.handleSpares(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/spares/"):
			s.handleSpareByID(w, r)
		case r.URL.Path == "/api/inspections":
			s.handleInspections(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/inspections/"):
			s.handleInspectionByID(w, r)
		// PRMT-155 §4: /api/runbooks/{key}, /api/cases,
		// /api/reports/ops, and /api/reports/reconcile are thin
		// identity-forwarding proxies over core /v1/{runbooks/{key},
		// cases, reports/ops, reports/reconcile} — same identity +
		// tenant check + GetV1AsTenant contract as the PRMT-141/
		// 151/153/154 reads above (L81: Gateway carries identity,
		// holds no resource-scope).
		case strings.HasPrefix(r.URL.Path, "/api/runbooks/"):
			s.handleRunbookByKey(w, r)
		case r.URL.Path == "/api/cases":
			s.handleCases(w, r)
		case r.URL.Path == "/api/reports/ops":
			s.handleReportOps(w, r)
		case r.URL.Path == "/api/reports/reconcile":
			s.handleReportReconcile(w, r)
		// PRMT-208: customer portal status + SLA read proxies (E3.4).
		// Aggregate status from /v1/alarms (+ /v1/sites); SLA is Q4
		// constants with optional forward to core /v1/sla.
		case r.URL.Path == "/api/customer/status":
			s.handleCustomerStatus(w, r)
		case r.URL.Path == "/api/customer/sla":
			s.handleCustomerSLA(w, r)
		case r.URL.Path == "/api/customer/usage":
			s.handleCustomerUsage(w, r)
		case isSiteStreamPath(r.URL.Path):
			s.handleSiteStream(w, r)
		case parseOmniversePath(r.URL.Path):
			s.handleOmniverse(w, r)
		default:
			WriteProblem(w, http.StatusNotFound,
				"path-not-found", "API path not found",
				"no handler registered for "+r.URL.Path,
				r.URL.Path)
		}
	})
	// PRMT-104: bind the server's STS + PDP into the package-level
	// AuthMiddleware before registering it on the mux. We do this
	// here (rather than inside AuthMiddleware itself) so the
	// signature `func AuthMiddleware(next http.Handler) http.Handler`
	// stays stable per PRMT-101 §4, and so a test that calls
	// AuthMiddleware(next) without going through NewServer still
	// gets the original pass-through behaviour (see
	// TestAuthMiddleware_NoOp).
	bindAuthDeps(s.sts, s.pdp)
	// PRMT-120: outer access-log wrapper. Records every /api/*
	// request (incl. 401/403 short-circuited inside
	// AuthMiddleware) via log/slog. Fields:
	// method/path/status/request_id/duration_ms — never
	// Authorization / Cookie / token / body / query.
	s.mux.Handle("/api/", accessLogMiddleware(AuthMiddleware(apiHandler)))

	// /auth/{realm}/* (PRMT-102 §3). Bypass AuthMiddleware — the
	// /auth/* surface IS the login flow; wrapping it would 401
	// the user before they could authenticate.
	//
	// PRMT-103 also adds /auth/{realm}/token (POST). When the
	// STS is configured we mount a single dispatcher that
	// recognises the /token suffix and delegates everything else
	// to the authn handler. The dispatcher lives in this file so
	// no import cycle is introduced (server.go is the only
	// place allowed to be modified for PRMT-103).
	if s.authHandler != nil {
		if s.sts != nil {
			s.mux.Handle("/auth/", s.dispatcherWithToken(s.authHandler))
		} else {
			s.mux.Handle("/auth/", s.authHandler)
		}
	} else if s.sts != nil {
		// No authn surface, but a token endpoint on its own
		// isn't useful — a request without a session can never
		// mint a token. Skip mounting so the route returns 404
		// rather than 500.
		_ = errors.New("apigw: token route mounted without auth handler — skipped")
	}

	// PRMT-108: POST /auth/token (client_credentials grant).
	// This sits OUTSIDE the /auth/{realm}/token dispatcher because
	// the credential model is different (service-account vs
	// session cookie) and a single dispatcher would either grow
	// a switch on URL shape or accept an unauthenticated path
	// at /auth/. Mounting it as its own exact-path route keeps
	// the existing /auth/{realm}/* surface untouched per the
	// prompt's whitelist (only this one registration line is
	// added; AuthMiddleware and the other handlers are not
	// changed).
	if s.sts != nil && s.serviceAccounts != nil {
		s.mux.HandleFunc("/auth/token", s.handleClientCredentialsToken)
	}
}

// dispatcherWithToken returns an http.Handler that delegates
// /auth/{realm}/token to s.handleToken and forwards everything
// else to next. This keeps the authn surface intact while
// layering the token route on top.
func (s *Server) dispatcherWithToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokenAction(r.URL.Path) == "token" {
			s.handleToken(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenAction returns the second path segment of /auth/<realm>
// /<action>, or "" if the path doesn't have one. Examples:
//
//	/auth/ops/token       → "token"
//	/auth/ops/login       → "login"
//	/auth/ops             → ""
//	/auth/                → ""
//
// We avoid net/http's prefix matching here because that would
// require registering the token route at a more specific prefix
// than the catch-all /auth/, which the stdlib mux does not
// support — later prefixes are shadowed by earlier ones.
func tokenAction(p string) string {
	rest := strings.TrimPrefix(p, "/auth/")
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return ""
	}
	return rest[i+1:]
}

// SetAuthHandler installs the /auth/{realm}/* handler from
// pkg/authn (PRMT-102 §3). Must be called before Routes() takes
// effect; NewServer does not register /auth/* until this is set.
// Passing nil is allowed but leaves /auth/* returning 404.
//
// We accept http.Handler (not *authn.Handler) so pkg/apigw does
// not grow a dependency on pkg/authn — authn is the consumer of
// the apigw surface, not the other way round.
func (s *Server) SetAuthHandler(h http.Handler) {
	s.authHandler = h
}

// handleHealthz returns 200 {"status":"ok"}. Intentionally cheap
// and synchronous so a load balancer probe is answered even if
// the upstream is degraded.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// AuthMiddleware is the placeholder for authentication /
// authorization on /api/*. In PRMT-101 it is a no-op pass-through
// so the Gateway can be exercised end-to-end without a working
// IdP/STS/PDP. PRMT-102 (OIDC authn), PRMT-103 (STS token
// exchange), PRMT-104 (PDP) replaced the body — the SIGNATURE
// stayed `(http.Handler) -> http.Handler` per PRMT-101 §4.
//
// Stable contract (PRMT-101 §4):
//
//	func AuthMiddleware(next http.Handler) http.Handler
//
// Wiring (PRMT-104): the STS + PDP instances are stored in a
// package-level holder that bindAuthDeps populates from
// NewServer (and that tests can populate directly via
// bindAuthDeps). When the holder is nil — i.e. a test calls
// AuthMiddleware(next) without going through NewServer — the
// middleware degrades to a pass-through. This preserves the
// PRMT-101 placeholder contract that TestAuthMiddleware_NoOp
// pins: a wrapped handler must still call its inner handler
// when no auth surface is configured.
func AuthMiddleware(next http.Handler) http.Handler {
	stsSvc, pdpSvc := currentAuthDeps()
	if stsSvc == nil && pdpSvc == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// PRMT-173 / PRMT-217: opt-in dev claims injection lives
			// in maybeInjectDevNoAuthClaims (compiled only under
			// -tags lab; prod is a no-op). LoadConfig refuses
			// DevNoAuth when the bypass is not compiled in.
			r = maybeInjectDevNoAuthClaims(r)
			next.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PRMT-104 §4 contract:
		//   1) Authorization: Bearer <token>. Missing/malformed → 401.
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			WriteProblem(w, http.StatusUnauthorized,
				"unauthorized", "missing bearer token",
				"the request did not carry an Authorization: Bearer header",
				r.URL.Path)
			return
		}
		//   2) sts.Verify (PRMT-103). Bad/expired/revoked → 401.
		//      Skip if no STS is wired (test-only path): treat the
		//      token claims as empty so the PDP still runs.
		var claims sts.TokenClaims
		if stsSvc != nil {
			c, err := stsSvc.Verify(token)
			if err != nil {
				WriteProblem(w, http.StatusUnauthorized,
					"unauthorized", "token invalid",
					"the bearer token failed to verify", r.URL.Path)
				return
			}
			claims = c
		}
		// PRMT-105: inject the verified claims into r.Context() so
		// downstream handlers (handleSites) can recover them via
		// ClaimsFrom and forward the caller identity to core /v1.
		// We do this ONLY after sts.Verify succeeds — a 401 path
		// MUST NOT carry claims into the next handler.
		//
		// M4 F4 / PRMT-114: also stamp the raw JWS so GetV1AsTenant /
		// GetV1As can forward Authorization to core (core authMW requires
		// a bearer). Without this, production apigw→core is always 401
		// and customer handlers fail-soft into empty "healthy" views.
		ctx := WithClaims(r.Context(), claims)
		ctx = WithRawToken(ctx, token)
		r = r.WithContext(ctx)
		//   3) Build the PDP Input. action is the static GET→read /
		//      else→write map pinned by PRMT-104 §4; the §6 MUST NOT
		//      list forbids treating token scope as resource scope, so
		//      we pass it as opaque input without inspecting it here.
		in := policy.Input{
			Realm:  claims.Realm,
			Action: actionForMethod(r.Method),
			Method: r.Method,
			Path:   r.URL.Path,
			MFA:    false, // PRMT-104 §4: this round pins false; PRMT-108 flips it on
			Time:   time.Now(),
			Scope:  claims.Scope,
		}
		//   4) pdp.Decision. deny OR err → 403 (fail-closed). PRMT-104
		//      §5 explicitly bans fail-open.
		if pdpSvc != nil {
			allow, err := pdpSvc.Decision(r.Context(), in)
			if err != nil || !allow {
				WriteProblem(w, http.StatusForbidden,
					"forbidden", "policy denied",
					"the policy engine denied this request", r.URL.Path)
				return
			}
		}
		//   5) Allow → next.
		next.ServeHTTP(w, r)
	})
}

// devNoAuthFlag is the snapshot of Config.DevNoAuth captured at
// boot time by cmd/cios-apigw/main.go (PRMT-173 §4.5). It is read
// by AuthMiddleware on every pass-through request, so it must be
// atomic. The LoadConfig sanity check (STS/OPA configured ⇒ false)
// runs before Store, so a true value here means "boot saw no
// STS/OPA AND the env var is truthy".
var devNoAuthFlag atomic.Bool

// devNoAuthEnabled returns devNoAuthFlag.Load().
func devNoAuthEnabled() bool { return devNoAuthFlag.Load() }

// SnapshotDevNoAuth is the boot-time bridge that cmd/cios-apigw/main.go
// uses to publish cfg.DevNoAuth into the request-time atomic (PRMT-173
// §4.6). Cross-package call site because main.go does not live in
// pkg/apigw; the underlying atomic stays unexported.
func SnapshotDevNoAuth(v bool) { devNoAuthFlag.Store(v) }

// DevBypassAvailable reports whether this binary was built with
// -tags lab and therefore includes the DevNoAuth claims inject path.
// Production builds return false; LoadConfig refuses DevNoAuth.
// PRMT-217 (report S-1).
func DevBypassAvailable() bool { return devBypassAvailable }

// bearerToken returns the raw token from an "Authorization:
// Bearer <token>" header, or ok=false if the header is missing,
// empty, or not in the Bearer scheme.
func bearerToken(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	tok := h[len(prefix):]
	if tok == "" {
		return "", false
	}
	return tok, true
}

// actionForMethod maps an HTTP verb to the PDP's action vocabulary
// per PRMT-104 §4: GET → read, everything else → write. The
// conservative mapping is intentional — write paths come online
// in PRMT-105/108 and will refine this.
func actionForMethod(m string) string {
	if m == http.MethodGet {
		return "read"
	}
	return "write"
}

// authDeps groups the optional STS + PDP that AuthMiddleware
// consults. Both fields are independently optional (the test
// suite uses PDP-only or STS-only stubs in some cases), so the
// pair is treated as "either wired, both consulted".
type authDeps struct {
	sts *sts.STS
	pdp policy.PDP
}

// authHolder is the package-level holder that AuthMiddleware reads
// on every request. Routes() updates it via bindAuthDeps. Tests
// that bypass NewServer (the existing TestAuthMiddleware_NoOp)
// leave it nil, in which case AuthMiddleware degrades to a
// pass-through.
//
// Concurrency: bindAuthDeps is called from NewServer → Routes
// during process start; it does not run concurrently with
// request handling in production. A test that calls NewServer in
// one goroutine while another sends requests through the
// constructed handler would race; this matches the existing
// Server constructor contract (SetSTS / SetSessionKey / SetAuthHandler
// are likewise init-time only) and is the reason the helpers
// exist as setters rather than open-ended mutex-guarded slots.
var authHolder atomic.Pointer[authDeps]

func bindAuthDeps(stsSvc *sts.STS, pdpSvc policy.PDP) {
	authHolder.Store(&authDeps{sts: stsSvc, pdp: pdpSvc})
}

func currentAuthDeps() (*sts.STS, policy.PDP) {
	d := authHolder.Load()
	if d == nil {
		return nil, nil
	}
	return d.sts, d.pdp
}

// problemTypeBase mirrors core/server.go's RFC 7807 type URL base.
// Keeping the constant in this package means pkg/apigw doesn't
// depend on internal core helpers and the Gateway can stand alone
// (spec-009 §7.1: Portal/Gateway sit ABOVE core /v1, never inside
// it). Spec-004 §4 is the registry of valid tails — PRMT-101 only
// emits "upstream-unavailable" and "bad-request".
const problemTypeBase = "https://cios.dev/errors/"

// WriteProblem writes an RFC 7807 Problem Details response
// (spec-004 §4) with Content-Type application/problem+json.
//
// ptype is the TYPE TAIL (e.g. "upstream-unavailable", "bad-request");
// WriteProblem prepends problemTypeBase so the emitted "type"
// matches the URL form spec-004 §4 defines and core/server.go
// emits. Callers MUST use a tail from spec-004 §4 — never invent
// new tails here.
//
// Field semantics:
//   - title    : short, human-readable summary of the type.
//   - detail   : request-specific explanation; safe for ops logs.
//   - instance : the URI of the offending request (the path).
//
// The status field in the JSON body is kept in sync with the HTTP
// status code. This is what the existing /v1 error path emits, so
// clients that already parse spec-004 errors keep working.
func WriteProblem(w http.ResponseWriter, status int, ptype, title, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	// ptype 若已是完整 URL（含 "://"），verbatim 使用，避免 problemTypeBase 双重前缀
	// （spec-004 §4：注册表登记 tail，但调用方误传全 URL 时不得产出 doubled URL）。
	typeVal := problemTypeBase + ptype
	if strings.Contains(ptype, "://") {
		typeVal = ptype
		log.Printf("WriteProblem: ptype contains ://, using verbatim (ptype=%q)", ptype)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     typeVal,
		"title":    title,
		"status":   status,
		"detail":   detail,
		"instance": instance,
	})
}

// sessionCookieWire is the public cookie name used by both
// authn.Handler (writer) and Server.handleToken (reader).
// PRMT-103 cannot modify pkg/authn, so we mirror the literal
// here; if pkg/authn's sessionCookieName ever changes, this
// constant must move with it. The duplication is intentional
// and documented; a follow-up PR can lift it into a shared
// constants package if the dependency arrow flips.
const sessionCookieWire = "cios_session"

// handleToken serves POST /auth/{realm}/token (PRMT-103 §2-bis).
//
// Contract:
//   - 200 {access_token, expires_in, token_type:"Bearer"} when
//     the request carries a valid session cookie AND its realm
//     matches the URL realm.
//   - 401 when the cookie is missing, expired, or fails HMAC.
//   - 403 when the cookie is valid but its realm differs from
//     the URL realm (cross-realm replay defence).
//   - 404 when the realm is not in {"ops","customer"}.
//
// The session cookie is decoded with authn.DecodeSession using
// the same key authn.Handler was configured with. The scopes
// passed to STS.Exchange are extracted from session.Claims
// (`roles`, with `scope` as a fallback) and copied verbatim
// (PRMT-103 §5: scope 不扩权).
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"only POST is accepted at /auth/{realm}/token", r.URL.Path)
		return
	}
	if s.sts == nil {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "token route disabled",
			"STS is not configured; set CIOS_STS_SIGNING_KEY", r.URL.Path)
		return
	}
	if len(s.sessionKey) == 0 {
		WriteProblem(w, http.StatusInternalServerError,
			"internal", "session key not configured",
			"CIOS_APIGW_SESSION_KEY must be set", r.URL.Path)
		return
	}

	urlRealm, ok := parseRealmFromTokenPath(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "token path malformed",
			"expected /auth/<realm>/token", r.URL.Path)
		return
	}

	cookie, err := r.Cookie(sessionCookieWire)
	if err != nil {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no session",
			"request did not carry a session cookie", r.URL.Path)
		return
	}
	if s.sessionDecoder == nil {
		WriteProblem(w, http.StatusInternalServerError,
			"internal", "session decoder not configured",
			"no SessionDecoder wired; set via SetSessionDecoder", r.URL.Path)
		return
	}
	info, err := s.sessionDecoder(s.sessionKey, cookie.Value)
	if err != nil {
		// 401 with a generic body — never echo err to the browser
		// (PRMT-103 §6 — surface area is identity-only).
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "session invalid",
			"the session cookie failed to verify", r.URL.Path)
		return
	}
	if err := sts.CheckRealm(urlRealm, info.Realm); err != nil {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "realm mismatch",
			"the session was minted for a different realm", r.URL.Path)
		return
	}

	scopes := sessionRoles(info)
	raw, expiresIn, err := s.sts.Exchange(info.Subject, info.Realm, scopes)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError,
			"internal", "could not mint token",
			err.Error(), r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": raw,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	})
}

// parseRealmFromTokenPath returns the realm segment of
// /auth/<realm>/token. It rejects paths that don't fit the
// shape or whose realm is not in {"ops","customer"}.
func parseRealmFromTokenPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/auth/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] != "token" {
		return "", false
	}
	switch parts[0] {
	case realmOps, realmCustomer:
		return parts[0], true
	default:
		return "", false
	}
}

// sessionRoles extracts the role/scope list from a SessionInfo's
// verified Claims. We look at `roles` first (PRMT-103 §4 says
// scopes come from session.Claims), falling back to `scope`
// (the standard OIDC claim, space-delimited) so an IdP that
// only emits `scope` still produces a usable token.
//
// The returned slice is freshly allocated — STS.Exchange copies
// it again defensively, so the caller never observes a
// subsequent mutation through the token.
func sessionRoles(info SessionInfo) []string {
	c := info.Claims
	if rs, ok := c["roles"].([]any); ok {
		out := make([]string, 0, len(rs))
		for _, v := range rs {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if sc, ok := c["scope"].(string); ok && sc != "" {
		parts := strings.Fields(sc)
		return parts
	}
	return nil
}

// handleClientCredentialsToken serves POST /auth/token
// (PRMT-108 §2 / §2-bis). It is the gateway-side machine-identity
// counterpart to /auth/{realm}/token: instead of a session cookie,
// the caller presents client_id / client_secret (RFC 6749 §4.4
// client_credentials grant) and gets back a bearer minted by the
// SAME STS that mints portal tokens. The token is therefore
// indistinguishable from a portal token at Verify / Revoke time
// (PRMT-108 §1 "与门户 token 同一吊销面").
//
// Wire shape (PRMT-108 §2-bis):
//
//	request : POST /auth/token
//	          Content-Type: application/x-www-form-urlencoded
//	          body: grant_type=client_credentials
//	                &client_id=<id>&client_secret=<secret>
//	                &scope=<space-separated-scope-list>
//	response: 200 application/json
//	          {"access_token":"<jwt>","token_type":"Bearer",
//	           "expires_in":<seconds>}
//	          401 — bad client_id / secret (collapsed; we never
//	                distinguish the two cases at the HTTP layer).
//	          403 — requested scope exceeds account MaxScope.
//	          415 — wrong Content-Type.
//	          503 — service-account store not configured.
func (s *Server) handleClientCredentialsToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"only POST is accepted at /auth/token", r.URL.Path)
		return
	}
	if s.sts == nil {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "token route disabled",
			"STS is not configured; set CIOS_STS_SIGNING_KEY", r.URL.Path)
		return
	}
	if s.serviceAccounts == nil {
		WriteProblem(w, http.StatusServiceUnavailable,
			"upstream-unavailable", "service accounts not configured",
			"CIOS_STS_SERVICE_ACCOUNTS is not set", r.URL.Path)
		return
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if ct != "application/x-www-form-urlencoded" {
		WriteProblem(w, http.StatusUnsupportedMediaType,
			"bad-request", "unsupported content type",
			"expected application/x-www-form-urlencoded", r.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "could not read body",
			err.Error(), r.URL.Path)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "malformed form body",
			err.Error(), r.URL.Path)
		return
	}
	if form.Get("grant_type") != "client_credentials" {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "unsupported grant_type",
			"only client_credentials is accepted at /auth/token",
			r.URL.Path)
		return
	}
	clientID := form.Get("client_id")
	clientSecret := form.Get("client_secret")
	if clientID == "" || clientSecret == "" {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "missing credentials",
			"client_id and client_secret are required", r.URL.Path)
		return
	}
	var reqScope []string
	if raw := form.Get("scope"); raw != "" {
		reqScope = strings.Fields(raw)
	}
	raw, expiresIn, err := s.sts.IssueClientCredentials(s.serviceAccounts, clientID, clientSecret, reqScope)
	if err != nil {
		switch {
		case errors.Is(err, sts.ErrBadCredentials):
			WriteProblem(w, http.StatusUnauthorized,
				"unauthorized", "invalid client credentials",
				"the supplied client_id / client_secret did not match",
				r.URL.Path)
		case errors.Is(err, sts.ErrScopeExceeded):
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "scope exceeds account maximum",
				"requested scope is not a subset of the account's max_scope",
				r.URL.Path)
		default:
			WriteProblem(w, http.StatusInternalServerError,
				"internal", "could not mint token",
				err.Error(), r.URL.Path)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": raw,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	})
}
