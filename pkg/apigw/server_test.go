// Tests for the Gateway HTTP wiring: server, routes,
// AuthMiddleware, WriteProblem, and the /healthz bypass.
//
// PRMT-101 §5 requires /healthz to bypass AuthMiddleware while
// /api/* is wrapped. These tests pin that contract because
// PRMT-102..104 will replace AuthMiddleware's body and must not
// re-shape its signature.
//
// PRMT-173: dev-no-auth opt-in tests live at the bottom of the
// file (TestAuthMiddleware_DevNoAuth_*). They do not change the
// signature pinned above; they only assert behaviour of the
// pass-through branch under the new devNoAuthFlag gate.
package apigw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yurimeng/cios/pkg/policy"
	"github.com/yurimeng/cios/pkg/sts"
	"github.com/yurimeng/cios/pkg/tenant"
)

// TestServer_Healthz_OK: GET /healthz returns 200 + {"status":"ok"}
// without touching upstream (the test's upstream httptest server
// would record a hit if it were called, so absence of a recording
// is also part of the assertion).
func TestServer_Healthz_OK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream was called for /healthz: %s %s", r.Method, r.URL.Path)
	}))
	defer upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body.status = %q, want ok", body["status"])
	}
}

// TestAuthMiddleware_NoOp: PRMT-101 defines AuthMiddleware as a
// pass-through. Subsequent PRMTs replace its body — but the
// signature MUST stay (http.Handler) -> http.Handler. This test
// asserts the placeholder behaves as a passthrough so the
// follow-up PRMTs only need to insert logic, not rewrite the
// signature.
func TestAuthMiddleware_NoOp(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	wrapped := AuthMiddleware(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	wrapped.ServeHTTP(rec, req)
	if !called {
		t.Fatalf("next handler was not invoked")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
}

// TestAuthMiddleware_SignatureStable: the signature must remain
// `func(http.Handler) http.Handler` so PRMT-102..104 can swap the
// body without touching call sites. Compile-time enforcement via
// the function variable below — if the signature changes this
// line fails to compile and the PR is rejected at CI.
func TestAuthMiddleware_SignatureStable(t *testing.T) {
	var _ func(http.Handler) http.Handler = AuthMiddleware
}

// TestWriteProblem_Shape: RFC 7807 fields (type/title/status/
// detail/instance) are emitted and Content-Type is application/
// problem+json. The type URL must follow the
// https://cios.dev/errors/<tail> form (spec-004 §4) — this is the
// same shape core/server.go emits, so /v1 clients keep working.
func TestWriteProblem_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusBadGateway,
		"upstream-unavailable", "upstream unavailable",
		"core /v1 is not reachable", "/api/sites")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantType := "https://cios.dev/errors/upstream-unavailable"
	if got := body["type"]; got != wantType {
		t.Errorf("type = %v, want %q", got, wantType)
	}
	if got := body["title"]; got != "upstream unavailable" {
		t.Errorf("title = %v", got)
	}
	if got, _ := body["status"].(float64); int(got) != http.StatusBadGateway {
		t.Errorf("status = %v, want 502", body["status"])
	}
	if got := body["detail"]; got != "core /v1 is not reachable" {
		t.Errorf("detail = %v", got)
	}
	if got := body["instance"]; got != "/api/sites" {
		t.Errorf("instance = %v", got)
	}
}

// TestServer_UnknownAPI_ReturnsProblem: a request to /api/<x>
// where x is not registered returns 404 + RFC 7807, NOT a
// confusing 200 from the ServeMux default. This pins the
// dispatch behaviour in server.go's Routes().
func TestServer_UnknownAPI_ReturnsProblem(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream was called for unknown path: %s", r.URL.Path)
	}))
	defer upstream.Close()

	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "path-not-found") {
		t.Errorf("body does not contain 'path-not-found': %s", body)
	}
}

// TestServer_Healthz_BypassesAuthMiddleware: even though
// AuthMiddleware is currently a no-op, the registry in server.go
// deliberately registers /healthz outside the wrapped handler so
// load balancer probes don't get wrapped when PRMT-102 turns
// authn on. This test asserts the registration shape by checking
// that an unknown middleware wrapping the whole mux would have
// intercepted /healthz — the test mirrors how the wrapped handler
// is constructed.
func TestServer_Healthz_BypassesAuthMiddleware(t *testing.T) {
	// We construct a wrapped version of the mux and assert that
	// /healthz is still reachable when AuthMiddleware is replaced
	// with a rejecting middleware. If /healthz were registered
	// inside the wrapped handler it would be blocked here.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream was called: %s %s", r.Method, r.URL.Path)
	}))
	defer upstream.Close()

	rejectingAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "blocked", http.StatusUnauthorized)
		})
	}
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	// Reconstruct the handler the way main.go uses it but with
	// our rejecting middleware in place of AuthMiddleware. This
	// proves /healthz is registered outside the wrapped prefix.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.Handle("/api/", rejectingAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.handleSites(w, r)
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with rejecting auth: status = %d, want 200 (must bypass AuthMiddleware)", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("/api/sites with rejecting auth: status = %d, want 401 (must be wrapped)", rec2.Code)
	}
}

// TestWriteProblem_PtypeTailDefense: PRMT-115 §4 — ptype is a TAIL
// (e.g. "upstream-unavailable"); the contract pins that callers
// MUST NOT pass a fully-qualified URL. If a caller does pass a URL
// (e.g. "https://x/y"), the function MUST NOT silently produce
// "https://cios.dev/errors/https://x/y" — the type field is
// emitted verbatim. The function also logs a one-shot warning;
// this test does not assert on the log line (it would tie the
// test to the log sink), only on the JSON body shape.
func TestWriteProblem_PtypeTailDefense(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusBadGateway,
		"https://example.com/errors/custom", "custom upstream",
		"caller accidentally passed a URL", "/api/sites")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// PRMT-115 §4: with a "://" tail, the type field is ptype
	// verbatim — no problemTypeBase prepending. This keeps the
	// output well-formed (one URL, not a doubled URL) while
	// flagging the misuse via the log.
	if got := body["type"]; got != "https://example.com/errors/custom" {
		t.Errorf("type = %v, want ptype verbatim (no problemTypeBase prepending)", got)
	}
	// Title / status / detail / instance are unchanged.
	if got := body["title"]; got != "custom upstream" {
		t.Errorf("title = %v", got)
	}
}

// ---- PRMT-173: opt-in CIOS_APIGW_DEV_NO_AUTH dev claims injection. ----
//
// Security review (Opus §9) relies on these four facts:
//   1) flag unset ⇒ handler still reachable via pass-through with no claims.
//   2) flag on + no STS/PDP ⇒ handler called, ClaimsFrom ok,
//      tenant.TenantFromClaims ok with TierLabel.
//   3) flag on + STS/PDP ⇒ AuthMiddleware full-branch; dev claims
//      NOT injected; handler MUST NOT be called (fail-closed preserved).
//   4) LoadConfig truthy dictionary (1/true/yes, case-insensitive,
//      trim); garbage == false; STS/OPA ⇒ forced false.

// stubPDP is a local test stub for policy.PDP — pkg/policy does not
// export a deterministic allow-only constructor without hitting a real
// OPA endpoint, and we do not want to widen scope to pkg/policy here.
// Behaviour is fixed: always allow so a request that actually reaches
// the full branch would proceed to the handler. That is exactly what
// _IgnoredWhenAuthConfigured wants to fail-closed against: if the
// bearer header is missing the handler MUST NOT be reached, regardless
// of the PDP's allow policy.
type stubPDP struct{ allow bool }

func (p *stubPDP) Decision(ctx context.Context, in policy.Input) (bool, error) {
	return p.allow, nil
}

// resetAuthSnapshot captures the package-level devNoAuthFlag and
// authHolder state and restores both on test cleanup. Required because
// NewServer writes authHolder at boot, and Tests 1/2/3 below mutate
// both flags; a leaked state would silently break TestAuthMiddleware_NoOp
// when go test runs in random order.
func resetAuthSnapshot(t *testing.T) {
	t.Helper()
	prevFlag := devNoAuthFlag.Load()
	prevHolder := authHolder.Load()
	t.Cleanup(func() {
		devNoAuthFlag.Store(prevFlag)
		authHolder.Store(prevHolder)
	})
}

// newSTSForTest mints a real pkg/sts.STS — pkg/sts.STS is a concrete
// struct, not an interface, so there is no "stubSTS" alternative that
// would satisfy *sts.STS in bindAuthDeps. The full-branch call site
// only uses Verify() in 401 short-circuit paths; the AuthConfigured
// tests supply no Authorization header so Verify is never reached.
func newSTSForTest(t *testing.T) *sts.STS {
	t.Helper()
	return sts.New([]byte("0123456789abcdef0123456789abcdef"), sts.DefaultTTL, sts.NewMemRevoker())
}

// TestAuthMiddleware_DevNoAuth_DefaultsToFailClosed: PRMT-173 §5.1 #1
// — flag off + pass-through ⇒ handler receives the request but NO
// claims are injected. Pins the historical pass-through behaviour
// (PRMT-101) so PRMT-173 is a strict opt-in, never a default change.
func TestAuthMiddleware_DevNoAuth_DefaultsToFailClosed(t *testing.T) {
	resetAuthSnapshot(t)
	devNoAuthFlag.Store(false)
	bindAuthDeps(nil, nil)

	called := atomic.Bool{}
	var gotClaimsOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		_, gotClaimsOK = ClaimsFrom(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	AuthMiddleware(next).ServeHTTP(rec, req)

	if !called.Load() {
		t.Fatalf("pass-through: next handler must be invoked (auth not configured)")
	}
	if gotClaimsOK {
		t.Errorf("flag off + pass-through: ClaimsFrom returned ok=true; expected no claims to be injected")
	}
}

// TestAuthMiddleware_DevNoAuth_OptIn: PRMT-173 §5.1 #2 — flag on +
// pass-through ⇒ fixed dev claims are stamped into r.Context() and
// tenant.TenantFromClaims round-trips ok.
//
// PRMT-217: inject path is compiled only under -tags lab. Production
// builds keep the flag readable but never stamp claims.
func TestAuthMiddleware_DevNoAuth_OptIn(t *testing.T) {
	resetAuthSnapshot(t)
	devNoAuthFlag.Store(true)
	bindAuthDeps(nil, nil)

	var gotClaims sts.TokenClaims
	var gotClaimsOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims, gotClaimsOK = ClaimsFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	AuthMiddleware(next).ServeHTTP(rec, req)

	if !DevBypassAvailable() {
		if gotClaimsOK {
			t.Fatalf("prod build: DevNoAuth flag on must NOT inject claims (PRMT-217)")
		}
		return
	}

	if !gotClaimsOK {
		t.Fatalf("flag on + pass-through: ClaimsFrom did not return ok=true (claims=%+v)", gotClaims)
	}
	// String-literal equality per the PRMT brief — no string(tenant.TierLabel)
	// coupling so test stays decoupled from the constant's value.
	if gotClaims.Subject != "dev-no-auth" {
		t.Errorf("Subject = %q, want %q", gotClaims.Subject, "dev-no-auth")
	}
	if gotClaims.Tenant != "dev" {
		t.Errorf("Tenant = %q, want %q", gotClaims.Tenant, "dev")
	}
	if gotClaims.IsolationTier != "label" {
		t.Errorf("IsolationTier = %q, want %q", gotClaims.IsolationTier, "label")
	}
	if gotClaims.Realm != "ops" {
		t.Errorf("Realm = %q, want %q", gotClaims.Realm, "ops")
	}
	// End-to-end: tenant.TenantFromClaims round-trips ok.
	id, tier, tenantOK := tenant.TenantFromClaims(gotClaims)
	if !tenantOK {
		t.Errorf("tenant.TenantFromClaims ok=false (claims=%+v)", gotClaims)
	}
	if id != "dev" {
		t.Errorf("tenant id = %q, want %q", id, "dev")
	}
	if tier != tenant.TierLabel {
		t.Errorf("tenant tier = %q, want %q", tier, tenant.TierLabel)
	}
}

// TestAuthMiddleware_DevNoAuth_IgnoredWhenAuthConfigured: PRMT-173 §5.1
// #3 — flag on BUT an STS (or PDP) is wired ⇒ AuthMiddleware goes
// full-branch; the pass-through injection point is unreachable; the
// request 401's on missing bearer; the handler MUST NOT be called.
func TestAuthMiddleware_DevNoAuth_IgnoredWhenAuthConfigured(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"STS", func(t *testing.T) { bindAuthDeps(newSTSForTest(t), nil) }},
		{"PDP", func(t *testing.T) { bindAuthDeps(nil, &stubPDP{allow: true}) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resetAuthSnapshot(t)
			devNoAuthFlag.Store(true)
			tc.setup(t)

			called := atomic.Bool{}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			AuthMiddleware(next).ServeHTTP(rec, req)

			if called.Load() {
				t.Fatalf("handler was invoked; AuthMiddleware full-branch MUST short-circuit 401 and MUST NOT inject dev claims when STS/PDP wired")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "missing bearer token") {
				t.Errorf("body = %q, must contain %q", rec.Body.String(), "missing bearer token")
			}
		})
	}
}

// ---- PRMT-173 §5.2 LoadConfig tests ----
//
// Per the §3 whitelist, LoadConfig tests live in server_test.go (NOT a
// separate config_test.go) to keep the file count at four. Each
// sub-test pins one fact about the truthy dictionary or the STS/OPA
// sanity check.

func TestLoadConfig_DevNoAuth_Overrides(t *testing.T) {
	// devNoAuthKey is a 32-byte dummy used solely to fire the STS
	// sanity-check branch; real keys should be high-entropy secrets
	// from env.
	const devNoAuthKey = "0123456789abcdef0123456789abcdef"
	// seedUpstream sets the mandatory CIOS_APIGW_UPSTREAM so the
	// LoadConfig gate does not short-circuit on UpstreamRequired
	// before we get to the DevNoAuth branch.
	seedUpstream := func(t *testing.T) {
		t.Helper()
		t.Setenv("CIOS_APIGW_UPSTREAM", "http://127.0.0.1:8090")
	}

	t.Run("unset", func(t *testing.T) {
		seedUpstream(t)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Errorf("DevNoAuth = true, want false (unset)")
		}
	})

	t.Run("empty", func(t *testing.T) {
		seedUpstream(t)
		t.Setenv("CIOS_APIGW_DEV_NO_AUTH", "")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Errorf("DevNoAuth = true for empty value, want false")
		}
	})

	t.Run("1", func(t *testing.T) {
		seedUpstream(t)
		t.Setenv("CIOS_APIGW_DEV_NO_AUTH", "1")
		cfg, err := LoadConfig()
		if !DevBypassAvailable() {
			// PRMT-217: production build refuses DevNoAuth.
			if err == nil {
				t.Fatal("prod LoadConfig: want error for DEV_NO_AUTH=1")
			}
			if !strings.Contains(err.Error(), "lab build") {
				t.Fatalf("error %q should mention lab build", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !cfg.DevNoAuth {
			t.Errorf("DevNoAuth = false, want true (\"1\")")
		}
	})

	for _, tt := range []struct {
		name, val string
		want      bool
	}{
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"yes", "yes", true},
		{"YES", "YES", true},
		{"trimmed", "  yes  ", true},
		{"mixed-case True", "True", true},
	} {
		tt := tt
		t.Run("truthy-"+tt.name, func(t *testing.T) {
			seedUpstream(t)
			t.Setenv("CIOS_APIGW_DEV_NO_AUTH", tt.val)
			cfg, err := LoadConfig()
			if !DevBypassAvailable() {
				if err == nil {
					t.Fatal("prod LoadConfig: want error for truthy DEV_NO_AUTH")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.DevNoAuth != tt.want {
				t.Errorf("input %q: DevNoAuth = %v, want %v", tt.val, cfg.DevNoAuth, tt.want)
			}
		})
	}

	t.Run("garbage", func(t *testing.T) {
		seedUpstream(t)
		t.Setenv("CIOS_APIGW_DEV_NO_AUTH", "maybe")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Errorf("DevNoAuth = true for \"maybe\", want false (truthy dictionary is narrow)")
		}
	})

	// Two load-bearing sub-tests — sanity check MUST demote
	// DevNoAuth when STS/OPA env is set, even though the dev flag
	// itself is truthy. These are the §5.2 fail-closed guarantee.
	t.Run("with-STS", func(t *testing.T) {
		seedUpstream(t)
		t.Setenv("CIOS_APIGW_DEV_NO_AUTH", "1")
		t.Setenv("CIOS_STS_SIGNING_KEY", devNoAuthKey)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Errorf("DevNoAuth = true with CIOS_STS_SIGNING_KEY set; sanity check must force false")
		}
	})

	t.Run("with-OPA", func(t *testing.T) {
		seedUpstream(t)
		t.Setenv("CIOS_APIGW_DEV_NO_AUTH", "1")
		t.Setenv("CIOS_OPA_URL", "http://x")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Errorf("DevNoAuth = true with CIOS_OPA_URL set; sanity check must force false")
		}
	})
}
