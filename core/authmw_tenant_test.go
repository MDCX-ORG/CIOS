// Package core — authmw_tenant_test.go: tests for PRMT-188's
// core-side tenant-identity seam. Verifies that authMiddleware
// extracts X-CIOS-Tenant into ctx on the allow path (and ONLY on
// the allow path) and that TenantFromContext mirrors the
// PrincipalFromContext accessor shape.
//
// Spec basis: PRMT-188 §2-bis (world after) + §7 (acceptance:
// header present → (tid, true); absent → ("", false);
// present-but-empty → ("", false); tenant NOT attached on a 401
// (bad token) or 403 (out-of-scope) response).
package core

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// tenantCapture mirrors the existing passthroughInner pattern but
// records what TenantFromContext returns from the request context.
// The handler is only invoked on the allow path; on 401/403 the
// middleware short-circuits BEFORE calling inner, so the captured
// bool+string tells us whether the middleware attached the tenant
// at all (not just whether it forwarded the header).
type tenantCapture struct {
	mu       sync.Mutex
	tenantID string
	present  bool
	reached  bool // true iff the inner handler actually ran
}

// tenantCaptureHandler returns 200 + records what the handler saw
// on ctx. Used to assert the middleware attached (or did not attach)
// the tenant before forwarding.
func tenantCaptureHandler(cap *tenantCapture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := TenantFromContext(r.Context())
		cap.mu.Lock()
		cap.tenantID = tid
		cap.present = ok
		cap.reached = true
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// tenantNotReachedHandler asserts (in t.Errorf form) that the inner
// handler was NOT reached. Used for the 401/403 deny-path tests
// where the middleware short-circuits; if we get here at all the
// middleware let the request through, which is the bug we're
// guarding against.
func tenantNotReachedHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("inner handler was reached on a deny path (status would be %d); middleware must short-circuit on 401/403 before forwarding", http.StatusOK)
		w.WriteHeader(http.StatusOK)
	})
}

// --- §2-bis acceptance cases ---------------------------------------------

// TestAuthMW_TenantFromContext_Present — header present and non-empty
// on an authorized /v1 request → (tid, true).
func TestAuthMW_TenantFromContext_Present(t *testing.T) {
	v, _, viewerTok, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	cap := &tenantCapture{}
	h := captureMW(v, tenantCaptureHandler(cap), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	req.Header.Set(tenantHeaderName, "acme")
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cap.mu.Lock()
	gotID, gotOK, reached := cap.tenantID, cap.present, cap.reached
	cap.mu.Unlock()
	if !reached {
		t.Fatal("inner handler was not reached on the allow path")
	}
	if !gotOK {
		t.Errorf("TenantFromContext ok = false, want true (header was non-empty)")
	}
	if gotID != "acme" {
		t.Errorf("TenantFromContext id = %q, want %q", gotID, "acme")
	}
}

// TestAuthMW_TenantFromContext_Absent — no X-CIOS-Tenant header on an
// authorized /v1 request → ("", false).
func TestAuthMW_TenantFromContext_Absent(t *testing.T) {
	v, _, viewerTok, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	cap := &tenantCapture{}
	h := captureMW(v, tenantCaptureHandler(cap), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	// NOTE: X-CIOS-Tenant deliberately NOT set.
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cap.mu.Lock()
	gotID, gotOK, reached := cap.tenantID, cap.present, cap.reached
	cap.mu.Unlock()
	if !reached {
		t.Fatal("inner handler was not reached on the allow path")
	}
	if gotOK {
		t.Errorf("TenantFromContext ok = true, want false (header was absent)")
	}
	if gotID != "" {
		t.Errorf("TenantFromContext id = %q, want \"\"", gotID)
	}
}

// TestAuthMW_TenantFromContext_EmptyTreatedAsAbsent — X-CIOS-Tenant
// present but empty value → ("", false) (empty is treated as absent
// per §4.4 extraction point).
func TestAuthMW_TenantFromContext_EmptyTreatedAsAbsent(t *testing.T) {
	v, _, viewerTok, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	cap := &tenantCapture{}
	h := captureMW(v, tenantCaptureHandler(cap), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	req.Header.Set(tenantHeaderName, "")
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cap.mu.Lock()
	gotID, gotOK, reached := cap.tenantID, cap.present, cap.reached
	cap.mu.Unlock()
	if !reached {
		t.Fatal("inner handler was not reached on the allow path")
	}
	if gotOK {
		t.Errorf("TenantFromContext ok = true, want false (empty header is treated as absent)")
	}
	if gotID != "" {
		t.Errorf("TenantFromContext id = %q, want \"\"", gotID)
	}
}

// TestAuthMW_TenantNotAttachedOn401 — bad (missing) bearer → 401,
// inner handler MUST NOT run, tenant MUST NOT be on ctx (the
// middleware short-circuits before the extraction point).
func TestAuthMW_TenantNotAttachedOn401(t *testing.T) {
	v, _, _, _ := buildVerifierForRoles(t, []string{"**"}, nil, nil)
	sink := &auditCapture{}
	// Inner handler MUST NOT be reached. We use a sentinel handler
	// that t.Errorf's if reached; captureMW wraps the authMW.
	h := captureMW(v, tenantNotReachedHandler(t), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	// Deliberately NO Authorization header.
	req.Header.Set(tenantHeaderName, "acme")
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	// tenantNotReachedHandler will have called t.Errorf if reached.
	// Also assert: TenantFromContext on the request ctx (which is
	// the ONLY ctx that exists — the middleware did not build a new
	// one with the tenant key) yields ("", false). The handler
	// never saw this ctx because it never ran, so we read r.Context()
	// on a fresh recorder context to mirror the contract.
	if tid, ok := TenantFromContext(req.Context()); ok || tid != "" {
		t.Errorf("TenantFromContext on pre-auth ctx = (%q, %v), want (\"\", false) — 401 must not propagate tenant", tid, ok)
	}
}

// TestAuthMW_TenantNotAttachedOn403 — token verifies but is
// out-of-scope for the requested (action, path) → 403, inner handler
// MUST NOT run, tenant MUST NOT be on ctx.
//
// We pin scope for the viewer token to `nope.**` so a /v1/assets read
// (which is list-scope → role-floor only and would pass roleAllows
// regardless) — to exercise the 403 path we hit a non-list endpoint
// like GET /v1/points/foo where roleAllows alone won't admit a
// viewer with `nope.**` scope. Per isListScopeEndpoint the points
// path is NOT list-scope, so the full authorize(role × scope ×
// action) decision runs and fails on scope mismatch.
func TestAuthMW_TenantNotAttachedOn403(t *testing.T) {
	v, _, viewerTok, _ := buildVerifierForRoles(t, []string{"nope.**"}, nil, nil)
	sink := &auditCapture{}
	h := captureMW(v, tenantNotReachedHandler(t), sink)

	req := httptest.NewRequest(http.MethodGet, "/v1/points/sgp01.pod002.cdu000.fan000.rpm", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	req.Header.Set(tenantHeaderName, "acme")
	w := httptest.NewRecorder()
	withRequestID(h).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if tid, ok := TenantFromContext(req.Context()); ok || tid != "" {
		t.Errorf("TenantFromContext on pre-auth ctx = (%q, %v), want (\"\", false) — 403 must not propagate tenant", tid, ok)
	}
}
