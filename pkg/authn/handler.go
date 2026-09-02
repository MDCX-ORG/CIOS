// pkg/authn/handler.go — HTTP handlers for /auth/{realm}/*.
//
// PRMT-102 §2-bis:
//
//	/auth/{realm}/login    → 302 to IdP authorize (state, nonce, PKCE)
//	/auth/{realm}/callback → verify id_token, mint session cookie, 302
//	/auth/{realm}/logout   → clear session cookie, 302
//
// Realm is the first URL segment. The handlers refuse any value
// outside {"ops", "customer"} so a request to /auth/admin/login
// never reaches the IdP.
//
// PRMT-102 §5 (MUST):
//   - state / nonce / PKCE-verifier kept in a short-lived
//     HttpOnly+Secure cookie (stateCookieName in realm.go),
//     cleared after /callback.
//   - session cookie: HttpOnly + Secure + SameSite=Lax,
//     HMAC-signed (EncodeSession / DecodeSession).
//   - Cross-realm callback/state rejected (a customer cookie
//     presented at /auth/ops/callback does not satisfy the
//     state-cookie check).
//   - Any verify failure → 401 RFC7807. No session minted.
//
// PRMT-102 §6 (MUST NOT):
//   - No token exchange (PRMT-103).
//   - No authorization decisions (PRMT-104).
//   - No new third-party deps.
package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Handler holds the per-realm Verifiers and the session-signing
// key. The key is sourced from env CIOS_APIGW_SESSION_KEY in
// cmd/cios-apigw/main.go (out of scope for this file; the
// requirement is "no hardcoded key", see PRMT-102 §3).
type Handler struct {
	verifiers  map[Realm]Verifier
	sessionKey []byte

	// redirectAfterLogin is where /callback sends the browser
	// after a successful login. Defaults to "/" if empty.
	redirectAfterLogin string

	// stateTTL bounds how long a state cookie stays valid.
	// OIDC core §3.1.2.6 recommends ≤1 minute; we use 5 minutes
	// to absorb clock skew between browser and gateway.
	stateTTL time.Duration
}

// HandlerConfig configures Handler. Verifiers is keyed by Realm;
// at least one entry is required (otherwise /login 404s).
type HandlerConfig struct {
	Verifiers          map[Realm]Verifier
	SessionKey         []byte
	RedirectAfterLogin string
	StateTTL           time.Duration
}

// NewHandler builds a Handler. Returns an error if SessionKey is
// empty (PRMT-102 §3 — must be sourced from env, never hardcoded)
// or no Verifier is registered.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if len(cfg.SessionKey) == 0 {
		return nil, errors.New("authn: SessionKey is empty (must come from CIOS_APIGW_SESSION_KEY)")
	}
	if len(cfg.Verifiers) == 0 {
		return nil, errors.New("authn: no Verifiers registered")
	}
	h := &Handler{
		verifiers:          cfg.Verifiers,
		sessionKey:         cfg.SessionKey,
		redirectAfterLogin: cfg.RedirectAfterLogin,
		stateTTL:           cfg.StateTTL,
	}
	if h.redirectAfterLogin == "" {
		h.redirectAfterLogin = "/"
	}
	if h.stateTTL <= 0 {
		h.stateTTL = 5 * time.Minute
	}
	return h, nil
}

// problemTypeBase mirrors pkg/apigw's RFC 7807 type URL base so
// pkg/authn can emit the same shape without depending on
// pkg/apigw (spec-009 §7.1: authn is independent of the
// upstream-reverse-proxy path).
const problemTypeBase = "https://cios.dev/errors/"

// writeProblem emits an RFC 7807 Problem Details body
// (spec-004 §4).
func writeProblem(w http.ResponseWriter, status int, ptype, title, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     problemTypeBase + ptype,
		"title":    title,
		"status":   status,
		"detail":   detail,
		"instance": instance,
	})
}

// statePayload is what we hide inside the state cookie. The
// verifier on /callback decodes it, checks realm matches the
// route, and consumes it (one-shot).
type statePayload struct {
	Realm        Realm  `json:"realm"`
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"cv"`
	IssuedAt     int64  `json:"iat"`
	RedirectTo   string `json:"to,omitempty"`
}

// Login handles GET /auth/{realm}/login. It mints a state/nonce/
// PKCE triple, stores it in a short-lived cookie, and 302s the
// browser to the IdP authorize endpoint.
//
// PRMT-102 §5: state/nonce一次性（存 short-TTL，回调校验后失效）.
// We achieve "one-shot" by clearing the cookie in /callback
// regardless of success.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	realm, ok := h.realmFromPath(w, r)
	if !ok {
		return
	}
	verifier, ok := h.verifierFor(w, realm, r)
	if !ok {
		return
	}

	// PKCE: code_verifier is 32 random bytes (43 base64url chars),
	// code_challenge is base64url(SHA-256(code_verifier)).
	verifierBytes, err := randomBytes(32)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError,
			"internal", "could not mint PKCE verifier",
			err.Error(), r.URL.Path)
		return
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	state, err := randomString(32)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError,
			"internal", "could not mint state",
			err.Error(), r.URL.Path)
		return
	}
	nonce, err := randomString(32)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError,
			"internal", "could not mint nonce",
			err.Error(), r.URL.Path)
		return
	}

	// Honour ?next=/some/path for post-login redirect, capped to
	// same-origin to defeat open-redirect. Anything else falls
	// back to the configured RedirectAfterLogin.
	next := h.redirectAfterLogin
	if q := r.URL.Query().Get("next"); q != "" && isSafeRedirect(q) {
		next = q
	}

	payload := statePayload{
		Realm:        realm,
		State:        state,
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		IssuedAt:     time.Now().Unix(),
		RedirectTo:   next,
	}
	if err := h.writeStateCookie(w, realm, payload); err != nil {
		writeProblem(w, http.StatusInternalServerError,
			"internal", "could not write state cookie",
			err.Error(), r.URL.Path)
		return
	}

	authURL := verifier.AuthCodeURL(realm, state, nonce, codeChallenge)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback handles GET /auth/{realm}/callback?code=...&state=...
// It validates the state cookie, exchanges the code, verifies
// the id_token, mints a session cookie, and 302s to the portal.
//
// Any failure (state mismatch, signature, expiry, nonce,
// audience) → 401 RFC7807, no session.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	realm, ok := h.realmFromPath(w, r)
	if !ok {
		return
	}
	verifier, ok := h.verifierFor(w, realm, r)
	if !ok {
		return
	}

	// Pull state cookie first; we need it before we know whether
	// to trust ?state=.
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "missing state cookie",
			"the OIDC state cookie was not sent; restart login", r.URL.Path)
		return
	}
	payload, err := h.decodeStateCookie(realm, cookie.Value)
	if err != nil {
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "invalid state cookie",
			"the OIDC state cookie failed to decode; restart login", r.URL.Path)
		return
	}
	// Cross-realm guard: state cookie for realm A cannot be
	// presented at /auth/<B>/callback.
	if payload.Realm != realm {
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "realm mismatch",
			"the OIDC state cookie realm does not match the callback URL", r.URL.Path)
		return
	}
	// State in cookie must equal ?state= in URL.
	if got := r.URL.Query().Get("state"); got != payload.State {
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "state mismatch",
			"the OIDC ?state= parameter does not match the state cookie", r.URL.Path)
		return
	}
	// One-shot: clear the cookie regardless of outcome.
	clearStateCookie(w, realm)

	code := r.URL.Query().Get("code")
	if code == "" {
		writeProblem(w, http.StatusBadRequest,
			"bad-request", "missing code",
			"OIDC callback missing ?code= parameter", r.URL.Path)
		return
	}

	claims, err := verifier.Exchange(r.Context(), realm, code, payload.CodeVerifier, payload.Nonce)
	if err != nil {
		// We deliberately don't echo err to the browser beyond a
		// generic "unauthorized" — surfacing the inner failure
		// (e.g. "id_token expired") is a small information leak.
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "OIDC exchange failed",
			"OIDC token exchange rejected; restart login", r.URL.Path)
		return
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		writeProblem(w, http.StatusUnauthorized,
			"unauthorized", "id_token missing sub",
			"OIDC id_token has no sub claim", r.URL.Path)
		return
	}

	// Compute the session expiry from id_token `exp` if present,
	// otherwise SessionTTL from session.go.
	var exp time.Time
	if expF, ok := claims["exp"].(float64); ok {
		exp = time.Unix(int64(expF), 0)
	} else {
		exp = time.Now().Add(SessionTTL)
	}
	sess := Session{
		subject:   sub,
		realm:     realm,
		expiresAt: exp.Unix(),
		claims:    claims,
	}
	cookieValue, err := EncodeSession(h.sessionKey, sess)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError,
			"internal", "could not encode session",
			err.Error(), r.URL.Path)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	})

	dest := payload.RedirectTo
	if dest == "" {
		dest = h.redirectAfterLogin
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// Logout handles POST (or GET) /auth/{realm}/logout. It clears
// the session cookie and 302s to the configured landing URL.
// IdP-side single-logout (RP-initiated) is out of scope for
// PRMT-102 — the gateway session is local-only.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.realmFromPath(w, r); !ok {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	dest := h.redirectAfterLogin
	if q := r.URL.Query().Get("next"); q != "" && isSafeRedirect(q) {
		dest = q
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// Routes returns an http.Handler with /auth/{realm}/{login,
// callback, logout} registered.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", h.routeAuth)
	return mux
}

func (h *Handler) routeAuth(w http.ResponseWriter, r *http.Request) {
	// Trim /auth/ prefix; remainder must be <realm>/<action>.
	rest := strings.TrimPrefix(r.URL.Path, "/auth/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeProblem(w, http.StatusNotFound,
			"path-not-found", "auth path not found",
			"expected /auth/<realm>/<login|callback|logout>", r.URL.Path)
		return
	}
	switch parts[1] {
	case "login":
		h.Login(w, r)
	case "callback":
		h.Callback(w, r)
	case "logout":
		h.Logout(w, r)
	default:
		writeProblem(w, http.StatusNotFound,
			"path-not-found", "auth action not found",
			"unknown auth action: "+parts[1], r.URL.Path)
	}
}

// realmFromPath extracts Realm from /auth/<realm>/<action>.
// 404s (path-not-found RFC7807) if the shape is wrong.
func (h *Handler) realmFromPath(w http.ResponseWriter, r *http.Request) (Realm, bool) {
	rest := strings.TrimPrefix(r.URL.Path, "/auth/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		writeProblem(w, http.StatusNotFound,
			"path-not-found", "auth realm missing",
			"expected /auth/<realm>/...", r.URL.Path)
		return "", false
	}
	realm, err := ParseRealm(parts[0])
	if err != nil {
		writeProblem(w, http.StatusNotFound,
			"path-not-found", "unknown auth realm",
			"unknown realm: "+parts[0], r.URL.Path)
		return "", false
	}
	return realm, true
}

func (h *Handler) verifierFor(w http.ResponseWriter, realm Realm, r *http.Request) (Verifier, bool) {
	v, ok := h.verifiers[realm]
	if !ok {
		writeProblem(w, http.StatusNotFound,
			"path-not-found", "no verifier for realm",
			"no OIDC verifier registered for realm "+string(realm), r.URL.Path)
		return nil, false
	}
	return v, true
}

// writeStateCookie mints a short-TTL cookie carrying the state
// payload. The cookie is keyed by realm so cross-realm state
// cannot be reused (defence in depth on top of payload.Realm).
//
// We sign the cookie value with the same HMAC key as the session
// cookie so a tampered state cookie (e.g. realm swapped,
// code_verifier rewritten) is rejected by the same constant-time
// check.
func (h *Handler) writeStateCookie(w http.ResponseWriter, realm Realm, p statePayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	val := signStateValue(h.sessionKey, body, realm)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    val,
		Path:     "/auth/" + string(realm) + "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.stateTTL.Seconds()),
	})
	return nil
}

func (h *Handler) decodeStateCookie(realm Realm, cookie string) (statePayload, error) {
	body, err := verifyStateValue(h.sessionKey, cookie, realm)
	if err != nil {
		return statePayload{}, err
	}
	var p statePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return statePayload{}, err
	}
	if time.Unix(p.IssuedAt, 0).Add(h.stateTTL).Before(time.Now()) {
		return statePayload{}, errors.New("authn: state cookie expired")
	}
	return p, nil
}

func clearStateCookie(w http.ResponseWriter, realm Realm) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth/" + string(realm) + "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// signStateValue produces a realm-tagged, HMAC-signed envelope
// around body. Format:
//
//	<base64url(body)>.<base64url(signature)>.<realm>
//
// where signature = HMAC-SHA256(key, body || "." || realm).
//
// The realm is appended outside the signature so verifyStateValue
// can reject state cookies minted for a different realm BEFORE
// running the HMAC check. (Cross-realm guards also happen at the
// payload level; this is belt-and-braces.)
func signStateValue(key []byte, body []byte, realm Realm) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	mac.Write([]byte{'.'})
	mac.Write([]byte(realm))
	return sessionEncoding.EncodeToString(body) + "." +
		sessionEncoding.EncodeToString(mac.Sum(nil)) + "." + string(realm)
}

func verifyStateValue(key []byte, cookie string, realm Realm) ([]byte, error) {
	parts := strings.SplitN(cookie, ".", 3)
	if len(parts) != 3 {
		return nil, errors.New("authn: state cookie malformed")
	}
	if parts[2] != string(realm) {
		return nil, errors.New("authn: state cookie realm mismatch")
	}
	body, err := sessionEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("authn: state cookie body b64")
	}
	want, err := sessionEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("authn: state cookie sig b64")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	mac.Write([]byte{'.'})
	mac.Write([]byte(realm))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return nil, errors.New("authn: state cookie signature invalid")
	}
	return body, nil
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// randomString returns a base64url-encoded random string of
// approximately n bytes of entropy.
func randomString(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// isSafeRedirect returns true if target is a same-origin path
// (starts with "/" and not "//"). Prevents open-redirect to
// attacker-controlled hosts via ?next=https://evil.example/...
func isSafeRedirect(target string) bool {
	if !strings.HasPrefix(target, "/") {
		return false
	}
	if strings.HasPrefix(target, "//") {
		return false
	}
	return true
}
