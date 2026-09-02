// Package apigw implements the CIOS north-bound experience-layer API
// Gateway (spec-009 §7.1, PRMT-101). It is the single external entry
// point for the Portal: it exposes /api/* on the outside and consumes
// core /v1/* on the inside. This file is the configuration surface.
package apigw

import (
	"log"
	"os"
	"strings"
)

// Config holds runtime settings for the API Gateway process.
//
// Fields are sourced from environment variables (PRMT-101 §4):
//
//   - CIOS_APIGW_LISTEN     → ListenAddr  (default ":8443")
//   - CIOS_APIGW_UPSTREAM   → UpstreamURL (REQUIRED; LoadConfig returns
//     an error if unset or empty)
//   - CIOS_APIGW_SCENE_DIR  → SceneDir    (optional, PRMT-170; empty
//     disables /api/twins/* — both routes 404
//     with a one-shot log at first request)
//   - CIOS_APIGW_DEV_NO_AUTH → DevNoAuth (optional, PRMT-173;
//     truthy ⇒ AuthMiddleware injects fixed dev claims on the
//     pass-through branch. Forces false if STS/OPA are also
//     configured at boot — fail-closed preserved.)
//   - CIOS_APIGW_SCENE_BUCKETS → SceneBucketsEnabled (optional,
//     PRMT-180; truthy ⇒ /api/twins/scene serves per-role
//     pre-pruned buckets under <SceneDir>/buckets/<bucket>/<site>.scene.json
//     and never the base scene. parseBool truthy set only;
//     default false = PRMT-170 passthrough. STS/OPA interaction
//     is intentionally NOT changed by this gate.)
type Config struct {
	ListenAddr  string // e.g. ":8443"
	UpstreamURL string // core /v1 base, e.g. "https://127.0.0.1:9443"
	// SceneDir is the on-disk directory holding Scene Engine v0
	// artefacts produced by PRMT-169 (offline transcoder): each
	// site's <site>.scene.json and <name>.glb geometry blob are
	// served read-only via /api/twins/* (PRMT-170). Empty means
	// the twins routes are disabled — every request to that
	// surface returns 404 with a one-shot stderr log so a missing
	// configuration is observable without crashing the gateway.
	SceneDir string // absolute or process-relative path; empty = disabled
	// DevNoAuth is the opt-in dev claims injection gate (PRMT-173).
	// When true AuthMiddleware's pass-through branch stamps a
	// fixed sts.TokenClaims (Subject="dev-no-auth", Tenant="dev",
	// IsolationTier="label") into r.Context() so handler-layer
	// ClaimsFrom returns ok. Default false; LoadConfig also forces
	// it false when CIOS_STS_SIGNING_KEY or CIOS_OPA_URL is set
	// so a production deployment can never anonymously land.
	DevNoAuth bool
	// SceneBucketsEnabled is the PRMT-180 wire gate. When true,
	// /api/twins/scene resolves a per-role pre-pruned bucket file
	// under <SceneDir>/buckets/<bucket>/<site>.scene.json — never
	// the base scene — and the gateway performs zero glob/identity
	// filtering of its own (L91 red line; filtering already happened
	// upstream in the offline generator). When false (default),
	// /api/twins/scene behaves exactly as PRMT-170: it serves
	// <SceneDir>/<site>.scene.json verbatim.
	SceneBucketsEnabled bool
}

const (
	envListen       = "CIOS_APIGW_LISTEN"
	envUpstream     = "CIOS_APIGW_UPSTREAM"
	envSceneDir     = "CIOS_APIGW_SCENE_DIR"
	envSceneBuckets = "CIOS_APIGW_SCENE_BUCKETS"

	defaultListen = ":8443"
)

// LoadConfig reads the gateway configuration from environment
// variables. UpstreamURL is mandatory; an empty value yields an
// error so the process fails fast at startup rather than silently
// serving requests against an unconfigured upstream.
//
// ListenAddr is optional; the default is ":8443".
//
// SceneDir is optional (PRMT-170). An empty value is allowed and
// disables /api/twins/* — the handler logs once at first request
// and 404s, matching the "missing config surfaces visibly, not
// silently" pattern used elsewhere in this package.
//
// DevNoAuth is opt-in (PRMT-173). A truthy value that co-exists
// with CIOS_STS_SIGNING_KEY / CIOS_OPA_URL is forced back to false
// with a one-shot WARN log so production auth wins.
func LoadConfig() (Config, error) {
	cfg := Config{
		ListenAddr:          os.Getenv(envListen),
		UpstreamURL:         os.Getenv(envUpstream),
		SceneDir:            os.Getenv(envSceneDir),
		DevNoAuth:           parseBool(os.Getenv(envDevNoAuth)),
		SceneBucketsEnabled: parseBool(os.Getenv(envSceneBuckets)),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListen
	}
	if cfg.UpstreamURL == "" {
		return Config{}, errUpstreamRequired
	}
	// PRMT-173 §4.2: STS/OPA configured ⇒ DevNoAuth silently
	// demoted to false. The injection point is also physically
	// unreachable from a wired auth state (AuthMiddleware goes
	// full-branch), so this is defence-in-depth.
	if cfg.DevNoAuth {
		if os.Getenv(envSTSSigningKey) != "" || os.Getenv(envOPAURL) != "" {
			log.Printf("WARN: CIOS_APIGW_DEV_NO_AUTH ignored: STS/OPA configured; dev claims will NOT be injected (fail-closed preserved)")
			cfg.DevNoAuth = false
		}
	}
	// PRMT-217 (report S-1): production builds compile out the
	// inject path; refusing DevNoAuth here is fail-closed rather
	// than a half-working no-auth gateway.
	if cfg.DevNoAuth && !devBypassAvailable {
		return Config{}, errDevNoAuthRequiresLab
	}
	return cfg, nil
}

// errDevNoAuthRequiresLab is returned by LoadConfig when
// CIOS_APIGW_DEV_NO_AUTH is set on a production (non-lab) build.
var errDevNoAuthRequiresLab = &configError{
	msg: "refusing to start: CIOS_APIGW_DEV_NO_AUTH requires a lab build (go build -tags lab); this binary has the auth bypass compiled out",
}

// parseBool accepts only "1", "true", "yes" (case-insensitive,
// surrounding whitespace ignored). Everything else — including the
// empty string — is false. Deliberately narrow: "on", "enable", and
// "non-empty ⇒ true" are explicitly out (PRMT-173 §6 MUST NOT).
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// errUpstreamRequired is returned by LoadConfig when the operator
// forgot to set CIOS_APIGW_UPSTREAM. Surfaced verbatim to the
// caller (main) so the process can exit non-zero with a clear
// stderr message.
var errUpstreamRequired = &configError{msg: "CIOS_APIGW_UPSTREAM is required"}

// configError is a tiny error type so LoadConfig can hand back a
// sentinel without pulling in fmt/strings just for one message.
// Satisfies errors.Is / errors.As by virtue of being a non-nil
// error value distinct from any other error in the package.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }
