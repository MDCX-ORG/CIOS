// pkg/authn/oidc_test.go — table-driven tests for the OIDC Verifier
// and the /auth/{realm}/* handlers (PRMT-102 §5).
//
// Mock IdP / JWKS via net/http/httptest so we exercise the real
// discovery + JWKS + token-endpoint flow with stdlib crypto. We
// DO NOT add any third-party dependency (PRMT-102 §6).
//
// Coverage (PRMT-102 §5):
//   - happy path: discovery → JWKS → code exchange → id_token
//     verify (RS256), all claims (sub/iss/aud/exp/nonce) match.
//   - bad signature: id_token signed by a different RSA key,
//     VerifyIDToken returns error.
//   - wrong nonce: /callback rejects because id_token nonce != state.
//   - expired id_token: exp in the past → rejected.
//   - wrong realm: state cookie minted for "ops" presented at
//     /auth/customer/callback → 401.
//   - unknown realm: /auth/admin/login → 404.
//   - tampered session cookie: covered in session_test.go.
package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------- mock IdP helpers ----------------------------------------

// idpMock bundles an httptest server that serves /.well-known/
// openid-configuration, /jwks, and /token. Tests inject per-
// request id_token behaviour through tokenHandler.
type idpMock struct {
	t            *testing.T
	srv          *httptest.Server
	issuer       string
	signKey      *rsa.PrivateKey
	ecSignKey    *ecdsa.PrivateKey
	exposedKeyID string // kid of the key advertised in JWKS
	signedAlg    string // "RS256" or "ES256"

	tokenHandler func(code, codeVerifier, clientID, redirectURI string) (idToken string, err error)
}

func newIDPMock(t *testing.T, tokenHandler func(string, string, string, string) (string, error)) *idpMock {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	m := &idpMock{
		t:            t,
		signKey:      rsaKey,
		ecSignKey:    ecKey,
		exposedKeyID: "k1",
		signedAlg:    "RS256",
		tokenHandler: tokenHandler,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("/jwks", m.handleJWKS)
	mux.HandleFunc("/token", m.handleToken)
	m.srv = httptest.NewServer(mux)
	m.issuer = m.srv.URL
	return m
}

func (m *idpMock) close() { m.srv.Close() }

func (m *idpMock) jwks() jwksDoc {
	switch m.signedAlg {
	case "ES256":
		pub := m.ecSignKey.Public().(*ecdsa.PublicKey)
		return jwksDoc{Keys: []jwk{{
			Kty: "EC", Kid: m.exposedKeyID, Use: "sig", Alg: "ES256",
			Crv: "P-256",
			X:   base64.RawURLEncoding.EncodeToString(pub.X.Bytes()),
			Y:   base64.RawURLEncoding.EncodeToString(pub.Y.Bytes()),
		}}}
	default:
		pub := m.signKey.Public().(*rsa.PublicKey)
		return jwksDoc{Keys: []jwk{{
			Kty: "RSA", Kid: m.exposedKeyID, Use: "sig", Alg: "RS256",
			N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(bigBytes(uint64(pub.E))),
		}}}
	}
}

func (m *idpMock) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(discoveryDoc{
		Issuer:        m.issuer,
		AuthEndpoint:  m.issuer + "/authorize",
		TokenEndpoint: m.issuer + "/token",
		JWKSURI:       m.issuer + "/jwks",
	})
}

func (m *idpMock) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.jwks())
}

func (m *idpMock) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idTok, err := m.tokenHandler(
		r.Form.Get("code"),
		r.Form.Get("code_verifier"),
		r.Form.Get("client_id"),
		r.Form.Get("redirect_uri"),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id_token": idTok})
}

// makeIDToken signs claims with the active key and returns the
// compact JWS.
func (m *idpMock) makeIDToken(claims Claims) string {
	header := jwtHeader{Alg: m.signedAlg, Kid: m.exposedKeyID, Typ: "JWT"}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	seg := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	h := sha256.Sum256([]byte(seg))
	var sig []byte
	var err error
	if m.signedAlg == "ES256" {
		sig, err = signECDSA(m.ecSignKey, h[:])
	} else {
		sig, err = rsa.SignPKCS1v15(rand.Reader, m.signKey, crypto.SHA256, h[:])
	}
	if err != nil {
		m.t.Fatalf("sign: %v", err)
	}
	return seg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// signECDSA returns the fixed-size R||S bytes (JWS form).
func signECDSA(k *ecdsa.PrivateKey, msg []byte) ([]byte, error) {
	h := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		return nil, err
	}
	rb, sb := r.Bytes(), s.Bytes()
	out := make([]byte, 64)
	copy(out[32-len(rb):32], rb)
	copy(out[64-len(sb):], sb)
	return out, nil
}

// bigBytes is a tiny big-endian uint64 encoder (we only need it
// for the RSA public exponent, which fits comfortably in 8 bytes).
func bigBytes(v uint64) []byte {
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	// Strip leading zero bytes.
	for len(out) > 1 && out[0] == 0 {
		out = out[1:]
	}
	return out
}

// ---------- test harness -------------------------------------------

// mintEnv assembles (Handler, *idpMock, *oidcVerifier) ready to
// drive /login /callback /logout against. The verifier is the
// concrete type so tests can call VerifyIDToken directly.
func mintEnv(t *testing.T) (*Handler, *idpMock, *oidcVerifier) {
	t.Helper()
	idp := newIDPMock(t, nil)
	cfg := OIDCConfig{
		Realm:       RealmOps,
		IssuerURL:   idp.issuer,
		ClientID:    "test-client",
		RedirectURL: "https://portal.test/auth/ops/callback",
	}
	v, err := NewOIDCVerifier(cfg)
	if err != nil {
		idp.close()
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	h, err := NewHandler(HandlerConfig{
		Verifiers:          map[Realm]Verifier{RealmOps: v},
		SessionKey:         []byte("test-session-key-must-be-long-enough-32"),
		RedirectAfterLogin: "/portal/ops",
	})
	if err != nil {
		idp.close()
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(idp.close)
	return h, idp, v.(*oidcVerifier)
}

// mintEnvTwoRealms is the same as mintEnv but also wires a
// customer-realm verifier. Used by cross-realm tests so the path
// /auth/customer/* is reachable and the realm guard inside
// /callback is exercised.
func mintEnvTwoRealms(t *testing.T) (*Handler, *idpMock, *oidcVerifier) {
	t.Helper()
	idp := newIDPMock(t, nil)
	ops, err := NewOIDCVerifier(OIDCConfig{
		Realm: RealmOps, IssuerURL: idp.issuer, ClientID: "test-client",
		RedirectURL: "https://portal.test/auth/ops/callback",
	})
	if err != nil {
		idp.close()
		t.Fatalf("NewOIDCVerifier(ops): %v", err)
	}
	cust, err := NewOIDCVerifier(OIDCConfig{
		Realm: RealmCustomer, IssuerURL: idp.issuer, ClientID: "test-client",
		RedirectURL: "https://portal.test/auth/customer/callback",
	})
	if err != nil {
		idp.close()
		t.Fatalf("NewOIDCVerifier(customer): %v", err)
	}
	h, err := NewHandler(HandlerConfig{
		Verifiers: map[Realm]Verifier{
			RealmOps:      ops,
			RealmCustomer: cust,
		},
		SessionKey:         []byte("test-session-key-must-be-long-enough-32"),
		RedirectAfterLogin: "/portal/ops",
	})
	if err != nil {
		idp.close()
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(idp.close)
	return h, idp, ops.(*oidcVerifier)
}

// happyIDToken returns a claims map wired to the supplied IdP.
func happyIDToken(issuer, clientID, sub, nonce string, exp time.Time) Claims {
	return Claims{
		"iss":   issuer,
		"aud":   clientID,
		"sub":   sub,
		"nonce": nonce,
		"exp":   float64(exp.Unix()),
		"iat":   float64(time.Now().Unix()),
	}
}

// driveLogin performs /login and returns (state, nonce, pkceVerifier,
// stateCookieValue). Tests then build the /callback request from
// these — using the SAME stateCookieValue as the browser would
// carry, so HMAC verification succeeds.
func driveLogin(t *testing.T, h *Handler, idp *idpMock) (state, nonce, verifier, stateCookie string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/ops/login", nil)
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("/login status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("bad login URL: %v", err)
	}
	q := u.Query()
	state = q.Get("state")
	nonce = q.Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("login URL missing state/nonce: %q", loc)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c.Value
		}
	}
	if stateCookie == "" {
		t.Fatalf("no state cookie from /login")
	}
	payload, err := h.decodeStateCookie(RealmOps, stateCookie)
	if err != nil {
		t.Fatalf("decode state cookie: %v", err)
	}
	verifier = payload.CodeVerifier
	_ = idp
	return state, nonce, verifier, stateCookie
}

// ---------- Verifier tests -----------------------------------------

func TestVerifier_HappyPath(t *testing.T) {
	_, idp, v := mintEnv(t)
	nonce := "nonce-abc"
	idp.tokenHandler = func(_, verifier, clientID, _ string) (string, error) {
		if verifier == "" {
			return "", errors.New("verifier empty")
		}
		return idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "user-1", nonce,
			time.Now().Add(time.Hour))), nil
	}
	claims, err := v.Exchange(context.Background(), RealmOps, "code-1", "verifier-xyz", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims["sub"] != "user-1" {
		t.Errorf("sub = %v, want user-1", claims["sub"])
	}
	if claims["iss"] != idp.issuer {
		t.Errorf("iss = %v, want %s", claims["iss"], idp.issuer)
	}
	if claims["nonce"] != nonce {
		t.Errorf("nonce = %v, want %s", claims["nonce"], nonce)
	}
}

func TestVerifier_BadSignature(t *testing.T) {
	_, idp, v := mintEnv(t)
	wrong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	header := jwtHeader{Alg: "RS256", Kid: "ghost", Typ: "JWT"}
	hb, _ := json.Marshal(header)
	claims := happyIDToken(idp.issuer, "test-client", "x", "n", time.Now().Add(time.Hour))
	cb, _ := json.Marshal(claims)
	seg := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	h := sha256.Sum256([]byte(seg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, wrong, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok := seg + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := v.VerifyIDToken(context.Background(), tok, "n"); err == nil {
		t.Fatalf("VerifyIDToken accepted token signed by foreign key")
	}
}

func TestVerifier_Expired(t *testing.T) {
	_, idp, v := mintEnv(t)
	tok := idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "u", "n",
		time.Now().Add(-time.Minute)))
	if _, err := v.VerifyIDToken(context.Background(), tok, "n"); err == nil {
		t.Fatalf("VerifyIDToken accepted expired token")
	}
}

func TestVerifier_WrongAudience(t *testing.T) {
	_, idp, v := mintEnv(t)
	tok := idp.makeIDToken(happyIDToken(idp.issuer, "wrong-client", "u", "n",
		time.Now().Add(time.Hour)))
	if _, err := v.VerifyIDToken(context.Background(), tok, "n"); err == nil {
		t.Fatalf("VerifyIDToken accepted token with wrong aud")
	}
}

func TestVerifier_WrongNonce(t *testing.T) {
	_, idp, v := mintEnv(t)
	tok := idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "u", "actual",
		time.Now().Add(time.Hour)))
	if _, err := v.VerifyIDToken(context.Background(), tok, "expected"); err == nil {
		t.Fatalf("VerifyIDToken accepted token with nonce mismatch")
	}
}

func TestVerifier_RealmMismatch(t *testing.T) {
	_, _, v := mintEnv(t)
	if _, err := v.Exchange(context.Background(), RealmCustomer, "c", "v", "n"); err == nil {
		t.Fatalf("Exchange accepted cross-realm call")
	}
}

// ---------- Handler tests ------------------------------------------

func TestHandler_Login_Redirects(t *testing.T) {
	h, idp, _ := mintEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/ops/login", nil)
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, idp.issuer+"/authorize") {
		t.Fatalf("Location = %q, want %s/authorize prefix", loc, idp.issuer)
	}
	if !strings.Contains(loc, "code_challenge=") ||
		!strings.Contains(loc, "code_challenge_method=S256") {
		t.Errorf("Location missing PKCE challenge: %q", loc)
	}
	if !strings.Contains(loc, "state=") || !strings.Contains(loc, "nonce=") {
		t.Errorf("Location missing state/nonce: %q", loc)
	}
	var saw bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookieName {
			saw = true
			if !c.HttpOnly {
				t.Errorf("state cookie HttpOnly = false")
			}
			if !c.Secure {
				t.Errorf("state cookie Secure = false")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("state cookie SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if !saw {
		t.Fatalf("no state cookie set")
	}
}

func TestHandler_Callback_Happy(t *testing.T) {
	h, idp, _ := mintEnv(t)
	state, nonce, _, stateCookie := driveLogin(t, h, idp)
	idp.tokenHandler = func(code, cv, clientID, redirectURI string) (string, error) {
		if code != "auth-code" {
			return "", fmt.Errorf("code = %q", code)
		}
		if cv == "" {
			return "", errors.New("verifier empty")
		}
		_ = clientID
		_ = redirectURI
		return idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "user-7", nonce,
			time.Now().Add(time.Hour))), nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/ops/callback?code=auth-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/portal/ops" {
		t.Errorf("Location = %q, want /portal/ops", loc)
	}
	var sess string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sess = c.Value
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie flags wrong: H=%v S=%v SS=%v",
					c.HttpOnly, c.Secure, c.SameSite)
			}
		}
	}
	if sess == "" {
		t.Fatalf("no session cookie set")
	}
}

func TestHandler_Callback_ExpiredToken(t *testing.T) {
	h, idp, _ := mintEnv(t)
	state, nonce, _, stateCookie := driveLogin(t, h, idp)
	idp.tokenHandler = func(_, _, _, _ string) (string, error) {
		return idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "u", nonce,
			time.Now().Add(-time.Minute))), nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/ops/callback?code=auth-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Errorf("session cookie must not be set on failure (val=%q)", c.Value)
		}
	}
}

func TestHandler_Callback_WrongNonce(t *testing.T) {
	h, idp, _ := mintEnv(t)
	state, _, _, stateCookie := driveLogin(t, h, idp)
	idp.tokenHandler = func(_, _, _, _ string) (string, error) {
		// id_token nonce ≠ what /login stored.
		return idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "u", "TAMPERED",
			time.Now().Add(time.Hour))), nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/ops/callback?code=auth-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_Callback_WrongRealm(t *testing.T) {
	// Register verifiers for BOTH realms so the cross-realm check
	// is reached (otherwise the missing-customer-verifier path
	// returns 404, which doesn't exercise the realm guard).
	h, idp, _ := mintEnvTwoRealms(t)
	state, nonce, _, stateCookie := driveLogin(t, h, idp)
	idp.tokenHandler = func(_, _, _, _ string) (string, error) {
		return idp.makeIDToken(happyIDToken(idp.issuer, "test-client", "u", nonce,
			time.Now().Add(time.Hour))), nil
	}
	// ops state cookie presented at customer callback URL.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/customer/callback?code=auth-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_Callback_UnknownRealm(t *testing.T) {
	h, _, _ := mintEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/admin/login", nil)
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_Logout_ClearsCookie(t *testing.T) {
	h, _, _ := mintEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/ops/logout", nil)
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	var saw bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			saw = true
			if c.MaxAge >= 0 {
				t.Errorf("session cookie MaxAge = %d, want < 0", c.MaxAge)
			}
		}
	}
	if !saw {
		t.Errorf("logout must clear the session cookie (MaxAge=-1)")
	}
}
