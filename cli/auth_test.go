// Package cli — auth_test.go: bearer attachment + token caching.
//
// These tests exercise cli/auth.go in isolation (no Doctor / Main
// runner) because (a) the tokenSource is unexported and (b) we need
// a deterministic fake /auth/token server, not the full fake-core
// suite in cli_test.go.
package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withEnv sets the three required env vars + optional scope and
// returns a cleanup that restores their previous state.
func withEnv(t *testing.T, tokenURL, clientID, clientSecret, scope string) {
	t.Helper()
	t.Setenv("CIOS_CLI_TOKEN_URL", tokenURL)
	t.Setenv("CIOS_CLI_CLIENT_ID", clientID)
	t.Setenv("CIOS_CLI_CLIENT_SECRET", clientSecret)
	if scope != "" {
		t.Setenv("CIOS_CLI_SCOPE", scope)
	}
}

func TestNewEnvTokenSource_MissingEnv_ReturnsNil(t *testing.T) {
	// All three required envs unset → (nil, nil) = no-auth mode.
	t.Setenv("CIOS_CLI_TOKEN_URL", "")
	t.Setenv("CIOS_CLI_CLIENT_ID", "")
	t.Setenv("CIOS_CLI_CLIENT_SECRET", "")
	ts, err := newEnvTokenSource(http.DefaultClient)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if ts != nil {
		t.Fatalf("want nil tokenSource, got %T", ts)
	}
}

func TestNewEnvTokenSource_PartialEnv_ReturnsNil(t *testing.T) {
	// Only one of the three set → still no-auth mode.
	t.Setenv("CIOS_CLI_TOKEN_URL", "http://example/token")
	t.Setenv("CIOS_CLI_CLIENT_ID", "")
	t.Setenv("CIOS_CLI_CLIENT_SECRET", "secret")
	ts, err := newEnvTokenSource(http.DefaultClient)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if ts != nil {
		t.Fatalf("want nil tokenSource for partial env, got %T", ts)
	}
}

func TestEnvTokenSource_FetchAndCache(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") != "client_credentials" {
			http.Error(w, "bad grant_type", http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "csec" {
			http.Error(w, "bad creds", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-1",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	withEnv(t, srv.URL, "cid", "csec", "")
	ts, err := newEnvTokenSource(http.DefaultClient)
	if err != nil {
		t.Fatalf("newEnvTokenSource: %v", err)
	}
	if ts == nil {
		t.Fatal("want tokenSource, got nil")
	}
	got, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "tok-1" {
		t.Fatalf("want tok-1, got %q", got)
	}
	// Cached: a second call must NOT hit the server again.
	got2, err := ts.Token()
	if err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if got2 != "tok-1" {
		t.Fatalf("cache miss: want tok-1, got %q", got2)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("want 1 token endpoint call, got %d", n)
	}
}

func TestEnvTokenSource_RefreshOnExpire(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-" + string(rune('A'+n-1)),
			// 1s TTL — well under the refresh window of 30s.
			"expires_in": 1,
		})
	}))
	defer srv.Close()

	withEnv(t, srv.URL, "cid", "csec", "")
	ts, _ := newEnvTokenSource(http.DefaultClient)
	if ts == nil {
		t.Fatal("want tokenSource")
	}
	first, _ := ts.Token()
	if first != "tok-A" {
		t.Fatalf("want tok-A, got %q", first)
	}
	// Wait past refresh window: 1s TTL + 30s refresh window = 31s.
	// We can't sleep 31s in a unit test, so directly back-date the
	// expiry via a re-fetch with a synthetic 0s TTL.
	ets := ts.(*envTokenSource)
	ets.mu.Lock()
	ets.expiry = time.Now().Add(-time.Second)
	ets.mu.Unlock()
	second, _ := ts.Token()
	if second == first {
		t.Fatalf("expected refresh, got same %q", second)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("want 2 token endpoint calls, got %d", n)
	}
}

func TestEnvTokenSource_EndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	withEnv(t, srv.URL, "cid", "csec", "")
	ts, _ := newEnvTokenSource(http.DefaultClient)
	_, err := ts.Token()
	if err == nil {
		t.Fatal("want err, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err should mention status 401, got %v", err)
	}
}

// --- Do() integration ---------------------------------------------------

func TestDo_AttachesBearerWhenConfigured(t *testing.T) {
	var tokenCalls int32
	var seenAuth string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "sts-tok-xyz",
			"expires_in":   600,
		})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer apiSrv.Close()

	withEnv(t, tokenSrv.URL, "cid", "csec", "")
	c := NewClient(apiSrv.URL)
	status, body, err := c.Do(http.MethodGet, "/v1/x", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if seenAuth != "Bearer sts-tok-xyz" {
		t.Fatalf("want Authorization: Bearer sts-tok-xyz, got %q", seenAuth)
	}
	if n := atomic.LoadInt32(&tokenCalls); n != 1 {
		t.Fatalf("want 1 token call, got %d", n)
	}
}

func TestDo_NoAuthHeaderWhenUnset(t *testing.T) {
	t.Setenv("CIOS_CLI_TOKEN_URL", "")
	t.Setenv("CIOS_CLI_CLIENT_ID", "")
	t.Setenv("CIOS_CLI_CLIENT_SECRET", "")

	var seenAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer apiSrv.Close()

	c := NewClient(apiSrv.URL)
	if c.tok != nil {
		t.Fatalf("want nil tokenSource, got %T", c.tok)
	}
	status, _, err := c.Do(http.MethodGet, "/v1/x", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if seenAuth != "" {
		t.Fatalf("want no Authorization, got %q", seenAuth)
	}
}

func TestDo_TokenFetchFailure_ReturnsNetError(t *testing.T) {
	// Token endpoint always 500 → Do must surface a *NetError and
	// MUST NOT make the API call.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer tokenSrv.Close()

	var apiCalls int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	withEnv(t, tokenSrv.URL, "cid", "csec", "")
	c := NewClient(apiSrv.URL)
	status, _, err := c.Do(http.MethodGet, "/v1/x", nil, nil)
	if ne, ok := IsNetError(err); !ok || ne.Op != "auth" {
		t.Fatalf("want *NetError{Op:\"auth\"}, got status=%d err=%v", status, err)
	}
	if n := atomic.LoadInt32(&apiCalls); n != 0 {
		t.Fatalf("must not call API when token fetch fails, got %d calls", n)
	}
}

// --- scope + URL parsing ----------------------------------------------

func TestNewEnvTokenSource_ScopeForwarded(t *testing.T) {
	var seenScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seenScope = r.PostForm.Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 60})
	}))
	defer srv.Close()

	withEnv(t, srv.URL, "cid", "csec", "read:assets write:assets")
	ts, _ := newEnvTokenSource(http.DefaultClient)
	if _, err := ts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if seenScope != "read:assets write:assets" {
		t.Fatalf("want scope forwarded, got %q", seenScope)
	}
}

func TestNewEnvTokenSource_InvalidURL_ReturnsError(t *testing.T) {
	t.Setenv("CIOS_CLI_TOKEN_URL", "://no-scheme")
	t.Setenv("CIOS_CLI_CLIENT_ID", "cid")
	t.Setenv("CIOS_CLI_CLIENT_SECRET", "csec")
	ts, err := newEnvTokenSource(http.DefaultClient)
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	if ts != nil {
		t.Fatalf("want nil tokenSource on parse error, got %T", ts)
	}
}

// silence unused import in case the suite gets trimmed.
var _ = url.Values{}
