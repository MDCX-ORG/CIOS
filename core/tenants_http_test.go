// Package core — tenants_http_test.go: end-to-end tests for the
// POST /v1/tenants/{id}:tier admin write path (PRMT-182 §7).
//
// Coverage matrix (fileStore parity; pg path guarded by DSN env):
//
//   - admin upgrade success  → 200, record updated, one tier_change audit row
//   - non-admin / anonymous  → 403, no record change, no audit row
//   - downgrade              → 409, record unchanged, ONE REFUSED audit row
//   - equal tier no-op       → 200, zero audit rows (idempotent, §Resolved #1)
//   - unknown tenant         → 404 (handler pre-check + mid-call race)
//   - invalid tier value     → 400
//   - non-POST method        → 405
//   - missing :tier suffix   → 404 (no /v1/tenants list/get/create surface)
//   - equal-tier no-op atomicity: equal-tier path returns nil, ZERO new rows
//
// §7 acceptance: `go test ./core/ -run 'TenantTier' -v`. The pgStore
// path is exercised by CIOS_TEST_PG_DSN (see tenant_store_test.go's
// existing pattern); the fileStore parity is what's pinned here.
package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedTenant inserts a tenant record directly into the in-memory
// fileStore so tests can drive the :tier endpoint without going
// through a provisioning path (PRMT-184 ships read-side only — the
// tier write is the only mutator in this PRMT, and the test's
// pre-state is "tenant seeded upstream").
func seedTenant(t *testing.T, srv *Server, id, name, tier string) {
	t.Helper()
	fs, ok := srv.st.(*fileStore)
	if !ok {
		t.Fatalf("seedTenant: expected *fileStore, got %T", srv.st)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants[id] = Tenant{
		ID:            id,
		DisplayName:   name,
		IsolationTier: tier,
		Status:        "active",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	fs.mu.Unlock()
}

// auditsFor returns the tenant_audit rows for one tenant_id, ordered
// as stored. Empty (not nil) for unknown ids.
func auditsFor(t *testing.T, srv *Server, tenantID string) []TenantAudit {
	t.Helper()
	rows, err := srv.st.ListTenantAudits(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ListTenantAudits(%s): %v", tenantID, err)
	}
	return rows
}

// findTierChange filters audit rows to op == "tier_change" so the
// tests can assert exact row counts without being confused by the
// tenant_create rows added in other test files (tenant_store_test).
func findTierChange(rows []TenantAudit) []TenantAudit {
	out := make([]TenantAudit, 0, len(rows))
	for _, r := range rows {
		if r.Op == "tier_change" {
			out = append(out, r)
		}
	}
	return out
}

// --- fileStore parity: upgrade success + audit row --------------------

func TestTenantTierHTTP_AdminUpgrade_AuditRow(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"row"}`, adminTok)
	if r.code != http.StatusOK {
		t.Fatalf("admin upgrade: code=%d body=%s, want 200", r.code, r.body)
	}

	// Record was updated.
	got, ok, err := srv.st.GetTenant(context.Background(), "acme")
	if err != nil || !ok {
		t.Fatalf("GetTenant post-upgrade: ok=%v err=%v", ok, err)
	}
	if got.IsolationTier != "row" {
		t.Errorf("IsolationTier = %q, want row", got.IsolationTier)
	}

	// Exactly one tier_change audit row was appended, with the
	// correct detail and principal.
	rows := findTierChange(auditsFor(t, srv, "acme"))
	if len(rows) != 1 {
		t.Fatalf("tier_change rows = %d, want 1 (rows=%+v)", len(rows), rows)
	}
	if rows[0].Detail != "label→row" {
		t.Errorf("audit detail = %q, want label→row", rows[0].Detail)
	}
	if rows[0].Principal != "svc:admin" {
		t.Errorf("audit principal = %q, want svc:admin", rows[0].Principal)
	}
}

// --- non-admin: 403 with no side effects ------------------------------

func TestTenantTierHTTP_NonAdminForbidden_NoSideEffects(t *testing.T) {
	v, viewerTok, operatorTok, _ := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	// Viewer.
	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"row"}`, viewerTok)
	if r.code != http.StatusForbidden {
		t.Errorf("viewer :tier: code=%d body=%s, want 403", r.code, r.body)
	}
	// Operator.
	r = doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"row"}`, operatorTok)
	if r.code != http.StatusForbidden {
		t.Errorf("operator :tier: code=%d body=%s, want 403", r.code, r.body)
	}
	// Anonymous (no token).
	r = doReq(t, ts, http.MethodPost, "/v1/tenants/acme:tier",
		`{"isolation_tier":"row"}`)
	if r.code != http.StatusUnauthorized {
		t.Errorf("anonymous :tier: code=%d body=%s, want 401", r.code, r.body)
	}

	// Record unchanged.
	got, ok, err := srv.st.GetTenant(context.Background(), "acme")
	if err != nil || !ok {
		t.Fatalf("GetTenant: ok=%v err=%v", ok, err)
	}
	if got.IsolationTier != "label" {
		t.Errorf("non-admin left record at %q, want label (unchanged)", got.IsolationTier)
	}
	// Zero tier_change rows.
	rows := findTierChange(auditsFor(t, srv, "acme"))
	if len(rows) != 0 {
		t.Errorf("non-admin produced %d tier_change rows, want 0 (rows=%+v)", len(rows), rows)
	}
}

// --- downgrade: 409 + REFUSED audit row + record unchanged -------------

func TestTenantTierHTTP_Downgrade_409_AndRefusedAudit(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "row")

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"label"}`, adminTok)
	if r.code != http.StatusConflict {
		t.Fatalf("downgrade: code=%d body=%s, want 409", r.code, r.body)
	}

	// Record unchanged.
	got, ok, err := srv.st.GetTenant(context.Background(), "acme")
	if err != nil || !ok {
		t.Fatalf("GetTenant post-downgrade: ok=%v err=%v", ok, err)
	}
	if got.IsolationTier != "row" {
		t.Errorf("downgrade left record at %q, want row (unchanged)", got.IsolationTier)
	}

	// Exactly one REFUSED audit row.
	rows := findTierChange(auditsFor(t, srv, "acme"))
	if len(rows) != 1 {
		t.Fatalf("tier_change rows = %d, want 1 (rows=%+v)", len(rows), rows)
	}
	if rows[0].Detail != "row→label REFUSED" {
		t.Errorf("audit detail = %q, want row→label REFUSED", rows[0].Detail)
	}
}

// --- equal tier: 200 + zero audit rows (idempotent no-op) --------------

func TestTenantTierHTTP_EqualTier_IdempotentZeroAudit(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "row")

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"row"}`, adminTok)
	if r.code != http.StatusOK {
		t.Fatalf("equal-tier: code=%d body=%s, want 200", r.code, r.body)
	}

	// Zero tier_change rows (PRMT-182 §Resolved #1: idempotent no-op
	// emits no audit row).
	rows := findTierChange(auditsFor(t, srv, "acme"))
	if len(rows) != 0 {
		t.Errorf("equal-tier produced %d tier_change rows, want 0 (rows=%+v)", len(rows), rows)
	}
}

// --- unknown tenant: 404 ----------------------------------------------

func TestTenantTierHTTP_UnknownTenant_404(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/ghost:tier",
		`{"isolation_tier":"row"}`, adminTok)
	if r.code != http.StatusNotFound {
		t.Errorf("unknown tenant: code=%d body=%s, want 404", r.code, r.body)
	}
}

// --- invalid tier value: 400 ------------------------------------------

func TestTenantTierHTTP_InvalidTier_400(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{"isolation_tier":"not-a-tier"}`, adminTok)
	if r.code != http.StatusBadRequest {
		t.Errorf("invalid tier: code=%d body=%s, want 400", r.code, r.body)
	}
	// Record unchanged.
	got, _, _ := srv.st.GetTenant(context.Background(), "acme")
	if got.IsolationTier != "label" {
		t.Errorf("invalid tier left record at %q, want label", got.IsolationTier)
	}
	// No audit row.
	rows := findTierChange(auditsFor(t, srv, "acme"))
	if len(rows) != 0 {
		t.Errorf("invalid tier produced %d tier_change rows, want 0", len(rows))
	}
}

// --- non-POST method: 405 ---------------------------------------------

func TestTenantTierHTTP_NonPOST_405(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := doReqWithAuth(t, ts, m, "/v1/tenants/acme:tier", "", adminTok)
		if r.code != http.StatusMethodNotAllowed {
			t.Errorf("%s :tier: code=%d body=%s, want 405", m, r.code, r.body)
		}
	}
}

// --- missing :tier suffix: 404 (no list/get/create surface) -----------

func TestTenantTierHTTP_MissingTierSuffix_404(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	// /v1/tenants/{id} (no subresource) — must 404, not 200.
	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme",
		`{"isolation_tier":"row"}`, adminTok)
	if r.code != http.StatusNotFound {
		t.Errorf("missing :tier: code=%d body=%s, want 404", r.code, r.body)
	}
}

// --- empty body / missing field: 400 ---------------------------------

func TestTenantTierHTTP_EmptyBody_400(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedTenant(t, srv, "acme", "ACME Inc", "label")

	r := doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		``, adminTok)
	if r.code != http.StatusBadRequest {
		t.Errorf("empty body: code=%d body=%s, want 400", r.code, r.body)
	}
	r = doReqWithAuth(t, ts, http.MethodPost,
		"/v1/tenants/acme:tier",
		`{}`, adminTok)
	if r.code != http.StatusBadRequest {
		t.Errorf("missing field: code=%d body=%s, want 400", r.code, r.body)
	}
}

// --- fileStore parity for the mutator directly (no HTTP) --------------

// TestTenantTierStore_UpgradeRecordsAudit: drives the fileStore
// mutator directly so the spec contract ("guard+update+audit
// atomic") is pinned at the Store layer independently of the HTTP
// handler. Mirrors the spares parity test shape.
func TestTenantTierStore_UpgradeRecordsAudit(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs, ok := st.(*fileStore)
	if !ok {
		t.Fatalf("expected *fileStore, got %T", st)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{
		ID: "acme", DisplayName: "ACME Inc", IsolationTier: "label",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	fs.mu.Unlock()

	ctx := context.Background()
	if err := st.UpdateTenantTier(ctx, "acme", "db", "u1"); err != nil {
		t.Fatalf("Upgrade label→db: %v", err)
	}
	got, ok, _ := st.GetTenant(ctx, "acme")
	if !ok || got.IsolationTier != "db" {
		t.Fatalf("post-upgrade: ok=%v tier=%q, want true db", ok, got.IsolationTier)
	}
	rows, _ := st.ListTenantAudits(ctx, "acme")
	tc := findTierChange(rows)
	if len(tc) != 1 || tc[0].Detail != "label→db" || tc[0].Principal != "u1" {
		t.Errorf("audit after upgrade: %+v, want 1 row label→db principal=u1", tc)
	}
}

// TestTenantTierStore_DowngradeWritesRefused: direct mutator drive
// to pin that a downgrade writes exactly one REFUSED row and
// leaves the record unchanged.
func TestTenantTierStore_DowngradeWritesRefused(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs, ok := st.(*fileStore)
	if !ok {
		t.Fatalf("expected *fileStore, got %T", st)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{
		ID: "acme", DisplayName: "ACME Inc", IsolationTier: "db",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	fs.mu.Unlock()

	ctx := context.Background()
	err = st.UpdateTenantTier(ctx, "acme", "label", "u2")
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade err = %v, want ErrTierDowngrade-shaped", err)
	}
	got, ok, _ := st.GetTenant(ctx, "acme")
	if !ok || got.IsolationTier != "db" {
		t.Errorf("post-downgrade: ok=%v tier=%q, want true db (unchanged)", ok, got.IsolationTier)
	}
	rows, _ := st.ListTenantAudits(ctx, "acme")
	tc := findTierChange(rows)
	if len(tc) != 1 || tc[0].Detail != "db→label REFUSED" {
		t.Errorf("REFUSED audit: %+v, want 1 row db→label REFUSED", tc)
	}
}

// TestTenantTierStore_EqualNoOp: idempotent no-op, zero audit rows.
func TestTenantTierStore_EqualNoOp(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs, ok := st.(*fileStore)
	if !ok {
		t.Fatalf("expected *fileStore, got %T", st)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{
		ID: "acme", DisplayName: "ACME Inc", IsolationTier: "row",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	fs.mu.Unlock()

	ctx := context.Background()
	if err := st.UpdateTenantTier(ctx, "acme", "row", "u3"); err != nil {
		t.Fatalf("equal no-op: %v", err)
	}
	rows, _ := st.ListTenantAudits(ctx, "acme")
	if len(findTierChange(rows)) != 0 {
		t.Errorf("equal no-op emitted tier_change rows: %+v", rows)
	}
}
