// pkg/policy/pdp.go — PDP interface and OPA HTTP implementation
// (PRMT-104 §4).
//
// PRMT-104 §5 hard rule: OPA must FAIL-CLOSED. If the OPA endpoint
// is unreachable, returns non-2xx, or responds with a malformed
// body, Decision MUST return (false, err). The middleware in
// pkg/apigw treats any non-nil err as deny (per the §4 contract).
//
// The wire shape we POST to OPA is:
//
//	POST {opaURL}/v1/data/cios/authz/allow
//	Content-Type: application/json
//	{"input": <Input>}
//
// OPA's standard response is {"result": <boolean>}. We accept
// either {"result": true/false} (the OPA /v1/data contract) or a
// bare JSON boolean (some embedded deployments). Anything else is
// an error and fail-closes.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PDP is the policy decision point. Implementations are expected
// to be safe for concurrent use (the gateway may serve many
// requests in parallel).
type PDP interface {
	// Decision consults the policy engine and returns whether the
	// supplied Input is permitted. A non-nil err means the engine
	// could not produce a definitive answer; callers MUST treat
	// that as deny (PRMT-104 §5 fail-closed).
	Decision(ctx context.Context, in Input) (allow bool, err error)
}

// AssembleInput builds the PDP Input from a verified STS token
// and the request's site target (PRMT-110 §4). The apigw
// middleware is the single caller: it stitches Org / Sites from
// the token claims and TargetSite from the request, then hands
// the Input to PDP.Decision. The HTTP-sidecar PDP forwards
// every field verbatim; there is no per-call normalisation in
// the wire path (PRMT-110 §6 "不 fail-open"; the rego rule
// enforces fail-closed on missing or empty Org / Sites /
// TargetSite).
//
// This helper exists so the gateway middleware cannot
// accidentally drop an org/site field on the floor — every
// caller goes through one function and the contract is unit
// testable. It is intentionally tiny: it copies three values
// and leaves all other Input fields (Realm / Action / Method /
// Path / MFA / Time / Scope) to the caller, which is the only
// place that knows how to map an HTTP request to a context.
func AssembleInput(claims OrgSiteClaims, targetSite, realm, action, method, path string, mfa bool, now time.Time, scope []string) Input {
	return Input{
		Realm:      realm,
		Action:     action,
		Method:     method,
		Path:       path,
		MFA:        mfa,
		Time:       now,
		Scope:      scope,
		Org:        claims.Org,
		Sites:      claims.Sites,
		TargetSite: targetSite,
	}
}

// OrgSiteClaims is the minimal subset of the token claim set
// the PDP assembly needs (PRMT-110 §4). pkg/policy keeps no
// dependency on pkg/sts: the apigw middleware reads the
// verified token, extracts Org + Sites, and passes them via
// this struct. This avoids the import cycle that would arise
// if pkg/policy pulled in pkg/sts directly.
type OrgSiteClaims struct {
	Org   string
	Sites []string
}

// NewOPAPDP returns a PDP that talks to an OPA sidecar over HTTP.
//
//   - opaURL: base URL of the OPA server (e.g. "http://127.0.0.1:8181").
//     If empty, Decision fails closed — the middleware sees a
//     non-nil err and denies.
//   - hc: HTTP client to use. Pass nil to use http.DefaultClient
//     with a conservative timeout; production deployments should
//     inject a client with shorter timeouts.
func NewOPAPDP(opaURL string, hc *http.Client) PDP {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Second}
	}
	return &opaPDP{url: strings.TrimRight(opaURL, "/"), hc: hc}
}

// opaPDP is the HTTP-sidecar PDP. The url field is the trimmed
// base (no trailing slash); the request is built per call to keep
// timeout / context behaviour tight.
type opaPDP struct {
	url string
	hc  *http.Client
}

// ErrOPAUnreachable is returned when the OPA endpoint is not
// reachable / returns non-2xx / returns an unparseable body. The
// middleware in pkg/apigw treats any non-nil err from Decision as
// deny (fail-closed), so callers don't need to inspect this type;
// it is exported only so tests can assert on the cause.
var ErrOPAUnreachable = errors.New("policy: opa unreachable or returned error")

// Decision posts the Input to OPA and returns its verdict.
//
// Wire shape (PRMT-104 §4):
//   - Method: POST
//   - Path:   {base}/v1/data/cios/authz/allow
//   - Body:   {"input": <Input>}
//
// Response accepted:
//   - {"result": true|false}   — OPA standard
//   - true|false               — bare boolean
//
// Anything else → (false, ErrOPAUnreachable). Network errors →
// (false, err) wrapping ErrOPAUnreachable so tests can still find
// it via errors.Is.
func (p *opaPDP) Decision(ctx context.Context, in Input) (bool, error) {
	if p.url == "" {
		return false, ErrOPAUnreachable
	}
	body, err := json.Marshal(struct {
		Input Input `json:"input"`
	}{Input: in})
	if err != nil {
		// json.Marshal of this struct cannot fail in practice
		// (Input has only basic types and a []string), but the
		// fail-closed path still applies if it ever does.
		return false, fmt.Errorf("%w: marshal: %v", ErrOPAUnreachable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.url+"/v1/data/cios/authz/allow", bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("%w: new request: %v", ErrOPAUnreachable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: do: %v", ErrOPAUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a small prefix so a hung connection doesn't pin
		// the goroutine; the body is not surfaced to the caller.
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return false, fmt.Errorf("%w: status %d", ErrOPAUnreachable, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return false, fmt.Errorf("%w: read body: %v", ErrOPAUnreachable, err)
	}

	allow, ok := parseOPAResult(raw)
	if !ok {
		return false, fmt.Errorf("%w: unparseable body %q", ErrOPAUnreachable, trim(raw, 64))
	}
	return allow, nil
}

// parseOPAResult accepts both {"result": <bool>} and a bare
// <bool>. Anything else returns ok=false so the caller can
// fail-close.
func parseOPAResult(raw []byte) (bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, false
	}
	// Bare boolean?
	if trimmed[0] == 't' || trimmed[0] == 'f' {
		var b bool
		if err := json.Unmarshal(trimmed, &b); err == nil {
			return b, true
		}
		return false, false
	}
	// {"result": ...} form.
	var wrapped struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return false, false
	}
	if len(wrapped.Result) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(wrapped.Result, &b); err != nil {
		return false, false
	}
	return b, true
}

// trim returns the first n bytes of s as a string for log/error
// diagnostics. Exported only as a local helper; kept inside the
// package because the caller path is tightly scoped.
func trim(s []byte, n int) string {
	if len(s) <= n {
		return string(s)
	}
	return string(s[:n]) + "..."
}
