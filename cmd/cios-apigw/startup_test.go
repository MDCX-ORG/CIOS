// Tests for cmd/cios-apigw/startup.go (PRMT-111 §5). Coverage:
//
//   - AUTH_MODE=on with missing SESSION_KEY → fail-fast error
//     that names the missing env var (s5).
//   - AUTH_MODE=on with missing STS_SIGNING_KEY → fail-fast error
//     that names BOTH session and STS keys (s5: "不止首个").
//   - AUTH_MODE unset without ALLOW_NO_AUTH/DEV_NO_AUTH → refuse (H1).
//   - AUTH_MODE unset + ALLOW_NO_AUTH=1 → succeeds with nil verifiers.
//   - TLS env all empty → UpstreamTLS nil, UpstreamHC still has
//     a Timeout (s5: "TLS 全空退化").
//   - TLS env all set with valid PEM files → UpstreamTLS non-nil
//     with RootCAs + Certificates, UpstreamHC.Transport wired
//     (s5: "TLS 齐全构建成功").
//   - Realm half-configured (one of four vars set) → fail-fast
//     error that lists the missing realm vars.
//   - Happy path: AUTH_MODE=on, all required env set, IdP mock
//     serving discovery + JWKS → verifiers map populated for
//     that realm; buildAuthHandler returns a non-nil handler.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/apigw"
	"github.com/yurimeng/cios/pkg/authn"
)

// withEnv sets the given env vars for the duration of fn, then
// restores the prior values. Empty value means "unset". This is
// the only env-mutation path the tests use, so we keep the
// restore ordering straightforward and don't leak state between
// subtests.
func withEnv(t *testing.T, vars map[string]string, fn func()) {
	t.Helper()
	prior := map[string]*string{}
	for k := range vars {
		if v, ok := os.LookupEnv(k); ok {
			s := v
			prior[k] = &s
		} else {
			prior[k] = nil
		}
	}
	for k, v := range vars {
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	defer func() {
		for k, v := range prior {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	}()
	fn()
}

// clearAuthEnv unsets every env var loadStartupConfig reads. Call
// this at the start of each test that wants a known-clean
// baseline.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CIOS_APIGW_AUTH_MODE", "CIOS_APIGW_ALLOW_NO_AUTH", "CIOS_APIGW_DEV_NO_AUTH",
		"CIOS_APIGW_ALLOW_PUBLIC_BIND",
		"CIOS_APIGW_SESSION_KEY",
		"CIOS_STS_SIGNING_KEY", "CIOS_OPA_URL", "CIOS_MTLS_MODE",
		"CIOS_APIGW_UPSTREAM_TIMEOUT",
		"CIOS_APIGW_TLS_CA", "CIOS_APIGW_TLS_CERT", "CIOS_APIGW_TLS_KEY",
		"CIOS_OIDC_OPS_ISSUER", "CIOS_OIDC_OPS_CLIENT_ID",
		"CIOS_OIDC_OPS_CLIENT_SECRET", "CIOS_OIDC_OPS_REDIRECT_URL",
		"CIOS_OIDC_CUSTOMER_ISSUER", "CIOS_OIDC_CUSTOMER_CLIENT_ID",
		"CIOS_OIDC_CUSTOMER_CLIENT_SECRET", "CIOS_OIDC_CUSTOMER_REDIRECT_URL",
	} {
		_ = os.Unsetenv(k)
	}
}

// TestLoadStartupConfig_AuthModeOn_MissingSessionKey covers §5
// "缺必填 env → 退出码 ≠ 0 且 stderr 含缺失变量名". We assert the
// error names SESSION_KEY, not a generic message.
func TestLoadStartupConfig_AuthModeOn_MissingSessionKey(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE": "on",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "CIOS_APIGW_SESSION_KEY") {
			t.Errorf("error should name SESSION_KEY; got %q", err.Error())
		}
	})
}

// TestLoadStartupConfig_AuthModeOn_MissingBothKeyAndSTS covers
// §5 "逐个列出，不止首个" — when both session and STS keys are
// missing, the error must mention BOTH.
func TestLoadStartupConfig_AuthModeOn_MissingBothKeyAndSTS(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE": "on",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "CIOS_APIGW_SESSION_KEY") {
			t.Errorf("error should name SESSION_KEY; got %q", err.Error())
		}
		if !strings.Contains(err.Error(), "CIOS_STS_SIGNING_KEY") {
			t.Errorf("error should name STS_SIGNING_KEY; got %q", err.Error())
		}
	})
}

// TestLoadStartupConfig_AuthModeOff_RefusesWithoutOptOut covers
// CODE-SCAN H1 / L104: AUTH_MODE unset without explicit allow → error.
func TestLoadStartupConfig_AuthModeOff_RefusesWithoutOptOut(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE": "",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatal("AUTH_MODE unset without ALLOW_NO_AUTH/DEV_NO_AUTH must error")
		}
		if !strings.Contains(err.Error(), "refusing to start without auth") {
			t.Errorf("error should name the refuse reason; got %v", err)
		}
	})
}

// TestLoadStartupConfig_AuthModeOff_AllowNoAuth covers explicit
// lab opt-out: AUTH_MODE unset + ALLOW_NO_AUTH=1 → boot with nil verifiers.
func TestLoadStartupConfig_AuthModeOff_AllowNoAuth(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE":     "",
		"CIOS_APIGW_ALLOW_NO_AUTH": "1",
	}, func() {
		sc, err := loadStartupConfig()
		if err != nil {
			t.Fatalf("ALLOW_NO_AUTH opt-out should not fail; got %v", err)
		}
		if sc.AuthMode {
			t.Errorf("AuthMode should be false")
		}
		if sc.Verifiers != nil {
			t.Errorf("Verifiers should be nil in off mode; got %v", sc.Verifiers)
		}
		if sc.UpstreamHC == nil {
			t.Fatalf("UpstreamHC should be non-nil even in off mode")
		}
		if sc.UpstreamHC.Timeout == 0 {
			t.Errorf("UpstreamHC.Timeout should be > 0 in off mode")
		}
	})
}

// TestLoadStartupConfig_AuthModeOff_DevNoAuth covers the existing
// lab recipe (scripts/m3-apigw-dev.sh, portal-live.sh): DEV_NO_AUTH=1
// alone is an explicit no-auth opt-out for the H1 boot gate.
func TestLoadStartupConfig_AuthModeOff_DevNoAuth(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE":   "",
		"CIOS_APIGW_DEV_NO_AUTH": "1",
	}, func() {
		sc, err := loadStartupConfig()
		if err != nil {
			t.Fatalf("DEV_NO_AUTH opt-out should not fail; got %v", err)
		}
		if sc.AuthMode {
			t.Errorf("AuthMode should be false")
		}
	})
}

// --- PRMT-216: DevNoAuth must not bind non-loopback -------------------

func TestValidateDevNoAuthListen_LoopbackOK(t *testing.T) {
	err := validateDevNoAuthListen(apigw.Config{
		DevNoAuth:  true,
		ListenAddr: "127.0.0.1:8089",
	})
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
}

func TestValidateDevNoAuthListen_PublicRefused(t *testing.T) {
	// §5.2 #7 / R2 matrix #7: no hatch → still refuse.
	t.Setenv(envAllowPublicBind, "")
	for _, addr := range []string{":8089", "0.0.0.0:8089", ":8443"} {
		err := validateDevNoAuthListen(apigw.Config{
			DevNoAuth:  true,
			ListenAddr: addr,
		})
		if err == nil {
			t.Fatalf("%q: want refuse", addr)
		}
		if !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("%q: %v", addr, err)
		}
	}
}

func TestValidateDevNoAuthListen_PublicAllowedWithHatch(t *testing.T) {
	// R2 matrix #6: DEV_NO_AUTH + non-loopback + ALLOW_PUBLIC_BIND=1.
	t.Setenv(envAllowPublicBind, "1")
	var logs strings.Builder
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	err := validateDevNoAuthListen(apigw.Config{
		DevNoAuth:  true,
		ListenAddr: ":8089",
	})
	if err != nil {
		t.Fatalf("hatch should allow: %v", err)
	}
	if !strings.Contains(logs.String(), "every reachable client bypasses auth") {
		t.Fatalf("WARN missing hatch phrase; log=%q", logs.String())
	}
	if !strings.Contains(logs.String(), envAllowPublicBind) {
		t.Fatalf("WARN should name hatch env; log=%q", logs.String())
	}
}

func TestValidateDevNoAuthListen_NoDevNoAuth_PublicOK(t *testing.T) {
	err := validateDevNoAuthListen(apigw.Config{
		DevNoAuth:  false,
		ListenAddr: ":8443",
	})
	if err != nil {
		t.Fatalf("auth on public: %v", err)
	}
}

func TestLoadConfig_DevNoAuth_STSDemote_ThenPublicOK(t *testing.T) {
	// Case 5.2 #5: DEV_NO_AUTH=1 + STS key demotes DevNoAuth → guard no-ops.
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_UPSTREAM":    "http://127.0.0.1:8090",
		"CIOS_APIGW_DEV_NO_AUTH": "1",
		"CIOS_APIGW_LISTEN":      ":8443",
		"CIOS_STS_SIGNING_KEY":   "test-signing-key-not-for-prod",
	}, func() {
		cfg, err := apigw.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.DevNoAuth {
			t.Fatal("expected STS demote DevNoAuth=false")
		}
		if err := validateDevNoAuthListen(cfg); err != nil {
			t.Fatalf("after demote guard should pass: %v", err)
		}
	})
}

// TestLoadStartupConfig_MTLSRequire_NeedsTLSTriple covers P793:
// CIOS_MTLS_MODE=require without client cert material must fail-fast.
func TestLoadStartupConfig_MTLSRequire_NeedsTLSTriple(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_ALLOW_NO_AUTH": "1",
		"CIOS_MTLS_MODE":           "require",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatal("MTLS require without TLS triple must error")
		}
		if !strings.Contains(err.Error(), "CIOS_MTLS_MODE=require") {
			t.Errorf("error should name require gate; got %v", err)
		}
	})
}

// TestLoadStartupConfig_TLSAllEmpty covers §5 "TLS env 全空 → 退化为
// 带 timeout 的普通客户端（非 http.DefaultClient）". The client is
// the package's own timeout-only client, NOT http.DefaultClient.
func TestLoadStartupConfig_TLSAllEmpty(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_ALLOW_NO_AUTH": "1", // H1: TLS-only test; auth off by design
	}, func() {
		sc, err := loadStartupConfig()
		if err != nil {
			t.Fatalf("TLS-empty should not fail; got %v", err)
		}
		if sc.TLSEnabled {
			t.Errorf("TLSEnabled should be false when all TLS envs empty")
		}
		if sc.UpstreamTLS != nil {
			t.Errorf("UpstreamTLS should be nil when all TLS envs empty")
		}
		if sc.UpstreamHC == nil {
			t.Fatalf("UpstreamHC should be non-nil")
		}
		if sc.UpstreamHC.Timeout == 0 {
			t.Errorf("UpstreamHC.Timeout should be > 0 even without mTLS")
		}
	})
}

// TestLoadStartupConfig_TLSPartialRejected — §4/§5: "全空 → 退化",
// "非空 → 启用". A partial set (only one of three) is neither;
// loadStartupConfig must surface that as an error so the
// operator sees the misconfiguration at startup.
func TestLoadStartupConfig_TLSPartialRejected(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_TLS_CA": "/some/path",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatalf("partial TLS env (only CA) should fail")
		}
		if !strings.Contains(err.Error(), "CIOS_APIGW_TLS_CA") {
			t.Errorf("error should name TLS envs; got %q", err.Error())
		}
	})
}

// TestLoadStartupConfig_TLSFullBuild covers §5 "TLS 齐全构建成功".
// We generate a self-signed CA + leaf cert in a temp dir, point
// the three TLS env vars at them, and assert UpstreamTLS ends up
// with RootCAs and Certificates set and the transport is wired.
func TestLoadStartupConfig_TLSFullBuild(t *testing.T) {
	caPEM, certPEM, keyPEM := generateSelfSignedPair(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_ALLOW_NO_AUTH": "1", // H1: TLS-only test; auth off by design
		"CIOS_APIGW_TLS_CA":        caPath,
		"CIOS_APIGW_TLS_CERT":      certPath,
		"CIOS_APIGW_TLS_KEY":       keyPath,
	}, func() {
		sc, err := loadStartupConfig()
		if err != nil {
			t.Fatalf("TLS full build should not fail; got %v", err)
		}
		if !sc.TLSEnabled {
			t.Errorf("TLSEnabled should be true when all three envs set")
		}
		if sc.UpstreamTLS == nil {
			t.Fatalf("UpstreamTLS should be non-nil")
		}
		if sc.UpstreamTLS.RootCAs == nil {
			t.Errorf("UpstreamTLS.RootCAs should be set")
		}
		if len(sc.UpstreamTLS.Certificates) == 0 {
			t.Errorf("UpstreamTLS.Certificates should contain the client cert")
		}
		if sc.UpstreamHC == nil || sc.UpstreamHC.Transport == nil {
			t.Errorf("UpstreamHC.Transport should be wired to the mTLS Transport")
		}
	})
}

// TestLoadStartupConfig_AuthModeOn_NoRealm covers §5 "缺必填 env"
// once the key checks pass: AUTH_MODE=on with no realm env vars
// at all must error (no verifier would be built; spec-009 §7.1
// red line — a gateway with AUTH_MODE=on and no /auth/* routes
// is a misconfiguration).
func TestLoadStartupConfig_AuthModeOn_NoRealm(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE":   "on",
		"CIOS_APIGW_SESSION_KEY": "0123456789abcdef0123456789abcdef",
		"CIOS_STS_SIGNING_KEY":   "fedcba9876543210fedcba9876543210",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatalf("AUTH_MODE=on with no realm env must error")
		}
		if !strings.Contains(err.Error(), "CIOS_OIDC") {
			t.Errorf("error should name OIDC realm vars; got %q", err.Error())
		}
	})
}

// TestLoadStartupConfig_AuthModeOn_RealmHalfConfigured covers
// "realm 启用时必填" — setting only one of the four ops env
// vars must fail with the missing three named.
func TestLoadStartupConfig_AuthModeOn_RealmHalfConfigured(t *testing.T) {
	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE":   "on",
		"CIOS_APIGW_SESSION_KEY": "0123456789abcdef0123456789abcdef",
		"CIOS_STS_SIGNING_KEY":   "fedcba9876543210fedcba9876543210",
		"CIOS_OIDC_OPS_ISSUER":   "https://idp.example.com/realms/ops",
	}, func() {
		_, err := loadStartupConfig()
		if err == nil {
			t.Fatalf("half-configured realm must error")
		}
		if !strings.Contains(err.Error(), "CIOS_OIDC_OPS_CLIENT_ID") {
			t.Errorf("error should name missing ops vars; got %q", err.Error())
		}
	})
}

// TestLoadStartupConfig_AuthModeOn_RealmHappyPath exercises the
// full verifier-build path against a mock IdP serving discovery
// + JWKS. We assert the verifier map is populated for the ops
// realm and buildAuthHandler returns a non-nil handler.
func TestLoadStartupConfig_AuthModeOn_RealmHappyPath(t *testing.T) {
	idp := newIDPMockForStartup(t)
	defer idp.Close()

	clearAuthEnv(t)
	withEnv(t, map[string]string{
		"CIOS_APIGW_AUTH_MODE":        "on",
		"CIOS_APIGW_SESSION_KEY":      "0123456789abcdef0123456789abcdef",
		"CIOS_STS_SIGNING_KEY":        "fedcba9876543210fedcba9876543210",
		"CIOS_OIDC_OPS_ISSUER":        idp.Issuer(),
		"CIOS_OIDC_OPS_CLIENT_ID":     "cios-portal",
		"CIOS_OIDC_OPS_CLIENT_SECRET": "shh",
		"CIOS_OIDC_OPS_REDIRECT_URL":  "https://portal.cios.dev/auth/ops/callback",
	}, func() {
		sc, err := loadStartupConfig()
		if err != nil {
			t.Fatalf("happy path should not fail; got %v", err)
		}
		if !sc.AuthMode {
			t.Errorf("AuthMode should be true")
		}
		if _, ok := sc.Verifiers[authn.RealmOps]; !ok {
			t.Errorf("verifier for ops realm should be set; got %v", keysOfVerifiers(sc.Verifiers))
		}
		h, err := buildAuthHandler(sc, []byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatalf("buildAuthHandler: %v", err)
		}
		if h == nil {
			t.Errorf("buildAuthHandler should return non-nil handler in on mode")
		}
	})
}

// TestBuildAuthHandler_OffModeReturnsNil covers the AUTH_MODE=off
// path of buildAuthHandler — must return (nil, nil) so main
// skips the SetAuthHandler call.
func TestBuildAuthHandler_OffModeReturnsNil(t *testing.T) {
	h, err := buildAuthHandler(startupConfig{AuthMode: false}, nil)
	if err != nil {
		t.Fatalf("off mode should not error; got %v", err)
	}
	if h != nil {
		t.Errorf("off mode should return nil handler")
	}
}

// TestParseUpstreamTimeout covers the CIOS_APIGW_UPSTREAM_TIMEOUT
// resolution rules: default 10s, valid duration honoured,
// invalid duration falls back to default.
func TestParseUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"default-empty", "", 10 * time.Second},
		{"valid-3s", "3s", 3 * time.Second},
		{"valid-200ms", "200ms", 200 * time.Millisecond},
		{"invalid-fallback", "not-a-duration", 10 * time.Second},
		{"zero-fallback", "0s", 10 * time.Second},
		{"negative-fallback", "-5s", 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAuthEnv(t)
			withEnv(t, map[string]string{
				"CIOS_APIGW_UPSTREAM_TIMEOUT": tc.set,
			}, func() {
				got := parseUpstreamTimeout()
				if got != tc.want {
					t.Errorf("parseUpstreamTimeout(%q) = %v; want %v", tc.set, got, tc.want)
				}
			})
		})
	}
}

// ---------- helpers -------------------------------------------------

// generateSelfSignedPair returns (caPEM, leafPEM, leafKeyPEM) for
// a self-signed CA used as the trust anchor, and a leaf cert
// signed by that CA. The cert is good for clientAuth so the
// resulting tls.Config can be used as a client mTLS cert.
func generateSelfSignedPair(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey (ca): %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cios-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey (leaf): %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "cios-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return caPEM, certPEM, keyPEM
}

// idpMockForStartup mirrors pkg/authn/oidc_test.go's idpMock but
// lives in package main so cmd/cios-apigw tests can use it
// without depending on pkg/authn test internals. It serves the
// minimum: discovery + JWKS (no /token — main never exchanges a
// code at startup).
type idpMockForStartup struct {
	srv    *httptest.Server
	issuer string
	sign   *rsa.PrivateKey
	kid    string
}

func newIDPMockForStartup(t *testing.T) *idpMockForStartup {
	t.Helper()
	sign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	m := &idpMockForStartup{sign: sign, kid: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("/jwks", m.handleJWKS)
	m.srv = httptest.NewServer(mux)
	m.issuer = m.srv.URL
	return m
}

func (m *idpMockForStartup) Issuer() string { return m.issuer }
func (m *idpMockForStartup) Close()         { m.srv.Close() }

func (m *idpMockForStartup) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"issuer":                 m.issuer,
		"authorization_endpoint": m.issuer + "/authorize",
		"token_endpoint":         m.issuer + "/token",
		"jwks_uri":               m.issuer + "/jwks",
	})
}

func (m *idpMockForStartup) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := m.sign.Public().(*rsa.PublicKey)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": m.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(bigIntBytes(pub.E)),
		}},
	})
}

func bigIntBytes(e int) []byte {
	// e is the public exponent; serialise as big-endian bytes.
	out := []byte{}
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	if len(out) == 0 {
		return []byte{0}
	}
	return out
}

func keysOfVerifiers(m map[authn.Realm]authn.Verifier) []authn.Realm {
	out := make([]authn.Realm, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
