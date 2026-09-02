// Omniverse service-token broker: PRMT-107 implements the
// Gateway-side /api/omniverse/* endpoint that proxies user requests
// to an Omniverse/Nucleus upstream using a machine-identity service
// token (spec-009 §7.1, §6, L49, L81).
//
// Red line (spec-009 §7.1 "Omniverse service token"): the user
// Authorization header MUST NOT be forwarded. The Gateway first
// verifies the user via AuthMiddleware (PRMT-104), then replaces
// the inbound Authorization/Cookie with the machine service token
// before the outbound request. If the service token is unavailable,
// the handler returns 502 RFC7807 — it NEVER falls back to the user
// token or to anonymous.
package apigw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// envOmniverseURL is the env var that supplies the Omniverse base
// URL. The handler MUST read this from env rather than hard-code it
// (PRMT-107 §3). An empty value yields 502 RFC7807 — the operator
// forgot to deploy the configuration, and silently succeeding
// would mask a misconfigured cluster.
const envOmniverseURL = "CIOS_OMNIVERSE_URL"

// envOmniverseServiceToken is the env var that supplies the static
// service token used to authenticate outbound calls to Omniverse.
// PRMT-107 §3: must not be hard-coded. The ServiceTokenSource
// interface (NewEnvServiceTokenSource) is the seam a future PRMT
// can use to layer rotation without changing the handler shape.
const envOmniverseServiceToken = "CIOS_OMNIVERSE_SERVICE_TOKEN"

// ServiceTokenSource produces the machine-identity service token
// attached to outbound calls to the Omniverse upstream. The
// interface is the seam a future PRMT (rotation, KMS, sidecar) can
// fill without changing handleOmniverse's wire shape. Token MUST
// NOT be empty — an empty token is treated as an error and the
// handler returns 502 (no fallback).
//
// Implementations MUST treat ctx as the cancellation boundary so
// a slow token source cannot wedge the request goroutine.
type ServiceTokenSource interface {
	Token(ctx context.Context) (string, error)
}

// envServiceTokenSource reads the service token from an env var
// (PRMT-107 §2). It is the static-token implementation that ships
// in this round; rotation lands in a follow-up PRMT. Returning an
// error on missing env matches the PRMT's "no fallback to user
// token / anonymous" red line — an operator who forgot to set the
// env should see a loud 502 in their logs rather than a silent
// pass-through.
type envServiceTokenSource struct {
	envVar string
}

// NewEnvServiceTokenSource returns a ServiceTokenSource that reads
// the token from envVar on every call. Reading on every call
// (rather than caching at construction) keeps the source testable
// — tests can flip the env between subtests and observe the new
// value. Production deployments can swap to a cached source via
// SetOmniverseTokenSource if rotation latency becomes a concern.
func NewEnvServiceTokenSource(envVar string) ServiceTokenSource {
	return &envServiceTokenSource{envVar: envVar}
}

// Token returns the current service token from env, or an error if
// the env var is unset/empty. The handler translates the error
// into 502 RFC7807 upstream-unavailable — there is no anonymous
// fallback (spec-009 §7.1).
func (s *envServiceTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.envVar == "" {
		return "", fmt.Errorf("apigw: omniverse service token source is not configured")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v := os.Getenv(s.envVar)
	if v == "" {
		return "", fmt.Errorf("apigw: omniverse service token env %q is empty", s.envVar)
	}
	return v, nil
}

// SetOmniverseTokenSource installs a ServiceTokenSource. Tests use
// this to inject a deterministic source; production wires the env
// source via NewServer's loadOmniverse hook. The setter follows
// the same init-time-only discipline as SetSTS / SetSource /
// SetPDP — concurrent swap during request handling would race the
// authHolder pattern, so callers are expected to set it before
// serving traffic.
func (s *Server) SetOmniverseTokenSource(src ServiceTokenSource) {
	s.omniverseToken = src
}

// loadOmniverse wires the default ServiceTokenSource from env
// (CIOS_OMNIVERSE_SERVICE_TOKEN). The hook is separate from
// NewServer's other loaders so the §5 MUST list can be verified
// by reading one function. An empty env does NOT error out at
// boot — handleOmniverse returns 502 per-request when the token
// is missing, matching the principle that misconfiguration
// surfaces as visible 502s in logs rather than a startup panic.
func (s *Server) loadOmniverse() {
	s.omniverseURL = os.Getenv(envOmniverseURL)
	s.omniverseToken = NewEnvServiceTokenSource(envOmniverseServiceToken)
}

// handleOmniverse proxies /api/omniverse/* to the Omniverse
// upstream. Inbound identity is verified by AuthMiddleware
// (PRMT-104) before this handler runs; this handler's job is
// the identity boundary: take the inbound request, drop any
// user Authorization/Cookie, attach the machine service token,
// and forward the rest verbatim.
//
// Wire contract (PRMT-107 §4):
//   - Method/path/query/body copied from the inbound request.
//   - Inbound `Authorization` header explicitly deleted.
//   - Inbound `Cookie` header explicitly deleted.
//   - Outbound `Authorization: Bearer <service token>` set.
//   - Service token unavailable → 502 RFC7807
//     (no fallback to user token, no anonymous).
//
// Status mapping (mirrors handleSites):
//   - Transport error → 502 upstream-unavailable.
//   - Upstream 5xx → 502 upstream-unavailable.
//   - Upstream 2xx/4xx → forwarded verbatim (Content-Type
//     defaulted by status: 4xx = problem+json, else json).
//   - Non-/api/omniverse/... path → 404 (defensive; dispatch
//     in Routes() already restricts this).
func (s *Server) handleOmniverse(w http.ResponseWriter, r *http.Request) {
	// Resolve the service token FIRST. If we can't authenticate
	// the outbound call as the machine, there's no point
	// touching the request body — and surfacing the 502 before
	// any outbound bytes keeps the failure mode loud.
	if s.omniverseToken == nil {
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"omniverse service token source is not wired", r.URL.Path)
		return
	}
	tok, err := s.omniverseToken.Token(r.Context())
	if err != nil {
		// PRMT-107 §4: service token missing → 502, NO fallback.
		// We do NOT echo err to the caller (network topology
		// leak); operators correlate via server logs.
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"omniverse service token is unavailable", r.URL.Path)
		return
	}

	// Resolve the upstream base URL. An empty URL is a config
	// error; treat as 502 so the operator sees a clear failure
	// in their access logs.
	base := strings.TrimRight(s.omniverseURL, "/")
	if base == "" {
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"omniverse base URL is not configured", r.URL.Path)
		return
	}

	// Strip the "/api/omniverse" prefix from the inbound path so
	// the upstream sees its own path layout. The dispatcher in
	// Routes() mounts /api/omniverse/* (PRMT-107 §3) so this
	// TrimPrefix is invariant: the inbound path always begins
	// with /api/omniverse/. We compute the suffix once and
	// build the outbound URL with explicit slash joining to
	// avoid doubled-slash issues some upstreams reject.
	suffix := strings.TrimPrefix(r.URL.Path, "/api/omniverse")
	if suffix == "" {
		suffix = "/"
	}
	outURL := base + suffix
	if r.URL.RawQuery != "" {
		outURL += "?" + r.URL.RawQuery
	}

	// Build the outbound request. The body is read from the
	// inbound request so the upstream sees the same payload the
	// Portal sent. For chunked / streaming bodies we'd want
	// http.MaxBytesReader; this round is a minimal proxy so we
	// copy the body verbatim and let the inbound HTTP server's
	// body limit apply.
	var bodyReader io.Reader
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		bodyReader = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, outURL, bodyReader)
	if err != nil {
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"could not build omniverse request", r.URL.Path)
		return
	}

	// PRMT-107 §5 MUST 1: explicitly delete inbound
	// Authorization and Cookie. We build the outbound Header
	// from scratch (not via req.Header = r.Header.Clone()) so
	// there is zero chance of leaking the user Authorization
	// to Omniverse — the inbound identity literally cannot
	// reach the outbound request.
	//
	// We DO forward safe request headers (Content-Type,
	// Accept, Content-Length when known, X-Forwarded-For) so
	// the upstream sees a coherent request shape. Any
	// hop-by-hop header (Connection, Keep-Alive, etc.) is
	// omitted; Go's stdlib http.Client manages those itself.
	copyOutboundHeaders(req, r)

	// PRMT-107 §5 MUST 1: set the machine-identity bearer.
	// spec-004 §6 pins the Authorization: Bearer shape; we
	// MUST NOT invent a custom header.
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := s.omniverseHTTPClient().Do(req)
	if err != nil {
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"omniverse upstream is not reachable", r.URL.Path)
		return
	}
	defer resp.Body.Close()

	// Status mapping: 5xx → 502; 2xx/4xx → passthrough. Mirrors
	// handleSites so the Portal sees a single error shape for
	// upstream problems regardless of which /api/* route it
	// hit.
	switch {
	case resp.StatusCode >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"omniverse upstream returned "+http.StatusText(resp.StatusCode),
			r.URL.Path)
		return
	default:
		// 2xx and 4xx forwarded unchanged. Headers we know are
		// safe (Content-Type, X-Request-Id) are copied; hop-by-
		// hop headers (Connection, Transfer-Encoding) are NOT
		// copied because Go's stdlib http.Client manages them
		// and copying them can corrupt the response.
		copyResponseHeaders(w, resp)
		if resp.StatusCode >= 400 {
			// Upstream 4xx: default to problem+json (spec-004
			// §4 says /v1 emits problem+json; Omniverse's
			// own error shape may differ — callers can still
			// see the raw Content-Type if we forwarded one).
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/problem+json")
			}
		} else {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// omniverseHTTPClient returns the http.Client used for outbound
// Omniverse calls. NewServer installs a default with no timeout;
// production deployments are expected to inject a tuned client
// (mTLS, sane timeout) via SetOmniverseHTTPClient — matching the
// discipline pkg/apigw/upstream.go documents for the core /v1
// client. The getter is a method so future PRMTs can layer a
// per-call timeout / circuit breaker without changing call sites.
func (s *Server) omniverseHTTPClient() *http.Client {
	if s.omniverseHTTP != nil {
		return s.omniverseHTTP
	}
	return http.DefaultClient
}

// SetOmniverseHTTPClient lets main.go inject a tuned *http.Client
// (mTLS, timeout) for the Omniverse upstream. Mirrors
// NewUpstream's contract (spec-006 §5): the Gateway itself does
// not own TLS credentials, so the caller wires them.
func (s *Server) SetOmniverseHTTPClient(hc *http.Client) {
	s.omniverseHTTP = hc
}

// copyOutboundHeaders copies the safe subset of inbound request
// headers to the outbound request. "Safe" here means headers that
// are meaningful for an upstream HTTP call and do NOT carry
// caller identity (PRMT-107 §5 MUST 1: delete Authorization and
// Cookie). We deliberately build req.Header from scratch rather
// than cloning r.Header, so a future header that turns out to
// carry identity cannot leak by accident.
//
// Headers forwarded:
//   - Content-Type (so upstream can parse the body)
//   - Accept (so upstream picks the right representation)
//   - User-Agent (only if the inbound had one)
//   - X-Forwarded-For (we append the inbound remote address so
//     upstream can log the original caller chain)
//
// Headers dropped (besides Authorization/Cookie):
//   - All hop-by-hop headers (Connection, Keep-Alive, etc.) —
//     Go's stdlib http.Client manages these itself and copying
//     them corrupts the outbound request.
func copyOutboundHeaders(req *http.Request, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if acc := r.Header.Get("Accept"); acc != "" {
		req.Header.Set("Accept", acc)
	}
	if ua := r.Header.Get("User-Agent"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// X-Forwarded-For is append-only (RFC 7239 §5.2): we add
	// the inbound remote address to any existing chain so the
	// upstream can trace the call back through the Gateway.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff+", "+r.RemoteAddr)
	} else if r.RemoteAddr != "" {
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
}

// copyResponseHeaders copies safe response headers from the
// upstream response to the Gateway response writer. Hop-by-hop
// headers are filtered (Go's stdlib http.Client sets these for
// the inbound response; copying them back would let the upstream
// dictate the Gateway's HTTP framing).
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	// We iterate over the upstream's response headers and pick
	// out the ones that are safe to forward. The allow-list
	// keeps us from accidentally relaying hop-by-hop headers.
	allowed := []string{
		"Content-Type",
		"X-Request-Id",
		"X-Trace-Id",
		"Cache-Control",
	}
	for _, h := range allowed {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// parseOmniversePath is a thin predicate that mirrors the dispatch
// used by Routes(): it returns true for /api/omniverse or
// /api/omniverse/<anything>. The Routes() switch uses this so the
// handler list stays in one place; tests use it directly.
func parseOmniversePath(p string) bool {
	if p == "/api/omniverse" {
		return true
	}
	if strings.HasPrefix(p, "/api/omniverse/") {
		return true
	}
	return false
}

// fileServiceTokenSource reads the service token from a file on
// disk, caches it, and re-reads when the file's mtime changes or
// when the TTL elapses (PRMT-118). This is the seam a sidecar or
// operator rotation workflow can use without restarting the
// Gateway: drop a new token into the mounted file, and the next
// Token() call observes it. Per spec-009 §7.1, a read failure
// returns an error so handleOmniverse can surface 502 rather
// than fall back to anonymous / user-token.
type fileServiceTokenSource struct {
	path     string
	ttl      time.Duration
	mu       sync.Mutex
	cached   string
	loadedAt time.Time
	mtime    time.Time
}

// NewFileServiceTokenSource returns a ServiceTokenSource that
// reads the token from path. A positive ttl caches successful
// reads for that duration; ttl<=0 means the source re-reads on
// every call (still checking mtime to avoid unnecessary syscalls
// once a read has succeeded). The path is captured at
// construction time — callers are expected to set it from a
// stable mount (PRMT-118 keeps env-reading out of scope; the
// constructor takes the path directly).
func NewFileServiceTokenSource(path string, ttl time.Duration) ServiceTokenSource {
	return &fileServiceTokenSource{path: path, ttl: ttl}
}

// Token returns the current service token from the file, or an
// error if the file is missing/unreadable or the token is empty
// after trimming. The cache is keyed on mtime + TTL: while the
// cached entry is fresh AND the mtime matches, no syscall is
// performed. Otherwise os.Stat and (on change) os.ReadFile run
// under the mutex; the entire critical section holds mu so
// concurrent callers never observe a torn (empty) cache.
func (s *fileServiceTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.path == "" {
		return "", fmt.Errorf("apigw: omniverse service token source is not configured")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.cached != "" && s.ttl > 0 && now.Sub(s.loadedAt) < s.ttl {
		return s.cached, nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return "", fmt.Errorf("apigw: stat omniverse service token file %q: %w", s.path, err)
	}
	mtime := info.ModTime()
	if s.cached != "" && mtime.Equal(s.mtime) {
		s.loadedAt = now
		return s.cached, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("apigw: read omniverse service token file %q: %w", s.path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("apigw: omniverse service token file %q is empty", s.path)
	}
	s.cached = tok
	s.loadedAt = now
	s.mtime = mtime
	return tok, nil
}
