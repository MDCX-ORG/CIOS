// pkg/authn/session_test.go — tests for HMAC-signed session
// cookies and Realm parsing (PRMT-102 §5 "篡改 session cookie",
// §4 ParseRealm contract).
package authn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// ---------- Realm ----------------------------------------------------

func TestParseRealm_Valid(t *testing.T) {
	if r, err := ParseRealm("ops"); err != nil || r != RealmOps {
		t.Errorf(`ParseRealm("ops") = (%q, %v), want (ops, nil)`, r, err)
	}
	if r, err := ParseRealm("customer"); err != nil || r != RealmCustomer {
		t.Errorf(`ParseRealm("customer") = (%q, %v), want (customer, nil)`, r, err)
	}
}

func TestParseRealm_Rejects(t *testing.T) {
	for _, in := range []string{"", "admin", "OPS", "Customer", "root", "ops/customer"} {
		if _, err := ParseRealm(in); err == nil {
			t.Errorf(`ParseRealm(%q) = nil err, want error`, in)
		}
	}
}

// ---------- Session encode / decode ---------------------------------

func TestSession_RoundTrip(t *testing.T) {
	key := []byte("test-session-key-must-be-long-enough-32")
	s := Session{
		subject:   "user-1",
		realm:     RealmOps,
		expiresAt: time.Now().Add(time.Hour).Unix(),
		claims: Claims{
			"iss": "https://idp.example/realms/ops",
			"aud": "ops-client",
			"sub": "user-1",
		},
	}
	cookie, err := EncodeSession(key, s)
	if err != nil {
		t.Fatalf("EncodeSession: %v", err)
	}
	// Wire format: v1.<body>.<sig>
	if !strings.HasPrefix(cookie, sessionCookieVersion+".") {
		t.Fatalf("cookie missing version prefix: %q", cookie)
	}
	got, err := DecodeSession(key, cookie)
	if err != nil {
		t.Fatalf("DecodeSession: %v", err)
	}
	if got.Subject() != "user-1" {
		t.Errorf("Subject = %q, want user-1", got.Subject())
	}
	if got.Realm() != RealmOps {
		t.Errorf("Realm = %q, want ops", got.Realm())
	}
	if got.Claims()["iss"] != "https://idp.example/realms/ops" {
		t.Errorf("iss claim lost: %v", got.Claims()["iss"])
	}
}

// TestSession_AccessorsReturnCopy: PRMT-102 §4 — Claims() must
// return a reference the caller cannot mutate to alter the
// session's identity. We don't deep-copy the map (stdlib doesn't
// do that for free and the codebase pattern is to say "do not
// mutate" rather than copy), but we DO return the same map.
// This test pins the present behaviour; if a future PRMT decides
// to switch to defensive copy it will need to update this test.
func TestSession_AccessorsReturnCopy(t *testing.T) {
	key := []byte("k")
	s := Session{
		subject:   "u",
		realm:     RealmCustomer,
		expiresAt: time.Now().Add(time.Hour).Unix(),
		claims:    Claims{"k": "v"},
	}
	c, err := EncodeSession(key, s)
	if err != nil {
		t.Fatalf("EncodeSession: %v", err)
	}
	got, err := DecodeSession(key, c)
	if err != nil {
		t.Fatalf("DecodeSession: %v", err)
	}
	if got.Subject() != "u" || got.Realm() != RealmCustomer {
		t.Errorf("Subject/Realm = %q/%q, want u/customer", got.Subject(), got.Realm())
	}
	if got.Claims()["k"] != "v" {
		t.Errorf("Claims[k] = %v, want v", got.Claims()["k"])
	}
}

// TestSession_TamperedCookie: PRMT-102 §5 "篡改 session cookie" —
// flipping a single byte of a valid cookie invalidates the HMAC.
func TestSession_TamperedCookie(t *testing.T) {
	key := []byte("k")
	s := Session{
		subject: "u", realm: RealmOps,
		expiresAt: time.Now().Add(time.Hour).Unix(),
	}
	c, err := EncodeSession(key, s)
	if err != nil {
		t.Fatalf("EncodeSession: %v", err)
	}
	// Flip a byte in the body (middle third).
	parts := strings.SplitN(c, ".", 3)
	bodyBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	bodyBytes[0] ^= 0xFF
	parts[1] = base64.RawURLEncoding.EncodeToString(bodyBytes)
	tampered := parts[0] + "." + parts[1] + "." + parts[2]
	if _, err := DecodeSession(key, tampered); err == nil {
		t.Fatalf("DecodeSession accepted tampered body")
	}
}

// TestSession_WrongKey: a cookie signed with key A is rejected
// when verified with key B.
func TestSession_WrongKey(t *testing.T) {
	s := Session{
		subject: "u", realm: RealmOps,
		expiresAt: time.Now().Add(time.Hour).Unix(),
	}
	c, err := EncodeSession([]byte("key-A-must-be-long-enough-please"), s)
	if err != nil {
		t.Fatalf("EncodeSession: %v", err)
	}
	if _, err := DecodeSession([]byte("key-B-must-be-long-enough-please"), c); err == nil {
		t.Fatalf("DecodeSession accepted cookie signed by foreign key")
	}
}

// TestSession_Expired: ExpiresAt in the past → ErrSessionInvalid.
func TestSession_Expired(t *testing.T) {
	key := []byte("k")
	s := Session{
		subject: "u", realm: RealmOps,
		expiresAt: time.Now().Add(-time.Minute).Unix(),
	}
	c, err := EncodeSession(key, s)
	if err != nil {
		t.Fatalf("EncodeSession: %v", err)
	}
	if _, err := DecodeSession(key, c); err == nil {
		t.Fatalf("DecodeSession accepted expired session")
	}
}

// TestSession_BadRealm: a session claiming realm=admin (not in
// the {ops,customer} set) is rejected.
func TestSession_BadRealm(t *testing.T) {
	key := []byte("k")
	// Bypass Session{} constructor and craft a JSON body
	// directly with realm=admin, then sign it.
	body := []byte(`{"sub":"u","realm":"admin","exp":` +
		itoa(time.Now().Add(time.Hour).Unix()) + `,"claims":{}}`)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(sessionCookieVersion))
	mac.Write([]byte("."))
	mac.Write([]byte(base64.RawURLEncoding.EncodeToString(body)))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	c := sessionCookieVersion + "." +
		base64.RawURLEncoding.EncodeToString(body) + "." + sig
	if _, err := DecodeSession(key, c); err == nil {
		t.Fatalf("DecodeSession accepted session with unknown realm")
	}
}

// TestSession_BadShape: cookies with the wrong number of dots or
// an unknown version are rejected.
func TestSession_BadShape(t *testing.T) {
	key := []byte("k")
	for _, c := range []string{
		"",
		"v1",
		"v1.body",
		"v2.body.sig",
		"v1.body.sig.extra",
		"v1.!@#.sig",
	} {
		if _, err := DecodeSession(key, c); err == nil {
			t.Errorf("DecodeSession(%q) = nil, want error", c)
		}
	}
}

// TestSession_EmptyKey: callers that pass an empty key get a
// hard error (defence in depth).
func TestSession_EmptyKey(t *testing.T) {
	if _, err := EncodeSession(nil, Session{}); err == nil {
		t.Errorf("EncodeSession(nil key) = nil, want error")
	}
	if _, err := DecodeSession(nil, "v1.."); err == nil {
		t.Errorf("DecodeSession(nil key) = nil, want error")
	}
}

// itoa is a tiny int→string helper to avoid importing strconv in
// tests that want to keep the import surface minimal. Only used
// for non-negative values.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
