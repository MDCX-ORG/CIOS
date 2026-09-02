// Package core — tenant_store_test.go: parity + round-trip tests
// for the tenant / org / tenant_audit substrate (E3.1 / PRMT-184 /
// spec-001 v1.1 §5bis).
package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTenantStore_FileRoundTrip(t *testing.T) {
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
	fs.tenants["beta"] = Tenant{ID: "beta", DisplayName: "Beta Ltd", IsolationTier: "row", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.orgs["og_acme_eng"] = Org{ID: "og_acme_eng", TenantID: "acme", Name: "engineering", CreatedAt: now}
	fs.orgs["og_acme_ops"] = Org{ID: "og_acme_ops", TenantID: "acme", Name: "ops", CreatedAt: now}
	fs.orgs["og_beta_all"] = Org{ID: "og_beta_all", TenantID: "beta", Name: "company", CreatedAt: now}
	fs.tenantAuds = []TenantAudit{
		{ID: "ta_old", TS: now.Add(-2 * time.Hour), Principal: "u1", TenantID: "acme", Op: "tenant_create", Detail: ""},
		{ID: "ta_new", TS: now.Add(-1 * time.Hour), Principal: "u2", TenantID: "acme", Op: "tier_change", Detail: "label->row"},
	}
	fs.mu.Unlock()

	ctx := context.Background()

	// GetTenant present + absent.
	tn, found, err := st.GetTenant(ctx, "acme")
	if err != nil || !found || tn.DisplayName != "ACME Inc" || tn.IsolationTier != "label" {
		t.Errorf("GetTenant acme: found=%v err=%v tn=%+v", found, err, tn)
	}
	if _, found, err := st.GetTenant(ctx, "missing"); err != nil {
		t.Errorf("GetTenant missing: err = %v, want nil", err)
	} else if found {
		t.Errorf("GetTenant missing: found = true, want false")
	}

	// ListTenants: ID ASC.
	all, err := st.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(all) != 2 || all[0].ID != "acme" || all[1].ID != "beta" {
		t.Errorf("ListTenants order: got [%s, %s], want [acme, beta]", idAt(all, 0), idAt(all, 1))
	}

	// GetOrg present + absent.
	o, found, err := st.GetOrg(ctx, "og_acme_eng")
	if err != nil || !found || o.Name != "engineering" || o.TenantID != "acme" {
		t.Errorf("GetOrg present: found=%v err=%v o=%+v", found, err, o)
	}
	if _, found, err := st.GetOrg(ctx, "missing"); err != nil {
		t.Errorf("GetOrg missing: err = %v", err)
	} else if found {
		t.Errorf("GetOrg missing: found = true")
	}

	// ListOrgs: filter + name ASC.
	acmeOrgs, err := st.ListOrgs(ctx, "acme")
	if err != nil {
		t.Fatalf("ListOrgs acme: %v", err)
	}
	if len(acmeOrgs) != 2 || acmeOrgs[0].Name != "engineering" || acmeOrgs[1].Name != "ops" {
		t.Errorf("ListOrgs acme order: got [%s, %s], want [engineering, ops]", nameAt(acmeOrgs, 0), nameAt(acmeOrgs, 1))
	}
	none, err := st.ListOrgs(ctx, "missing")
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("ListOrgs missing: len=%d err=%v none=%+v", len(none), err, none)
	}

	// ListTenantAudits: TS DESC.
	auds, err := st.ListTenantAudits(ctx, "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits: %v", err)
	}
	if len(auds) != 2 || auds[0].ID != "ta_new" || auds[1].ID != "ta_old" {
		t.Errorf("ListTenantAudits order: got [%s, %s], want [ta_new, ta_old]", aidAt(auds, 0), aidAt(auds, 1))
	}
	if empty, err := st.ListTenantAudits(ctx, "missing"); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("ListTenantAudits missing: len=%d err=%v", len(empty), err)
	}
}

func TestTenantStore_FileAppendTenantAudit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	st1, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := st1.AppendTenantAudit(context.Background(), TenantAudit{
		ID: newTenantAuditID(), TS: now, Principal: "u1",
		TenantID: "acme", Op: "tenant_create", Detail: "",
	}); err != nil {
		t.Fatalf("AppendTenantAudit: %v", err)
	}
	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore reload: %v", err)
	}
	got, err := st2.ListTenantAudits(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListTenantAudits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after reload: len = %d, want 1", len(got))
	}
	if got[0].Op != "tenant_create" || got[0].Principal != "u1" {
		t.Errorf("after reload: %+v", got[0])
	}
}

// withPGTenantEnv applies the full production migration set so
// tenants/orgs/tenant_audit (+ site_org 016) match production SQL.
func withPGTenantEnv(t *testing.T) (ctx context.Context, conn *pgxpool.Conn) {
	t.Helper()
	env := withPG(t)
	return env.Ctx, env.Conn
}

func TestTenantStore_PGParity(t *testing.T) {
	ctx, conn := withPGTenantEnv(t)

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for _, r := range []Tenant{
		{ID: "acme", DisplayName: "ACME Inc", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: "beta", DisplayName: "Beta Ltd", IsolationTier: "row", Status: "active", CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, display_name, isolation_tier, status, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			r.ID, r.DisplayName, r.IsolationTier, r.Status, r.CreatedAt, r.UpdatedAt); err != nil {
			t.Fatalf("insert tenant %s: %v", r.ID, err)
		}
	}

	all, err := listTenants(ctx, conn)
	if err != nil {
		t.Fatalf("listTenants: %v", err)
	}
	if len(all) != 2 || all[0].ID != "acme" || all[1].ID != "beta" {
		t.Errorf("listTenants order: got [%s, %s]", idAt(all, 0), idAt(all, 1))
	}

	if _, found, err := getTenant(ctx, conn, "acme"); err != nil || !found {
		t.Errorf("getTenant acme: found=%v err=%v", found, err)
	}
	if _, found, err := getTenant(ctx, conn, "missing"); err != nil {
		t.Errorf("getTenant missing: err = %v", err)
	} else if found {
		t.Errorf("getTenant missing: found = true")
	}

	for _, r := range []Org{
		{ID: "og_acme_eng", TenantID: "acme", Name: "engineering", CreatedAt: now},
		{ID: "og_acme_ops", TenantID: "acme", Name: "ops", CreatedAt: now},
		{ID: "og_beta_all", TenantID: "beta", Name: "company", CreatedAt: now},
	} {
		if _, err := conn.Exec(ctx, `INSERT INTO orgs (id, tenant_id, name, created_at) VALUES ($1,$2,$3,$4)`,
			r.ID, r.TenantID, r.Name, r.CreatedAt); err != nil {
			t.Fatalf("insert org %s: %v", r.ID, err)
		}
	}

	acmeOrgs, err := listOrgs(ctx, conn, "acme")
	if err != nil {
		t.Fatalf("listOrgs acme: %v", err)
	}
	if len(acmeOrgs) != 2 || acmeOrgs[0].Name != "engineering" || acmeOrgs[1].Name != "ops" {
		t.Errorf("listOrgs acme order: got [%s, %s]", nameAt(acmeOrgs, 0), nameAt(acmeOrgs, 1))
	}

	if _, found, err := getOrg(ctx, conn, "og_acme_eng"); err != nil || !found {
		t.Errorf("getOrg present: found=%v err=%v", found, err)
	}
	if _, found, err := getOrg(ctx, conn, "missing"); err != nil {
		t.Errorf("getOrg missing: err = %v", err)
	} else if found {
		t.Errorf("getOrg missing: found = true")
	}

	for _, a := range []TenantAudit{
		{ID: "ta_old", TS: now.Add(-2 * time.Hour), Principal: "u1", TenantID: "acme", Op: "tenant_create", Detail: ""},
		{ID: "ta_new", TS: now.Add(-1 * time.Hour), Principal: "u2", TenantID: "acme", Op: "tier_change", Detail: "label->row"},
	} {
		if err := appendTenantAudit(ctx, conn, a); err != nil {
			t.Fatalf("appendTenantAudit %s: %v", a.ID, err)
		}
	}
	got, err := listTenantAudits(ctx, conn, "acme")
	if err != nil {
		t.Fatalf("listTenantAudits: %v", err)
	}
	if len(got) != 2 || got[0].ID != "ta_new" || got[1].ID != "ta_old" {
		t.Errorf("listTenantAudits order: got [%s, %s]", aidAt(got, 0), aidAt(got, 1))
	}

	// CHECK constraints guard the vocabularies.
	if _, err := conn.Exec(ctx, `INSERT INTO tenant_audit (id, ts, principal, tenant_id, op, detail) VALUES ('ta_bad', $1, 'u', 'acme', 'bogus_op', '')`, now); err == nil {
		t.Errorf("CHECK did not reject tenant_audit.op='bogus_op'")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, display_name, isolation_tier, status, created_at, updated_at) VALUES ('bogus','X','schema','active',$1,$1)`, now); err == nil {
		t.Errorf("CHECK did not reject tenants.isolation_tier='schema'")
	}
}

func TestValidTenantSlug(t *testing.T) {
	// Spec-001 §5bis.1 id grammar: [a-z][a-z0-9-]{1,30} = first char
	// lowercase letter, then 1–30 chars of lowercase letter /
	// digit / dash. Total length 2..31 chars.
	for _, s := range []string{"ab", "a1", "a-b", "site01", "acme-corp", "a123456789012345678901234567890"} {
		if !validTenantSlug(s) {
			t.Errorf("validTenantSlug(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "a", "A", "1abc", "-abc", "a_b", "a.b", "a1234567890123456789012345678901", "a b", "aé"} {
		if validTenantSlug(s) {
			t.Errorf("validTenantSlug(%q) = true, want false", s)
		}
	}
}

func TestTenantIDGenerators(t *testing.T) {
	for i := 0; i < 8; i++ {
		id := newOrgID()
		if !strings.HasPrefix(id, "og_") || len(id) != len("og_")+16 {
			t.Errorf("newOrgID() = %q (len=%d), want og_+16", id, len(id))
		}
	}
	for i := 0; i < 8; i++ {
		id := newTenantAuditID()
		if !strings.HasPrefix(id, "ta_") || len(id) != len("ta_")+16 {
			t.Errorf("newTenantAuditID() = %q (len=%d), want ta_+16", id, len(id))
		}
	}
}

func idAt(s []Tenant, i int) string {
	if i < 0 || i >= len(s) {
		return "?"
	}
	return s[i].ID
}

func nameAt(s []Org, i int) string {
	if i < 0 || i >= len(s) {
		return "?"
	}
	return s[i].Name
}

func aidAt(s []TenantAudit, i int) string {
	if i < 0 || i >= len(s) {
		return "?"
	}
	return s[i].ID
}
