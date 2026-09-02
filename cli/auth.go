// Package cli — auth.go: optional client_credentials bearer token source.
//
// L81 requires CLI bearers to be issued by the gateway STS. PRMT-108
// exposed POST /auth/token (client_credentials). This file provides
// an env-driven tokenSource that, when configured, fetches and caches
// STS-scoped bearers. When env is missing, newEnvTokenSource returns
// (nil, nil) and the Client runs in the no-auth mode that pre-dates
// PRMT-116.
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// refreshWindow is how long before a token's stated expiry we treat
// it as expired. The number is small on purpose: STS-issued tokens
// are short-lived, and we'd rather re-issue early than issue a
// request with a token that will 401 in flight.
const refreshWindow = 30 * time.Second

// envTokenSource fetches and caches client_credentials tokens.
type envTokenSource struct {
	hc           *http.Client
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// tokenSource is the minimal interface Client.Do needs.
type tokenSource interface {
	Token() (string, error)
}

// newEnvTokenSource returns an envTokenSource when all three required
// env vars are set, or (nil, nil) when any of them is missing (= no-auth
// mode). An error is returned only for parseable-but-broken env
// (e.g. an unparsable tokenURL), which would otherwise misbehave
// silently.
func newEnvTokenSource(hc *http.Client) (tokenSource, error) {
	tokenURL := os.Getenv("CIOS_CLI_TOKEN_URL")
	clientID := os.Getenv("CIOS_CLI_CLIENT_ID")
	clientSecret := os.Getenv("CIOS_CLI_CLIENT_SECRET")
	if tokenURL == "" || clientID == "" || clientSecret == "" {
		return nil, nil
	}
	if _, err := url.Parse(tokenURL); err != nil {
		return nil, fmt.Errorf("CIOS_CLI_TOKEN_URL: %w", err)
	}
	return &envTokenSource{
		hc:           hc,
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		scope:        os.Getenv("CIOS_CLI_SCOPE"),
	}, nil
}

// Token returns a valid bearer. It returns the cached value if it is
// still fresh; otherwise it issues a new client_credentials grant.
func (e *envTokenSource) Token() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cached != "" && time.Now().Add(refreshWindow).Before(e.expiry) {
		return e.cached, nil
	}
	if err := e.fetchLocked(); err != nil {
		return "", err
	}
	return e.cached, nil
}

// fetchLocked performs the POST and updates cached/expiry. Caller
// must hold e.mu.
func (e *envTokenSource) fetchLocked() error {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {e.clientID},
		"client_secret": {e.clientSecret},
	}
	if e.scope != "" {
		form.Set("scope", e.scope)
	}
	req, err := http.NewRequest(http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return fmt.Errorf("token endpoint returned empty access_token")
	}
	e.cached = body.AccessToken
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	e.expiry = time.Now().Add(ttl)
	return nil
}
