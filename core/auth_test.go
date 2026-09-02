// Package core — auth_test.go: end-to-end tests for the
// PRMT-019 bearer-token + RBAC middleware.
//
// Coverage:
//
//   - 401 path: no token, malformed header, unknown token.
//   - 403 path: viewer attempting a write; operator attempting an
//     apply; in-role but out-of-scope read; out-of-scope write.
//   - allow path: viewer read of in-scope subtree (L50 implicit
//     subtree); operator write requires explicit subtree pattern
//     (L50 writes do NOT imply subtree); admin bypass.
//   - audit-log format and "token plaintext never appears" invariant.
//   - "Auth==nil ⇒ M0 behaviour" — server_test.go is the canonical
//     M0 regression; here we re-assert that the same handler with
//     Auth nil takes the un-gated path.
//   - NewStaticTokenVerifier rejects bad scope patterns at load time.
//   - Direct authorize() unit tests for the role × action matrix.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yurimeng/cios/pkg/cpath"
)

// --- helpers ---------------------------------------------------------------

// auditCapture is a thread-safe sink for the audit middleware's log
// lines. Tests inject it as authMW.auditLog and then assert on lines.
type auditCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *auditCapture) log(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *auditCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// newAuthTestServer builds a Server with auth optional. Uses fileStore
// over t.TempDir() so the dedup/store side effects are isolated.
// Distinct from server_test.go's newTestServer (which returns an
// httptest.Server pair) so the two helpers coexist.
func newAuthTestServer(t *testing.T, withAuth *AuthConfig) *Server {
	t.Helper()
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	dict := mustAuthTestDict(t)
	srv := NewServer(st, dict, "http://127.0.0.1:0")
	srv.auth = withAuth
	return srv
}

// mustAuthTestDict loads protocol/ via the repo-relative path; named
// to avoid collision with server_test.go's helpers.
func mustAuthTestDict(t *testing.T) *cpath.Dict {
	t.Helper()
	d, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("load dict: %v", err)
	}
	return d
}

// buildVerifierForRoles is a small builder that returns a verifier
// holding three tokens (viewer/operator/admin) with the given scopes.
// Returns the verifier and the three plaintext tokens so callers can
// assert "plaintext NEVER appears in audit logs" with concrete strings.
func buildVerifierForRoles(t *testing.T, viewerScopes, operatorScopes, adminScopes []string) (*staticTokenVerifier, string, string, string) {
	t.Helper()
	const (
		viewerTok   = "viewer-plaintext-token-do-not-leak"
		operatorTok = "operator-plaintext-token-do-not-leak"
		adminTok    = "admin-plaintext-token-do-not-leak"
	)
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	v, err := NewStaticTokenVerifier(map[string]Principal{
		h(viewerTok):   {Subject: "svc:grafana", Role: RoleViewer, Scopes: viewerScopes},
		h(operatorTok): {Subject: "svc:cooling-ops", Role: RoleOperator, Scopes: operatorScopes},
		h(adminTok):    {Subject: "svc:admin", Role: RoleAdmin, Scopes: adminScopes},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	return v, viewerTok, operatorTok, adminTok
}

// captureMW wraps newAuthMiddleware and overrides auditLog with the
// supplied sink. Returns the http.Handler chain (mux-equivalent) the
// test will hit via httptest.
func captureMW(v TokenVerifier, inner http.Handler, sink *auditCapture) http.Handler {
	mw := &authMW{verifier: v, inner: inner, auditLog: sink.log}
	return mw
}

// passthroughInner returns 200 with a short body so allow paths
// produce a distinct status (vs. our 401/403). Records the Principal
// from ctx so the test can assert it was attached.
func passthroughInner(seen *Principal, mu *sync.Mutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFromContext(r.Context()); ok {
			mu.Lock()
			*seen = p
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// --- middleware-level tests (401/403/allow) -------------------------------

func TestAuthMW_NoBearer_Returns401(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	var seen Principal
	var mu sync.Mutex
	h := captureMW(v, passthroughInner(&seen, &mu), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(w.Body.String(), `"type":"https://cios.dev/errors/unauthorized"`) {
		t.Errorf("body missing unauthorized type tail: %s", w.Body.String())
	}
}

func TestAuthMW_BadBearerScheme_Returns401(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	cases := []string{
		"",                              // empty
		"Basic xyz",                     // wrong scheme
		"Bearer",                        // missing space + token
		"Bearer ",                       // empty token
		"Bearertoken-no-space",          // missing space
		"Bearer  doublespaceNotAllowed", // double space — our parser accepts only canonical
	}
	for _, hdr := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		withRequestID(h).ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("hdr=%q status=%d, want 401", hdr, w.Code)
		}
	}
}

func TestAuthMW_UnknownToken_Returns401(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	req.Header.Set("Authorization", "Bearer never-registered-token")
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAuthMW_ViewerCannotWrite_Returns403(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t,
		[]string{"site01.**"}, // viewer scope: whole site
		nil, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodPut, "/v1/assets/site01.pod002.cdu000", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"type":"https://cios.dev/errors/forbidden"`) {
		t.Errorf("body missing forbidden type tail: %s", w.Body.String())
	}
}

func TestAuthMW_OperatorCannotApply_Returns403(t *testing.T) {
	_, _, operatorTok, _ := buildVerifierForRoles(t,
		nil,
		[]string{"site01.**"},
		nil,
	)
	v, _, _, _ := buildVerifierForRoles(t, nil, []string{"site01.**"}, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodPut, "/v1/assets/site01.pod002.cdu000", nil)
	req.Header.Set("Authorization", "Bearer "+operatorTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestAuthMW_L50_ReadImpliesSubtree confirms a viewer with scope
// "site01.pod002" can read points/asset under site01.pod002.cdu000.*.
func TestAuthMW_L50_ReadImpliesSubtree(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t,
		[]string{"site01.pod002"},
		nil, nil)
	var seen Principal
	var mu sync.Mutex
	h := captureMW(v, passthroughInner(&seen, &mu), &auditCapture{})

	// read on a child path under the scope: must allow.
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/site01.pod002.cdu000", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s), want 200", w.Code, w.Body.String())
	}
	mu.Lock()
	if seen.Subject != "svc:grafana" {
		t.Errorf("inner did not receive principal: %+v", seen)
	}
	mu.Unlock()
}

// TestAuthMW_L50_WriteDoesNotImplySubtree confirms operator with
// scope "site01.pod002" cannot write under site01.pod002.cdu000.*;
// the operator must hold "site01.pod002.**" (explicit subtree).
func TestAuthMW_L50_WriteDoesNotImplySubtree(t *testing.T) {
	v, _, operatorTok, _ := buildVerifierForRoles(t,
		nil,
		[]string{"site01.pod002"}, // NOT "site01.pod002.**"
		nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodPut,
		"/v1/points/site01.pod002.cdu000.fan000.rpm:set", nil)
	req.Header.Set("Authorization", "Bearer "+operatorTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (L50: writes do NOT imply subtree)", w.Code)
	}
}

// TestAuthMW_L50_WriteExplicitSubtreeAllows: operator with explicit
// "site01.pod002.**" can write to a point under the subtree.
func TestAuthMW_L50_WriteExplicitSubtreeAllows(t *testing.T) {
	v, _, operatorTok, _ := buildVerifierForRoles(t,
		nil,
		[]string{"site01.pod002.**"},
		nil)
	var seen Principal
	var mu sync.Mutex
	h := captureMW(v, passthroughInner(&seen, &mu), &auditCapture{})

	req := httptest.NewRequest(http.MethodPut,
		"/v1/points/site01.pod002.cdu000.fan000.rpm:set", nil)
	req.Header.Set("Authorization", "Bearer "+operatorTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s), want 200", w.Code, w.Body.String())
	}
	mu.Lock()
	if seen.Role != RoleOperator {
		t.Errorf("ctx principal role = %q, want operator", seen.Role)
	}
	mu.Unlock()
}

func TestAuthMW_AdminBypassesScope(t *testing.T) {
	// Admin gets no scope at all — role bypass should still allow.
	v, _, _, adminTok := buildVerifierForRoles(t, nil, nil, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodPut, "/v1/assets/site99.pod000.cdu000", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admin bypass)", w.Code)
	}
}

// --- audit-log tests -------------------------------------------------------

// TestAuthMW_AuditLog_PlaintextTokenNeverAppears is the security
// invariant the entire prompt rides on: an audit line MAY mention
// the subject, role, action, path, decision, request_id — but NEVER
// the raw bearer token. We assert by string-grepping the audit sink.
func TestAuthMW_AuditLog_PlaintextTokenNeverAppears(t *testing.T) {
	v, viewerTok, operatorTok, adminTok := buildVerifierForRoles(t,
		[]string{"site01.**"},
		[]string{"site01.pod002.**"},
		nil)
	sink := &auditCapture{}
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), sink)

	// Mix of allow, deny-forbidden, deny-unauth — all should log.
	exercise := []struct {
		method, url, hdr string
	}{
		{http.MethodGet, "/v1/assets/site01.pod002.cdu000", "Bearer " + viewerTok},
		{http.MethodPut, "/v1/points/site01.pod002.cdu000.fan000.rpm:set", "Bearer " + operatorTok},
		{http.MethodPut, "/v1/assets/site01.pod002.cdu000", "Bearer " + viewerTok}, // 403
		{http.MethodPut, "/v1/assets/site01.pod002.cdu000", "Bearer " + adminTok},  // 200 admin bypass
		{http.MethodGet, "/v1/assets", ""},                                         // 401
		{http.MethodGet, "/v1/assets", "Bearer not-a-real-token"},                  // 401
	}
	for _, e := range exercise {
		req := httptest.NewRequest(e.method, e.url, nil)
		if e.hdr != "" {
			req.Header.Set("Authorization", e.hdr)
		}
		withRequestID(h).ServeHTTP(httptest.NewRecorder(), req)
	}

	lines := sink.snapshot()
	if len(lines) != len(exercise) {
		t.Fatalf("audit lines = %d, want %d", len(lines), len(exercise))
	}
	for i, line := range lines {
		for _, tok := range []string{viewerTok, operatorTok, adminTok, "not-a-real-token"} {
			if strings.Contains(line, tok) {
				t.Errorf("audit line %d leaked token plaintext %q: %s", i, tok, line)
			}
		}
		// And confirm the line follows the §4.5 format skeleton.
		for _, want := range []string{"audit principal=", "role=", "action=", "path=", "decision=", "request_id="} {
			if !strings.Contains(line, want) {
				t.Errorf("audit line %d missing %q: %s", i, want, line)
			}
		}
	}
}

func TestAuthMW_AuditLog_DecisionValues(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t,
		[]string{"site01.**"}, nil, nil)
	sink := &auditCapture{}
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), sink)

	// allow
	r1 := httptest.NewRequest(http.MethodGet, "/v1/assets/site01.pod002.cdu000", nil)
	r1.Header.Set("Authorization", "Bearer "+viewerTok)
	withRequestID(h).ServeHTTP(httptest.NewRecorder(), r1)

	// deny-forbidden (viewer trying apply)
	r2 := httptest.NewRequest(http.MethodPut, "/v1/assets/site01.pod002.cdu000", nil)
	r2.Header.Set("Authorization", "Bearer "+viewerTok)
	withRequestID(h).ServeHTTP(httptest.NewRecorder(), r2)

	// deny-unauth
	r3 := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	withRequestID(h).ServeHTTP(httptest.NewRecorder(), r3)

	lines := sink.snapshot()
	if len(lines) != 3 {
		t.Fatalf("audit lines = %d, want 3", len(lines))
	}
	want := []string{`decision="allow"`, `decision="deny-forbidden"`, `decision="deny-unauth"`}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d missing %q: %s", i, w, lines[i])
		}
	}
}

// --- Auth==nil regression --------------------------------------------------

// TestServer_AuthNil_NoRegression confirms that with Auth==nil the
// Handler() chain still produces a server identical to the M0
// behaviour — i.e. /v1/assets returns 200/405/etc as before, with
// no Authorization header required.
func TestServer_AuthNil_NoRegression(t *testing.T) {
	srv := newAuthTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// GET /v1/assets without any Authorization header must reach the
	// inner handler, not the (absent) auth middleware.
	resp, err := http.Get(ts.URL + "/v1/assets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("Auth==nil but got %d — middleware engaged on disabled auth", resp.StatusCode)
	}
}

// TestServer_AuthEnabled_GatesEverything: turn auth on and confirm
// the same /v1/assets call without a token now returns 401.
func TestServer_AuthEnabled_GatesEverything(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/assets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Auth enabled, no token → status = %d, want 401", resp.StatusCode)
	}
}

// --- NewStaticTokenVerifier load-time validation ---------------------------

func TestStaticTokenVerifier_RejectsBadScopeAtLoad(t *testing.T) {
	_, err := NewStaticTokenVerifier(map[string]Principal{
		"deadbeef": {Subject: "x", Role: RoleViewer, Scopes: []string{"site01..bad"}},
	})
	if err == nil {
		t.Fatalf("NewStaticTokenVerifier accepted bad scope; want error")
	}
}

func TestStaticTokenVerifier_HashCollisionMiss(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)

	if _, err := v.Verify(viewerTok); err != nil {
		t.Fatalf("Verify(viewerTok) err = %v", err)
	}
	if _, err := v.Verify("anything-else"); err != ErrUnauthorized {
		t.Fatalf("Verify(unknown) err = %v, want ErrUnauthorized", err)
	}
	if _, err := v.Verify(""); err != ErrUnauthorized {
		t.Fatalf("Verify(\"\") err = %v, want ErrUnauthorized", err)
	}
}

// --- authorize() direct matrix ---------------------------------------------

func TestAuthorize_Matrix(t *testing.T) {
	cases := []struct {
		name   string
		role   Role
		scopes []string
		action Action
		path   string
		allow  bool
	}{
		// viewer
		{"viewer read in scope literal", RoleViewer, []string{"site01.pod002.cdu000"}, ActionRead, "site01.pod002.cdu000", true},
		{"viewer read subtree implied", RoleViewer, []string{"site01.pod002"}, ActionRead, "site01.pod002.cdu000.fan000.rpm", true},
		{"viewer read out of scope", RoleViewer, []string{"site01.pod001"}, ActionRead, "site01.pod002.cdu000", false},
		{"viewer cannot write", RoleViewer, []string{"site01.**"}, ActionControlWrite, "site01.pod002.cdu000.fan000.rpm", false},
		{"viewer cannot apply", RoleViewer, []string{"**"}, ActionApply, "site01.pod002.cdu000", false},

		// operator
		{"operator read subtree implied", RoleOperator, []string{"site01.pod002"}, ActionRead, "site01.pod002.cdu000.fan000.rpm", true},
		{"operator write needs explicit subtree", RoleOperator, []string{"site01.pod002"}, ActionControlWrite, "site01.pod002.cdu000.fan000.rpm", false},
		{"operator write with explicit subtree", RoleOperator, []string{"site01.pod002.**"}, ActionControlWrite, "site01.pod002.cdu000.fan000.rpm", true},
		{"operator cannot apply", RoleOperator, []string{"**"}, ActionApply, "site01.pod002.cdu000", false},

		// admin
		{"admin bypass apply", RoleAdmin, nil, ActionApply, "anywhere", true},
		{"admin bypass write", RoleAdmin, nil, ActionControlWrite, "anywhere", true},

		// unknown role fails closed
		{"unknown role fails closed", Role("tenant"), []string{"**"}, ActionRead, "site01.pod002", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Principal{Subject: "test", Role: tc.role, Scopes: tc.scopes}
			err := authorize(p, tc.action, tc.path)
			gotAllow := err == nil
			if gotAllow != tc.allow {
				t.Fatalf("authorize(%v,%v,%v) allow=%v, want %v (err=%v)",
					tc.role, tc.action, tc.path, gotAllow, tc.allow, err)
			}
		})
	}
}

// TestMapRequest_NonAPIBypass: /metrics, /healthz, /foo all return
// isAPI=false so the middleware does not gate them.
func TestMapRequest_NonAPIBypass(t *testing.T) {
	for _, u := range []string{"/", "/metrics", "/healthz", "/v2/anything"} {
		req := httptest.NewRequest(http.MethodGet, u, nil)
		_, _, isAPI := mapRequest(req)
		if isAPI {
			t.Errorf("mapRequest(%q) isAPI=true, want false", u)
		}
	}
}

func TestMapRequest_KnownEndpoints(t *testing.T) {
	cases := []struct {
		method, url string
		wantAction  Action
		wantPath    string
		wantAPI     bool
	}{
		{http.MethodGet, "/v1/assets", ActionRead, "**", true},
		{http.MethodGet, "/v1/assets/site01.pod002.cdu000", ActionRead, "site01.pod002.cdu000", true},
		{http.MethodPut, "/v1/assets/site01.pod002.cdu000", ActionApply, "site01.pod002.cdu000", true},
		{http.MethodDelete, "/v1/assets/site01.pod002.cdu000", ActionApply, "site01.pod002.cdu000", true},
		{http.MethodGet, "/v1/alarms", ActionRead, "**", true},
		{http.MethodGet, "/v1/reports/ops", ActionRead, "**", true},
		{http.MethodGet, "/v1/metrics/query", ActionRead, "**", true},
		{http.MethodGet, "/v1/metrics/query_range", ActionRead, "**", true},
		{http.MethodGet, "/v1/points/site01.pod002.cdu000.fan000.rpm", ActionRead, "site01.pod002.cdu000.fan000.rpm", true},
		{http.MethodPut, "/v1/points/site01.pod002.cdu000.fan000.rpm:set", ActionControlWrite, "site01.pod002.cdu000.fan000.rpm", true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.url, nil)
		a, p, isAPI := mapRequest(req)
		if a != tc.wantAction || p != tc.wantPath || isAPI != tc.wantAPI {
			t.Errorf("%s %s → (%v,%q,%v), want (%v,%q,%v)",
				tc.method, tc.url, a, p, isAPI, tc.wantAction, tc.wantPath, tc.wantAPI)
		}
	}
}

// --- hashToken helper sanity (so the example yaml document is testable) ----

func TestHashToken_Deterministic(t *testing.T) {
	a := hashToken("hello")
	b := hashToken("hello")
	if a != b {
		t.Fatalf("hashToken not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hashToken length = %d, want 64", len(a))
	}
}

// TestBearerFromHeader_CaseInsensitiveScheme: confirm "bearer xyz"
// and "BEARER xyz" are accepted even though RFC 6750 says the scheme
// is case-insensitive — we accept the case-insensitive prefix but
// require exactly one space.
func TestBearerFromHeader_CaseInsensitiveScheme(t *testing.T) {
	if _, ok := bearerFromHeader("bearer abc"); !ok {
		t.Errorf("lowercase bearer rejected")
	}
	if _, ok := bearerFromHeader("BEARER abc"); !ok {
		t.Errorf("UPPERCASE bearer rejected")
	}
	if _, ok := bearerFromHeader("Basic abc"); ok {
		t.Errorf("Basic scheme accepted as Bearer")
	}
}

// TestAuthMW_KnownTokenButOutOfScopeRead: deny-forbidden on a read
// with a valid token whose scope does not cover the requested path.
func TestAuthMW_KnownTokenButOutOfScopeRead(t *testing.T) {
	v, viewerTok, _, _ := buildVerifierForRoles(t,
		[]string{"site01.pod001"},
		nil, nil)
	h := captureMW(v, passthroughInner(&Principal{}, &sync.Mutex{}), &auditCapture{})

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/site02.pod000.cdu000", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
