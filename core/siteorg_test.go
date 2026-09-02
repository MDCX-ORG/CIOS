// Package core — siteorg_test.go: parity + idempotency + audit tests
// for the site→Org mapping substrate (PRMT-189 / spec-001 v1.1
// §5bis.2 "site 挂 Org，可改挂，改挂记审计").
//
// Test layout mirrors tenant_store_test.go: a fileStore round-trip
// that exercises every contract bullet from PRMT §5, plus a
// Postgres parity test gated on CIOS_PG_DSN that confirms the
// single-tx upsert+audit writes happen on a real SQL backend. The
// fileStore tests cover the full bullet list (1)–(8) below without
// needing a DSN; the PG test confirms the same shapes on the real
// `site_orgs` table + `org_reattach` CHECK token.
package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedSiteOrgStore builds a fileStore pre-populated with the
// tenant + org records the SiteOrg tests need. It mirrors the
// fixture pattern used by TestTenantStore_FileRoundTrip: bypass the
// store API, write the seed records directly into the in-memory
// maps under the write lock, then call the public methods.
func seedSiteOrgStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs, ok := st.(*fileStore)
	if !ok {
		t.Fatalf("expected *fileStore, got %T", st)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{ID: "acme", DisplayName: "ACME Inc", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.orgs["og_acme_eng"] = Org{ID: "og_acme_eng", TenantID: "acme", Name: "engineering", CreatedAt: now}
	fs.orgs["og_acme_ops"] = Org{ID: "og_acme_ops", TenantID: "acme", Name: "ops", CreatedAt: now}
	fs.mu.Unlock()
	return st
}

// --- (1) fileStore parity / round-trip ----------------------------------

func TestSiteOrg_FileRoundTrip(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	// AttachSiteToOrg — first attach writes one org_reattach audit row.
	if err := st.AttachSiteToOrg(ctx, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("AttachSiteToOrg fra01→og_acme_eng: %v", err)
	}

	// GetSiteOrg present.
	so, found, err := st.GetSiteOrg(ctx, "fra01")
	if err != nil {
		t.Fatalf("GetSiteOrg fra01: %v", err)
	}
	if !found {
		t.Fatalf("GetSiteOrg fra01: found = false, want true")
	}
	if so.Site != "fra01" || so.OrgID != "og_acme_eng" {
		t.Errorf("GetSiteOrg fra01: got %+v, want {Site:fra01, OrgID:og_acme_eng}", so)
	}
	if so.CreatedAt.IsZero() || so.UpdatedAt.IsZero() {
		t.Errorf("GetSiteOrg fra01: timestamps not set: %+v", so)
	}

	// GetSiteOrg absent → (SiteOrg{}, false, nil).
	if _, found, err := st.GetSiteOrg(ctx, "missing"); err != nil {
		t.Errorf("GetSiteOrg missing: err = %v, want nil", err)
	} else if found {
		t.Errorf("GetSiteOrg missing: found = true, want false")
	}

	// Attach a second site so ListSiteOrgs is non-trivial.
	if err := st.AttachSiteToOrg(ctx, "sgp02", "og_acme_ops", "svc:mig"); err != nil {
		t.Fatalf("AttachSiteToOrg sgp02→og_acme_ops: %v", err)
	}

	// ListSiteOrgs: site ASC, never nil.
	all, err := st.ListSiteOrgs(ctx)
	if err != nil {
		t.Fatalf("ListSiteOrgs: %v", err)
	}
	if all == nil {
		t.Fatalf("ListSiteOrgs: returned nil, want non-nil")
	}
	if len(all) != 2 || all[0].Site != "fra01" || all[1].Site != "sgp02" {
		t.Errorf("ListSiteOrgs order: got [%s, %s], want [fra01, sgp02]", siteAt(all, 0), siteAt(all, 1))
	}

	// CountSitesByOrg: each org owns exactly one site so far.
	if n, err := st.CountSitesByOrg(ctx, "og_acme_eng"); err != nil || n != 1 {
		t.Errorf("CountSitesByOrg og_acme_eng: n=%d err=%v, want 1", n, err)
	}
	if n, err := st.CountSitesByOrg(ctx, "og_acme_ops"); err != nil || n != 1 {
		t.Errorf("CountSitesByOrg og_acme_ops: n=%d err=%v, want 1", n, err)
	}
	if n, err := st.CountSitesByOrg(ctx, "missing"); err != nil || n != 0 {
		t.Errorf("CountSitesByOrg missing: n=%d err=%v, want 0", n, err)
	}
}

// --- (2) AttachSiteToOrg idempotency -----------------------------------

func TestSiteOrg_AttachIdempotent(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	if err := st.AttachSiteToOrg(ctx, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	// Capture the audit count after the first attach; it must NOT
	// grow on the idempotent re-attach.
	audsBefore, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits before: %v", err)
	}

	// Same (site, org) twice → no-op, no audit.
	if err := st.AttachSiteToOrg(ctx, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("idempotent re-attach: %v", err)
	}

	audsAfter, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits after: %v", err)
	}
	if len(audsAfter) != len(audsBefore) {
		t.Errorf("idempotent re-attach wrote audit row: before=%d after=%d", len(audsBefore), len(audsAfter))
	}

	// Row content unchanged.
	so, found, err := st.GetSiteOrg(ctx, "fra01")
	if err != nil || !found {
		t.Fatalf("GetSiteOrg fra01: found=%v err=%v", found, err)
	}
	if so.OrgID != "og_acme_eng" {
		t.Errorf("GetSiteOrg fra01: OrgID=%q, want og_acme_eng", so.OrgID)
	}
}

// --- (3) Re-home writes a single audit row with "<site>: <old>→<new>" ---

func TestSiteOrg_AttachReHome_AuditDetail(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	if err := st.AttachSiteToOrg(ctx, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	audsBefore, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits before: %v", err)
	}
	beforeCount := len(audsBefore)

	// Re-home to a different org.
	if err := st.AttachSiteToOrg(ctx, "fra01", "og_acme_ops", "svc:ops"); err != nil {
		t.Fatalf("re-home: %v", err)
	}

	audsAfter, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits after: %v", err)
	}
	if len(audsAfter) != beforeCount+1 {
		t.Fatalf("re-home: audit rows before=%d after=%d, want before+1", beforeCount, len(audsAfter))
	}

	// The newest audit row is the re-home.
	newest := audsAfter[0] // TS DESC ordering
	if newest.Op != "org_reattach" {
		t.Errorf("re-home audit op=%q, want org_reattach", newest.Op)
	}
	if newest.Principal != "svc:ops" {
		t.Errorf("re-home audit principal=%q, want svc:ops", newest.Principal)
	}
	if newest.Detail != "fra01: og_acme_eng→og_acme_ops" {
		t.Errorf("re-home audit detail=%q, want %q", newest.Detail, "fra01: og_acme_eng→og_acme_ops")
	}

	// Row updated.
	so, found, err := st.GetSiteOrg(ctx, "fra01")
	if err != nil || !found {
		t.Fatalf("GetSiteOrg fra01: found=%v err=%v", found, err)
	}
	if so.OrgID != "og_acme_ops" {
		t.Errorf("re-home row: OrgID=%q, want og_acme_ops", so.OrgID)
	}
}

// --- (4) Bad slug → siteSlugError, no row, no audit --------------------

func TestSiteOrg_AttachBadSlug(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	audsBefore, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits before: %v", err)
	}

	for _, bad := range []string{"", "ab", "fra", "FRA01", "fra1", "fra011", "fra0000", "fra_01", "fra-01"} {
		err := st.AttachSiteToOrg(ctx, bad, "og_acme_eng", "svc:mig")
		if !errors.Is(err, siteSlugError) {
			t.Errorf("AttachSiteToOrg(%q): err=%v, want siteSlugError", bad, err)
		}
	}

	// No row written for any of the bad slugs.
	for _, bad := range []string{"", "FRA01", "fra0000"} {
		if _, found, err := st.GetSiteOrg(ctx, bad); err != nil {
			t.Errorf("GetSiteOrg(%q) after bad-slug attempts: err=%v", bad, err)
		} else if found {
			t.Errorf("GetSiteOrg(%q) after bad-slug attempts: found=true, want false", bad)
		}
	}

	// No audit row written for any of the bad slugs.
	audsAfter, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits after: %v", err)
	}
	if len(audsAfter) != len(audsBefore) {
		t.Errorf("bad-slug attempts wrote audit row: before=%d after=%d", len(audsBefore), len(audsAfter))
	}
}

// --- (5) Unknown org → not-found, no row, no audit ----------------------

func TestSiteOrg_AttachUnknownOrg(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	audsBefore, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits before: %v", err)
	}

	err = st.AttachSiteToOrg(ctx, "fra01", "og_does_not_exist", "svc:mig")
	if err == nil {
		t.Fatalf("AttachSiteToOrg unknown-org: err=nil, want error")
	}
	if !errors.Is(err, siteOrgNotFoundError) {
		t.Errorf("AttachSiteToOrg unknown-org: err=%v, want siteOrgNotFoundError", err)
	}

	// No row written.
	if _, found, err := st.GetSiteOrg(ctx, "fra01"); err != nil {
		t.Errorf("GetSiteOrg fra01 after unknown-org: err=%v", err)
	} else if found {
		t.Errorf("GetSiteOrg fra01 after unknown-org: found=true, want false")
	}

	// No audit row written.
	audsAfter, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits after: %v", err)
	}
	if len(audsAfter) != len(audsBefore) {
		t.Errorf("unknown-org attempt wrote audit row: before=%d after=%d", len(audsBefore), len(audsAfter))
	}
}

// --- (6) GetSiteOrg absent → ("", false, nil) — already covered by (1),
//         repeat with a focused test for the contract bullet alone.

func TestSiteOrg_GetSiteOrgAbsent(t *testing.T) {
	st := seedSiteOrgStore(t)
	so, found, err := st.GetSiteOrg(context.Background(), "never_attached")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found {
		t.Errorf("found = true, want false")
	}
	if so.Site != "" || so.OrgID != "" {
		t.Errorf("absent SiteOrg = %+v, want zero value", so)
	}
}

// --- (7) ListSiteOrgs empty store → non-nil empty, ordered -------------

func TestSiteOrg_ListSiteOrgsEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	all, err := st.ListSiteOrgs(context.Background())
	if err != nil {
		t.Fatalf("ListSiteOrgs: %v", err)
	}
	if all == nil {
		t.Fatalf("ListSiteOrgs empty: returned nil, want non-nil empty slice")
	}
	if len(all) != 0 {
		t.Errorf("ListSiteOrgs empty: len=%d, want 0", len(all))
	}
}

func TestSiteOrg_ListSiteOrgsOrder(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	// Insert in non-alphabetical order to prove sort is by site, not insertion.
	for _, p := range []struct{ site, org string }{
		{"sgp02", "og_acme_ops"},
		{"fra01", "og_acme_eng"},
		{"lax03", "og_acme_eng"},
	} {
		if err := st.AttachSiteToOrg(ctx, p.site, p.org, "svc:mig"); err != nil {
			t.Fatalf("AttachSiteToOrg %s→%s: %v", p.site, p.org, err)
		}
	}

	all, err := st.ListSiteOrgs(ctx)
	if err != nil {
		t.Fatalf("ListSiteOrgs: %v", err)
	}
	want := []string{"fra01", "lax03", "sgp02"}
	if len(all) != len(want) {
		t.Fatalf("ListSiteOrgs: len=%d, want %d", len(all), len(want))
	}
	for i, w := range want {
		if all[i].Site != w {
			t.Errorf("ListSiteOrgs[%d].Site=%q, want %q", i, all[i].Site, w)
		}
	}
}

// --- (8) CountSitesByOrg → exact count ---------------------------------

func TestSiteOrg_CountSitesByOrg(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()

	// og_acme_eng starts at 0; attach three sites.
	for _, site := range []string{"fra01", "lax03", "nrt05"} {
		if err := st.AttachSiteToOrg(ctx, site, "og_acme_eng", "svc:mig"); err != nil {
			t.Fatalf("AttachSiteToOrg %s→og_acme_eng: %v", site, err)
		}
	}
	// og_acme_ops gets one.
	if err := st.AttachSiteToOrg(ctx, "sgp02", "og_acme_ops", "svc:mig"); err != nil {
		t.Fatalf("AttachSiteToOrg sgp02→og_acme_ops: %v", err)
	}

	if n, err := st.CountSitesByOrg(ctx, "og_acme_eng"); err != nil || n != 3 {
		t.Errorf("CountSitesByOrg og_acme_eng: n=%d err=%v, want 3", n, err)
	}
	if n, err := st.CountSitesByOrg(ctx, "og_acme_ops"); err != nil || n != 1 {
		t.Errorf("CountSitesByOrg og_acme_ops: n=%d err=%v, want 1", n, err)
	}
	if n, err := st.CountSitesByOrg(ctx, "missing"); err != nil || n != 0 {
		t.Errorf("CountSitesByOrg missing: n=%d err=%v, want 0", n, err)
	}
}

// --- validSiteSlug grammar check (mirrors TestValidTenantSlug) ----------

func TestValidSiteSlug(t *testing.T) {
	// Grammar: [a-z]{2,8}[0-9]{2}, "00" tail rejected.
	for _, s := range []string{"ab01", "fra01", "sgp02", "abcdefgh01"} {
		if !validSiteSlug(s) {
			t.Errorf("validSiteSlug(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "a", "ab", "a01", "ab1", "ab001", "FRA01", "fra1", "fra011", "fra_01", "fra-01", "fra00", "abcdefghi01", "ab 01"} {
		if validSiteSlug(s) {
			t.Errorf("validSiteSlug(%q) = true, want false", s)
		}
	}
}

// --- PG parity (skipped without CIOS_PG_DSN; mirrors tenant_store_test) --

func TestSiteOrg_PGParity(t *testing.T) {
	ctx, conn := withPGTenantEnv(t)

	// Apply 016 so site_orgs exists with its FK to orgs.
	raw, err := os.ReadFile(filepath.Join(migrationsDir(t), "016_site_org.sql"))
	if err != nil {
		t.Fatalf("read 016_site_org.sql: %v", err)
	}
	if _, err := conn.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("apply 016: %v", err)
	}

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, display_name, isolation_tier, status, created_at, updated_at) VALUES ('acme','ACME','label','active',$1,$1)`, now); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	for _, og := range []Org{
		{ID: "og_acme_eng", TenantID: "acme", Name: "engineering", CreatedAt: now},
		{ID: "og_acme_ops", TenantID: "acme", Name: "ops", CreatedAt: now},
	} {
		if _, err := conn.Exec(ctx, `INSERT INTO orgs (id, tenant_id, name, created_at) VALUES ($1,$2,$3,$4)`, og.ID, og.TenantID, og.Name, og.CreatedAt); err != nil {
			t.Fatalf("insert org %s: %v", og.ID, err)
		}
	}

	// First attach via the package-private helper on the pinned conn.
	if err := attachSiteToOrg(ctx, conn, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("attachSiteToOrg first: %v", err)
	}

	// GetSiteOrg present + absent.
	so, found, err := getSiteOrg(ctx, conn, "fra01")
	if err != nil || !found || so.OrgID != "og_acme_eng" || so.Site != "fra01" {
		t.Errorf("getSiteOrg fra01: found=%v err=%v so=%+v", found, err, so)
	}
	if _, found, err := getSiteOrg(ctx, conn, "missing"); err != nil {
		t.Errorf("getSiteOrg missing: err=%v", err)
	} else if found {
		t.Errorf("getSiteOrg missing: found=true")
	}

	// Attach a second site so ListSiteOrgs has shape.
	if err := attachSiteToOrg(ctx, conn, "sgp02", "og_acme_ops", "svc:mig"); err != nil {
		t.Fatalf("attachSiteToOrg second: %v", err)
	}
	all, err := listSiteOrgs(ctx, conn)
	if err != nil {
		t.Fatalf("listSiteOrgs: %v", err)
	}
	if all == nil || len(all) != 2 || all[0].Site != "fra01" || all[1].Site != "sgp02" {
		t.Errorf("listSiteOrgs order: got [%s, %s], want [fra01, sgp02]", siteAt(all, 0), siteAt(all, 1))
	}

	// CountSitesByOrg.
	if n, err := countSitesByOrg(ctx, conn, "og_acme_eng"); err != nil || n != 1 {
		t.Errorf("countSitesByOrg og_acme_eng: n=%d err=%v, want 1", n, err)
	}
	if n, err := countSitesByOrg(ctx, conn, "missing"); err != nil || n != 0 {
		t.Errorf("countSitesByOrg missing: n=%d err=%v, want 0", n, err)
	}

	// Idempotent re-attach via attachSiteToOrg: no second audit row.
	audsBefore, err := listTenantAudits(ctx, conn, "acme")
	if err != nil {
		t.Fatalf("listTenantAudits before: %v", err)
	}
	if err := attachSiteToOrg(ctx, conn, "fra01", "og_acme_eng", "svc:mig"); err != nil {
		t.Fatalf("attachSiteToOrg idempotent: %v", err)
	}
	audsAfter, err := listTenantAudits(ctx, conn, "acme")
	if err != nil {
		t.Fatalf("listTenantAudits after: %v", err)
	}
	if len(audsAfter) != len(audsBefore) {
		t.Errorf("idempotent re-attach wrote audit row: before=%d after=%d", len(audsBefore), len(audsAfter))
	}

	// Re-home writes ONE audit row with detail "fra01: og_acme_eng→og_acme_ops".
	if err := attachSiteToOrg(ctx, conn, "fra01", "og_acme_ops", "svc:ops"); err != nil {
		t.Fatalf("attachSiteToOrg rehome: %v", err)
	}
	audsRe, err := listTenantAudits(ctx, conn, "acme")
	if err != nil {
		t.Fatalf("listTenantAudits rehome: %v", err)
	}
	if len(audsRe) != len(audsBefore)+1 {
		t.Errorf("rehome audit count: before=%d after=%d, want before+1", len(audsBefore), len(audsRe))
	}
	newest := audsRe[0]
	if newest.Op != "org_reattach" {
		t.Errorf("rehome audit op=%q, want org_reattach", newest.Op)
	}
	if newest.Detail != "fra01: og_acme_eng→og_acme_ops" {
		t.Errorf("rehome audit detail=%q, want %q", newest.Detail, "fra01: og_acme_eng→og_acme_ops")
	}

	// Unknown org id → not-found (mirrors fileStore siteOrgNotFoundError).
	if err := attachSiteToOrg(ctx, conn, "lax03", "og_does_not_exist", "svc:mig"); err == nil {
		t.Errorf("attachSiteToOrg unknown-org: err=nil, want error")
	}

	// CHECK vocabulary: a bogus op token MUST be rejected (parity for
	// 015's CHECK list which the 189 audit row piggybacks on).
	if _, err := conn.Exec(ctx, `INSERT INTO tenant_audit (id, ts, principal, tenant_id, op, detail) VALUES ('ta_bogus',$1,'u','acme','site_org_set','')`, now); err == nil {
		t.Errorf("CHECK accepted tenant_audit.op='site_org_set'; should reject (189 must reuse org_reattach, not invent)")
	}
}

// --- local helpers -----------------------------------------------------

func siteAt(s []SiteOrg, i int) string {
	if i < 0 || i >= len(s) {
		return "?"
	}
	return s[i].Site
}
