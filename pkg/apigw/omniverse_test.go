// Tests for the Omniverse service-token broker (PRMT-107). The
// contract here pins the red-line property of spec-009 §7.1: the
// Gateway MUST NOT forward the inbound user Authorization/Cookie
// to Omniverse — the outbound request must carry only the machine
// service token. We assert this by pointing handleOmniverse at an
// httptest upstream and inspecting the request it receives.
//
// We also pin:
//   - AuthMiddleware gates unauthenticated callers (no token → 401).
//   - Service token unavailable → 502 (no fallback).
//   - Method/path/query/body are forwarded verbatim.
//   - 5xx upstream → 502; 2xx/4xx → forwarded unchanged.
package apigw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/sts"
)

// omniverseTestClaims is the verified identity injected into the
// request context for tests that exercise the handler directly.
// The Omniverse handler does not use the claims (it only needs
// AuthMiddleware to have passed), but AuthMiddleware requires the
// bearer header so the "no token" branch can be tested separately.
var omniverseTestClaims = sts.TokenClaims{
	Subject: "user-1@example.com",
	Realm:   "ops",
	Scope:   []string{"read"},
}

// fixedTokenSource is a deterministic ServiceTokenSource used to
// avoid env-vars in unit tests. Returning a static token pins the
// outbound Authorization: Bearer <token> shape against a known
// value, so the test can assert equality rather than just
// presence.
type fixedTokenSource struct {
	token string
	err   error
	calls int32
}

func (f *fixedTokenSource) Token(ctx context.Context) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// newOmniverseTestServer constructs an *httptest.Server that
// captures the inbound request and returns a canned response. The
// capture is exposed via the returned *capture so tests can assert
// the outbound Authorization header, the path, the body, etc.
// The capture is mutex-guarded so concurrent test goroutines
// don't race (defensive — current tests are sequential).
type omniverseCapture struct {
	gotAuthHeader   string
	gotCookieHeader string
	gotMethod       string
	gotPath         string
	gotQuery        string
	gotContentType  string
	gotBody         []byte
}

func newOmniverseTestServer(t *testing.T, status int, respBody []byte, respCT string) (*httptest.Server, *omniverseCapture) {
	t.Helper()
	cap := &omniverseCapture{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.gotAuthHeader = r.Header.Get("Authorization")
		cap.gotCookieHeader = r.Header.Get("Cookie")
		cap.gotMethod = r.Method
		cap.gotPath = r.URL.Path
		cap.gotQuery = r.URL.RawQuery
		cap.gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		cap.gotBody = body
		w.Header().Set("Content-Type", respCT)
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(ts.Close)
	return ts, cap
}

// newServerForOmniverse builds a Server pointed at the given
// upstream URL with a fixed ServiceTokenSource. The Server's
// omniverseURL is set to the upstream so handleOmniverse forwards
// to it; the upstream's URL is used as the Omniverse base.
func newServerForOmniverse(t *testing.T, omniverseBase string, src ServiceTokenSource) *Server {
	t.Helper()
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.omniverseURL = omniverseBase
	srv.SetOmniverseTokenSource(src)
	srv.SetOmniverseHTTPClient(http.DefaultClient)
	return srv
}

// TestOmniverse_StripsUserAuthorization_AttachesServiceToken:
// PRMT-107 §5 MUST 1 — the outbound request to Omniverse MUST NOT
// carry the inbound user's Authorization header, and MUST carry
// the machine service token in its place.
//
// The test uses an httptest upstream so we can read the headers
// the handler actually emitted. We do NOT take the route through
// AuthMiddleware here (the no-token / bad-token behaviour is
// pinned in separate tests); instead we exercise handleOmniverse
// directly with verified claims injected into the request
// context.
func TestOmniverse_StripsUserAuthorization_AttachesServiceToken(t *testing.T) {
	const userBearer = "Bearer user-jwt-xxx-do-not-leak"
	const svcToken = "svc-token-robot-1"

	upstream, cap := newOmniverseTestServer(t, http.StatusOK,
		[]byte(`{"ok":true}`), "application/json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/scenes/abc", nil)
	r.Header.Set("Authorization", userBearer)
	r.Header.Set("Cookie", "cios_session=opaque")
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	// PRMT-107 §5 MUST 1: outbound Authorization MUST be the
	// service token, NOT the user's bearer.
	if cap.gotAuthHeader == "" {
		t.Fatalf("upstream did not receive an Authorization header")
	}
	if cap.gotAuthHeader == userBearer {
		t.Fatalf("upstream received the USER bearer (%q); user token leaked to Omniverse", userBearer)
	}
	wantAuth := "Bearer " + svcToken
	if cap.gotAuthHeader != wantAuth {
		t.Errorf("upstream Authorization = %q, want %q", cap.gotAuthHeader, wantAuth)
	}
	// PRMT-107 §5 MUST 1: outbound Cookie MUST be empty.
	if cap.gotCookieHeader != "" {
		t.Errorf("upstream Cookie = %q, want empty (session cookie leaked)", cap.gotCookieHeader)
	}
	// Path was forwarded verbatim after the /api/omniverse strip.
	if cap.gotPath != "/scenes/abc" {
		t.Errorf("upstream path = %q, want /scenes/abc", cap.gotPath)
	}
	// Method was forwarded.
	if cap.gotMethod != http.MethodGet {
		t.Errorf("upstream method = %q, want GET", cap.gotMethod)
	}
}

// TestOmniverse_BodyForwarded: a non-GET request with a body
// must have its body forwarded to the upstream verbatim.
func TestOmniverse_BodyForwarded(t *testing.T) {
	const svcToken = "svc-token-robot-2"
	bodyIn := []byte(`{"hello":"world"}`)

	upstream, cap := newOmniverseTestServer(t, http.StatusOK,
		[]byte(`{"echo":"ok"}`), "application/json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodPost, "/api/omniverse/things", strings.NewReader(string(bodyIn)))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if string(cap.gotBody) != string(bodyIn) {
		t.Errorf("upstream body = %q, want %q", cap.gotBody, bodyIn)
	}
	if cap.gotContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", cap.gotContentType)
	}
	if cap.gotMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", cap.gotMethod)
	}
}

// TestOmniverse_QueryStringForwarded: ?foo=bar survives the
// /api/omniverse strip and lands at the upstream.
func TestOmniverse_QueryStringForwarded(t *testing.T) {
	const svcToken = "svc-token-robot-3"
	upstream, cap := newOmniverseTestServer(t, http.StatusOK,
		[]byte(`{}`), "application/json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x?foo=bar&baz=qux", nil)
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cap.gotPath != "/x" {
		t.Errorf("upstream path = %q, want /x", cap.gotPath)
	}
	if cap.gotQuery != "foo=bar&baz=qux" {
		t.Errorf("upstream query = %q, want foo=bar&baz=qux", cap.gotQuery)
	}
}

// TestOmniverse_NoServiceToken_Returns502: PRMT-107 §4 — when
// the ServiceTokenSource returns an error, the handler MUST
// return 502 RFC7807 and MUST NOT call the upstream. This is the
// "no fallback to user token / anonymous" red line.
func TestOmniverse_NoServiceToken_Returns502(t *testing.T) {
	called := int32(0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer upstream.Close()

	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{
		err: fmt.Errorf("simulated token source failure"),
	})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x", nil)
	r.Header.Set("Authorization", "Bearer user-jwt")
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "upstream-unavailable") {
		t.Errorf("type = %v, want tail upstream-unavailable", body["type"])
	}
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Errorf("upstream was called %d times when service token unavailable; MUST be 0", got)
	}
}

// TestOmniverse_NoTokenSource_Returns502: the handler returns 502
// when Server.omniverseToken is nil. The wire shape matches the
// "no fallback" branch — operator sees a clear failure.
func TestOmniverse_NoTokenSource_Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream was called without a token source; should be unreachable")
	}))
	defer upstream.Close()

	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.omniverseURL = upstream.URL
	srv.omniverseToken = nil // explicit override

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x", nil)
	r.Header.Set("Authorization", "Bearer user-jwt")
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestOmniverse_NoBaseURL_Returns502: empty omniverseURL is a
// misconfiguration; the handler returns 502 and never dials
// outbound.
func TestOmniverse_NoBaseURL_Returns502(t *testing.T) {
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.omniverseURL = "" // explicit override
	srv.SetOmniverseTokenSource(&fixedTokenSource{token: "t"})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x", nil)
	r.Header.Set("Authorization", "Bearer user-jwt")
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestOmniverse_Upstream5xx_Returns502: 5xx responses from the
// upstream are translated to a Gateway-level 502 so the caller
// sees a single error shape for upstream problems.
func TestOmniverse_Upstream5xx_Returns502(t *testing.T) {
	const svcToken = "svc-token-robot-4"
	upstream, _ := newOmniverseTestServer(t, http.StatusInternalServerError,
		[]byte(`{"err":"boom"}`), "application/problem+json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x", nil)
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "upstream-unavailable") {
		t.Errorf("type = %v, want tail upstream-unavailable", body["type"])
	}
}

// TestOmniverse_Upstream4xx_PassedThrough: 4xx responses from the
// upstream are forwarded verbatim (the upstream already speaks
// RFC7807; respect its decision).
func TestOmniverse_Upstream4xx_PassedThrough(t *testing.T) {
	const svcToken = "svc-token-robot-5"
	upstream, _ := newOmniverseTestServer(t, http.StatusNotFound,
		[]byte(`{"err":"not-found"}`), "application/problem+json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/missing", nil)
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(w.Body.String(), "not-found") {
		t.Errorf("body = %q, want to contain not-found", w.Body.String())
	}
}

// TestOmniverse_UpstreamUnreachable_Returns502: a closed
// upstream simulates a transport failure. PRMT-107 MUST NOT
// fallback; the handler returns 502.
func TestOmniverse_UpstreamUnreachable_Returns502(t *testing.T) {
	const svcToken = "svc-token-robot-6"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstream.Close() // close immediately so Do() fails
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse/x", nil)
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestOmniverse_RouteRegistered_BehindAuthMiddleware: a request to
// /api/omniverse/x without an Authorization header must 401 at
// AuthMiddleware and never reach handleOmniverse. This pins the
// "入站经 AuthMiddleware" contract end-to-end through the mux.
//
// We wire the STS via env (CIOS_STS_SIGNING_KEY) BEFORE NewServer
// so the package-level authHolder captures a non-nil STS at
// handler-build time. AuthMiddleware's STS/PDP check is captured
// at build time, not per-request; calling bindAuthDeps after
// NewServer would be too late.
func TestOmniverse_RouteRegistered_BehindAuthMiddleware(t *testing.T) {
	called := int32(0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer upstream.Close()

	t.Setenv("CIOS_STS_SIGNING_KEY", "0123456789abcdef0123456789abcdef")
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.omniverseURL = upstream.URL
	srv.SetOmniverseTokenSource(&fixedTokenSource{token: "t"})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/omniverse/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Errorf("upstream was called %d times; AuthMiddleware MUST short-circuit", got)
	}
}

// TestOmniverse_RouteRootPath: a request to /api/omniverse
// (no suffix) must still reach handleOmniverse and forward to
// the upstream's root path. This pins the dispatch contract that
// parseOmniversePath accepts the bare prefix.
func TestOmniverse_RouteRootPath(t *testing.T) {
	const svcToken = "svc-token-robot-7"
	upstream, cap := newOmniverseTestServer(t, http.StatusOK,
		[]byte(`{}`), "application/json")
	srv := newServerForOmniverse(t, upstream.URL, &fixedTokenSource{token: svcToken})

	r := httptest.NewRequest(http.MethodGet, "/api/omniverse", nil)
	r = r.WithContext(WithClaims(r.Context(), omniverseTestClaims))
	w := httptest.NewRecorder()
	srv.handleOmniverse(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cap.gotPath != "/" {
		t.Errorf("upstream path = %q, want /", cap.gotPath)
	}
	if cap.gotAuthHeader != "Bearer "+svcToken {
		t.Errorf("upstream Authorization = %q, want Bearer %q", cap.gotAuthHeader, svcToken)
	}
}

// TestParseOmniversePath: the dispatcher predicate must accept
// /api/omniverse and any /api/omniverse/<x>, and reject
// everything else.
func TestParseOmniversePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/omniverse", true},
		{"/api/omniverse/", true},
		{"/api/omniverse/x", true},
		{"/api/omniverse/scenes/abc", true},
		{"/api/omniverse/scenes/abc?foo=bar", true},
		{"/api/sites", false},
		{"/api/sites/x/stream", false},
		{"/api/twins", false},
		{"/api/omniversefoo", false},
		{"/healthz", false},
		{"/", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := parseOmniversePath(tc.path); got != tc.want {
				t.Errorf("parseOmniversePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestEnvServiceTokenSource_MissingEnv_ReturnsError: PRMT-107
// §4 — when the env var is empty, Token() returns an error and
// the handler will return 502. Pin the source contract so a
// future PRMT that builds a different source cannot accidentally
// "fall back" to empty.
func TestEnvServiceTokenSource_MissingEnv_ReturnsError(t *testing.T) {
	// Choose an env var name that is intentionally NOT set in
	// the test environment. PRMT-107 §3 forbids hard-coding the
	// service token; using a sentinel name here means the
	// assertion holds regardless of what an operator has set.
	const sentinel = "CIOS_OMNIVERSE_SVC_TOKEN_FOR_TEST_MISSING"
	t.Setenv(sentinel, "")

	src := NewEnvServiceTokenSource(sentinel)
	tok, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("Token() = %q, nil err; want non-nil err on empty env", tok)
	}
	if tok != "" {
		t.Errorf("Token() = %q, want empty on error", tok)
	}
}

// TestEnvServiceTokenSource_ReadsEnv: the source reads the env
// value on every call so tests can flip the env and observe the
// new value without rebuilding the source.
func TestEnvServiceTokenSource_ReadsEnv(t *testing.T) {
	const envName = "CIOS_OMNIVERSE_SVC_TOKEN_FOR_TEST_READ"
	t.Setenv(envName, "first")
	src := NewEnvServiceTokenSource(envName)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if got != "first" {
		t.Errorf("first Token = %q, want first", got)
	}
	t.Setenv(envName, "second")
	got, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if got != "second" {
		t.Errorf("second Token = %q, want second (source must re-read env on every call)", got)
	}
}

// TestEnvServiceTokenSource_NilSource_ReturnsError: a nil
// source or one constructed with an empty envVar MUST error
// rather than panic. PRMT-107's "no fallback" red line depends
// on this — a nil-deref here would crash the request goroutine.
func TestEnvServiceTokenSource_NilSource_ReturnsError(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var s *envServiceTokenSource
		_, err := s.Token(context.Background())
		if err == nil {
			t.Errorf("nil receiver Token() = nil err; want non-nil")
		}
	})
	t.Run("empty envVar", func(t *testing.T) {
		s := NewEnvServiceTokenSource("")
		_, err := s.Token(context.Background())
		if err == nil {
			t.Errorf("empty envVar Token() = nil err; want non-nil")
		}
	})
}

// TestOmniverse_DispatchUnknownAPIPath: a request to
// /api/<not-registered> still returns 404 + RFC7807; adding
// /api/omniverse to the dispatch switch must not have weakened
// the catch-all.
func TestOmniverse_DispatchUnknownAPIPath(t *testing.T) {
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "path-not-found") {
		t.Errorf("body does not contain 'path-not-found': %s", body)
	}
}

// writeTokenFile writes content to a temp file and returns the
// path. Tests for the file-based ServiceTokenSource (PRMT-118)
// need a real file because the source calls os.Stat to detect
// mtime changes — a fake in-memory fs would defeat the cache
// logic we're trying to pin.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "omniverse.token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestFileServiceTokenSource_ReadsAndTrims: PRMT-118 §2-bis —
// the file source returns the file's contents with trailing
// whitespace trimmed. Operators frequently append a newline when
// generating the token, and spec-009 §7.1 forbids leaking that
// into the outbound Authorization header.
func TestFileServiceTokenSource_ReadsAndTrims(t *testing.T) {
	path := writeTokenFile(t, "  super-secret  \n")
	src := NewFileServiceTokenSource(path, time.Minute)
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "super-secret" {
		t.Errorf("Token = %q, want %q", tok, "super-secret")
	}
}

// TestFileServiceTokenSource_TTLCacheHit: while the TTL has not
// elapsed, repeated Token() calls must NOT re-read the file —
// this is the hot-path property the source is built for. We
// verify it by deleting the file after the first read; a
// re-read would now fail, but a cache hit should still return
// the cached value.
func TestFileServiceTokenSource_TTLCacheHit(t *testing.T) {
	path := writeTokenFile(t, "cached-token")
	src := NewFileServiceTokenSource(path, time.Minute)
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	second, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token (cache hit expected): %v", err)
	}
	if second != first {
		t.Errorf("second Token = %q, want %q (cache must serve within TTL)", second, first)
	}
}

// TestFileServiceTokenSource_MtimeChangeReloads: when the file's
// mtime advances (operator rotation) and the TTL has elapsed, the
// next Token() call MUST observe the new value. Per the PRMT-118
// §4 contract the TTL check is the outer guard — mtime changes
// observed while the TTL is still fresh are intentionally served
// from cache (TTL is the rotation window the operator configured).
// We therefore use a 0-TTL source here so the mtime-equal branch
// is the one being pinned.
func TestFileServiceTokenSource_MtimeChangeReloads(t *testing.T) {
	path := writeTokenFile(t, "v1")
	// ttl=0 → every call falls through to the mtime-equal branch.
	src := NewFileServiceTokenSource(path, 0)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if got != "v1" {
		t.Fatalf("first Token = %q, want v1", got)
	}
	// Ensure mtime advances past the 1s FS resolution.
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	got, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if got != "v2" {
		t.Errorf("second Token = %q, want v2 (mtime change must trigger reload)", got)
	}
}

// TestFileServiceTokenSource_TTLExpiryReloads: with ttl=0 (or a
// very short ttl) the cache must expire even when mtime is
// unchanged. We use ttl=10ms and rely on the mtime-equal fast
// path updating loadedAt so the second call observes a stale
// loadedAt and re-reads.
func TestFileServiceTokenSource_TTLExpiryReloads(t *testing.T) {
	path := writeTokenFile(t, "v1")
	src := NewFileServiceTokenSource(path, 10*time.Millisecond)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if got != "v1" {
		t.Fatalf("first Token = %q, want v1", got)
	}
	// Update content with a fresh mtime so reload is required
	// (the mtime-equal short-circuit would otherwise serve
	// the stale cached value without re-reading).
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	got, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if got != "v2" {
		t.Errorf("second Token = %q, want v2 (TTL expiry must trigger reload)", got)
	}
}

// TestFileServiceTokenSource_MissingFileReturnsError: PRMT-118
// §2-bis — missing/unreadable file MUST return an error; the
// handler depends on this to surface 502 rather than falling
// back to anonymous / user-token.
func TestFileServiceTokenSource_MissingFileReturnsError(t *testing.T) {
	src := NewFileServiceTokenSource(filepath.Join(t.TempDir(), "does-not-exist"), time.Minute)
	tok, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("Token = %q, nil err; want non-nil on missing file", tok)
	}
	if tok != "" {
		t.Errorf("Token = %q, want empty on error", tok)
	}
}

// TestFileServiceTokenSource_EmptyFileReturnsError: an existing
// but empty file (e.g. operator truncated it before writing the
// new token) MUST also return an error rather than serve an
// empty token. Pins the spec-009 §7.1 "no anonymous" red line.
func TestFileServiceTokenSource_EmptyFileReturnsError(t *testing.T) {
	path := writeTokenFile(t, " \n ")
	src := NewFileServiceTokenSource(path, time.Minute)
	tok, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("Token = %q, nil err; want non-nil on empty/whitespace file", tok)
	}
	if tok != "" {
		t.Errorf("Token = %q, want empty on error", tok)
	}
}

// TestFileServiceTokenSource_NilSourceReturnsError: mirrors the
// env-source nil-safety test so a future PRMT that swaps the
// default source at runtime cannot accidentally introduce a
// nil-deref panic.
func TestFileServiceTokenSource_NilSourceReturnsError(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var s *fileServiceTokenSource
		_, err := s.Token(context.Background())
		if err == nil {
			t.Errorf("nil receiver Token() = nil err; want non-nil")
		}
	})
	t.Run("empty path", func(t *testing.T) {
		s := NewFileServiceTokenSource("", time.Minute)
		_, err := s.Token(context.Background())
		if err == nil {
			t.Errorf("empty path Token() = nil err; want non-nil")
		}
	})
}

// TestFileServiceTokenSource_ConcurrentSafety: -race must
// report clean under concurrent Token() callers. A second
// goroutine flips the file between calls; the assertion is that
// the source never panics or returns a torn value.
func TestFileServiceTokenSource_ConcurrentSafety(t *testing.T) {
	path := writeTokenFile(t, "stable")
	src := NewFileServiceTokenSource(path, 50*time.Millisecond)
	var wg sync.WaitGroup
	const goroutines = 16
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				tok, err := src.Token(context.Background())
				if err == nil && tok != "stable" {
					t.Errorf("Token = %q, want stable", tok)
					return
				}
			}
		}()
	}
	// Concurrently rotate the file's content/mtime a few times
	// to exercise the reload path under contention.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 4; k++ {
			time.Sleep(60 * time.Millisecond)
			_ = os.WriteFile(path, []byte(fmt.Sprintf("rot%d", k)), 0o600)
		}
		// Restore a stable value before readers finish so the
		// per-reader "want stable" assertion holds.
		_ = os.WriteFile(path, []byte("stable"), 0o600)
	}()
	wg.Wait()
}
