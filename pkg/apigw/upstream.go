// Upstream: thin reverse-HTTP client used by the Gateway to consume
// core /v1. PRMT-101 §4 pins the contract to a single GET helper;
// PRMT-105 adds GetV1As for identity-bearing reverse calls. Future
// PRMTs may add other verbs when /api/twins, /api/omniverse, or
// write paths arrive, but this file MUST stay read-only.
//
// PRMT-109: GetV1AsTenant dispatches by isolation_tier
// (label / row / db). Label tier injects a tenant="<id>" label
// into PromQL via pkg/tenant.InjectTenantLabel (L53); row/db
// tiers attach a tenant propagation header so core can apply
// row-level predicates or per-tenant database routing. The
// Gateway itself never inspects crn / path-globs (L81 red line).
package apigw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/yurimeng/cios/pkg/sts"
	"github.com/yurimeng/cios/pkg/tenant"
)

// Upstream is a single-purpose reverse HTTP client targeting the
// core /v1 base URL. It holds no state beyond the base URL and the
// HTTP client (which is injected so tests can substitute timeouts,
// transport stubs, or an in-process httptest server).
//
// Security note (spec-006 §5): production deployments MUST inject
// an *http.Client wired with mTLS credentials and a sane timeout.
// PRMT-101 does not enforce that here — callers (cmd/cios-apigw
// main.go and tests) own the client construction.
type Upstream struct {
	baseURL string
	hc      *http.Client
}

// NewUpstream constructs an Upstream bound to baseURL. The baseURL
// must be a fully-qualified URL with scheme and host (no trailing
// slash required; trailing slashes are normalised so callers can
// pass either form). An empty baseURL returns nil so callers can
// fail fast on misconfiguration.
func NewUpstream(baseURL string, hc *http.Client) *Upstream {
	if baseURL == "" {
		return nil
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Upstream{baseURL: strings.TrimRight(baseURL, "/"), hc: hc}
}

// GetV1 issues GET {baseURL}{path} with ctx propagation and returns
// the upstream status code, body bytes, contentType, and any
// transport error.
//
// contentType is the verbatim value of the upstream response's
// Content-Type header (resp.Header.Get("Content-Type")) — empty
// when the upstream omitted it OR on a transport failure. The
// caller (handleSites) is expected to set the response
// Content-Type from this value, falling back to a status-based
// default only when contentType is empty (PRMT-115 §2: handler
// 优先用上游 Content-Type，缺失才回退推断). This avoids the
// previous shape where 2xx non-JSON upstreams (e.g. text/csv)
// would have their Content-Type silently rewritten to
// application/json.
//
// Error semantics (PRMT-101 §4):
//   - Transport error (DNS failure, connection refused, ctx
//     cancelled, timeout): returns (0, nil, "", err). The caller
//     is expected to translate this into an RFC7807 502
//     "upstream-unavailable".
//   - Upstream responded (any status): returns (status, body,
//     contentType, nil) and the body is fully read and closed
//     regardless of status code. The caller decides how to
//     surface 4xx/5xx upstream responses (see pkg/apigw/sites.go).
//
// path is concatenated onto baseURL with a single "/" join —
// callers must NOT include a leading slash, since that would
// produce a doubled slash which some upstreams reject.
func (u *Upstream) GetV1(ctx context.Context, path string) (status int, body []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream request: %w", err)
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		// Network/transport failure: no status, no body, no
		// Content-Type. Callers translate this to RFC7807 502
		// upstream-unavailable.
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	// PRMT-115 §4: capture upstream Content-Type verbatim so the
	// handler can mirror it on the response. Header.Get returns
	// "" when the upstream omitted the header, which is also
	// exactly the value we want the handler to fall back on a
	// status-based default.
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// GetV1As is the identity-bearing counterpart to GetV1 (PRMT-105 §4).
// It builds the same {baseURL}{path} GET request but additionally
// attaches an Authorization: Bearer header carrying the original
// STS-issued JWS, so core /v1 can verify the token (L81: core is
// the resource-scope authority and MUST see a verifiable identity).
// The Gateway itself does NOT inspect claims to decide visibility —
// it only carries them across the boundary.
//
// Per spec-004 §6 the scoped bearer is the standard shape for
// machine identity. PRMT-114 §2 changes the bearer value: instead
// of forwarding claims.Subject (a bare subject string), the
// Gateway forwards rawToken (the original JWS) so core can
// re-verify. The claims argument is still required because
// downstream helpers (GetV1AsTenant) consume it for tenant
// derivation; GetV1As itself does not inspect claims, but the
// parameter is kept for API symmetry and to avoid having a
// second, claim-less helper that would otherwise proliferate
// at the handler call sites.
//
// When rawToken is empty, the Authorization header is omitted —
// PRMT-114 §2-bis: "无 rawToken（防御路径）时不附 Authorization，
// 沿用既有 401 上游约定". The handleSites / handleSiteStream call
// sites always supply a verified rawToken (AuthMiddleware only
// injects it on a successful verify path), so the empty branch
// is defensive. The header name is spec-004 §6; we MUST NOT
// invent a custom one.
//
// contentType is the verbatim value of the upstream response's
// Content-Type header (PRMT-115 §4); empty on transport error or
// when upstream omitted it. See GetV1 for the full contract —
// GetV1As returns the same shape so handlers can use either
// helper uniformly.
//
// Error semantics mirror GetV1: transport failure →
// (0, nil, "", err); upstream responded (any status) →
// (status, body, contentType, nil). The caller (handleSites)
// applies the same 5xx→502 / 4xx→passthrough switch.
func (u *Upstream) GetV1As(ctx context.Context, claims sts.TokenClaims, rawToken, path string) (status int, body []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream request: %w", err)
	}
	// PRMT-114 §4: attach the raw JWS as the bearer. PRMT-104 has
	// already verified the token via sts.Verify; we forward it
	// rather than re-signing or re-scoping (the Gateway has no
	// authority over scope — L81 red line). claims is unused here
	// at the wire layer, but is part of the contract for parity
	// with GetV1AsTenant and to keep the call sites uniform.
	_ = claims
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// GetV1AsTenant dispatches a /v1 GET by the resolved tenant
// isolation tier (PRMT-109 §2 / §4). It is the tier-aware counterpart
// to GetV1As: callers (handleSites) recover the tenant from claims
// and then forward to core /v1 with the correct header set.
//
// Behaviour matrix (PRMT-109 §2 + §5):
//
//	tier = label  : X-CIOS-Tenant header attached. PromQL label
//	                enforcement (L53) happens on PromQL-bearing
//	                routes via pkg/tenant.InjectTenantLabel; for
//	                non-PromQL routes (e.g. /v1/sites) the header
//	                is the only tenant signal — core's per-tenant
//	                listing layer is the downstream enforcement
//	                (PRMT-109 §10).
//	tier = row    : X-CIOS-Tenant header attached. Core /v1
//	                applies row-level predicates (deferred per
//	                PRMT-109 §10).
//	tier = db     : X-CIOS-Tenant header attached. Core /v1 picks
//	                the per-tenant database (deferred per
//	                PRMT-109 §10).
//	tenant/tier missing or invalid → ErrTenantMissing (handler → 403).
//
// The Authorization: Bearer header is set from rawToken (the
// original STS-issued JWS) per PRMT-114 §4 — core /v1 must receive
// a verifiable bearer. The tenant header is an additional,
// independent dimension — bearer = caller identity (JWS), tenant
// header = per-tenant data scope. PRMT-109 §4 mandates this split
// so core RBAC (L34/L50) and tenant isolation (L83) compose without
// one overloading the other.
//
// Error semantics mirror GetV1As. The new sentinel error
// (ErrTenantMissing) is a caller-side signal, NOT a transport
// failure — it short-circuits before u.hc.Do is called.
//
// contentType is the verbatim value of the upstream response's
// Content-Type header (PRMT-115 §4); empty on transport error,
// tenant-resolution failure, or when upstream omitted it. The
// caller (handleSites) sets the response Content-Type from this
// value, falling back to a status-based default only when
// contentType is empty.
//
// The PromQL label-injection lives in pkg/tenant.InjectTenantLabel
// and is invoked from the handler (or a future PromQL-bearing
// route), not from this method — the handler knows the request
// shape (where the query string lives); this method is the
// transport-level tier dispatch.
func (u *Upstream) GetV1AsTenant(ctx context.Context, claims sts.TokenClaims, rawToken, path string) (status int, body []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	tenantID, _, ok := tenant.TenantFromClaims(claims)
	if !ok {
		// PRMT-109 §5 fail-closed: tier 缺失/非法 → 403. Surfaced
		// as a distinct sentinel so the handler can map it to
		// 403 without inspecting the wrapped error text.
		return 0, nil, "", ErrTenantMissing
	}

	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream request: %w", err)
	}
	// PRMT-114 §4: attach the raw JWS as the bearer. PRMT-109 §4
	// still mandates the tenant header as an independent dimension
	// — bearer = caller identity (JWS), tenant header = per-tenant
	// data scope. The two compose without overloading.
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	// Attach the tenant propagation header for every tier. Label
	// tier uses it as a cross-check signal (the actual enforcement
	// is the PromQL rewrite done at the handler); row/db tier
	// uses it for the actual enforcement (L83). One header, one
	// consumer contract — keeps the wire shape uniform.
	headerName, headerValue := tenant.TenantPropagationHeader(tenantID)
	req.Header.Set(headerName, headerValue)

	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// PostV1As is the POST counterpart to GetV1As (no tenant header).
// Used by platform-admin routes (L109 site-orgs / role-bindings) that
// do not require a tenant claim on the token.
func (u *Upstream) PostV1As(ctx context.Context, claims sts.TokenClaims, rawToken, path string, body []byte) (status int, respBody []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	_ = claims
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream POST: %w", err)
	}
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// PutV1As is PUT with bearer only (L109 model pack bindings).
func (u *Upstream) PutV1As(ctx context.Context, claims sts.TokenClaims, rawToken, path string, body []byte) (status int, respBody []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	_ = claims
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, full, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream PUT: %w", err)
	}
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// DeleteV1As is DELETE with bearer only (no tenant header). L109 P803.
func (u *Upstream) DeleteV1As(ctx context.Context, claims sts.TokenClaims, rawToken, path string) (status int, respBody []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	_ = claims
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, full, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream DELETE: %w", err)
	}
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// PostV1AsTenant is the POST counterpart to GetV1AsTenant (PRMT-199).
// Forwards method=POST, Content-Type application/json, body bytes,
// Authorization bearer (raw JWS), and X-CIOS-Tenant. Same tenant
// fail-closed and Content-Type pass-through contracts as GET.
func (u *Upstream) PostV1AsTenant(ctx context.Context, claims sts.TokenClaims, rawToken, path string, body []byte) (status int, respBody []byte, contentType string, err error) {
	if u == nil {
		return 0, nil, "", errUpstreamNil
	}
	if path == "" {
		return 0, nil, "", errUpstreamEmptyPath
	}
	tenantID, _, ok := tenant.TenantFromClaims(claims)
	if !ok {
		return 0, nil, "", ErrTenantMissing
	}
	full, err := joinURL(u.baseURL, path)
	if err != nil {
		return 0, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: build upstream POST: %w", err)
	}
	if rawToken != "" {
		req.Header.Set("Authorization", "Bearer "+rawToken)
	}
	headerName, headerValue := tenant.TenantPropagationHeader(tenantID)
	req.Header.Set(headerName, headerValue)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("apigw: read upstream body: %w", err)
	}
	return resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// joinURL concatenates base + path while guaranteeing exactly one
// slash between them. net/url.JoinPath would work too but adds a
// dependency on Go 1.19+ url parsing edge cases we don't need;
// the explicit split is easier to audit.
func joinURL(base, path string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("apigw: parse base URL %q: %w", base, err)
	}
	if b.Scheme == "" || b.Host == "" {
		return "", fmt.Errorf("apigw: base URL %q missing scheme or host", base)
	}
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}
	return strings.TrimRight(b.String(), "/") + "/" + path, nil
}

// Sentinels for misuse. Surfaced verbatim to the caller; the
// caller (handleSites) treats them like any other transport error
// (i.e. maps them to RFC7807 502 upstream-unavailable).
var (
	errUpstreamNil       = fmt.Errorf("apigw: upstream is nil")
	errUpstreamEmptyPath = fmt.Errorf("apigw: upstream path is empty")
)

// ErrTenantMissing is returned by GetV1AsTenant when the verified
// claims do not carry a usable (id, tier) pair (PRMT-109 §5
// fail-closed). It is a caller-side signal — not a transport
// failure — and the handler maps it to 403 (RFC 7807
// "forbidden"), distinct from the 502 the transport sentinels
// trigger.
var ErrTenantMissing = fmt.Errorf("apigw: tenant or isolation tier missing/invalid")
