// Package core — ListOrgs allocation regression (PRMT-212).
//
// Before: make([]Org, 0, len(s.orgs)) pre-sized to the whole store and a
// name-sort + full re-scan path; profile showed ListOrgs = 92% of alloc_space.
// After: exact-capacity subset + direct sort of matches.
package core

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestListOrgs_AllocRegression_TargetOneAmong2000(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs := st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	fs.mu.Lock()
	// 2000 other tenants with one org each + one target tenant with one org.
	for i := 0; i < 2000; i++ {
		tid := fmt.Sprintf("t%04d", i)
		fs.tenants[tid] = Tenant{
			ID: tid, DisplayName: tid, IsolationTier: "label", Status: "active",
			CreatedAt: now, UpdatedAt: now,
		}
		oid := fmt.Sprintf("og_other%04dxxxx", i) // keep unique map keys
		fs.orgs[oid] = Org{ID: oid, TenantID: tid, Name: "default", CreatedAt: now}
	}
	fs.tenants["target"] = Tenant{
		ID: "target", DisplayName: "Target", IsolationTier: "label", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	fs.orgs["og_target_onlyxxxx"] = Org{
		ID: "og_target_onlyxxxx", TenantID: "target", Name: "solo", CreatedAt: now,
	}
	fs.mu.Unlock()

	ctx := context.Background()
	// Correctness first (single call).
	got, err := st.ListOrgs(ctx, "target")
	if err != nil {
		t.Fatalf("ListOrgs target: %v", err)
	}
	if got == nil || len(got) != 1 || got[0].Name != "solo" || got[0].TenantID != "target" {
		t.Fatalf("ListOrgs target: got %+v", got)
	}

	res := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = st.ListOrgs(ctx, "target")
		}
	})
	// Fix-before: ~len(s.orgs)×72B ≈ 144 KB/op; fix-after: < 4 KB/op (PRMT-212 §6.2).
	if got := res.AllocedBytesPerOp(); got > 4096 {
		t.Errorf("ListOrgs allocates %d B/op, want < 4096 (PRMT-212)", got)
	}
	t.Logf("ListOrgs AllocedBytesPerOp=%d N=%d", res.AllocedBytesPerOp(), res.N)
}

func TestListOrgs_NameASC_AndNonNilEmpty_AndTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs := st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{ID: "acme", DisplayName: "A", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.tenants["beta"] = Tenant{ID: "beta", DisplayName: "B", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	// Insert out of name order.
	fs.orgs["og_acme_z"] = Org{ID: "og_acme_z", TenantID: "acme", Name: "zebra", CreatedAt: now}
	fs.orgs["og_acme_a"] = Org{ID: "og_acme_a", TenantID: "acme", Name: "alpha", CreatedAt: now}
	fs.orgs["og_acme_m"] = Org{ID: "og_acme_m", TenantID: "acme", Name: "mid", CreatedAt: now}
	fs.orgs["og_beta_x"] = Org{ID: "og_beta_x", TenantID: "beta", Name: "xray", CreatedAt: now}
	fs.mu.Unlock()

	ctx := context.Background()
	acme, err := st.ListOrgs(ctx, "acme")
	if err != nil {
		t.Fatalf("ListOrgs acme: %v", err)
	}
	if len(acme) != 3 {
		t.Fatalf("ListOrgs acme len=%d want 3", len(acme))
	}
	if acme[0].Name != "alpha" || acme[1].Name != "mid" || acme[2].Name != "zebra" {
		t.Errorf("name ASC: got [%s,%s,%s]", acme[0].Name, acme[1].Name, acme[2].Name)
	}
	for _, o := range acme {
		if o.TenantID != "acme" {
			t.Errorf("tenant isolation: got tenant_id=%q", o.TenantID)
		}
	}

	none, err := st.ListOrgs(ctx, "missing")
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("missing tenant: none=%+v err=%v (want non-nil empty)", none, err)
	}

	beta, err := st.ListOrgs(ctx, "beta")
	if err != nil || len(beta) != 1 || beta[0].Name != "xray" {
		t.Errorf("beta: %+v err=%v", beta, err)
	}
}
