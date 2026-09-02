package authn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Session is the in-memory representation of a logged-in user,
// surfaced to downstream PRMTs (103 STS, 104 PDP). It is the
// ONLY piece of evidence PRMT-103/104 get to make any
// authorization decision — they MUST NOT consult request
// headers or URL paths to determine identity.
//
// Fields:
//
//   - Subject   : OIDC `sub` claim (stable per-user IdP id).
//   - Realm     : which realm minted this session (ops vs
//     customer); PRMT-104 will use this in policy.
//   - ExpiresAt : absolute UTC expiry; DecodeSession refuses
//     any session whose ExpiresAt has passed.
//   - Claims    : full set of verified id_token claims (incl.
//     iss/aud/exp/nonce). Read-only by convention.
//
// Session values are produced exclusively by handler.go after a
// successful OIDC code exchange and id_token verification.
//
// Fields are unexported on purpose (PRMT-102 §4): callers go
// through Subject()/Realm()/Claims(). The wire format used by
// EncodeSession/DecodeSession is fixed by MarshalJSON/
// UnmarshalJSON below — do not change field names there without
// bumping sessionCookieVersion.
type Session struct {
	subject   string
	realm     Realm
	expiresAt int64 // unix seconds (UTC)
	claims    Claims
}

// sessionWire is the on-the-wire JSON shape for Session.
// Keep in sync with MarshalJSON / UnmarshalJSON.
type sessionWire struct {
	Sub    string `json:"sub"`
	Realm  Realm  `json:"realm"`
	Exp    int64  `json:"exp"`
	Claims Claims `json:"claims"`
}

// MarshalJSON encodes Session using the canonical OIDC-style
// claim names. Required because the underlying fields are
// unexported (PRMT-102 §4 — public access is via accessors).
func (s Session) MarshalJSON() ([]byte, error) {
	return json.Marshal(sessionWire{
		Sub:    s.subject,
		Realm:  s.realm,
		Exp:    s.expiresAt,
		Claims: s.claims,
	})
}

// UnmarshalJSON is the inverse of MarshalJSON; called by
// DecodeSession. Decoding into the unexported fields requires
// a pointer receiver, hence the indirection through sessionWire.
func (s *Session) UnmarshalJSON(data []byte) error {
	var w sessionWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	s.subject = w.Sub
	s.realm = w.Realm
	s.expiresAt = w.Exp
	s.claims = w.Claims
	return nil
}

// Subject returns the OIDC `sub` claim. Stable per user per IdP.
func (s Session) Subject() string { return s.subject }

// Realm returns the realm this session was minted under.
func (s Session) Realm() Realm { return s.realm }

// Claims returns the verified id_token claims. PRMT-103/104
// MUST NOT mutate the returned map.
func (s Session) Claims() Claims { return s.claims }

// sessionCookieVersion is the wire version baked into every
// cookie value. Bump on a backwards-incompatible layout change
// (e.g. switch to AES-GCM, or change Claims representation);
// old cookies then simply fail to decode instead of being
// misinterpreted.
const sessionCookieVersion = "v1"

// sessionEncoding is base64.RawURLEncoding so the cookie value
// is safe to drop into a Set-Cookie header without further
// quoting. Stdlib JWTs use the same encoding.
var sessionEncoding = base64.RawURLEncoding

// HMAC truncation length is left at the full SHA-256 output
// (32 bytes). Browsers allow ≥4 KiB cookies in practice so the
// extra ~22 base64 chars are not worth a security trade-off.

// EncodeSession signs s with HMAC-SHA256(key) and returns the
// cookie value to place in Set-Cookie. Layout:
//
//	<version> "." <b64url(payload)> "." <b64url(signature)>
//
// payload = JSON(Session). signature = HMAC-SHA256(key, version + "." + b64url(payload)).
//
// The key is supplied by the caller (handler.go); it is sourced
// from env CIOS_APIGW_SESSION_KEY (see handler.go). A zero key
// is rejected here as a defence-in-depth measure so a buggy
// caller can't accidentally mint unverifiable cookies.
func EncodeSession(key []byte, s Session) (string, error) {
	if len(key) == 0 {
		return "", errors.New("authn: session key is empty")
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("authn: marshal session: %w", err)
	}
	body := sessionEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(sessionCookieVersion))
	mac.Write([]byte("."))
	mac.Write([]byte(body))
	sig := sessionEncoding.EncodeToString(mac.Sum(nil))
	return sessionCookieVersion + "." + body + "." + sig, nil
}

// ErrSessionInvalid is the umbrella error for any failure to
// decode/verify a session cookie. handler.go turns this into
// 401 RFC7807 "unauthorized". More specific causes are wrapped
// so they show up in structured logs but are not surfaced to
// the browser (we never tell an unauthenticated caller WHY the
// cookie failed — that is itself a small information leak).
var ErrSessionInvalid = errors.New("authn: session invalid")

// DecodeSession verifies cookie and returns the Session inside.
// It checks (in order): version prefix, signature (constant-time),
// JSON shape, ExpiresAt > now. Any failure → ErrSessionInvalid.
func DecodeSession(key []byte, cookie string) (Session, error) {
	if len(key) == 0 {
		return Session{}, ErrSessionInvalid
	}
	// Expect exactly two dots: <version>.<body>.<sig>.
	parts := splitDots(cookie)
	if len(parts) != 3 || parts[0] != sessionCookieVersion {
		return Session{}, ErrSessionInvalid
	}
	bodyBytes, err := sessionEncoding.DecodeString(parts[1])
	if err != nil {
		return Session{}, ErrSessionInvalid
	}
	sigBytes, err := sessionEncoding.DecodeString(parts[2])
	if err != nil {
		return Session{}, ErrSessionInvalid
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	mac.Write([]byte("."))
	mac.Write([]byte(parts[1]))
	// hmac.Equal is constant-time, so a caller can't probe
	// valid signature prefixes by timing.
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return Session{}, ErrSessionInvalid
	}
	var s Session
	if err := json.Unmarshal(bodyBytes, &s); err != nil {
		return Session{}, ErrSessionInvalid
	}
	if s.realm != RealmOps && s.realm != RealmCustomer {
		return Session{}, ErrSessionInvalid
	}
	if s.subject == "" {
		return Session{}, ErrSessionInvalid
	}
	if s.expiresAt <= time.Now().Unix() {
		return Session{}, ErrSessionInvalid
	}
	return s, nil
}

// splitDots splits on '.' into at most 3 parts (the third
// piece being whatever follows the last dot, which for us is
// the base64url signature — itself dot-free). Returning more
// than 3 parts means the cookie is malformed.
func splitDots(s string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	if len(out) > 3 {
		return out[:3]
	}
	return out
}

// SessionTTL is the duration a freshly-minted session is valid
// for. 8h is the conventional workday for ops staff; PRMT-104
// can lower it via policy for high-privilege roles if needed.
const SessionTTL = 8 * time.Hour
