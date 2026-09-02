// Package core — middleware_test.go: tests for the PRMT-074
// request-id + access-log middleware pair.
//
// The request-id half re-uses the M0 withRequestID closure
// (which itself lives in server.go); the new code under test is
// the wrapping order in Handler() and the access-log emission.
// The test file is in the PRMT-074 §3 whitelist.
package core

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// --- access-log sink -----------------------------------------------------

// accessLogCapture is a thread-safe sink analogous to
// auth_test.go's auditCapture: tests inject it via
// SetAccessLogForTest and then assert on the captured lines.
type accessLogCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *accessLogCapture) log(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *accessLogCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// --- helpers -------------------------------------------------------------

// echoRIDHandler is a tiny terminal handler that records the
// request_id it sees on the request context and writes a 200
// with a fixed body. Used to confirm request-id flows from
// middleware → handler (and the same value reaches both).
type echoRIDHandler struct {
	mu       sync.Mutex
	seenRID  string
	seenMeth string
	seenPath string
}

func (h *echoRIDHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.seenRID = RequestIDFromContext(r.Context())
	h.seenMeth = r.Method
	h.seenPath = r.URL.Path
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// chain is the production-shape composition
// (requestID → accessLog → inner) so the tests exercise the
// same wrapping order Handler() builds.
func chain(inner http.Handler) http.Handler {
	return requestIDMiddleware(accessLogMiddleware(inner))
}

// --- request-id tests ----------------------------------------------------

func TestRequestID_GeneratesWhenHeaderMissing(t *testing.T) {
	var h echoRIDHandler
	ts := httptest.NewServer(chain(&h))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/health/whatever")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := resp.Header.Get("X-Request-Id")
	if got == "" {
		t.Fatal("response missing X-Request-Id header")
	}
	// The handler must have seen the SAME id on the request
	// context — the wire echo and the ctx value are the same
	// generator call, not two independent IDs.
	h.mu.Lock()
	seen := h.seenRID
	h.mu.Unlock()
	if seen == "" {
		t.Fatal("handler saw empty request_id in context")
	}
	if seen != got {
		t.Errorf("ctx request_id=%q != response header=%q (must be one id, not two)", seen, got)
	}
}

func TestRequestID_PassesThroughExistingHeader(t *testing.T) {
	const want = "client-supplied-id-12345"
	var h echoRIDHandler
	ts := httptest.NewServer(chain(&h))
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Request-Id", want)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != want {
		t.Errorf("response X-Request-Id = %q, want %q (middleware must pass through)", got, want)
	}
	h.mu.Lock()
	seen := h.seenRID
	h.mu.Unlock()
	if seen != want {
		t.Errorf("ctx request_id = %q, want %q (must be the same id as wire echo)", seen, want)
	}
}

func TestRequestID_ResponseHeaderAlwaysSet(t *testing.T) {
	// Even a 404 from the inner mux (not our 200 echo) must carry
	// the X-Request-Id header — that's the whole point of the
	// middleware being outermost.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(chain(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Errorf("404 response missing X-Request-Id header (middleware should be outermost)")
	}
}

func TestRequestID_GeneratedIDsAreUnique(t *testing.T) {
	// Two consecutive calls must produce distinct ids (regression
	// guard against a middleware that cached a singleton id).
	ts := httptest.NewServer(chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer ts.Close()
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		resp, err := http.Get(ts.URL + "/v1/x")
		if err != nil {
			t.Fatalf("GET[%d]: %v", i, err)
		}
		resp.Body.Close()
		id := resp.Header.Get("X-Request-Id")
		if id == "" {
			t.Fatalf("iter %d: missing id", i)
		}
		if seen[id] {
			t.Errorf("duplicate id %q across requests", id)
		}
		seen[id] = true
	}
}

// --- access-log tests ----------------------------------------------------

// accessLogLineRe matches a single access-log line; the field
// order is fixed per PRMT-074 §2. We don't pin the prefix (just
// the suffix key=value tail) so a future change to the
// transport-level log line (e.g. hostname, pid) doesn't break
// these assertions.
var accessLogLineRe = regexp.MustCompile(
	`method="([^"]*)" path="([^"]*)" status=(\d+) dur_ms=(\d+) principal="([^"]*)" request_id="([^"]*)"`,
)

func TestAccessLog_EmitsExpectedFields(t *testing.T) {
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	var h echoRIDHandler
	ts := httptest.NewServer(chain(&h))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/health/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1: %v", len(lines), lines)
	}
	m := accessLogLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("access line missing required fields: %q", lines[0])
	}
	method, path, status, dur, principal, rid := m[1], m[2], m[3], m[4], m[5], m[6]
	if method != http.MethodGet {
		t.Errorf("method = %q, want GET", method)
	}
	if path != "/v1/health/anything" {
		t.Errorf("path = %q, want /v1/health/anything", path)
	}
	if status != "200" {
		t.Errorf("status = %q, want 200", status)
	}
	if dur == "" {
		t.Errorf("dur_ms empty: %q", lines[0])
	}
	// No auth in this chain → principal is the M0 "-" sentinel
	// (mirrors the audit line's no-principal rendering).
	if principal != "-" {
		t.Errorf("principal = %q, want %q (no auth in this chain)", principal, "-")
	}
	if rid == "" || rid == "-" {
		t.Errorf("request_id = %q, want a real id (requestIDMiddleware is outermost)", rid)
	}
	if rid != resp.Header.Get("X-Request-Id") {
		t.Errorf("request_id in log = %q != response header = %q", rid, resp.Header.Get("X-Request-Id"))
	}
}

func TestAccessLog_CapturesNon200Status(t *testing.T) {
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	})
	ts := httptest.NewServer(chain(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}

	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], `status=418`) {
		t.Errorf("expected status=418 in line, got: %q", lines[0])
	}
}

func TestAccessLog_CapturesImplicit200(t *testing.T) {
	// A handler that writes a body without an explicit
	// WriteHeader is implicitly 200 OK (Go stdlib contract);
	// our statusCapturingResponseWriter must mirror that.
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	ts := httptest.NewServer(chain(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], `status=200`) {
		t.Errorf("expected implicit 200 in line, got: %q", lines[0])
	}
}

func TestAccessLog_PrincipalFieldAlwaysPresent(t *testing.T) {
	// The access log line always carries the principal= field
	// per PRMT-074 §5 (6-field structured log). In the
	// canonical middleware order (request-id → access-log → auth)
	// the access-log middleware sits OUTSIDE auth, so by the
	// time it reads PrincipalFromContext the auth middleware
	// has not yet set the principal on this r — the field is
	// therefore rendered as the "-" sentinel for all requests.
	// The principal is logged separately by authmw.go's audit
	// line (which DOES see the principal context). This test
	// pins the field-presence invariant so a future refactor
	// that drops the field is caught; the value is "-" here.
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	inner := passthroughInner(&Principal{}, &sync.Mutex{})

	h := requestIDMiddleware(accessLogMiddleware(newAuthMiddleware(v, inner)))

	ts := httptest.NewServer(h)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/assets", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer viewer-plaintext-token-do-not-leak")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `principal="-"`) {
		t.Errorf("expected principal=\"-\" in access line (outside auth), got: %q", lines[0])
	}
	// SECURITY: the bearer token plaintext must never leak into
	// the access log. (authmw.go's audit line has the same
	// invariant — the only difference is that line uses the
	// Principal object, not the raw header value.)
	if strings.Contains(lines[0], "viewer-plaintext-token-do-not-leak") {
		t.Errorf("access log leaked bearer token: %q", lines[0])
	}
}

func TestAccessLog_DenyPathsStillLogged(t *testing.T) {
	// 401 from the auth middleware must still produce an access
	// line — the whole point of putting access-log OUTSIDE
	// auth is that the operator can see denied requests in the
	// log stream.
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	h := requestIDMiddleware(accessLogMiddleware(newAuthMiddleware(v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))))

	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/assets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `status=401`) {
		t.Errorf("expected status=401 in line, got: %q", lines[0])
	}
	// No Principal on the deny path → "-" sentinel.
	if !strings.Contains(lines[0], `principal="-"`) {
		t.Errorf("expected principal=\"-\" in line, got: %q", lines[0])
	}
}

// --- no-auth-regression sanity ------------------------------------------

func TestRequestID_NoAuthRegression(t *testing.T) {
	// The PRMT-074 changes touch Handler(); this confirms the
	// M0 unauthenticated behaviour (auth == nil) is still
	// wired: request-id present on response, access line
	// emitted, handler runs.
	sink := &accessLogCapture{}
	restore := SetAccessLogForTest(sink.log)
	defer restore()

	dict := mustAuthTestDict(t)
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	srv := NewServer(st, dict, "http://127.0.0.1:0")
	// auth left nil — M0 path.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/assets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (M0 unauthenticated must still serve)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Errorf("M0 response missing X-Request-Id")
	}
	lines := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1: %v", len(lines), lines)
	}
	if !accessLogLineRe.MatchString(lines[0]) {
		t.Errorf("access line malformed: %q", lines[0])
	}
}

// --- nil-arg sanity ------------------------------------------------------

func TestAccessLogLogger_DefaultsToPrintf(t *testing.T) {
	// The default accessLogLogger must be non-nil; the
	// production path relies on it. (We don't actually call it
	// here — that would write to stderr; we just check the
	// pointer is wired.)
	if accessLogLogger == nil {
		t.Fatal("accessLogLogger is nil (default not set)")
	}
}

// silence unused import in case bytes drops out of a future
// refactor (tests that compare bodies via bytes.Buffer go in
// server_test.go).
var _ = bytes.NewBuffer
