// Package cli — client.go: thin HTTP client for the cios-core M0 API.
//
// The client follows the R0 勘误 contract (PRMT-012 §4.3):
//   - network error → (status=0, body=nil, err=*NetError)
//   - RFC 7807 problem response → (status, body, err=*Problem)
//   - any other response → (status, body, err=nil)
//
// Callers use errors.As to discriminate and follow the §5 stderr rules.
package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/reqid"
)

// Client is the API client. Base holds the cios-core root URL with no
// trailing slash. HTTP is fixed to a 10s timeout client; callers do
// not get to override it. tok is populated from CIOS_CLI_* env when
// present; nil means no-auth mode (pre-PRMT-116 behavior).
type Client struct {
	Base string
	HTTP *http.Client
	tok  tokenSource
}

// NewClient builds a client with the prescribed 10s timeout. When
// CIOS_CLI_TOKEN_URL + CIOS_CLI_CLIENT_ID + CIOS_CLI_CLIENT_SECRET
// are all set, Do will attach an Authorization: Bearer <STS token>
// header. The signature is unchanged; env-missing silently keeps the
// pre-PRMT-116 no-auth path.
func NewClient(base string) *Client {
	hc := &http.Client{Timeout: 10 * time.Second}
	ts, _ := newEnvTokenSource(hc)
	return &Client{
		Base: strings.TrimRight(base, "/"),
		HTTP: hc,
		tok:  ts,
	}
}

// Problem mirrors the RFC 7807 fields the core emits. Empty fields
// are tolerated; Error() omits empty segments and their parens.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Instance  string `json:"instance"`
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
}

// Error renders "<title>: <detail> (request_id=<id>)", omitting any
// empty segment and the parens if request_id is empty.
func (p *Problem) Error() string {
	head := strings.TrimSpace(p.Title)
	if p.Detail != "" {
		if head != "" {
			head += ": "
		}
		head += p.Detail
	}
	if head == "" {
		head = "problem"
	}
	if p.RequestID == "" {
		return head
	}
	return head + " (request_id=" + p.RequestID + ")"
}

// NetError wraps a transport-level failure so the caller can render
// "error: net <op>: <err>" per §5 MUST.
type NetError struct {
	Op  string
	Err error
}

func (e *NetError) Error() string { return "net " + e.Op + ": " + e.Err.Error() }
func (e *NetError) Unwrap() error { return e.Err }

// Do executes one request. See package doc for the return contract.
// body may be nil; otherwise it is JSON-encoded and Content-Type
// application/json is set.
func (c *Client) Do(method, path string, query url.Values, body any) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, &NetError{Op: "marshal", Err: err}
		}
		bodyReader = strings.NewReader(string(buf))
	}
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, bodyReader)
	if err != nil {
		return 0, nil, &NetError{Op: "new request", Err: err}
	}
	req.Header.Set("X-Request-Id", reqid.New())
	req.Header.Set("User-Agent", "cios/dev")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.tok != nil {
		t, err := c.tok.Token()
		if err != nil {
			return 0, nil, &NetError{Op: "auth", Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, &NetError{Op: "do", Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, &NetError{Op: "read body", Err: err}
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "application/problem+json") {
		var p Problem
		if err := json.Unmarshal(raw, &p); err != nil {
			// Per §4.3: prefix matched but body not valid JSON → fall
			// back to "other response" (err=nil, status preserved).
			return resp.StatusCode, raw, nil
		}
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return resp.StatusCode, raw, &p
	}
	return resp.StatusCode, raw, nil
}

// IsNetError / IsProblem let tests branch without errors.As imports.
func IsNetError(err error) (*NetError, bool) {
	var ne *NetError
	if errors.As(err, &ne) {
		return ne, true
	}
	return nil, false
}

func IsProblem(err error) (*Problem, bool) {
	var p *Problem
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

// newRequestID moved to pkg/reqid (PRMT-030 §A). Call sites import
// github.com/yurimeng/cios/pkg/reqid directly.
