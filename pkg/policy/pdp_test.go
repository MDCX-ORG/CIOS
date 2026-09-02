// pkg/policy/pdp_test.go — table-driven tests for the OPA HTTP
// PDP. PRMT-104 §5 acceptance: every MUST has at least one test.
//
// Coverage map:
//   - happy allow / deny round-trip via httptest mock OPA
//   - bare {"result":bool} and bare bool response shapes
//   - non-2xx → fail-closed (403-equivalent at middleware layer)
//   - network failure (closed server) → fail-closed
//   - empty opaURL → fail-closed
//   - malformed body → fail-closed
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newMockOPA spins up an httptest server that always answers with
// the supplied allow verdict in {"result": bool} form. Returning
// the cleanup func lets a subtest release the port on failure.
func newMockOPA(t *testing.T, allow bool) (*httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("mock OPA got method %q, want POST", r.Method)
		}
		// Consume the body so the client side closes cleanly.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result": ` + boolJSON(allow) + `}`))
	}))
	return srv, srv.Close
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestOPAPDP_Allow: a 200 with {"result":true} → (true, nil).
func TestOPAPDP_Allow(t *testing.T) {
	srv, cleanup := newMockOPA(t, true)
	defer cleanup()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	allow, err := pdp.Decision(context.Background(), Input{
		Realm: "ops", Action: "read", Method: "GET", Path: "/api/sites",
		Time: time.Now(),
	})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if !allow {
		t.Errorf("allow = false, want true")
	}
}

// TestOPAPDP_Deny: a 200 with {"result":false} → (false, nil).
// The middleware translates that to 403; the PDP itself does not
// treat deny as an error.
func TestOPAPDP_Deny(t *testing.T) {
	srv, cleanup := newMockOPA(t, false)
	defer cleanup()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	allow, err := pdp.Decision(context.Background(), Input{
		Realm: "customer", Action: "write", Method: "POST", Path: "/api/x",
		MFA:  false,
		Time: time.Now(),
	})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if allow {
		t.Errorf("allow = true, want false")
	}
}

// TestOPAPDP_BareBoolResponse: a deployment that returns a bare
// JSON boolean (no {"result":...} wrapper) must also be
// understood. The wire contract is the OPA standard one; we
// accept the bare form as a defensive courtesy.
func TestOPAPDP_BareBoolResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`true`))
	}))
	defer srv.Close()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	allow, err := pdp.Decision(context.Background(), Input{Realm: "ops", Action: "read", Time: time.Now()})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if !allow {
		t.Errorf("bare true: allow = false")
	}
}

// TestOPAPDP_5xx_FailClosed: non-2xx → fail-closed. PRMT-104 §5
// explicitly bans fail-open.
func TestOPAPDP_5xx_FailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "opa down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	allow, err := pdp.Decision(context.Background(), Input{Realm: "ops", Action: "read", Time: time.Now()})
	if err == nil {
		t.Fatalf("Decision: nil err, want ErrOPAUnreachable")
	}
	if !errors.Is(err, ErrOPAUnreachable) {
		t.Errorf("err = %v, want wraps ErrOPAUnreachable", err)
	}
	if allow {
		t.Errorf("allow = true on 5xx; want false (fail-closed)")
	}
}

// TestOPAPDP_MalformedBody_FailClosed: 200 with garbage body must
// not leak through as allow. Anything we cannot parse is deny.
func TestOPAPDP_MalformedBody_FailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result": "not-a-bool"}`))
	}))
	defer srv.Close()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	allow, err := pdp.Decision(context.Background(), Input{Realm: "ops", Action: "read", Time: time.Now()})
	if err == nil {
		t.Fatalf("Decision: nil err, want ErrOPAUnreachable")
	}
	if allow {
		t.Errorf("allow = true on garbage body; want false")
	}
}

// TestOPAPDP_NetworkError_FailClosed: a closed server simulates
// the case where OPA is not running. Must fail closed.
func TestOPAPDP_NetworkError_FailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	pdp := NewOPAPDP(srv.URL, srv.Client())
	srv.Close() // immediately stop; next call will get ECONNREFUSED.

	allow, err := pdp.Decision(context.Background(), Input{Realm: "ops", Action: "read", Time: time.Now()})
	if err == nil {
		t.Fatalf("Decision: nil err, want ErrOPAUnreachable")
	}
	if !errors.Is(err, ErrOPAUnreachable) {
		t.Errorf("err = %v, want wraps ErrOPAUnreachable", err)
	}
	if allow {
		t.Errorf("allow = true on connection refused; want false")
	}
}

// TestOPAPDP_EmptyURL_FailClosed: an empty opaURL is the
// "operator forgot to deploy OPA" case. Fail closed (deny) at the
// PDP layer rather than allow.
func TestOPAPDP_EmptyURL_FailClosed(t *testing.T) {
	pdp := NewOPAPDP("", http.DefaultClient)
	allow, err := pdp.Decision(context.Background(), Input{Realm: "ops", Action: "read", Time: time.Now()})
	if err == nil {
		t.Fatalf("Decision: nil err, want ErrOPAUnreachable")
	}
	if !errors.Is(err, ErrOPAUnreachable) {
		t.Errorf("err = %v, want ErrOPAUnreachable", err)
	}
	if allow {
		t.Errorf("allow = true on empty URL; want false")
	}
}

// TestOPAPDP_CarriesInput: the JSON body sent to OPA must
// serialise the Input verbatim. The middleware in pkg/apigw
// depends on this shape (it sets Time to time.Now() and pins
// Action=read for GET). A regression here is silent until OPA
// denies, so we pin the wire shape explicitly.
func TestOPAPDP_CarriesInput(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"result": false}`))
	}))
	defer srv.Close()
	pdp := NewOPAPDP(srv.URL, srv.Client())

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	_, _ = pdp.Decision(context.Background(), Input{
		Realm: "ops", Action: "read", Method: "GET", Path: "/api/sites",
		MFA: false, Time: now, Scope: []string{"viewer"},
	})
	if gotPath != "/v1/data/cios/authz/allow" {
		t.Errorf("OPA path = %q, want /v1/data/cios/authz/allow", gotPath)
	}
	in, ok := gotBody["input"].(map[string]any)
	if !ok {
		t.Fatalf("body.input is not an object: %#v", gotBody)
	}
	if in["realm"] != "ops" || in["action"] != "read" || in["method"] != "GET" {
		t.Errorf("input fields wrong: %#v", in)
	}
	if in["path"] != "/api/sites" {
		t.Errorf("input.path = %v, want /api/sites", in["path"])
	}
	if in["mfa"] != false {
		t.Errorf("input.mfa = %v, want false", in["mfa"])
	}
	scope, ok := in["scope"].([]any)
	if !ok || len(scope) != 1 || scope[0] != "viewer" {
		t.Errorf("input.scope = %v, want [viewer]", in["scope"])
	}
}
