package core

// PRMT-235: class-A two-man rule — two-phase pending + second-token
// approve. Identities per the fixed CI accounts (config/rbac.lab-sample.yaml).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

const approvePathA = "sgp01.pod000.chiller000.compressor000.status"

// newApproveTestServer builds a fileStore-backed server (no auth
// middleware; principals injected per request via asPrincipal).
func newApproveTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil)
	return srv, srv.Handler()
}

// openPending PUTs a class-A :set as subject and returns the pending id.
func openPending(t *testing.T, h http.Handler, subject string) string {
	t.Helper()
	body, _ := json.Marshal(SetRequest{Value: 1, TTLSeconds: 30})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/"+approvePathA+":set", bytes.NewReader(body))
	req = asPrincipal(req, subject)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("open pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "pending" {
		t.Fatalf("want status=pending got %v", resp)
	}
	id, _ := resp["pending_id"].(string)
	if id == "" {
		t.Fatalf("no pending_id: %v", resp)
	}
	return id
}

func doApprove(h http.Handler, id string, mutate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/control/"+id+":approve", nil)
	if mutate != nil {
		req = mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestControlApprove_SelfApproval403(t *testing.T) {
	_, h := newApproveTestServer(t)
	id := openPending(t, h, ciOperator)
	rec := doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciOperator) })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestControlApprove_ApproverScope403(t *testing.T) {
	_, h := newApproveTestServer(t)

	// viewer role floor
	id := openPending(t, h, ciAdmin)
	rec := doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciViewer) })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer approve: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// operator whose scope does not cover the path (write is explicit
	// per L50 — site99.** cannot match sgp01.…). §7.2: fixed subject,
	// scopes minimized per use case.
	rec = doApprove(h, id, func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal,
			Principal{Subject: ciOperator, Role: RoleOperator, Scopes: []string{"site99.**"}}))
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped approve: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// same pending still approvable by a proper second bearer
	rec = doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciOperator) })
	if rec.Code != http.StatusAccepted {
		t.Fatalf("good approve after denials: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestControlApprove_Expired410(t *testing.T) {
	srv, h := newApproveTestServer(t)
	id := openPending(t, h, ciOperator) // ttl_seconds=30
	srv.now = func() time.Time { return time.Now().Add(31 * time.Second) }
	rec := doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciAdmin) })
	if rec.Code != http.StatusGone {
		t.Fatalf("expired approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestControlApprove_ConsumedOnce(t *testing.T) {
	_, h := newApproveTestServer(t)
	id := openPending(t, h, ciOperator)
	if rec := doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciAdmin) }); rec.Code != http.StatusAccepted {
		t.Fatalf("first approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doApprove(h, id, func(r *http.Request) *http.Request { return asPrincipal(r, ciAdmin) }); rec.Code != http.StatusNotFound {
		t.Fatalf("second approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestControlApprove_NoPrincipal401(t *testing.T) {
	_, h := newApproveTestServer(t)

	// A-class :set with only the client-controlled header → 401.
	body, _ := json.Marshal(SetRequest{Value: 1, TTLSeconds: 30})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/"+approvePathA+":set", bytes.NewReader(body))
	req.Header.Set("X-CIOS-Actor", "mallory")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header-only A set: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// :approve without a principal → 401.
	id := openPending(t, h, ciOperator)
	if rec := doApprove(h, id, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-principal approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestControlApprove_CClassHeaderFallbackUnchanged(t *testing.T) {
	_, h := newApproveTestServer(t)
	// class C (gpu.clock): single actor + audit; the header fallback
	// for lab demos stays — PRMT-235 restricts it for class A only.
	body, _ := json.Marshal(SetRequest{Value: 1200, TTLSeconds: 10})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/sgp01.pod000.rack000.node000.gpu0.clock:set", bytes.NewReader(body))
	req.Header.Set("X-CIOS-Actor", "e2e-operator")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("C header set: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "accepted" || resp["risk_class"] != "c" {
		t.Fatalf("resp=%v", resp)
	}
}
