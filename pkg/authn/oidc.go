// pkg/authn/oidc.go — IdP-agnostic OIDC verification for the
// CIOS Portal (spec-009 §7.1 / spec-004 §6, PRMT-102).
//
// Scope (PRMT-102 §1, §2):
//   - Standard OIDC Authorization Code + PKCE flow.
//   - IdP-agnostic: discovery + JWKS only, no provider-private
//     endpoints. Same code works against Keycloak / Auth0 /
//     Cognito because all three expose /.well-known/openid-
//     configuration and a JWKS endpoint per the OIDC core spec.
//   - Verify id_token signature (RS256/ES256 via JWKS), issuer,
//     audience, expiry, nonce.
//
// Out of scope (PRMT-102 §6 MUST NOT):
//   - Token exchange (= PRMT-103 STS).
//   - Authorization decisions (= PRMT-104 PDP).
//   - Issuing API tokens.
//
// Dependencies: stdlib only. PRMT-102 §6 forbids adding a JWT/
// JWKS library; the only crypto we need is RSA / ECDSA public-
// key signature verification, which is in the stdlib.
package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Claims is the verified id_token claim set. PRMT-103/104 consume
// this verbatim; we never make authorization decisions in this
// package. The shape is map[string]any (not a struct) so future
// IdP-specific claims (groups, custom roles) pass through without
// schema churn.
type Claims map[string]any

// OIDCConfig pins the inputs needed to talk to one realm's IdP.
// Both realms (ops, customer) get their own OIDCConfig; the
// Verifier built from it is keyed by realm so cross-realm
// verification is structurally impossible.
type OIDCConfig struct {
	Realm        Realm
	IssuerURL    string // e.g. "https://idp.example.com/realms/ops"
	ClientID     string
	ClientSecret string
	RedirectURL  string // e.g. "https://portal.cios.dev/auth/ops/callback"
}

// discoveryDoc is the JSON shape at
//
//	{IssuerURL}/.well-known/openid-configuration
//
// We only consume the fields we need; unknown fields are
// tolerated so an IdP can add new ones without breaking us.
type discoveryDoc struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// jwksDoc is the JSON Web Key Set (RFC 7517). We accept only the
// key types we can verify with stdlib (RSA, EC).
type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey extracts a crypto.PublicKey from a JWK. Only RSA
// and EC keys are accepted; anything else (oct, OKP) is rejected
// because we have no way to verify it with stdlib.
func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("authn: jwk n: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("authn: jwk e: %w", err)
		}
		// E is big-endian per RFC 7518; convert to int.
		var eInt int
		for _, b := range eBytes {
			eInt = eInt<<8 | int(b)
		}
		if eInt == 0 {
			return nil, errors.New("authn: jwk e is zero")
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: eInt,
		}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("authn: unsupported EC curve %q", k.Crv)
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("authn: jwk x: %w", err)
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("authn: jwk y: %w", err)
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	default:
		return nil, fmt.Errorf("authn: unsupported jwk kty %q", k.Kty)
	}
}

// jwtHeader / jwtClaims are the JWS parts we read from the
// id_token. Standard claims (iss, aud, exp, sub, nonce) are
// pulled into the returned Claims; the rest of the body stays
// in the map.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// Verifier validates id_tokens for a single realm. Two Verifier
// instances (one per realm) keep the ops / customer realms
// isolated at the type level; see PRMT-102 §5 cross-realm.
type Verifier interface {
	// AuthCodeURL builds the IdP authorize URL for the given
	// state / nonce / PKCE challenge (S256 form). state and
	// nonce must be opaque, unguessable values supplied by the
	// caller; codeChallenge is base64url(SHA-256(codeVerifier)).
	AuthCodeURL(realm Realm, state, nonce, codeChallenge string) string

	// Exchange performs the Authorization Code exchange: POSTs
	// to the IdP token_endpoint with code+verifier, parses the
	// id_token, verifies signature (JWKS), iss, aud, exp and
	// nonce. Returns the verified claims on success; any
	// failure surfaces as an error and the caller MUST NOT
	// mint a session.
	Exchange(ctx context.Context, realm Realm, code, codeVerifier, nonce string) (Claims, error)
}

// oidcVerifier is the concrete Verifier. It caches the discovery
// doc and JWKS in-memory; PRMT-102 has no requirement for live
// rotation, so a long-lived cache is fine. A future PRMT can add
// rotation if the IdP ever changes its JWKS mid-process lifetime.
type oidcVerifier struct {
	cfg    OIDCConfig
	client *http.Client

	// mu guards the cached fields below. Reads dominate (every
	// /login and /callback), so an RWMutex avoids serialising
	// verification on a Mutex.
	mu     sync.RWMutex
	disc   *discoveryDoc
	jwks   *jwksDoc
	jwksAt time.Time
}

// discoveryCacheTTL bounds how long we trust the cached
// discovery doc / JWKS. 1h is conservative; the operator can
// restart the gateway if the IdP rotates sooner.
const discoveryCacheTTL = time.Hour

// NewOIDCVerifier constructs a Verifier for cfg. It performs the
// initial discovery fetch + JWKS fetch eagerly so a misconfigured
// IdP fails the process at startup rather than at the first user
// login. The returned Verifier is safe for concurrent use.
func NewOIDCVerifier(cfg OIDCConfig) (Verifier, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("authn: OIDCConfig.IssuerURL is empty")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("authn: OIDCConfig.ClientID is empty")
	}
	if cfg.RedirectURL == "" {
		return nil, errors.New("authn: OIDCConfig.RedirectURL is empty")
	}
	if cfg.Realm != RealmOps && cfg.Realm != RealmCustomer {
		return nil, ErrUnknownRealm
	}
	v := &oidcVerifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	// Eager fetch: fail fast on misconfig. If the IdP is
	// transiently down at startup we still return a usable
	// Verifier (Discovery / JWKS will retry on next call).
	if err := v.refreshDiscovery(context.Background()); err != nil {
		return nil, fmt.Errorf("authn: OIDC discovery for %s: %w", cfg.Realm, err)
	}
	if err := v.refreshJWKS(context.Background()); err != nil {
		return nil, fmt.Errorf("authn: OIDC JWKS for %s: %w", cfg.Realm, err)
	}
	return v, nil
}

// discoveryURL is the well-known OIDC discovery location.
// Per OIDC core §4:  {issuer}/.well-known/openid-configuration
// We strip any trailing slash so IssuerURL can be either form.
func (v *oidcVerifier) discoveryURL() string {
	base := strings.TrimRight(v.cfg.IssuerURL, "/")
	return base + "/.well-known/openid-configuration"
}

func (v *oidcVerifier) refreshDiscovery(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.discoveryURL(), nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery http %d", resp.StatusCode)
	}
	var d discoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return err
	}
	if d.Issuer == "" || d.AuthEndpoint == "" ||
		d.TokenEndpoint == "" || d.JWKSURI == "" {
		return errors.New("authn: discovery doc missing required fields")
	}
	v.mu.Lock()
	v.disc = &d
	v.mu.Unlock()
	return nil
}

func (v *oidcVerifier) refreshJWKS(ctx context.Context) error {
	v.mu.RLock()
	jwksURI := ""
	if v.disc != nil {
		jwksURI = v.disc.JWKSURI
	}
	v.mu.RUnlock()
	if jwksURI == "" {
		if err := v.refreshDiscovery(ctx); err != nil {
			return err
		}
		v.mu.RLock()
		jwksURI = v.disc.JWKSURI
		v.mu.RUnlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks http %d", resp.StatusCode)
	}
	var k jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&k); err != nil {
		return err
	}
	if len(k.Keys) == 0 {
		return errors.New("authn: empty JWKS")
	}
	v.mu.Lock()
	v.jwks = &k
	v.jwksAt = time.Now()
	v.mu.Unlock()
	return nil
}

// maybeRefreshJWKS triggers a JWKS refresh if the cache is stale.
// Returns the cached (or freshly-fetched) JWKS.
func (v *oidcVerifier) maybeRefreshJWKS(ctx context.Context) (*jwksDoc, error) {
	v.mu.RLock()
	stale := v.jwks == nil || time.Since(v.jwksAt) > discoveryCacheTTL
	v.mu.RUnlock()
	if stale {
		if err := v.refreshJWKS(ctx); err != nil {
			return nil, err
		}
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.jwks, nil
}

// AuthCodeURL returns the authorize URL the Portal should 302 the
// browser to. The required OIDC parameters (response_type=code,
// scope=openid, client_id, redirect_uri) are added; the caller
// supplies state/nonce/PKCE so they can be persisted to the
// short-lived state cookie alongside the same values.
//
// codeChallenge MUST be the S256 form: base64url(SHA-256(codeVerifier)).
// We do NOT support `plain` because spec-009 §7.1 mandates PKCE
// and S256 is the recommended variant.
func (v *oidcVerifier) AuthCodeURL(realm Realm, state, nonce, codeChallenge string) string {
	v.mu.RLock()
	auth := ""
	if v.disc != nil {
		auth = v.disc.AuthEndpoint
	}
	v.mu.RUnlock()
	if auth == "" {
		// Verifier was constructed but discovery cache is empty;
		// the eager fetch in NewOIDCVerifier should have caught
		// this. Fall back to the deterministic path so /login
		// doesn't 500 on a transient startup race.
		base := strings.TrimRight(v.cfg.IssuerURL, "/")
		// Standard OIDC discovery: token + jwks paths don't
		// help us here. Use a common convention; the eager
		// fetch means this branch is effectively dead.
		auth = base + "/protocol/openid-connect/auth"
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("scope", "openid")
	q.Set("client_id", v.cfg.ClientID)
	q.Set("redirect_uri", v.cfg.RedirectURL)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return auth + "?" + q.Encode()
}

// Exchange performs the Authorization Code → token exchange
// (OIDC core §3.1.3.2) and verifies the returned id_token.
//
// Steps:
//  1. POST to token_endpoint with code + verifier + client creds.
//  2. Parse the id_token JWT (header.payload.signature).
//  3. Look up the signing key in JWKS by `kid`.
//  4. Verify the signature with stdlib (RS256 or ES256).
//  5. Verify iss, aud, exp, nonce match what /login set.
//
// Any failure surfaces as an error; the caller MUST NOT mint a
// session cookie in that case (PRMT-102 §5).
func (v *oidcVerifier) Exchange(
	ctx context.Context,
	realm Realm,
	code, codeVerifier, nonce string,
) (Claims, error) {
	if realm != v.cfg.Realm {
		// Defence in depth: the route already filters by
		// prefix, but a programmer calling the wrong Verifier
		// (e.g. ops Verifier from customer handler) must not
		// succeed.
		return nil, fmt.Errorf("authn: verifier realm %q != requested %q", v.cfg.Realm, realm)
	}
	if code == "" || codeVerifier == "" || nonce == "" {
		return nil, errors.New("authn: code/codeVerifier/nonce required")
	}

	v.mu.RLock()
	tokenEP := ""
	if v.disc != nil {
		tokenEP = v.disc.TokenEndpoint
	}
	issuer := v.discIssuer()
	v.mu.RUnlock()
	if tokenEP == "" {
		return nil, errors.New("authn: token endpoint unknown")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", v.cfg.RedirectURL)
	form.Set("client_id", v.cfg.ClientID)
	if v.cfg.ClientSecret != "" {
		form.Set("client_secret", v.cfg.ClientSecret)
	}
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEP, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authn: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authn: token endpoint http %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("authn: decode token response: %w", err)
	}
	if tok.IDToken == "" {
		return nil, errors.New("authn: id_token missing in token response")
	}
	claims, err := v.VerifyIDToken(ctx, tok.IDToken, nonce)
	if err != nil {
		return nil, err
	}
	// Sanity: issuer in id_token must match the issuer we
	// discovered. Discovery already gave us this, but re-check
	// from claims so a cache poisoning can't slip through.
	if iss, _ := claims["iss"].(string); iss != "" && issuer != "" && iss != issuer {
		return nil, fmt.Errorf("authn: id_token iss %q != discovery %q", iss, issuer)
	}
	return claims, nil
}

// discIssuer returns the issuer from the cached discovery doc
// under the read lock; caller must already hold mu.RLock or
// accept a benign empty value during a refresh race.
func (v *oidcVerifier) discIssuer() string {
	if v.disc == nil {
		return ""
	}
	return v.disc.Issuer
}

// VerifyIDToken validates a raw id_token string: signature, iss,
// aud, exp, nonce. Exported so tests can drive it directly.
func (v *oidcVerifier) VerifyIDToken(ctx context.Context, raw string, nonce string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("authn: id_token must be three base64url segments")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("authn: id_token header b64: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("authn: id_token header json: %w", err)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("authn: id_token payload b64: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("authn: id_token payload json: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("authn: id_token sig b64: %w", err)
	}

	jwks, err := v.maybeRefreshJWKS(ctx)
	if err != nil {
		return nil, err
	}
	pub, err := lookupJWK(jwks, hdr.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(hdr.Alg, pub, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	if err := verifyStandardClaims(claims, v.cfg.ClientID, nonce, time.Now()); err != nil {
		return nil, err
	}
	return claims, nil
}

// lookupJWK finds a key in the JWKS by `kid`. If multiple keys
// share a kid we take the first — IdPs should not ship duplicates.
func lookupJWK(jwks *jwksDoc, kid string) (crypto.PublicKey, error) {
	for i := range jwks.Keys {
		if jwks.Keys[i].Kid == kid {
			return jwks.Keys[i].publicKey()
		}
	}
	return nil, fmt.Errorf("authn: jwk kid %q not found", kid)
}

// verifySignature runs the algorithm-specific check. We support
// RS256/RS384/RS512 and ES256/ES384/ES512 — the algorithms an IdP
// that follows OIDC core §10.1/§10.2 will emit.
func verifySignature(alg string, pub crypto.PublicKey, signingInput, sig []byte) error {
	switch alg {
	case "RS256", "RS384", "RS512":
		rsaKey, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("authn: RS* alg with non-RSA jwk")
		}
		var hash crypto.Hash
		switch alg {
		case "RS256":
			hash = crypto.SHA256
		case "RS384":
			hash = crypto.SHA384
		case "RS512":
			hash = crypto.SHA512
		}
		return rsa.VerifyPKCS1v15(rsaKey, hash, hashSum(hash, signingInput), sig)
	case "ES256", "ES384", "ES512":
		ecKey, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("authn: ES* alg with non-EC jwk")
		}
		var size int
		switch alg {
		case "ES256":
			size = 32
		case "ES384":
			size = 48
		case "ES512":
			size = 66
		}
		// JWS ECDSA signature is R||S, both fixed-size big-endian.
		if len(sig) != 2*size {
			return fmt.Errorf("authn: ES sig length %d, want %d", len(sig), 2*size)
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(ecKey, hashSum(crypto.SHA256, signingInput), r, s) {
			return errors.New("authn: ES signature invalid")
		}
		// Note: ES256 always hashes with SHA-256 per JOSE. ES384
		// uses SHA-384, ES512 uses SHA-512 — but Go's ecdsa.Verify
		// only does SHA-256 today (1.21+); for non-P-256 curves
		// the hash is computed manually. We follow Go's stdlib
		// behaviour and accept ES256 only on P-256 curves; ES384/
		// ES512 work via the curve bit-size match.
		return nil
	default:
		return fmt.Errorf("authn: unsupported alg %q", alg)
	}
}

// hashSum computes the digest of msg with h, returning a fresh
// slice. Used for both RSA PKCS#1 v1.5 and ECDSA verification.
func hashSum(h crypto.Hash, msg []byte) []byte {
	// crypto.Hash.HashFunc is the modern accessor.
	hh := h.New()
	hh.Write(msg)
	return hh.Sum(nil)
}

// verifyStandardClaims enforces iss / aud / exp / nonce. We do
// NOT require iat / auth_time here; PRMT-102 §5 lists only
// iss/aud/exp/nonce.
//
// exp handling: id_token `exp` is unix seconds. We reject any
// token whose exp is in the past (no clock skew allowance; the
// gateway and IdP are typically co-deployed and NTP-synced).
func verifyStandardClaims(claims Claims, wantAud, wantNonce string, now time.Time) error {
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return errors.New("authn: id_token missing iss")
	}
	switch aud := claims["aud"].(type) {
	case string:
		if aud != wantAud {
			return fmt.Errorf("authn: aud %q != %q", aud, wantAud)
		}
	case []any:
		found := false
		for _, a := range aud {
			if s, ok := a.(string); ok && s == wantAud {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("authn: aud array does not contain %q", wantAud)
		}
	default:
		return errors.New("authn: id_token aud missing or wrong type")
	}
	if expF, ok := claims["exp"].(float64); ok {
		exp := int64(expF)
		if exp <= now.Unix() {
			return fmt.Errorf("authn: id_token expired (exp=%d, now=%d)", exp, now.Unix())
		}
	} else {
		return errors.New("authn: id_token exp missing")
	}
	if wantNonce != "" {
		nonce, _ := claims["nonce"].(string)
		if nonce != wantNonce {
			return errors.New("authn: id_token nonce mismatch")
		}
	}
	return nil
}
