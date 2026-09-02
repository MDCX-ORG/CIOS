// Tests for the three PRMT-141 read routes:
//   - GET /api/assets         → /v1/assets
//   - GET /api/alarms         → /v1/alarms
//   - GET /api/metrics/query  → /v1/metrics/query?<rawQuery>
//
// plus the PRMT-151 fourth read route:
//   - GET /api/topology       → /v1/topology
//
// plus the four PRMT-153 E3.5 ops-portal read routes:
//   - GET /api/tickets            → /v1/tickets?<rawQuery>
//   - GET /api/tickets/{id}       → /v1/tickets/{id}
//   - GET /api/capacity           → /v1/capacity?<rawQuery>
//   - GET /api/capacity/metrics   → /v1/capacity/metrics?<rawQuery>
//
// plus the seven PRMT-154 E3.5 ops-portal read routes:
//   - GET /api/maintenance/upcoming   → /v1/maintenance/upcoming?<rawQuery>
//   - GET /api/pm/schedules           → /v1/pm/schedules?<rawQuery>
//   - GET /api/pm/schedules/{id}      → /v1/pm/schedules/{id}
//   - GET /api/spares                 → /v1/spares?<rawQuery>
//   - GET /api/spares/{id}            → /v1/spares/{id}
//   - GET /api/inspections            → /v1/inspections?<rawQuery>
//   - GET /api/inspections/{id}       → /v1/inspections/{id}
//
// plus the four PRMT-155 E3.5 ops-portal read routes:
//   - GET /api/runbooks/{key}       → /v1/runbooks/{key}
//   - GET /api/cases                → /v1/cases?<rawQuery>
//   - GET /api/reports/ops          → /v1/reports/ops?<rawQuery>
//   - GET /api/reports/reconcile    → /v1/reports/reconcile?<rawQuery>
//
// Coverage mirrors the sites_test.go pattern (PRMT-101 §5 +
// PRMT-141 §5 MUST 6):
//   - upstream 200 → 200 + verbatim body
//   - upstream 4xx → forward status + RFC 7807
//   - upstream 5xx → 502 + RFC 7807 "upstream-unavailable"
//   - transport error → 502 + RFC 7807 "upstream-unavailable"
//   - non-GET → 405 + RFC 7807 "bad-request" (POST/PUT/DELETE/PATCH)
//   - PRMT-105: verified identity forwarded as Bearer (no assertions
//     on body transform — these are byte-for-byte passthroughs)
//   - PRMT-109: missing tenant → 403 (fail-closed)
//   - PRMT-141 §4: /api/metrics/query forwards r.URL.RawQuery
//     byte-for-byte to the upstream stub.
package apigw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/sts"
)

// readsTestClaims is the verified identity every PRMT-141 test
// injects into r.Context(). PRMT-109 requires the (tenant, tier)
// pair to be present so the handler does not 403 before reaching
// the upstream — same shape as sitesTestClaims.
var readsTestClaims = sts.TokenClaims{
	Subject:       "test-user@example.com",
	Realm:         "ops",
	Tenant:        "test-tenant",
	IsolationTier: "label",
}

// readsCtx returns a request whose context carries readsTestClaims.
func readsCtx() func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(WithClaims(r.Context(), readsTestClaims))
	}
}

// readsRec captures the inbound request fields the PRMT-141
// tests assert on. The /api/metrics/query test in particular
// needs the raw query string that arrived at the upstream stub.
type readsRec struct {
	Method  string
	Path    string
	RawPath string // r.URL.RequestURI() — includes "?<query>"
	AuthHdr string
	Calls   int
}

// newReadsServer wires an httptest server that records a single
// call and responds with the supplied status + body. Returns the
// server (for URL / Client) and the recorder.
func newReadsServer(t *testing.T, status int, body string, contentType string) (*httptest.Server, *readsRec) {
	t.Helper()
	rec := &readsRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.RawPath = r.URL.RequestURI()
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

// ---- /api/assets ---------------------------------------------------------

func TestHandleAssets_HappyPath(t *testing.T) {
	const body = `[{"id":"a1","name":"Asset 1"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/assets" {
		t.Errorf("upstream path = %s, want /v1/assets", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleAssets_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"not found","status":404,"detail":"x","instance":"/v1/assets"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
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

func TestHandleAssets_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	r = readsCtx()(r)
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

func TestHandleAssets_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	r = readsCtx()(r)
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

func TestHandleAssets_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/assets", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleAssets_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	// Bypass the full Handler (which would route through
	// AuthMiddleware) and call handleAssets directly so the only
	// code path under test is the ClaimsFrom check.
	r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	w := httptest.NewRecorder()
	srv.handleAssets(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleAssets_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleAssets(w, r)

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

// ---- /api/alarms ---------------------------------------------------------

func TestHandleAlarms_HappyPath(t *testing.T) {
	const body = `[{"id":"alm1","severity":"warn"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/alarms" {
		t.Errorf("upstream path = %s, want /v1/alarms", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleAlarms_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/alarms"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleAlarms_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	r = readsCtx()(r)
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

func TestHandleAlarms_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	r = readsCtx()(r)
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

func TestHandleAlarms_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/alarms", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleAlarms_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	w := httptest.NewRecorder()
	srv.handleAlarms(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleAlarms_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleAlarms(w, r)

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

// ---- /api/metrics/query --------------------------------------------------

func TestHandleMetricsQuery_HappyPath(t *testing.T) {
	const body = `{"resultType":"matrix","result":[]}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=up", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/metrics/query" {
		t.Errorf("upstream path = %s, want /v1/metrics/query", rec.Path)
	}
	if rec.RawPath != "/v1/metrics/query?query=up" {
		t.Errorf("upstream RequestURI = %q, want /v1/metrics/query?query=up (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

// PRMT-228: /api/metrics/query_range must dispatch (not 404) and
// forward to core /v1/metrics/query_range with RawQuery intact.
func TestHandleMetricsQueryRange_HappyPath(t *testing.T) {
	const body = `{"resultType":"matrix","result":[]}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query_range?query=up&step=15s", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if rec.Path != "/v1/metrics/query_range" {
		t.Errorf("upstream path = %s, want /v1/metrics/query_range", rec.Path)
	}
	if rec.RawPath != "/v1/metrics/query_range?query=up&step=15s" {
		t.Errorf("upstream RequestURI = %q, want /v1/metrics/query_range?query=up&step=15s", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

// TestHandleMetricsQuery_ForwardsRawQuery exercises the PRMT-141
// §4 "forward r.URL.RawQuery verbatim" contract under three
// shapes: a single PromQL key, multiple keys, and a percent-
// encoded PromQL expression (label tier injects the tenant label
// at upstream via pkg/tenant.InjectTenantLabel — but the handler
// itself must not mangle the inbound bytes).
func TestHandleMetricsQuery_ForwardsRawQuery(t *testing.T) {
	cases := []struct {
		name    string
		rawQS   string
		wantRUI string
	}{
		{
			name:    "single",
			rawQS:   "query=up",
			wantRUI: "/v1/metrics/query?query=up",
		},
		{
			name:    "multi",
			rawQS:   "query=up&time=2026-06-28T00:00:00Z&step=15s",
			wantRUI: "/v1/metrics/query?query=up&time=2026-06-28T00:00:00Z&step=15s",
		},
		{
			name:    "percent_encoded_promql",
			rawQS:   "query=rate(node_cpu%7Bmode%3D%22idle%22%7D%5B5m%5D)",
			wantRUI: "/v1/metrics/query?query=rate(node_cpu%7Bmode%3D%22idle%22%7D%5B5m%5D)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, rec := newReadsServer(t, http.StatusOK, `{}`, "application/json")
			srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
				NewUpstream(upstream.URL, upstream.Client()))

			r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?"+tc.rawQS, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if rec.Calls != 1 {
				t.Errorf("upstream calls = %d, want 1", rec.Calls)
			}
			if rec.Path != "/v1/metrics/query" {
				t.Errorf("upstream path = %q, want /v1/metrics/query", rec.Path)
			}
			if rec.RawPath != tc.wantRUI {
				t.Errorf("upstream RequestURI = %q, want %q (RawQuery forwarded byte-for-byte, PRMT-141 §4)", rec.RawPath, tc.wantRUI)
			}
		})
	}
}

// TestHandleMetricsQuery_NoRawQuery: the upstream path stays
// /v1/metrics/query (no "?") when r.URL.RawQuery is empty.
// joinURL (upstream.go) must not synthesise a "?" in that case.
func TestHandleMetricsQuery_NoRawQuery(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, `{}`, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if rec.RawPath != "/v1/metrics/query" {
		t.Errorf("upstream RequestURI = %q, want /v1/metrics/query (no synthesised '?')", rec.RawPath)
	}
}

func TestHandleMetricsQuery_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/bad-query","title":"bad query","status":400,"detail":"syntax","instance":"/v1/metrics/query"}`
	upstream, rec := newReadsServer(t, http.StatusBadRequest, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=BAD", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if rec.RawPath != "/v1/metrics/query?query=BAD" {
		t.Errorf("upstream RequestURI = %q, want /v1/metrics/query?query=BAD", rec.RawPath)
	}
	if !strings.Contains(w.Body.String(), "bad-query") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleMetricsQuery_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=up", nil)
	r = readsCtx()(r)
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

func TestHandleMetricsQuery_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=up", nil)
	r = readsCtx()(r)
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

func TestHandleMetricsQuery_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/metrics/query?query=up", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleMetricsQuery_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=up", nil)
	w := httptest.NewRecorder()
	srv.handleMetricsQuery(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleMetricsQuery_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/metrics/query?query=up", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleMetricsQuery(w, r)

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

// TestHandleReads_RoutesRegistered_BehindAuthMiddleware: walks
// the full Server.Handler() and confirms each of the three new
// /api/* paths lands on the corresponding handleX (via the
// "upstream called" signal on the stub). If a /api/ route is
// not registered in server.go's switch the default 404 falls
// through and the test fails on the upstream call assertion.
// If a route is registered OUTSIDE AuthMiddleware (e.g. on a
// separate mux.HandleFunc) the call would still work, so the
// per-route path does not prove middleware wrapping; the
// server.go diff (L443–454, +12/-0) is the source of truth
// for "behind AuthMiddleware" — this test only proves the
// dispatch.
func TestHandleReads_RoutesRegistered_BehindAuthMiddleware(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		wantUpstream string
	}{
		{"/api/assets", "/api/assets", "/v1/assets"},
		{"/api/alarms", "/api/alarms", "/v1/alarms"},
		{"/api/metrics/query", "/api/metrics/query?query=up", "/v1/metrics/query"},
		{"/api/topology", "/api/topology", "/v1/topology"},
		// PRMT-153: E3.5 ops-portal reads.
		{"/api/tickets", "/api/tickets?status=open", "/v1/tickets"},
		{"/api/tickets/{id}", "/api/tickets/t-1234", "/v1/tickets/t-1234"},
		{"/api/capacity", "/api/capacity?site=sgp01", "/v1/capacity"},
		{"/api/capacity/metrics", "/api/capacity/metrics?window=1h", "/v1/capacity/metrics"},
		// PRMT-193: Commercial Platform usage list.
		{"/api/usage", "/api/usage?tenant_id=tn_a", "/v1/usage"},
		// PRMT-154: E3.5 maintenance/PM/spares/inspections reads.
		{"/api/maintenance/upcoming", "/api/maintenance/upcoming?site=sgp01", "/v1/maintenance/upcoming"},
		{"/api/pm/schedules", "/api/pm/schedules?asset_id=a-1", "/v1/pm/schedules"},
		{"/api/pm/schedules/{id}", "/api/pm/schedules/pm-1234", "/v1/pm/schedules/pm-1234"},
		{"/api/spares", "/api/spares?site=sgp01", "/v1/spares"},
		{"/api/spares/{id}", "/api/spares/sp-1234", "/v1/spares/sp-1234"},
		{"/api/inspections", "/api/inspections?asset_id=a-1", "/v1/inspections"},
		{"/api/inspections/{id}", "/api/inspections/in-1234", "/v1/inspections/in-1234"},
		// PRMT-155: E3.5 runbook/case/report reads.
		{"/api/runbooks/{key}", "/api/runbooks/rb-1234", "/v1/runbooks/rb-1234"},
		{"/api/cases", "/api/cases?status=open", "/v1/cases"},
		{"/api/reports/ops", "/api/reports/ops?window=1h", "/v1/reports/ops"},
		{"/api/reports/reconcile", "/api/reports/reconcile?site=sgp01", "/v1/reports/reconcile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, rec := newReadsServer(t, http.StatusOK, `{}`, "application/json")
			srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
				NewUpstream(upstream.URL, upstream.Client()))

			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (route %s not registered → would 404)", w.Code, tc.path)
			}
			if rec.Calls != 1 {
				t.Errorf("upstream calls = %d, want 1", rec.Calls)
			}
			if rec.Path != tc.wantUpstream {
				t.Errorf("upstream path = %q, want %q", rec.Path, tc.wantUpstream)
			}
		})
	}
}

// ---- /api/topology -------------------------------------------------------
// PRMT-151: spec-001 §7 relationship graph (feeds/cools/connects).
// Byte-for-byte mirror of the handleAssets test cases.

func TestHandleTopology_HappyPath(t *testing.T) {
	const body = `{"nodes":[{"id":"sgp01.pod001.rack01.cdu000"}],"edges":[{"from":"a","to":"b","relation":"feeds"}]}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/topology" {
		t.Errorf("upstream path = %s, want /v1/topology", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleTopology_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/topology"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleTopology_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
	r = readsCtx()(r)
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

func TestHandleTopology_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
	r = readsCtx()(r)
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

func TestHandleTopology_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/topology", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleTopology_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	// Bypass the full Handler (which would route through
	// AuthMiddleware) and call handleTopology directly so the only
	// code path under test is the ClaimsFrom check.
	r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
	w := httptest.NewRecorder()
	srv.handleTopology(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleTopology_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/topology", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleTopology(w, r)

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

// ---- /api/tickets -------------------------------------------------------
// PRMT-153: E3.5 ops-portal tickets read route. Byte-for-byte
// mirror of the handleAssets test cases plus the raw-query
// forwarding shape used by handleMetricsQuery.

func TestHandleTickets_HappyPath(t *testing.T) {
	const body = `[{"id":"t-1","status":"open"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets?status=open", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/tickets" {
		t.Errorf("upstream path = %s, want /v1/tickets", rec.Path)
	}
	if rec.RawPath != "/v1/tickets?status=open" {
		t.Errorf("upstream RequestURI = %q, want /v1/tickets?status=open (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleTickets_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/tickets"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleTickets_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	r = readsCtx()(r)
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

func TestHandleTickets_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	r = readsCtx()(r)
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

func TestHandleTickets_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/tickets", nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("method %s: status = %d, want 405", m, w.Code)
			}
			if allow := w.Header().Get("Allow"); allow != "GET, POST" {
				t.Errorf("method %s: Allow = %q, want \"GET, POST\"", m, allow)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleTickets_PostCreate_Proxied(t *testing.T) {
	const respBody = `{"id":"tk_1","alarm_id":"AL-1","state":"open"}`
	upstream, rec := newReadsServer(t, http.StatusCreated, respBody, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	reqBody := `{"asset_path":"site01.pod000.cdu000","title":"pump leak","severity":"major","alarm_id":"AL-1"}`
	r := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(reqBody))
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if rec.Method != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", rec.Method)
	}
	if rec.Path != "/v1/tickets" {
		t.Errorf("upstream path = %s, want /v1/tickets", rec.Path)
	}
	if w.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleTickets_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	w := httptest.NewRecorder()
	srv.handleTickets(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleTickets_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleTickets(w, r)

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

// ---- /api/tickets/{id} --------------------------------------------------
// PRMT-153: per-ticket detail read. Mirrors handleTickets for
// the parts that are not id-extraction; adds an id-extraction
// negative case for the 404 path-not-found path.

func TestHandleTicketByID_HappyPath(t *testing.T) {
	const body = `{"id":"t-1234","status":"open"}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-1234", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/tickets/t-1234" {
		t.Errorf("upstream path = %s, want /v1/tickets/t-1234", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleTicketByID_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"not found","status":404,"detail":"x","instance":"/v1/tickets/t-9"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-9", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
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

func TestHandleTicketByID_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-1", nil)
	r = readsCtx()(r)
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

func TestHandleTicketByID_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-1", nil)
	r = readsCtx()(r)
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

func TestHandleTicketByID_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/tickets/t-1", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleTicketByID_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-1", nil)
	w := httptest.NewRecorder()
	srv.handleTicketByID(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleTicketByID_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/tickets/t-1", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleTicketByID(w, r)

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

// TestHandleTicketByID_BadID_Returns404 covers PRMT-153 §4
// "reject empty/`/`-containing id with 404 path-not-found". The
// prefix dispatch in server.go (`strings.HasPrefix(...,
// "/api/tickets/")`) never fires for the bare /api/tickets path
// (handled by handleTickets) but it WILL fire for /api/tickets/
// (with a trailing slash and no id) — the handler must 404
// rather than synthesize an empty id.
func TestHandleTicketByID_BadID_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, p := range []string{"/api/tickets/", "/api/tickets/with/slash"} {
		t.Run(p, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("path %q: status = %d, want 404", p, w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode problem body: %v", err)
			}
			if !strings.HasSuffix(body["type"].(string), "path-not-found") {
				t.Errorf("path %q: type = %v, want tail path-not-found", p, body["type"])
			}
		})
	}
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; bad-id must 404 before reverse-proxy", rec.Calls)
	}
}

// ---- /api/capacity ------------------------------------------------------
// PRMT-153: E3.5 ops-portal capacity read route. Byte-for-byte
// mirror of the handleAssets test cases plus raw-query
// forwarding.

func TestHandleCapacity_HappyPath(t *testing.T) {
	const body = `[{"site":"sgp01","cpu_pct":73}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity?site=sgp01", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/capacity" {
		t.Errorf("upstream path = %s, want /v1/capacity", rec.Path)
	}
	if rec.RawPath != "/v1/capacity?site=sgp01" {
		t.Errorf("upstream RequestURI = %q, want /v1/capacity?site=sgp01 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleCapacityForecast_HappyPath(t *testing.T) {
	const body = `{"method":"linear_growth","horizons":[]}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/forecast?horizons=30d,90d", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if rec.Path != "/v1/capacity/forecast" {
		t.Errorf("upstream path = %s, want /v1/capacity/forecast", rec.Path)
	}
	if rec.RawPath != "/v1/capacity/forecast?horizons=30d,90d" {
		t.Errorf("upstream RequestURI = %q", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleCapacity_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/capacity"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleCapacity_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity", nil)
	r = readsCtx()(r)
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

func TestHandleCapacity_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity", nil)
	r = readsCtx()(r)
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

func TestHandleCapacity_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/capacity", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleCapacity_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity", nil)
	w := httptest.NewRecorder()
	srv.handleCapacity(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleCapacity_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/capacity", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleCapacity(w, r)

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

// ---- /api/capacity/metrics ----------------------------------------------
// PRMT-153: E3.5 ops-portal capacity metrics read route.
// Byte-for-byte mirror of the handleAssets test cases plus
// raw-query forwarding.

func TestHandleCapacityMetrics_HappyPath(t *testing.T) {
	const body = `[{"site":"sgp01","cpu_pct":73}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics?window=1h", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/capacity/metrics" {
		t.Errorf("upstream path = %s, want /v1/capacity/metrics", rec.Path)
	}
	if rec.RawPath != "/v1/capacity/metrics?window=1h" {
		t.Errorf("upstream RequestURI = %q, want /v1/capacity/metrics?window=1h (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleCapacityMetrics_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/capacity/metrics"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleCapacityMetrics_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics", nil)
	r = readsCtx()(r)
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

func TestHandleCapacityMetrics_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics", nil)
	r = readsCtx()(r)
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

func TestHandleCapacityMetrics_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/capacity/metrics", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleCapacityMetrics_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleCapacityMetrics(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleCapacityMetrics_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/capacity/metrics", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleCapacityMetrics(w, r)

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

// ---- /api/usage ---------------------------------------------------------
// PRMT-193: Commercial Platform usage list proxy. Mirror of
// handleCapacity (happy path + raw query + method + missing tenant).

func TestHandleUsage_HappyPath(t *testing.T) {
	const body = `{"items":[{"id":"us_TEST","kind":"energy"}],"next_page_token":""}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/usage?tenant_id=tn_a&kind=energy", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/usage" {
		t.Errorf("upstream path = %s, want /v1/usage", rec.Path)
	}
	if rec.RawPath != "/v1/usage?tenant_id=tn_a&kind=energy" {
		t.Errorf("upstream RequestURI = %q, want query forwarded", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleUsage_MethodNotAllowed(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(m, "/api/usage", nil)
		r = readsCtx()(r)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, w.Code)
		}
	}
}

func TestHandleUsage_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	r := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	w := httptest.NewRecorder()
	srv.handleUsage(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if rec.Calls != 0 {
		t.Errorf("upstream calls = %d; must not call without claims", rec.Calls)
	}
}

// ---- PRMT-154: E3.5 maintenance/PM/spares/inspections reads ----------
// Byte-for-byte mirror of the handleAssets / handleTickets test
// cases, applied per-handler (handleMaintenanceUpcoming,
// handlePMSchedules, handlePMScheduleByID, handleSpares,
// handleSpareByID, handleInspections, handleInspectionByID).
// Coverage matrix mirrors handleAssets exactly (PRMT-141 §5 +
// PRMT-153 §5 MUST):
//   - upstream 200 → 200 + verbatim body
//   - upstream 4xx → forward status + RFC 7807
//   - upstream 5xx → 502 + RFC 7807 "upstream-unavailable"
//   - transport error → 502 + RFC 7807 "upstream-unavailable"
//   - non-GET → 405 + RFC 7807 "bad-request" (POST/PUT/DELETE/PATCH)
//   - missing claims → 401 (no upstream call)
//   - missing (tenant, tier) → 403 (no upstream call)
//   - malformed {id} (where applicable) → 404 path-not-found
//
// Test plan repeats for each of the 7 PRMT-154 handlers. To keep
// the diff proportional (the structural shape is identical to
// handleTickets / handleTicketByID tests), the seven test blocks
// follow PRMT-153 layout one-for-one.

// ---- /api/maintenance/upcoming -----------------------------------------

func TestHandleMaintenanceUpcoming_HappyPath(t *testing.T) {
	const body = `[{"id":"m-1","due":"2026-07-01"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming?site=sgp01", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/maintenance/upcoming" {
		t.Errorf("upstream path = %s, want /v1/maintenance/upcoming", rec.Path)
	}
	if rec.RawPath != "/v1/maintenance/upcoming?site=sgp01" {
		t.Errorf("upstream RequestURI = %q, want /v1/maintenance/upcoming?site=sgp01 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleMaintenanceUpcoming_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/maintenance/upcoming"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleMaintenanceUpcoming_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming", nil)
	r = readsCtx()(r)
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

func TestHandleMaintenanceUpcoming_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming", nil)
	r = readsCtx()(r)
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

func TestHandleMaintenanceUpcoming_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/maintenance/upcoming", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleMaintenanceUpcoming_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming", nil)
	w := httptest.NewRecorder()
	srv.handleMaintenanceUpcoming(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleMaintenanceUpcoming_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/maintenance/upcoming", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleMaintenanceUpcoming(w, r)

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

// ---- /api/pm/schedules --------------------------------------------------

func TestHandlePMSchedules_HappyPath(t *testing.T) {
	const body = `[{"id":"pm-1","asset_id":"a-1","due":"2026-07-01"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules?asset_id=a-1", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/pm/schedules" {
		t.Errorf("upstream path = %s, want /v1/pm/schedules", rec.Path)
	}
	if rec.RawPath != "/v1/pm/schedules?asset_id=a-1" {
		t.Errorf("upstream RequestURI = %q, want /v1/pm/schedules?asset_id=a-1 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandlePMSchedules_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/pm/schedules"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandlePMSchedules_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules", nil)
	r = readsCtx()(r)
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

func TestHandlePMSchedules_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules", nil)
	r = readsCtx()(r)
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

func TestHandlePMSchedules_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/pm/schedules", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandlePMSchedules_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules", nil)
	w := httptest.NewRecorder()
	srv.handlePMSchedules(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandlePMSchedules_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handlePMSchedules(w, r)

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

// ---- /api/pm/schedules/{id} ---------------------------------------------

func TestHandlePMScheduleByID_HappyPath(t *testing.T) {
	const body = `{"id":"pm-1234","asset_id":"a-1","due":"2026-07-01"}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-1234", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/pm/schedules/pm-1234" {
		t.Errorf("upstream path = %s, want /v1/pm/schedules/pm-1234", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandlePMScheduleByID_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"not found","status":404,"detail":"x","instance":"/v1/pm/schedules/pm-9"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-9", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
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

func TestHandlePMScheduleByID_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-1", nil)
	r = readsCtx()(r)
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

func TestHandlePMScheduleByID_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-1", nil)
	r = readsCtx()(r)
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

func TestHandlePMScheduleByID_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/pm/schedules/pm-1", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandlePMScheduleByID_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-1", nil)
	w := httptest.NewRecorder()
	srv.handlePMScheduleByID(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandlePMScheduleByID_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/pm/schedules/pm-1", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handlePMScheduleByID(w, r)

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

func TestHandlePMScheduleByID_MalformedID_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name string
		path string
	}{
		{"trailing_slash", "/api/pm/schedules/"},
		{"nested", "/api/pm/schedules/pm-1/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (malformed {id})", w.Code)
			}
			if rec.Calls != 0 {
				t.Errorf("upstream was called %d times; malformed id MUST NOT reach /v1", rec.Calls)
			}
		})
	}
}

// ---- /api/spares --------------------------------------------------------

func TestHandleSpares_HappyPath(t *testing.T) {
	const body = `[{"id":"sp-1","part_no":"P-123","qty":3}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares?site=sgp01", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/spares" {
		t.Errorf("upstream path = %s, want /v1/spares", rec.Path)
	}
	if rec.RawPath != "/v1/spares?site=sgp01" {
		t.Errorf("upstream RequestURI = %q, want /v1/spares?site=sgp01 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleSpares_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/spares"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleSpares_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares", nil)
	r = readsCtx()(r)
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

func TestHandleSpares_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/spares", nil)
	r = readsCtx()(r)
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

func TestHandleSpares_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/spares", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleSpares_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares", nil)
	w := httptest.NewRecorder()
	srv.handleSpares(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleSpares_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/spares", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleSpares(w, r)

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

// ---- /api/spares/{id} ---------------------------------------------------

func TestHandleSpareByID_HappyPath(t *testing.T) {
	const body = `{"id":"sp-1234","part_no":"P-123","qty":3}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-1234", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/spares/sp-1234" {
		t.Errorf("upstream path = %s, want /v1/spares/sp-1234", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleSpareByID_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"not found","status":404,"detail":"x","instance":"/v1/spares/sp-9"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-9", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
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

func TestHandleSpareByID_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-1", nil)
	r = readsCtx()(r)
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

func TestHandleSpareByID_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-1", nil)
	r = readsCtx()(r)
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

func TestHandleSpareByID_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/spares/sp-1", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleSpareByID_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-1", nil)
	w := httptest.NewRecorder()
	srv.handleSpareByID(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleSpareByID_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/spares/sp-1", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleSpareByID(w, r)

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

func TestHandleSpareByID_MalformedID_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name string
		path string
	}{
		{"trailing_slash", "/api/spares/"},
		{"nested", "/api/spares/sp-1/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (malformed {id})", w.Code)
			}
			if rec.Calls != 0 {
				t.Errorf("upstream was called %d times; malformed id MUST NOT reach /v1", rec.Calls)
			}
		})
	}
}

// ---- /api/inspections ---------------------------------------------------

func TestHandleInspections_HappyPath(t *testing.T) {
	const body = `[{"id":"in-1","asset_id":"a-1","status":"pending"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections?asset_id=a-1", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/inspections" {
		t.Errorf("upstream path = %s, want /v1/inspections", rec.Path)
	}
	if rec.RawPath != "/v1/inspections?asset_id=a-1" {
		t.Errorf("upstream RequestURI = %q, want /v1/inspections?asset_id=a-1 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleInspections_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/inspections"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleInspections_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections", nil)
	r = readsCtx()(r)
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

func TestHandleInspections_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections", nil)
	r = readsCtx()(r)
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

func TestHandleInspections_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/inspections", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleInspections_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections", nil)
	w := httptest.NewRecorder()
	srv.handleInspections(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleInspections_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/inspections", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleInspections(w, r)

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

// ---- /api/inspections/{id} ----------------------------------------------

func TestHandleInspectionByID_HappyPath(t *testing.T) {
	const body = `{"id":"in-1234","asset_id":"a-1","status":"pending"}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-1234", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/inspections/in-1234" {
		t.Errorf("upstream path = %s, want /v1/inspections/in-1234", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleInspectionByID_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/path-not-found","title":"not found","status":404,"detail":"x","instance":"/v1/inspections/in-9"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-9", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
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

func TestHandleInspectionByID_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-1", nil)
	r = readsCtx()(r)
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

func TestHandleInspectionByID_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-1", nil)
	r = readsCtx()(r)
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

func TestHandleInspectionByID_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/inspections/in-1", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleInspectionByID_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-1", nil)
	w := httptest.NewRecorder()
	srv.handleInspectionByID(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleInspectionByID_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/inspections/in-1", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleInspectionByID(w, r)

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

func TestHandleInspectionByID_MalformedID_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name string
		path string
	}{
		{"trailing_slash", "/api/inspections/"},
		{"nested", "/api/inspections/in-1/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (malformed {id})", w.Code)
			}
			if rec.Calls != 0 {
				t.Errorf("upstream was called %d times; malformed id MUST NOT reach /v1", rec.Calls)
			}
		})
	}
}

// ---- /api/runbooks/{key} -------------------------------------------------
// PRMT-155: E3.5 ops-portal runbook-by-key read route. Byte-for-
// byte mirror of the handleInspectionByID test cases.

func TestHandleRunbookByKey_HappyPath(t *testing.T) {
	const body = `{"key":"rb-1","title":"Runbook 1","steps":["a","b"]}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/runbooks/rb-1" {
		t.Errorf("upstream path = %s, want /v1/runbooks/rb-1", rec.Path)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleRunbookByKey_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/not-found","title":"not found","status":404,"detail":"x","instance":"/v1/runbooks/rb-1"}`
	upstream, rec := newReadsServer(t, http.StatusNotFound, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "not found") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleRunbookByKey_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
	r = readsCtx()(r)
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

func TestHandleRunbookByKey_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
	r = readsCtx()(r)
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

func TestHandleRunbookByKey_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/runbooks/rb-1", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleRunbookByKey_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
	w := httptest.NewRecorder()
	srv.handleRunbookByKey(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleRunbookByKey_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/runbooks/rb-1", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleRunbookByKey(w, r)

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

func TestHandleRunbookByKey_MalformedKey_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	cases := []struct {
		name string
		path string
	}{
		{"trailing_slash", "/api/runbooks/"},
		{"nested", "/api/runbooks/rb-1/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = readsCtx()(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (malformed {key})", w.Code)
			}
			if rec.Calls != 0 {
				t.Errorf("upstream was called %d times; malformed key MUST NOT reach /v1", rec.Calls)
			}
		})
	}
}

// ---- /api/cases ----------------------------------------------------------
// PRMT-155: E3.5 ops-portal cases read route. Byte-for-byte
// mirror of the handleTickets test cases plus the raw-query
// forwarding shape used by handleMetricsQuery.

func TestHandleCases_HappyPath(t *testing.T) {
	const body = `[{"id":"c-1","status":"open"}]`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/cases?status=open", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/cases" {
		t.Errorf("upstream path = %s, want /v1/cases", rec.Path)
	}
	if rec.RawPath != "/v1/cases?status=open" {
		t.Errorf("upstream RequestURI = %q, want /v1/cases?status=open (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleCases_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/cases"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/cases", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleCases_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/cases", nil)
	r = readsCtx()(r)
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

func TestHandleCases_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/cases", nil)
	r = readsCtx()(r)
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

func TestHandleCases_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/cases", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleCases_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/cases", nil)
	w := httptest.NewRecorder()
	srv.handleCases(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleCases_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/cases", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleCases(w, r)

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

// ---- /api/reports/ops ----------------------------------------------------
// PRMT-155: E3.5 ops-portal ops-report read route. Byte-for-byte
// mirror of the handleTickets test cases.

func TestHandleReportOps_HappyPath(t *testing.T) {
	const body = `{"window":"1h","totals":{"alarms":12}}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/ops?window=1h", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/reports/ops" {
		t.Errorf("upstream path = %s, want /v1/reports/ops", rec.Path)
	}
	if rec.RawPath != "/v1/reports/ops?window=1h" {
		t.Errorf("upstream RequestURI = %q, want /v1/reports/ops?window=1h (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleReportOps_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/reports/ops"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/ops", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleReportOps_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/ops", nil)
	r = readsCtx()(r)
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

func TestHandleReportOps_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/ops", nil)
	r = readsCtx()(r)
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

func TestHandleReportOps_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/reports/ops", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleReportOps_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/ops", nil)
	w := httptest.NewRecorder()
	srv.handleReportOps(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleReportOps_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/reports/ops", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleReportOps(w, r)

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

// ---- /api/reports/reconcile ----------------------------------------------
// PRMT-155: E3.5 ops-portal reconcile-report read route. Byte-for-
// byte mirror of the handleTickets test cases.

func TestHandleReportReconcile_HappyPath(t *testing.T) {
	const body = `{"matched":42,"unmatched":3}`
	upstream, rec := newReadsServer(t, http.StatusOK, body, "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile?site=sgp01", nil)
	r = readsCtx()(r)
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
	if rec.Path != "/v1/reports/reconcile" {
		t.Errorf("upstream path = %s, want /v1/reports/reconcile", rec.Path)
	}
	if rec.RawPath != "/v1/reports/reconcile?site=sgp01" {
		t.Errorf("upstream RequestURI = %q, want /v1/reports/reconcile?site=sgp01 (RawQuery forwarded)", rec.RawPath)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want verbatim passthrough", w.Body.String())
	}
}

func TestHandleReportReconcile_Upstream4xx_Forwarded(t *testing.T) {
	const upstreamBody = `{"type":"https://cios.dev/errors/forbidden","title":"forbidden","status":403,"detail":"x","instance":"/v1/reports/reconcile"}`
	upstream, rec := newReadsServer(t, http.StatusForbidden, upstreamBody, "application/problem+json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forward upstream 4xx)", w.Code)
	}
	if rec.Calls != 1 {
		t.Errorf("upstream calls = %d, want 1", rec.Calls)
	}
	if !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("body did not forward upstream RFC7807: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHandleReportReconcile_Upstream5xx_Becomes502(t *testing.T) {
	upstream, _ := newReadsServer(t, http.StatusInternalServerError, "boom", "text/plain")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile", nil)
	r = readsCtx()(r)
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

func TestHandleReportReconcile_NetworkError_Becomes502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	client := upstream.Client()
	upstream.Close() // force connection refused

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: url},
		NewUpstream(url, client))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile", nil)
	r = readsCtx()(r)
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

func TestHandleReportReconcile_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			r := httptest.NewRequest(m, "/api/reports/reconcile", nil)
			r = readsCtx()(r)
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
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire before reverse-proxy", rec.Calls)
	}
}

func TestHandleReportReconcile_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile", nil)
	w := httptest.NewRecorder()
	srv.handleReportReconcile(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
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

func TestHandleReportReconcile_MissingTenantReturns403(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
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
			r := httptest.NewRequest(http.MethodGet, "/api/reports/reconcile", nil)
			r = r.WithContext(WithClaims(r.Context(), tc.claims))
			w := httptest.NewRecorder()
			srv.handleReportReconcile(w, r)

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

// ---- /api/alarms/{id}:ack (PRMT-230) -------------------------------------

// TestHandleAlarmAck_ProxiesPost asserts the ack proxy forwards
// POST → core /v1/alarms/{id}:ack with the X-CIOS-Tenant
// propagation header (PostV1AsTenant, PRMT-230 §3.6). A local
// recorder is used because newReadsServer does not capture the
// tenant header.
func TestHandleAlarmAck_ProxiesPost(t *testing.T) {
	var gotMethod, gotPath, gotTenant string
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod, gotPath = r.Method, r.URL.Path
		gotTenant = r.Header.Get("X-CIOS-Tenant")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"A1","state":"acked"}`)
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodPost, "/api/alarms/A1:ack", strings.NewReader("{}"))
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/alarms/A1:ack" {
		t.Errorf("upstream path = %q, want /v1/alarms/A1:ack", gotPath)
	}
	if gotTenant == "" {
		t.Errorf("X-CIOS-Tenant header empty; PostV1AsTenant must propagate it")
	}
}

func TestHandleAlarmAck_NoClaims_Returns401(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodPost, "/api/alarms/A1:ack", nil)
	w := httptest.NewRecorder()
	srv.handleAlarmAck(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; handler MUST NOT call /v1 without verified identity", rec.Calls)
	}
}

func TestHandleAlarmAck_MethodNotAllowed(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/alarms/A1:ack", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; method gate must fire first", rec.Calls)
	}
}

func TestHandleAlarmAck_NestedID_Returns404(t *testing.T) {
	upstream, rec := newReadsServer(t, http.StatusOK, "{}", "application/json")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodPost, "/api/alarms/x/y:ack", nil)
	r = readsCtx()(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if rec.Calls != 0 {
		t.Errorf("upstream was called %d times; nested id must never reach core", rec.Calls)
	}
}
