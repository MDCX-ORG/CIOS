// Tests for the GET /api/sites handler. PRMT-101 §5 requires
// table-driven coverage of the behaviour matrix:
//
//   - upstream 200 → 200 + verbatim body
//   - upstream 4xx → forward status + RFC7807
//   - upstream 5xx → 502 + RFC7807 "upstream-unavailable"
//   - transport error → 502 + RFC7807 "upstream-unavailable"
//   - non-GET → 405 + RFC7807 "bad-request"
//   - PRMT-105: verified identity is forwarded to /v1 as
//     Authorization: Bearer (spec-004 §6); different claims
//     produce different headers; the body is not modified.
package apigw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/sts"
)

// sitesBody is the shape core /v1/sites returns in M0/M1. The
// Gateway doesn't parse it — it just forwards bytes — but having
// a struct here makes the happy-path assertion stronger.
type sitesBody struct {
	Items []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"items"`
}

// sitesTestClaims is the verified identity every pre-existing
// /api/sites test injects into r.Context(). PRMT-105 requires
// handleSites to recover claims from ctx (per §5 "身份缺失 ...
// 不附 Authorization 且按 401 上游约定处理"); tests that previously
// relied on the pre-PRMT-105 pass-through now have to supply
// claims explicitly. The Subject is the only field GetV1As
// consumes (upstream.go), so a single fixed value per test is
// sufficient — the new identity-passthrough tests (TestHandleSites_*IdentityPassThrough)
// exercise scope variation.
//
// PRMT-109: handleSites now also dispatches on the resolved tenant
// isolation tier (PRMT-109 §2 / §5). The pre-existing happy-path
// fixtures MUST therefore carry Tenant + IsolationTier in their
// claims — otherwise the handler would 403 them out (fail-closed).
// sitesTestClaims pins a label-tier identity so the tests exercise
// the most common PRMT-109 code path (label injection + header
// attachment) without each test having to repeat the fields.
var sitesTestClaims = sts.TokenClaims{
	Subject:       "test-user@example.com",
	Realm:         "ops",
	Tenant:        "test-tenant",
	IsolationTier: "label",
}

// sitesCtx returns a request whose context carries sitesTestClaims.
// Pre-PRMT-105 tests used a bare httptest.NewRequest; this helper
// keeps the diff to one line per call site.
func sitesCtx() func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(WithClaims(r.Context(), sitesTestClaims))
	}
}

// sitesCtxWithRawToken returns a request whose context carries
// sitesTestClaims AND a fixed raw JWS bearer. PRMT-114 §2: the
// Authorization header reaching core /v1 is keyed off the raw
// JWS in ctx, not the bare claims.Subject, so tests that go
// through handleSites (and bypass AuthMiddleware's injection)
// must supply the raw token explicitly. AuthMiddleware in
// production populates both keys on a successful verify path.
func sitesCtxWithRawToken(raw string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		ctx := WithClaims(r.Context(), sitesTestClaims)
		ctx = WithRawToken(ctx, raw)
		return r.WithContext(ctx)
	}
}

// newSitesServer wires an httptest server that records a single
// call and responds with the supplied status + body. It returns
// the server (so the test can read .URL / .Client) and a pointer
// to a recorded-call struct.
func newSitesServer(t *testing.T, status int, body string, contentType string) (*httptest.Server, *struct {
	Method    string
	Path      string
	Calls     int
	AuthHdr   string
	TenantHdr string
}) {
	t.Helper()
	rec := &struct {
		Method    string
		Path      string
		Calls     int
		AuthHdr   string
		TenantHdr string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.AuthHdr = r.Header.Get("Authorization")
		rec.Calls++
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestHandleSites_HappyPath(t *testing.T) {
	const body = `{"items":[{"id":"site01","name":"Site 01"}]}`
	upstream, rec := newSitesServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if rec.Method != http.MethodGet {
		t.Errorf("upstream method = %s, want GET", rec.Method)
	}
	if rec.Path != "/v1/sites" {
		t.Errorf("upstream path = %s, want /v1/sites", rec.Path)
	}
	if !strings.Contains(w.Body.String(), "site01") {
		t.Errorf("body missing forwarded payload: %s", w.Body.String())
	}
	// Decode to prove the body is real JSON, not just a string
	// containing the literal. Belt-and-braces: PRMT-101 is
	// "透传 upstream GET /v1/sites", so we MUST NOT transform.
	var parsed sitesBody
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].ID != "site01" {
		t.Errorf("decoded items = %+v, want one site01", parsed.Items)
	}
}

func TestHandleSites_Upstream4xx_Forwarded(t *testing.T) {
	// Core /v1 emits RFC 7807 on every error path. The Gateway
	// must pass that through verbatim — the spec-004 contract is
	// the same on /v1 and /api/*.
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"site not found","status":404,"detail":"site01.pod009","instance":"/v1/sites/site01.pod009"}`
	upstream, rec := newSitesServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (must forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "path-not-found") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleSites_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newSitesServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "upstream-unavailable") {
		t.Errorf("type = %v, want tail upstream-unavailable", body["type"])
	}
	if int(body["status"].(float64)) != http.StatusBadGateway {
		t.Errorf("status field = %v, want 502", body["status"])
	}
}

func TestHandleSites_NetworkError_Becomes502(t *testing.T) {
	// Start a server and immediately close it so the URL points
	// at a port that nothing is listening on. NewUpstream binds
	// its *http.Client to upstream.Client() which has its own
	// transport — we use that Client so the dial failure
	// reproduces in the test.
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (transport failure)", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "upstream-unavailable") {
		t.Errorf("type = %v, want tail upstream-unavailable", body["type"])
	}
}

func TestHandleSites_MethodNotAllowed(t *testing.T) {
	upstream, rec := newSitesServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/sites", nil)
			r = sitesCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("method %s: status = %d, want 405", m, w.Code)
			}
			if allow := w.Header().Get("Allow"); allow != http.MethodGet {
				t.Errorf("method %s: Allow = %q, want GET", m, allow)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode problem body: %v", err)
			}
			if !strings.HasSuffix(body["type"].(string), "bad-request") {
				t.Errorf("method %s: type = %v, want tail bad-request", m, body["type"])
			}
		})
	}
	// Method gate must short-circuit BEFORE the upstream is
	// touched. If upstream.Calls>0 the gate is in the wrong
	// place and the test will start failing in CI.
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

// TestConfig_LoadConfig_Defaults: CIOS_APIGW_LISTEN unset →
// ":8443" default. CIOS_APIGW_UPSTREAM unset → error.
func TestConfig_LoadConfig_Defaults(t *testing.T) {
	t.Setenv("CIOS_APIGW_LISTEN", "")
	t.Setenv("CIOS_APIGW_UPSTREAM", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatalf("LoadConfig with no env: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "CIOS_APIGW_UPSTREAM") {
		t.Errorf("error %q does not name CIOS_APIGW_UPSTREAM", err.Error())
	}
}

// TestConfig_LoadConfig_FromEnv: explicit env values pass through.
func TestConfig_LoadConfig_FromEnv(t *testing.T) {
	t.Setenv("CIOS_APIGW_LISTEN", "127.0.0.1:9999")
	t.Setenv("CIOS_APIGW_UPSTREAM", "https://core.example.com:9443")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.UpstreamURL != "https://core.example.com:9443" {
		t.Errorf("UpstreamURL = %q", cfg.UpstreamURL)
	}
}

// TestUpstream_GetV1_Nil: GetV1 on a nil receiver returns an
// error rather than panicking. PRMT-101 §4 pins the contract:
// "网络错误 → (0, nil, err)". A nil receiver is misuse, not a
// transport failure, but treating it as (0, nil, err) keeps the
// caller's switch simple.
func TestUpstream_GetV1_Nil(t *testing.T) {
	var u *Upstream
	status, body, contentType, err := u.GetV1(nil, "/v1/sites")
	if err == nil {
		t.Fatalf("err = nil, want error")
	}
	if status != 0 || body != nil {
		t.Errorf("status=%d body=%v, want (0, nil)", status, body)
	}
	if contentType != "" {
		t.Errorf("contentType = %q, want \"\" on error path", contentType)
	}
}

// TestUpstream_GetV1_EmptyPath: same reasoning as nil — protect
// callers from a bad path argument blowing up.
func TestUpstream_GetV1_EmptyPath(t *testing.T) {
	u := NewUpstream("http://example.com", nil)
	status, body, contentType, err := u.GetV1(nil, "")
	if err == nil {
		t.Fatalf("err = nil, want error")
	}
	if status != 0 || body != nil {
		t.Errorf("status=%d body=%v, want (0, nil)", status, body)
	}
	if contentType != "" {
		t.Errorf("contentType = %q, want \"\" on error path", contentType)
	}
}

// TestUpstream_GetV1_OK: round-trip through a real httptest
// server — verifies the join + method + body plumbing of the
// Upstream client in isolation from the handler.
func TestUpstream_GetV1_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/sites" {
			t.Errorf("upstream path = %s, want /v1/sites", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL, srv.Client())
	status, body, contentType, err := u.GetV1(t.Context(), "/v1/sites")
	if err != nil {
		t.Fatalf("GetV1: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
	// PRMT-115 §4: GetV1 surfaces upstream Content-Type verbatim.
	// The mock didn't set it, but stdlib net/http auto-detects
	// "text/plain; charset=utf-8" for an ASCII body — we just
	// check that whatever upstream returns, we capture it (no
	// nil, no rewrite). The contract is "verbatim capture";
	// asserting on a specific value would couple the test to
	// stdlib internals.
	if contentType == "" {
		t.Errorf("contentType = \"\", want non-empty (stdlib auto-detected Content-Type should be surfaced verbatim)")
	}
}

// TestHandleSites_IdentityPassThrough_HeaderSet: the handler must
// forward the original JWS bearer to core /v1 as
// Authorization: Bearer <rawToken> (spec-004 §6 carrier). The
// mock upstream asserts the header is present and well-formed.
// PRMT-114 §2: bearer is the raw JWS, NOT the bare claims.Subject.
// PRMT-105 §5 / §7: "mock upstream 断言收到 Authorization 头".
func TestHandleSites_IdentityPassThrough_HeaderSet(t *testing.T) {
	const body = `{"items":[]}`
	const rawJWS = "raw-jws-handle-sites-headers"
	upstream, rec := newSitesServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtxWithRawToken(rawJWS)(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.Calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", rec.Calls)
	}
	got := rec.AuthHdr
	const want = "Bearer " + rawJWS
	if got != want {
		t.Errorf("Authorization header = %q, want %q (gateway MUST forward raw JWS, PRMT-114 §2)", got, want)
	}
}

// TestHandleSites_IdentityPassThrough_DifferentClaimsDifferentHeaders:
// Two distinct verified identities must produce two distinct
// Authorization headers at /v1 — the gateway does NOT inspect
// the claims to filter the body, so the only observable
// difference is the forwarded header. PRMT-114 §2: the bearer
// is keyed off the raw JWS in ctx, not the bare subject, so each
// subtest injects its own rawToken to mirror what AuthMiddleware
// would do on a successful verify.
func TestHandleSites_IdentityPassThrough_DifferentClaimsDifferentHeaders(t *testing.T) {
	const body = `{"items":[]}`
	upstream, rec := newSitesServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name    string
		subject string
		rawJWS  string
		scope   []string
	}{
		{
			name:    "scope_limited_to_site01",
			subject: "alice@example.com",
			rawJWS:  "raw-jws-alice",
			scope:   []string{"viewer", "site01.*"},
		},
		{
			name:    "scope_global",
			subject: "bob@example.com",
			rawJWS:  "raw-jws-bob",
			scope:   []string{"admin"},
		},
	}
	seen := make(map[string]string)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
			ctx := WithClaims(r.Context(), sts.TokenClaims{
				Subject:       tc.subject,
				Realm:         "ops",
				Scope:         tc.scope,
				Tenant:        "test-tenant",
				IsolationTier: "label",
			})
			ctx = WithRawToken(ctx, tc.rawJWS)
			r = r.WithContext(ctx)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if rec.Calls < 1 {
				t.Fatalf("upstream calls = %d, want >=1", rec.Calls)
			}
			want := "Bearer " + tc.rawJWS
			got := rec.AuthHdr
			if got != want {
				t.Errorf("Authorization = %q, want %q (raw JWS, not subject)", got, want)
			}
			if prev, dup := seen[got]; dup {
				t.Errorf("Authorization %q already seen for %q; gateway must NOT collapse distinct callers", got, prev)
			}
			seen[got] = tc.name
			// Body must NOT be transformed. Forwarded verbatim.
			if w.Body.String() != body {
				t.Errorf("body transformed: got %q, want %q", w.Body.String(), body)
			}
		})
	}
}

// TestHandleSites_NoClaims_Returns401: PRMT-105 §5 — identity
// missing MUST fail closed (401) rather than silently forwarding
// an anonymous request. In practice AuthMiddleware gates this
// path; the test exercises the handler's defensive branch by
// invoking handleSites directly with a context that lacks the
// claims key.
func TestHandleSites_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newSitesServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	// Bypass the full Handler (which would route through
	// AuthMiddleware) and call handleSites directly so the only
	// code path under test is the ClaimsFrom check.
	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	w := httptest.NewRecorder()
	srv.handleSites(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (PRMT-105 §5)", w.Code)
	}
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; handler MUST NOT call /v1 without verified identity", rec.Calls)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "unauthorized") {
		t.Errorf("type = %v, want tail unauthorized", body["type"])
	}
}

// TestUpstream_GetV1As_ForwardsAuthorization: exercise the
// Upstream.GetV1As method directly to confirm the Authorization
// header reaches the wire and the body is not altered. PRMT-114
// §2: the bearer MUST be the original JWS (rawToken), not the
// bare claims.Subject string — core /v1 needs a verifiable token.
func TestUpstream_GetV1As_ForwardsAuthorization(t *testing.T) {
	const rawJWS = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJjYXJvbEBleGFtcGxlLmNvbSJ9.signature"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+rawJWS {
			t.Errorf("Authorization = %q, want %q (raw JWS MUST reach upstream, PRMT-114 §2)", got, "Bearer "+rawJWS)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL, srv.Client())
	status, body, contentType, err := u.GetV1As(t.Context(), sts.TokenClaims{
		Subject: "carol@example.com",
		Realm:   "ops",
	}, rawJWS, "/v1/sites")
	if err != nil {
		t.Fatalf("GetV1As: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if string(body) != `{"items":[]}` {
		t.Errorf("body = %q", body)
	}
	// PRMT-115 §4: see TestUpstream_GetV1_OK — the upstream mock
	// doesn't set Content-Type but stdlib fills it in; we just
	// verify the value is captured (not nil, not rewritten).
	if contentType == "" {
		t.Errorf("contentType = \"\", want non-empty (upstream Content-Type must surface verbatim)")
	}
}

// TestUpstream_GetV1As_EmptyRawTokenOmitsHeader: defensive branch
// in GetV1As — when rawToken is empty we MUST NOT emit a
// malformed "Bearer " header (would 401 the upstream with an
// unauthenticatable token). PRMT-114 §2-bis: "无 rawToken（防御
// 路径）时不附 Authorization". This is the PRMT-114 successor of
// the PRMT-105 Subject-empty guard; the contract now keys off
// the rawToken arg instead of claims.Subject.
func TestUpstream_GetV1As_EmptyRawTokenOmitsHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty (empty rawToken MUST omit the header)", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL, srv.Client())
	if _, _, _, err := u.GetV1As(t.Context(), sts.TokenClaims{}, "", "/v1/sites"); err != nil {
		t.Fatalf("GetV1As: %v", err)
	}
}

// TestWithClaims_RoundTrip: the context helpers must round-trip
// the claims value with the presence boolean distinguishing
// "never set" from "set to zero value".
func TestWithClaims_RoundTrip(t *testing.T) {
	c := sts.TokenClaims{Subject: "x", Realm: "ops"}
	ctx := WithClaims(t.Context(), c)
	got, ok := ClaimsFrom(ctx)
	if !ok {
		t.Fatalf("ClaimsFrom: ok = false, want true")
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("ClaimsFrom = %+v, want %+v", got, c)
	}
	if _, ok := ClaimsFrom(t.Context()); ok {
		t.Errorf("ClaimsFrom on bare ctx: ok = true, want false")
	}
}

// TestWithRawToken_RoundTrip: PRMT-114 §4 — WithRawToken /
// RawTokenFrom must round-trip the original JWS string with the
// presence boolean distinguishing "never set" from "set to empty
// string" (defensive: empty rawToken omits the Authorization
// header at the upstream call site).
func TestWithRawToken_RoundTrip(t *testing.T) {
	const raw = "eyJhbGciOiJFUzI1NiJ9.payload.sig"
	ctx := WithRawToken(t.Context(), raw)
	got, ok := RawTokenFrom(ctx)
	if !ok {
		t.Fatalf("RawTokenFrom: ok = false, want true")
	}
	if got != raw {
		t.Errorf("RawTokenFrom = %q, want %q", got, raw)
	}
	if _, ok := RawTokenFrom(t.Context()); ok {
		t.Errorf("RawTokenFrom on bare ctx: ok = true, want false")
	}
	// Empty rawToken is a valid carrier value (defensive branch);
	// presence is reported as true so callers can distinguish it
	// from "never injected".
	emptyCtx := WithRawToken(t.Context(), "")
	if got, ok := RawTokenFrom(emptyCtx); !ok || got != "" {
		t.Errorf("RawTokenFrom(empty) = (%q, %v), want (\"\", true)", got, ok)
	}
}

// TestHandleSites_TenantPropagatesToUpstream: PRMT-109 §4 — for
// every tier (label / row / db), handleSites must attach the
// X-CIOS-Tenant propagation header to the upstream call so core
// can apply per-tenant row/db enforcement. PRMT-114 R2 — and
// must simultaneously forward the raw JWS bearer (rawToken) so
// that the chain-composition coverage (tier propagation × raw
// JWS passthrough) is not silently retracted when both PRMTs
// apply on the same request. The mock upstream records both
// headers so the test asserts on each directly.
func TestHandleSites_TenantPropagatesToUpstream(t *testing.T) {
	const body = `{"items":[]}`
	const rawJWS = "raw-jws-tenant"
	upstream, rec := newSitesServer(t, http.StatusOK, body, "application/json")
	// Extend the recorder to capture X-CIOS-Tenant without
	// modifying the shared helper signature (which other tests
	// rely on). We re-read it from the request via a separate
	// recorder struct.
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, tier := range []string{"label", "row", "db"} {
		t.Run(tier, func(t *testing.T) {
			// Capture the tenant header by replacing the mock
			// upstream's handler for the duration of this subtest.
			upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.AuthHdr = r.Header.Get("Authorization")
				rec.TenantHdr = r.Header.Get("X-CIOS-Tenant")
				rec.Calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})
			rec.AuthHdr = ""
			rec.TenantHdr = ""
			rec.Calls = 0

			r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
			// PRMT-114 R2: inject rawJWS via sitesCtxWithRawToken
			// (or equivalent WithClaims + WithRawToken pair) so the
			// Authorization header survives the R1 amendment and
			// reaches the upstream under every tier.
			ctx := WithClaims(r.Context(), sts.TokenClaims{
				Subject:       "u@example.com",
				Realm:         "ops",
				Tenant:        "acme",
				IsolationTier: tier,
			})
			ctx = WithRawToken(ctx, rawJWS)
			r = r.WithContext(ctx)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if rec.TenantHdr != "acme" {
				t.Errorf("X-CIOS-Tenant header = %q, want acme (PRMT-109 §4 must propagate)", rec.TenantHdr)
			}
			// PRMT-114 R2: per-tier rawJWS passthrough assertion.
			if rec.AuthHdr != "Bearer "+rawJWS {
				t.Errorf("Authorization header = %q, want %q (PRMT-114 R2 must forward rawJWS under tier %q)", rec.AuthHdr, "Bearer "+rawJWS, tier)
			}
		})
	}
}

// TestHandleSites_MissingTenantReturns403: PRMT-109 §5 — when
// the verified claims do not carry a usable (tenant, tier) pair,
// handleSites must fail closed with 403, never silently forwarding
// a tenant-less upstream call.
func TestHandleSites_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newSitesServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name   string
		claims sts.TokenClaims
	}{
		{"no_tenant_no_tier", sts.TokenClaims{Subject: "u@example.com", Realm: "ops"}},
		{"tier_only", sts.TokenClaims{Subject: "u@example.com", Realm: "ops", IsolationTier: "label"}},
		{"tenant_only", sts.TokenClaims{Subject: "u@example.com", Realm: "ops", Tenant: "acme"}},
		{"invalid_tier", sts.TokenClaims{Subject: "u@example.com", Realm: "ops", Tenant: "acme", IsolationTier: "database"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleSites(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (PRMT-109 §5 fail-closed)", w.Code)
			}
			if rec.Calls != 0 {
				t.Errorf("upstream calls = %d; handler MUST NOT call /v1 without a tenant identity", rec.Calls)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode problem body: %v", err)
			}
			if !strings.HasSuffix(body["type"].(string), "forbidden") {
				t.Errorf("type = %v, want tail forbidden", body["type"])
			}
		})
	}
}

// TestHandleSites_UpstreamContentTypePassthrough_NonJSON2xx:
// PRMT-115 §4 — the gateway MUST mirror the upstream's Content-Type
// verbatim. If core /v1 returns a 2xx with text/csv (or any other
// non-JSON type), the gateway MUST NOT rewrite the response
// Content-Type to application/json — that would corrupt the body
// shape. The fix is in handleSites: it now uses the contentType
// returned by GetV1AsTenant, only falling back to a status-based
// default when the upstream omitted the header.
func TestHandleSites_UpstreamContentTypePassthrough_NonJSON2xx(t *testing.T) {
	const csvBody = "id,name\nsite01,Site 01\nsite02,Site 02\n"
	upstream, _ := newSitesServer(t, http.StatusOK, csvBody, "text/csv; charset=utf-8")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv passthrough (PRMT-115 §2)", got)
	}
	if w.Body.String() != csvBody {
		t.Errorf("body = %q, want %q (verbatim passthrough)", w.Body.String(), csvBody)
	}
}

// TestHandleSites_UpstreamContentTypePassthrough_ProblemJSON4xx:
// PRMT-115 §2-bis — an upstream 4xx with application/problem+json
// (the spec-004 §4 error shape) MUST be forwarded unchanged. The
// header is now picked from the upstream response rather than
// synthesised by the handler.
func TestHandleSites_UpstreamContentTypePassthrough_ProblemJSON4xx(t *testing.T) {
	const upstreamBody = `{"type":"https://example.com/errors/foo","title":"foo","status":409,"detail":"conflict","instance":"/v1/sites"}`
	upstream, _ := newSitesServer(t, http.StatusConflict, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r = sitesCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (forwarded)", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	if !strings.Contains(w.Body.String(), "example.com/errors/foo") {
		t.Errorf("body did not forward upstream problem+json: %s", w.Body.String())
	}
}

// TestHandleSites_NoUpstreamContentType_FallsBackToDefault:
// PRMT-115 §4 — when the upstream omits Content-Type, the handler
// must fall back to the status-based default (problem+json for 4xx,
// application/json for 2xx) — preserving pre-PRMT-115 behaviour
// for upstreams that don't bother setting the header. This
// guarantees no regression on the existing happy-path / 4xx
// fixtures.
//
// In practice stdlib net/http auto-detects Content-Type when the
// handler omits it (via DetectContentType on the body bytes), so
// httptest.NewServer responses always carry some Content-Type. To
// exercise the true "no upstream Content-Type" branch we use a
// custom http.RoundTripper that strips the Content-Type header
// from the response before GetV1AsTenant sees it.
func TestHandleSites_NoUpstreamContentType_FallsBackToDefault(t *testing.T) {
	stripCT := func(orig *http.Client) *http.Client {
		c := *orig
		c.Transport = &stripContentTypeTransport{base: c.Transport}
		return &c
	}
	newUpstream := func(t *testing.T, status int, body string) (*httptest.Server, *Upstream) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Don't set Content-Type here — rely on the strip
			// transport to also remove anything net/http fills in.
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv, NewUpstream(srv.URL, stripCT(srv.Client()))
	}

	// 2xx without Content-Type → default application/json.
	t.Run("2xx_default_json", func(t *testing.T) {
		_, up := newUpstream(t, http.StatusOK, `{"items":[]}`)
		srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: "http://placeholder"}, up)
		r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
		r = sitesCtx()(r)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json default", got)
		}
	})

	// 4xx without Content-Type → default application/problem+json.
	t.Run("4xx_default_problem_json", func(t *testing.T) {
		const body = `{"type":"https://x/y","title":"y","status":404,"detail":"d","instance":"/v1/sites"}`
		_, up := newUpstream(t, http.StatusNotFound, body)
		srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: "http://placeholder"}, up)
		r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
		r = sitesCtx()(r)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
			t.Errorf("Content-Type = %q, want application/problem+json default", got)
		}
	})
}

// stripContentTypeTransport wraps an http.RoundTripper and removes
// the Content-Type header from the response so callers see a
// "no upstream Content-Type" scenario (stdlib net/http otherwise
// auto-detects one via DetectContentType).
type stripContentTypeTransport struct {
	base http.RoundTripper
}

func (t *stripContentTypeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Header.Del("Content-Type")
	return resp, nil
}

// TestUpstream_GetV1_ContentTypeCaptured: PRMT-115 §4 — when the
// upstream sets Content-Type, GetV1 surfaces it verbatim so the
// handler can mirror it. Includes charset parameters — the
// verbatim header value (sans normalisation) is what callers
// receive.
func TestUpstream_GetV1_ContentTypeCaptured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "a,b\n1,2\n")
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL, srv.Client())
	status, body, contentType, err := u.GetV1(t.Context(), "/v1/sites")
	if err != nil {
		t.Fatalf("GetV1: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if contentType != "text/csv; charset=utf-8" {
		t.Errorf("contentType = %q, want text/csv; charset=utf-8", contentType)
	}
	if string(body) != "a,b\n1,2\n" {
		t.Errorf("body = %q", body)
	}
}
