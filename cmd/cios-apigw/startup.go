// Startup wiring for cios-apigw (PRMT-111). Builds the OIDC
// /auth/{realm}/* handler, the optional mTLS-tuned upstream
// client, and validates the auth-mode env at startup so missing
// required configuration fails the process before any request is
// served (replacing PRMT-101's silent-degrade behaviour).
//
// This file is in package main: it is the assembly step between
// pkg/apigw, pkg/authn, and pkg/policy. No business logic — every
// behaviour comes from a constructor we own and pin.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/apigw"
	"github.com/yurimeng/cios/pkg/authn"
	"github.com/yurimeng/cios/pkg/mtls"
	"github.com/yurimeng/cios/pkg/policy"
)

// Env names read by loadStartupConfig. PRMT-111 §4 — all
// CIOS_-prefixed, no hardcoded defaults for secrets.
const (
	envAuthMode    = "CIOS_APIGW_AUTH_MODE"
	envAllowNoAuth = "CIOS_APIGW_ALLOW_NO_AUTH" // CODE-SCAN H1 / L104: explicit no-auth opt-out
	envDevNoAuth   = "CIOS_APIGW_DEV_NO_AUTH"   // PRMT-173; also counts as explicit no-auth opt-out
	// PRMT-216 R2: mirrors core's -allow-public-bind. The original PRMT
	// declined this hatch on the (incorrect) premise that apigw is never
	// containerised; deploy/edge/docker-compose.apps.yml does run it in a
	// container, where binding loopback inside the container would make
	// the published port unreachable.
	envAllowPublicBind  = "CIOS_APIGW_ALLOW_PUBLIC_BIND"
	envSessionKey       = "CIOS_APIGW_SESSION_KEY" // mirrors apigw.envSessionKey
	envSTSSigningKey    = "CIOS_STS_SIGNING_KEY"   // mirrors apigw.envSTSSigningKey
	envOPAURL           = "CIOS_OPA_URL"           // mirrors apigw.envOPAURL
	envUpstreamTimeout  = "CIOS_APIGW_UPSTREAM_TIMEOUT"
	envTLSCA            = "CIOS_APIGW_TLS_CA"
	envTLSCert          = "CIOS_APIGW_TLS_CERT"
	envTLSKey           = "CIOS_APIGW_TLS_KEY"
	envMTLSMode         = "CIOS_MTLS_MODE" // P793: off|require (shared with core)
	envOIDCOpsIssuer    = "CIOS_OIDC_OPS_ISSUER"
	envOIDCOpsClientID  = "CIOS_OIDC_OPS_CLIENT_ID"
	envOIDCOpsSecret    = "CIOS_OIDC_OPS_CLIENT_SECRET"
	envOIDCOpsRedirect  = "CIOS_OIDC_OPS_REDIRECT_URL"
	envOIDCCustIssuer   = "CIOS_OIDC_CUSTOMER_ISSUER"
	envOIDCCustClientID = "CIOS_OIDC_CUSTOMER_CLIENT_ID"
	envOIDCCustSecret   = "CIOS_OIDC_CUSTOMER_CLIENT_SECRET"
	envOIDCCustRedirect = "CIOS_OIDC_CUSTOMER_REDIRECT_URL"
)

const defaultUpstreamTimeout = 10 * time.Second

// startupConfig is the value main wires into NewServer / NewUpstream /
// SetPDP / SetOmniverseHTTPClient / SetAuthHandler. See PRMT-111 §4.
type startupConfig struct {
	AuthMode    bool
	TLSEnabled  bool
	UpstreamTLS *tls.Config                    // nil → no mTLS, plain TLS client
	UpstreamHC  *http.Client                   // always non-nil; carries timeout + optional mTLS
	OPAURL      string                         // empty → no PDP
	Verifiers   map[authn.Realm]authn.Verifier // nil when AuthMode=false
}

// loadStartupConfig reads env, validates the auth-mode contract
// (PRMT-111 §2/§5 + CODE-SCAN-2026-07-16 H1 / L104 / spec-006 §5.0bis),
// and builds the mTLS client and OIDC verifiers. Any required env
// missing under AUTH_MODE=on yields a non-nil error whose message
// names the missing variable; main translates that into log.Fatalf
// (non-zero exit).
//
// validateDevNoAuthListen enforces PRMT-216 (report S-3): DevNoAuth
// injects fixed claims. Non-loopback bind requires an explicit
// CIOS_APIGW_ALLOW_PUBLIC_BIND=1 hatch (R2; mirrors core
// -allow-public-bind for containerised lab stacks).
//
// Call after apigw.LoadConfig so STS/OPA demotion of DevNoAuth runs first.
func validateDevNoAuthListen(cfg apigw.Config) error {
	if !cfg.DevNoAuth {
		return nil
	}
	lo, err := isLoopbackHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid CIOS_APIGW_LISTEN %q: %w", cfg.ListenAddr, err)
	}
	if lo {
		return nil
	}
	// "1" only (PRMT-216 R2); other truthy forms stay closed.
	allowPublic := strings.TrimSpace(os.Getenv(envAllowPublicBind)) == "1"
	if !allowPublic {
		return fmt.Errorf(
			"refusing to start: CIOS_APIGW_DEV_NO_AUTH=1 with non-loopback listen %q "+
				"would expose fixed dev claims on the network; set CIOS_APIGW_LISTEN to a "+
				"loopback address (e.g. 127.0.0.1:8089), or set %s=1 if the exposure is "+
				"intentional (e.g. container with loopback-only port publishing)",
			cfg.ListenAddr, envAllowPublicBind)
	}
	log.Printf("WARN: CIOS_APIGW_DEV_NO_AUTH=1 on non-loopback %s with %s=1: "+
		"every reachable client bypasses auth (dev only)",
		cfg.ListenAddr, envAllowPublicBind)
	return nil
}

// isLoopbackHostPort reports whether addr binds only the loopback
// interface. Empty host (":8089") means all interfaces — not loopback.
func isLoopbackHostPort(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" {
		return false, nil
	}
	if h == "127.0.0.1" || h == "::1" || h == "localhost" || h == "[::1]" {
		return true, nil
	}
	return false, nil
}

// Fail-closed boot (H1): when AUTH_MODE is unset or not "on", the
// process refuses to start unless the operator explicitly opts out
// via CIOS_APIGW_ALLOW_NO_AUTH=1 or CIOS_APIGW_DEV_NO_AUTH=1 (lab
// recipe; mirrors cios-core -allow-no-auth). Silent no-auth boot is
// no longer allowed.
func loadStartupConfig() (startupConfig, error) {
	sc := startupConfig{
		AuthMode: strings.EqualFold(os.Getenv(envAuthMode), "on"),
		OPAURL:   os.Getenv(envOPAURL),
		UpstreamHC: &http.Client{
			Timeout: parseUpstreamTimeout(),
		},
	}

	tlsCfg, tlsEnabled, err := buildUpstreamTLS()
	if err != nil {
		return startupConfig{}, err
	}
	// P793: CIOS_MTLS_MODE=require forces upstream client mTLS material.
	// Unknown mode → refuse boot (fail-closed; M4 F2).
	mtlsMode, err := mtls.ParseMode(os.Getenv(envMTLSMode))
	if err != nil {
		return startupConfig{}, err
	}
	if mtlsMode == mtls.ModeRequire {
		if !tlsEnabled {
			return startupConfig{}, fmt.Errorf(
				"CIOS_MTLS_MODE=require requires %s + %s + %s (apigw→core client mTLS)",
				envTLSCA, envTLSCert, envTLSKey)
		}
	}
	sc.UpstreamTLS = tlsCfg
	sc.TLSEnabled = tlsEnabled
	if tlsEnabled {
		// Inject the mTLS config into the transport so upstream
		// HTTPS requests authenticate with the operator-supplied
		// client cert (spec-006 §5 / P793).
		sc.UpstreamHC.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}

	if !sc.AuthMode {
		// H1: require explicit opt-out (ALLOW_NO_AUTH or DEV_NO_AUTH).
		allow := truthyEnv(envAllowNoAuth) || truthyEnv(envDevNoAuth)
		if !allow {
			return startupConfig{}, fmt.Errorf(
				"refusing to start without auth: set %s=on (with required keys) or pass %s=1 / %s=1 for lab/dev",
				envAuthMode, envAllowNoAuth, envDevNoAuth)
		}
		return sc, nil
	}

	// AUTH_MODE=on path. Validate session + STS keys first so
	// fail-fast messages name the most fundamental missing input
	// before we get into OIDC realm details.
	var missing []string
	if os.Getenv(envSessionKey) == "" {
		missing = append(missing, envSessionKey)
	}
	if os.Getenv(envSTSSigningKey) == "" {
		missing = append(missing, envSTSSigningKey)
	}
	if len(missing) > 0 {
		return startupConfig{}, fmt.Errorf(
			"CIOS_APIGW_AUTH_MODE=on requires: %s",
			strings.Join(missing, ", "))
	}

	verifiers, err := buildOIDCVerifiers()
	if err != nil {
		return startupConfig{}, err
	}
	sc.Verifiers = verifiers
	return sc, nil
}

// buildOIDCVerifiers materialises the per-realm Verifier map
// driven by CIOS_OIDC_{OPS,CUSTOMER}_* env vars. A realm is
// considered "enabled" if any of its four vars is set; when
// enabled, all four must be set (spec-009 §7.1 + PRMT-102 §3 —
// half-configured OIDC is a misconfiguration we want surfaced at
// boot, not at first login).
func buildOIDCVerifiers() (map[authn.Realm]authn.Verifier, error) {
	out := map[authn.Realm]authn.Verifier{}
	var errs []string

	opsVals := map[string]string{
		envOIDCOpsIssuer:   os.Getenv(envOIDCOpsIssuer),
		envOIDCOpsClientID: os.Getenv(envOIDCOpsClientID),
		envOIDCOpsSecret:   os.Getenv(envOIDCOpsSecret),
		envOIDCOpsRedirect: os.Getenv(envOIDCOpsRedirect),
	}
	if realmEnabled(opsVals) {
		if missing := requiredMissing(opsVals); len(missing) > 0 {
			errs = append(errs, fmt.Sprintf("ops realm missing: %s", strings.Join(missing, ", ")))
		} else {
			v, err := authn.NewOIDCVerifier(authn.OIDCConfig{
				Realm:        authn.RealmOps,
				IssuerURL:    opsVals[envOIDCOpsIssuer],
				ClientID:     opsVals[envOIDCOpsClientID],
				ClientSecret: opsVals[envOIDCOpsSecret],
				RedirectURL:  opsVals[envOIDCOpsRedirect],
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("ops realm verifier: %v", err))
			} else {
				out[authn.RealmOps] = v
			}
		}
	}

	custVals := map[string]string{
		envOIDCCustIssuer:   os.Getenv(envOIDCCustIssuer),
		envOIDCCustClientID: os.Getenv(envOIDCCustClientID),
		envOIDCCustSecret:   os.Getenv(envOIDCCustSecret),
		envOIDCCustRedirect: os.Getenv(envOIDCCustRedirect),
	}
	if realmEnabled(custVals) {
		if missing := requiredMissing(custVals); len(missing) > 0 {
			errs = append(errs, fmt.Sprintf("customer realm missing: %s", strings.Join(missing, ", ")))
		} else {
			v, err := authn.NewOIDCVerifier(authn.OIDCConfig{
				Realm:        authn.RealmCustomer,
				IssuerURL:    custVals[envOIDCCustIssuer],
				ClientID:     custVals[envOIDCCustClientID],
				ClientSecret: custVals[envOIDCCustSecret],
				RedirectURL:  custVals[envOIDCCustRedirect],
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("customer realm verifier: %v", err))
			} else {
				out[authn.RealmCustomer] = v
			}
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"CIOS_APIGW_AUTH_MODE=on requires at least one realm " +
				"(set CIOS_OIDC_OPS_* or CIOS_OIDC_CUSTOMER_* env vars)")
	}
	return out, nil
}

// realmEnabled reports whether any of the four OIDC env vars for
// a realm is set. "Any" is the contract: an operator who sets
// CIOS_OIDC_OPS_ISSUER has clearly opted into the ops realm and
// must supply the rest.
func realmEnabled(vals map[string]string) bool {
	for _, v := range vals {
		if v != "" {
			return true
		}
	}
	return false
}

// requiredMissing returns the env var names in vals whose value
// is empty. Order is not significant; the caller joins the slice
// into a stderr message.
func requiredMissing(vals map[string]string) []string {
	var missing []string
	for k, v := range vals {
		if v == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

// truthyEnv reports whether env var name is set to a truthy value
// (1/true/yes/on, case-insensitive). Used for ALLOW_NO_AUTH / DEV_NO_AUTH.
func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseUpstreamTimeout reads CIOS_APIGW_UPSTREAM_TIMEOUT;
// invalid or empty falls back to the 10s default (PRMT-111 §4).
func parseUpstreamTimeout() time.Duration {
	raw := os.Getenv(envUpstreamTimeout)
	if raw == "" {
		return defaultUpstreamTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultUpstreamTimeout
	}
	return d
}

// buildUpstreamTLS reads the optional CIOS_APIGW_TLS_{CA,CERT,KEY}
// triple. Per PRMT-111 §4: "任一非空" → mTLS is enabled and all
// three must be readable; "全空" → no mTLS and a plain timeout-only
// client. Returns (cfg, enabled, error).
func buildUpstreamTLS() (*tls.Config, bool, error) {
	caPath := os.Getenv(envTLSCA)
	certPath := os.Getenv(envTLSCert)
	keyPath := os.Getenv(envTLSKey)
	if caPath == "" && certPath == "" && keyPath == "" {
		return nil, false, nil
	}
	// All three must be present together — partial mTLS is
	// either a config bug or a deliberate override we want to
	// surface loudly.
	if caPath == "" || certPath == "" || keyPath == "" {
		return nil, false, fmt.Errorf(
			"%s, %s, %s must be set together (got CA=%q CERT=%q KEY=%q)",
			envTLSCA, envTLSCert, envTLSKey,
			caPath, certPath, keyPath)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", envTLSCA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, false, fmt.Errorf("%s has no valid PEM certificates", envTLSCA)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, false, fmt.Errorf("load client cert/key: %w", err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, true, nil
}

// buildAuthHandler materialises the /auth/{realm}/* handler from
// the per-realm verifiers and the session HMAC key. When
// AuthMode is off (or no verifiers are present), returns
// (nil, nil) so main skips the SetAuthHandler call — preserving
// PRMT-101's "no auth required" boot path.
func buildAuthHandler(sc startupConfig, sessionKey []byte) (*authn.Handler, error) {
	if !sc.AuthMode || len(sc.Verifiers) == 0 {
		return nil, nil
	}
	return authn.NewHandler(authn.HandlerConfig{
		Verifiers:  sc.Verifiers,
		SessionKey: sessionKey,
	})
}

// buildOPAPDP is exposed so main has a single call site for the
// "PDP only when CIOS_OPA_URL is set" decision. Empty URL →
// (nil, nil) so main skips the SetPDP call (server.loadPDP would
// also no-op, but doing the check here keeps the wiring intent
// visible at the assembly layer rather than buried in pkg/apigw).
func buildOPAPDP(opaURL string, hc *http.Client) policy.PDP {
	if opaURL == "" {
		return nil
	}
	return policy.NewOPAPDP(opaURL, hc)
}
