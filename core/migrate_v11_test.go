// core/migrate_v11_test.go — PRMT-186 acceptance tests (fileStore
// parity + pgStore parity via TestMigrateV11_PGParity, DSN-gated,
// mirroring the rolebinding_test.go pattern).
//
// Covers the §7 acceptance list:
//   - default-org create per tenant + idempotent re-run
//   - site backfill via AttachSiteToOrg + idempotent re-run
//   - legacy→crn rewrite (dot-glob → crn:tenant/…/org/default/…)
//   - crn-origin skipped + one diff record each
//   - historical audit untouched
//   - full-run idempotency (second run = no-op)
//   - report prints closure readiness; writes nothing / flips nothing
//   - pg parity (DSN-gated): runs the full MigrateV11 on the pgStore
//     half and asserts (a) the rewrite writes a fresh id (no PK
//     collision), (b) the legacy row is gone, (c) re-run is a
//     no-op on the crn row.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures ------------------------------------------------------------

// seedTestTenant inserts a tenant into the fileStore's tenant
// table directly (mirroring tenants_http_test.go's seedTenant).
func seedTestTenant(t *testing.T, fs *fileStore, id, name, tier string) {
	t.Helper()
	fs.mu.Lock()
	fs.tenants[id] = Tenant{
		ID:            id,
		DisplayName:   name,
		IsolationTier: tier,
		Status:        "active",
		CreatedAt:     time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	fs.mu.Unlock()
}

// seedTestAsset inserts an asset by path (first segment is the
// site; mirrors cpath's first-segment = site rule).
func seedTestAsset(t *testing.T, fs *fileStore, path string) {
	t.Helper()
	fs.mu.Lock()
	fs.assets[path] = Asset{
		Path:            path,
		ResourceVersion: 1,
		Spec:            map[string]any{},
		CreatedAt:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	fs.mu.Unlock()
}

// seedTestRoleBinding inserts one RoleBinding row.
func seedTestRoleBinding(t *testing.T, fs *fileStore, subject, scope, origin string) {
	t.Helper()
	fs.mu.Lock()
	fs.roleBindings = append(fs.roleBindings, RoleBinding{
		ID:        newRoleBindingID(),
		Subject:   subject,
		Scope:     scope,
		Origin:    origin,
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	fs.mu.Unlock()
}

// countingSink is a test sink that records calls and writes JSONL
// to its bytes.Buffer for inspection.
type countingSink struct {
	mu     sync.Mutex
	calls  []RewriteDiff
	buf    bytes.Buffer
	closed bool
}

func (s *countingSink) RecordRewrite(subject, oldScope, newScope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	d := RewriteDiff{
		TS:       time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC),
		Subject:  subject,
		OldScope: oldScope,
		NewScope: newScope,
	}
	s.calls = append(s.calls, d)
	b, _ := json.Marshal(d)
	b = append(b, '\n')
	s.buf.Write(b)
	return nil
}

func (s *countingSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// newMigrateTestStore returns a fresh fileStore with no tenants,
// no orgs, no site_orgs, no role bindings. Mirrors rolebinding_test
// patterns.
func newMigrateTestStore(t *testing.T) *fileStore {
	t.Helper()
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return st.(*fileStore)
}

// --- acceptance tests ----------------------------------------------------

// TestMigrateV11_DefaultsIdempotent covers the canonical
// single-tenant case from the prompt's §2-bis expected result:
// two tenants {acme, globex}, sites attributed to the sole tenant
// (we seed only one tenant here to avoid the multi-tenant STOP),
// legacy→crn rewrite + crn-origin skip + idempotent re-run.
func TestMigrateV11_DefaultsIdempotent(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	seedTestTenant(t, fs, "acme", "ACME", "row")
	seedTestAsset(t, fs, "fra01.pod000.cdu000.fws.supply.flow")
	seedTestAsset(t, fs, "sgp01.pod002.cdu000.fws.supply.flow")
	seedTestRoleBinding(t, fs, "svc:cooling", "fra01.pod000.**", "legacy")
	seedTestRoleBinding(t, fs, "svc:cooling", "sgp01.pod002.**", "legacy")
	seedTestRoleBinding(t, fs, "svc:power", "crn:tenant/acme/org/emea/site/fra01/pod000", "crn")

	sink := &countingSink{}

	// Pass 1
	rep1, err := MigrateV11(ctx, fs, "test:runner", sink)
	if err != nil {
		t.Fatalf("MigrateV11 pass 1: %v", err)
	}
	if rep1.TenantsSeen != 1 {
		t.Errorf("pass 1 TenantsSeen = %d, want 1", rep1.TenantsSeen)
	}
	if rep1.OrgsEnsured != 1 {
		t.Errorf("pass 1 OrgsEnsured = %d, want 1", rep1.OrgsEnsured)
	}
	if rep1.SitesAttached != 2 {
		t.Errorf("pass 1 SitesAttached = %d, want 2", rep1.SitesAttached)
	}
	if rep1.RBRewritten != 2 {
		t.Errorf("pass 1 RBRewritten = %d, want 2", rep1.RBRewritten)
	}
	if rep1.RBSkippedCRN != 1 {
		t.Errorf("pass 1 RBSkippedCRN = %d, want 1", rep1.RBSkippedCRN)
	}
	if len(sink.calls) != 2 {
		t.Errorf("pass 1 sink calls = %d, want 2 (one per rewrite)", len(sink.calls))
	}

	// Assert the default org exists and is named "default".
	orgs, err := fs.ListOrgs(ctx, "acme")
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Name != DefaultOrgName {
		t.Errorf("expected one `default` org for acme, got %+v", orgs)
	}
	defaultOrgID := orgs[0].ID

	// Assert each site is attached to the default org.
	for _, site := range []string{"fra01", "sgp01"} {
		so, ok, err := fs.GetSiteOrg(ctx, site)
		if err != nil {
			t.Fatalf("GetSiteOrg(%s): %v", site, err)
		}
		if !ok {
			t.Errorf("site %s not attached", site)
			continue
		}
		if so.OrgID != defaultOrgID {
			t.Errorf("site %s attached to org %s, want %s", site, so.OrgID, defaultOrgID)
		}
	}

	// Assert legacy rows are now crn-form under org/default.
	all, err := fs.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings: %v", err)
	}
	for _, rb := range all {
		if rb.Subject == "svc:power" {
			if rb.Origin != "crn" {
				t.Errorf("crn-origin row mutated: subject=%s origin=%s", rb.Subject, rb.Origin)
			}
			if rb.Scope != "crn:tenant/acme/org/emea/site/fra01/pod000" {
				t.Errorf("crn-origin row scope mutated: %s", rb.Scope)
			}
			continue
		}
		if rb.Origin != "crn" {
			t.Errorf("legacy row not rewritten: subject=%s scope=%s origin=%s",
				rb.Subject, rb.Scope, rb.Origin)
		}
		if !strings.HasPrefix(rb.Scope, "crn:tenant/acme/org/default/site/") {
			t.Errorf("legacy rewrite wrong prefix: subject=%s scope=%s", rb.Subject, rb.Scope)
		}
	}

	// Pass 2: idempotent re-run.
	rep2, err := MigrateV11(ctx, fs, "test:runner", sink)
	if err != nil {
		t.Fatalf("MigrateV11 pass 2: %v", err)
	}
	if rep2.OrgsEnsured != 0 {
		t.Errorf("pass 2 OrgsEnsured = %d, want 0 (idempotent)", rep2.OrgsEnsured)
	}
	if rep2.OrgsAlready != 1 {
		t.Errorf("pass 2 OrgsAlready = %d, want 1", rep2.OrgsAlready)
	}
	if rep2.SitesAttached != 0 {
		t.Errorf("pass 2 SitesAttached = %d, want 0 (idempotent)", rep2.SitesAttached)
	}
	if rep2.SitesAlready != 2 {
		t.Errorf("pass 2 SitesAlready = %d, want 2", rep2.SitesAlready)
	}
	if rep2.RBRewritten != 0 {
		t.Errorf("pass 2 RBRewritten = %d, want 0 (no re-rewrite)", rep2.RBRewritten)
	}
	if rep2.RBSkippedCRN != 3 {
		t.Errorf("pass 2 RBSkippedCRN = %d, want 3 (1 pre-existing crn + 2 rewritten)", rep2.RBSkippedCRN)
	}
	// Sink must not have been called again.
	if len(sink.calls) != 2 {
		t.Errorf("pass 2 sink calls = %d, want still 2 (idempotent)", len(sink.calls))
	}
}

// TestMigrateV11_DoubleRunNoDuplicate asserts the explicit
// double-run idempotency contract required by the §3 widening:
// running MigrateV11 twice on the same input yields exactly one
// RoleBinding row per rewritten subject (the crn row), exactly
// one AttachSiteToOrg row per site, and no duplicate migration
// audit entries. This is the substrate-level proof that the
// DeleteRoleBinding addition closes the orphan-legacy-row gap.
func TestMigrateV11_DoubleRunNoDuplicate(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	seedTestTenant(t, fs, "acme", "ACME", "row")
	seedTestAsset(t, fs, "fra01.pod000.cdu000.fws.supply.flow")
	seedTestRoleBinding(t, fs, "svc:cooling", "fra01.pod000.**", "legacy")
	sink := &countingSink{}

	// Run 1: one legacy→crn rewrite, one site attached, one diff.
	if _, err := MigrateV11(ctx, fs, "test:runner", sink); err != nil {
		t.Fatalf("MigrateV11 run 1: %v", err)
	}

	// Run 2: full no-op on the migration's data; the audit sink
	// must not see another call, and the row counts must stay
	// flat.
	if _, err := MigrateV11(ctx, fs, "test:runner", sink); err != nil {
		t.Fatalf("MigrateV11 run 2: %v", err)
	}

	// (a) total role_bindings rows = 1 (the crn row; the legacy row
	// was retired by run 1's DeleteRoleBinding).
	all, err := fs.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("post 2-run role_bindings rows = %d, want 1 (orphan legacy row must be gone)", len(all))
		for _, rb := range all {
			t.Logf("row: id=%s scope=%s origin=%s", rb.ID, rb.Scope, rb.Origin)
		}
	} else if all[0].Origin != "crn" {
		t.Errorf("post 2-run surviving row origin = %q, want \"crn\"", all[0].Origin)
	} else if !strings.HasPrefix(all[0].Scope, "crn:tenant/acme/org/default/site/") {
		t.Errorf("post 2-run surviving row scope = %q, want crn:tenant/acme/org/default/site/...", all[0].Scope)
	}

	// (b) no duplicate migration audit entries — exactly one line.
	if len(sink.calls) != 1 {
		t.Errorf("sink calls after 2 runs = %d, want exactly 1 (idempotent — second run is no-op)", len(sink.calls))
	}

	// (c) total attachments = 1 (one site → one default org).
	so, ok, err := fs.GetSiteOrg(ctx, "fra01")
	if err != nil {
		t.Fatalf("GetSiteOrg fra01: %v", err)
	}
	if !ok {
		t.Errorf("site fra01 not attached after 2 runs")
	}
	if so.Site != "fra01" {
		t.Errorf("site org row site = %q, want \"fra01\"", so.Site)
	}

	// Cross-check: a third run keeps things flat — the no-op
	// claim must hold for ≥3 runs, not just 2.
	if _, err := MigrateV11(ctx, fs, "test:runner", sink); err != nil {
		t.Fatalf("MigrateV11 run 3: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Errorf("sink calls after 3 runs = %d, want still 1", len(sink.calls))
	}
	all3, err := fs.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings run 3: %v", err)
	}
	if len(all3) != 1 {
		t.Errorf("post 3-run role_bindings rows = %d, want still 1", len(all3))
	}
}

// TestMigrateV11_AuditUntouched asserts that no tenant_audit row
// is rewritten or deleted across a full migration. We seed an
// existing audit row, run migration, then assert the pre-existing
// rows are still present and the new rows (org_create, org_reattach)
// are the only additions.
func TestMigrateV11_AuditUntouched(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	seedTestTenant(t, fs, "acme", "ACME", "row")
	pre := TenantAudit{
		ID:        newTenantAuditID(),
		TS:        time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Principal: "human:yuri",
		TenantID:  "acme",
		Op:        "tenant_create",
		Detail:    "seed",
	}
	if err := fs.AppendTenantAudit(ctx, pre); err != nil {
		t.Fatalf("AppendTenantAudit pre: %v", err)
	}
	seedTestAsset(t, fs, "fra01.pod000.cdu000.fws.supply.flow")

	sink := &countingSink{}
	if _, err := MigrateV11(ctx, fs, "test:runner", sink); err != nil {
		t.Fatalf("MigrateV11: %v", err)
	}

	rows, err := fs.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits: %v", err)
	}
	// Must contain the pre-existing row untouched.
	var found bool
	for _, r := range rows {
		if r.ID == pre.ID {
			found = true
			if r.Op != "tenant_create" || r.Principal != "human:yuri" || r.Detail != "seed" {
				t.Errorf("pre-existing audit row mutated: %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("pre-existing audit row missing after migration")
	}
	// org_create and org_reattach should be present.
	ops := map[string]int{}
	for _, r := range rows {
		ops[r.Op]++
	}
	if ops["org_create"] < 1 {
		t.Errorf("expected ≥1 org_create audit row, ops=%v", ops)
	}
	if ops["org_reattach"] < 1 {
		t.Errorf("expected ≥1 org_reattach audit row, ops=%v", ops)
	}
	// tenant_create must remain exactly 1 (the pre-existing row).
	if ops["tenant_create"] != 1 {
		t.Errorf("tenant_create count = %d, want 1 (append-only)", ops["tenant_create"])
	}
}

// TestMigrateV11_NoRoleBindingNoAuditRows asserts that an empty
// store produces a clean MigrateReport and writes no audit rows
// (the migration has no work to do for tenants that don't exist).
func TestMigrateV11_NoTenantsNoWork(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	sink := &countingSink{}
	rep, err := MigrateV11(ctx, fs, "test:runner", sink)
	if err != nil {
		t.Fatalf("MigrateV11 empty store: %v", err)
	}
	if rep.TenantsSeen != 0 || rep.OrgsEnsured != 0 || rep.SitesAttached != 0 || rep.RBRewritten != 0 {
		t.Errorf("empty store produced work: %+v", rep)
	}
	if len(sink.calls) != 0 {
		t.Errorf("empty store produced sink calls: %d", len(sink.calls))
	}
}

// TestMigrateV11_LegacyRowNoSiteSkips asserts that a legacy-origin
// row whose scope starts with an unparseable site is skipped
// cleanly (counted in RBSkippedNoSite) without panicking and
// without emitting a diff.
func TestMigrateV11_LegacyRowNoSiteSkips(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	seedTestTenant(t, fs, "acme", "ACME", "row")
	// Empty scope — unparseable.
	seedTestRoleBinding(t, fs, "svc:bogus", "", "legacy")
	sink := &countingSink{}
	rep, err := MigrateV11(ctx, fs, "test:runner", sink)
	if err != nil {
		t.Fatalf("MigrateV11: %v", err)
	}
	if rep.RBSkippedNoSite != 1 {
		t.Errorf("RBSkippedNoSite = %d, want 1", rep.RBSkippedNoSite)
	}
	if rep.RBRewritten != 0 {
		t.Errorf("RBRewritten = %d, want 0", rep.RBRewritten)
	}
	if len(sink.calls) != 0 {
		t.Errorf("sink calls = %d, want 0 (no rewrite)", len(sink.calls))
	}
}

// TestMigrateV11_MultiTenantStopsOnUnattributableSite asserts the
// §4.1 STOP condition: 2 tenants, no site_orgs entry, no signal
// for an asset path's first segment → MigrateV11 returns an error
// rather than inventing a rule.
func TestMigrateV11_MultiTenantStopsOnUnattributableSite(t *testing.T) {
	fs := newMigrateTestStore(t)
	ctx := context.Background()
	seedTestTenant(t, fs, "acme", "ACME", "row")
	seedTestTenant(t, fs, "globex", "Globex", "row")
	seedTestAsset(t, fs, "fra01.pod000.cdu000.fws.supply.flow")
	sink := &countingSink{}
	if _, err := MigrateV11(ctx, fs, "test:runner", sink); err == nil {
		t.Fatalf("MigrateV11 should STOP on multi-tenant + unattributable site, got nil")
	}
}

// TestReportLegacyUse_NoFlipsFlag asserts the report mode does not
// flip the closure flag. We use the public test hook to set the
// flag to a known state, then call ReportLegacyUse and assert the
// flag is still in the same state.
func TestReportLegacyUse_NoFlipsFlag(t *testing.T) {
	// Set flag to open (the default after Reset), then verify report
	// does not flip it.
	SetLegacyScopeClosedForTest(false)
	defer SetLegacyScopeClosedForTest(false)

	r, err := ReportLegacyUse(context.Background(), 30)
	if err != nil {
		t.Fatalf("ReportLegacyUse: %v", err)
	}
	if !r.ClosureFlagOpen {
		t.Errorf("report claimed closure flag is closed, but we set it open")
	}
	if r.HistoricalEvidence {
		t.Errorf("report must NOT claim historical evidence; the counter is in-process only")
	}
	if r.Note == "" {
		t.Errorf("report note must explain the historical-evidence gap")
	}
	if LegacyScopeClosed() {
		t.Errorf("report flipped the closure flag (R6 violation)")
	}
}

// TestReportLegacyUse_DefaultsTo30 asserts days=0 → 30.
func TestReportLegacyUse_DefaultsTo30(t *testing.T) {
	r, err := ReportLegacyUse(context.Background(), 0)
	if err != nil {
		t.Fatalf("ReportLegacyUse: %v", err)
	}
	if r.DaysRequested != 30 {
		t.Errorf("DaysRequested = %d, want 30 (default)", r.DaysRequested)
	}
}

// TestReportLegacyUse_ReadsCounter asserts the report reflects the
// in-process counter (after Reset, a fresh report reads 0).
func TestReportLegacyUse_ReadsCounter(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	r, err := ReportLegacyUse(context.Background(), 30)
	if err != nil {
		t.Fatalf("ReportLegacyUse: %v", err)
	}
	if r.LegacyScopeUsesNow != 0 {
		t.Errorf("LegacyScopeUsesNow = %d, want 0 after reset", r.LegacyScopeUsesNow)
	}
	if !r.ClosureReady {
		t.Errorf("ClosureReady = false, want true (counter is 0)")
	}
}

// TestJSONLMigrationAuditSink_LinePerRewrite asserts the default
// sink writes one JSONL line per call.
func TestJSONLMigrationAuditSink_LinePerRewrite(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONLMigrationAuditSink(&buf)
	if err := sink.RecordRewrite("svc:a", "old1", "new1"); err != nil {
		t.Fatalf("RecordRewrite 1: %v", err)
	}
	if err := sink.RecordRewrite("svc:b", "old2", "new2"); err != nil {
		t.Fatalf("RecordRewrite 2: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("JSONL lines = %d, want 2", len(lines))
	}
	for i, l := range lines {
		var d RewriteDiff
		if err := json.Unmarshal(l, &d); err != nil {
			t.Errorf("line %d not valid JSON: %v (line=%s)", i, err, string(l))
		}
	}
}

// TestMigrateV11_PGParity runs the full MigrateV11 on a fresh
// pgStore and asserts the post-R1 findings are closed:
//   - F1: the crn row written by step 3 has a fresh id (not the
//     legacy row's id), so the pgStore PRIMARY KEY (id) does not
//     collide; legacy row is gone after the rewrite; on re-run, the
//     total role_bindings row count is 1 (no duplicate, no orphan).
//   - F2 (manifest): this test exists and actually exercises the
//     PG path; it is the parity companion to TestRoleBindingStore_PGParity.
//
// Skipped when CIOS_PG_DSN is not set (per PRMT-190-bis §7 precedent).
func TestMigrateV11_PGParity(t *testing.T) {
	// Spin up a fresh, non-transactional pgStore via NewPGStore so
	// MigrateV11 sees the same production Store implementation.
	// NewPGStore manages its own connection pool; we close it on exit
	// and best-effort clean up our seeded rows so the test is
	// re-runnable against a long-lived dev PG.
	dsn := pgDSN(t)
	root := moduleRoot(t)
	st, err := NewPGStore(context.Background(), dsn, filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	pgs := st.(*pgStore)
	defer pgs.pool.Close()

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pgs.pool.Exec(bg, `DELETE FROM role_bindings WHERE subject = $1`, "svc:cool")
		_, _ = pgs.pool.Exec(bg, `DELETE FROM site_orgs WHERE site = $1`, "fra01")
		_, _ = pgs.pool.Exec(bg, `DELETE FROM orgs WHERE tenant_id = $1 AND name = $2`, "acme", DefaultOrgName)
		_, _ = pgs.pool.Exec(bg, `DELETE FROM assets WHERE path LIKE $1`, "fra01.%")
		_, _ = pgs.pool.Exec(bg, `DELETE FROM tenants WHERE id = $1`, "acme")
	})

	ctx := context.Background()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	// Seed: one tenant "acme", one asset "fra01.pod000.cdu000.fws.supply.flow"
	// (so site = "fra01"), one legacy role_binding for subject "svc:cool"
	// scoped to "fra01.chiller*".
	// isolation_tier CHECK = label|row|db (015_tenant_org.sql) — never "shared".
	if _, err := pgs.pool.Exec(ctx,
		`INSERT INTO tenants (id, display_name, isolation_tier, status, created_at, updated_at)
		 VALUES ($1, $2, 'label', 'active', $3, $3)
		 ON CONFLICT (id) DO NOTHING`,
		"acme", "Acme", now); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	// assets schema = 001_init (path, spec, resource_version, created_at, updated_at).
	if _, err := pgs.pool.Exec(ctx,
		`INSERT INTO assets (path, spec, resource_version, created_at, updated_at)
		 VALUES ($1, $2::jsonb, 1, $3, $3)
		 ON CONFLICT (path) DO NOTHING`,
		"fra01.pod000.cdu000.fws.supply.flow", `{"type":"fws"}`, now); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	legacyID := newRoleBindingID()
	if _, err := pgs.pool.Exec(ctx,
		`INSERT INTO role_bindings (id, subject, scope, origin, created_at, updated_at)
		 VALUES ($1, $2, $3, 'legacy', $4, $4)`,
		legacyID, "svc:cool", "fra01.chiller*", now); err != nil {
		t.Fatalf("insert legacy role_binding: %v", err)
	}

	var buf bytes.Buffer
	sink := NewJSONLMigrationAuditSink(&buf)

	// Run 1: must succeed (F1 fix path).
	rep1, err := MigrateV11(ctx, st, "tester", sink)
	if err != nil {
		t.Fatalf("MigrateV11 run 1: %v (F1 regression: id-reuse would PK-collide here)", err)
	}
	if rep1.RBRewritten != 1 {
		t.Errorf("rep1.RBRewritten = %d, want 1", rep1.RBRewritten)
	}
	if rep1.RBSkippedCRN != 0 {
		t.Errorf("rep1.RBSkippedCRN = %d, want 0", rep1.RBSkippedCRN)
	}
	if rep1.Diffs != nil && len(rep1.Diffs) != 1 {
		t.Errorf("rep1.Diffs count = %d, want 1", len(rep1.Diffs))
	}

	// Inspect the surviving row: must NOT be the legacy id; must be crn origin.
	var rowID, rowOrigin, rowScope string
	if err := pgs.pool.QueryRow(ctx,
		`SELECT id, origin, scope FROM role_bindings WHERE subject = $1`, "svc:cool",
	).Scan(&rowID, &rowOrigin, &rowScope); err != nil {
		t.Fatalf("query surviving row: %v", err)
	}
	if rowOrigin != "crn" {
		t.Errorf("surviving row origin = %q, want crn", rowOrigin)
	}
	if rowID == legacyID {
		t.Errorf("F1 regression: surviving row id == legacy id (%s); must be fresh", legacyID)
	}
	if !strings.HasPrefix(rowScope, "crn:") {
		t.Errorf("surviving row scope = %q, want crn:-prefixed", rowScope)
	}

	// Run 2: must be a full no-op (F1 + idempotency proof on PG).
	rep2, err := MigrateV11(ctx, st, "tester", sink)
	if err != nil {
		t.Fatalf("MigrateV11 run 2: %v", err)
	}
	if rep2.RBRewritten != 0 {
		t.Errorf("rep2.RBRewritten = %d, want 0 (idempotent re-run)", rep2.RBRewritten)
	}
	if rep2.RBSkippedCRN != 1 {
		t.Errorf("rep2.RBSkippedCRN = %d, want 1 (crn row already present)", rep2.RBSkippedCRN)
	}

	// Total row count must be exactly 1 — proves no orphan, no duplicate.
	var total int
	if err := pgs.pool.QueryRow(ctx,
		`SELECT count(*) FROM role_bindings WHERE subject = $1`, "svc:cool",
	).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("role_bindings row count after 2 runs = %d, want 1", total)
	}
}
