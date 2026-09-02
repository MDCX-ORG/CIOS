// Package core — ListOrgsAll correctness (PRMT-214).
//
// Removes the per-tenant ListOrgs N+1 in serveTenantsList via one bulk
// map of tenant_id → []Org (name ASC per group).
package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestListOrgsAll_GroupSortEquivalenceEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fs := st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// 3 tenants: acme 2 orgs, beta 1 org, gamma 0 orgs (key absent).
	fs.mu.Lock()
	fs.tenants["acme"] = Tenant{ID: "acme", DisplayName: "A", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.tenants["beta"] = Tenant{ID: "beta", DisplayName: "B", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	fs.tenants["gamma"] = Tenant{ID: "gamma", DisplayName: "G", IsolationTier: "label", Status: "active", CreatedAt: now, UpdatedAt: now}
	// Insert acme out of name order.
	fs.orgs["og_acme_z"] = Org{ID: "og_acme_z", TenantID: "acme", Name: "zebra", CreatedAt: now}
	fs.orgs["og_acme_a"] = Org{ID: "og_acme_a", TenantID: "acme", Name: "alpha", CreatedAt: now}
	fs.orgs["og_beta_x"] = Org{ID: "og_beta_x", TenantID: "beta", Name: "xray", CreatedAt: now}
	fs.mu.Unlock()

	ctx := context.Background()
	all, err := st.ListOrgsAll(ctx)
	if err != nil {
		t.Fatalf("ListOrgsAll: %v", err)
	}
	if all == nil {
		t.Fatal("ListOrgsAll returned nil map")
	}
	// 1. Grouping: 2 keys; gamma (0 orgs) absent.
	if len(all) != 2 {
		t.Fatalf("map keys=%d want 2 (gamma absent), keys=%v", len(all), keysOf(all))
	}
	if _, ok := all["gamma"]; ok {
		t.Errorf("gamma present with orgs=%+v; want key absent", all["gamma"])
	}
	if len(all["acme"]) != 2 || len(all["beta"]) != 1 {
		t.Fatalf("group sizes acme=%d beta=%d", len(all["acme"]), len(all["beta"]))
	}

	// 2. name ASC per group
	if all["acme"][0].Name != "alpha" || all["acme"][1].Name != "zebra" {
		t.Errorf("acme name ASC: got [%s, %s]", all["acme"][0].Name, all["acme"][1].Name)
	}
	if all["beta"][0].Name != "xray" {
		t.Errorf("beta name: %s", all["beta"][0].Name)
	}

	// 3. Equivalence with per-tenant ListOrgs for every tenant id.
	for _, tid := range []string{"acme", "beta", "gamma", "missing"} {
		want, err := st.ListOrgs(ctx, tid)
		if err != nil {
			t.Fatalf("ListOrgs(%s): %v", tid, err)
		}
		got := all[tid] // missing key → nil
		if tid == "gamma" || tid == "missing" {
			if len(got) != 0 {
				t.Errorf("ListOrgsAll[%s]=%+v want absent/empty; ListOrgs len=%d", tid, got, len(want))
			}
			if len(want) != 0 {
				t.Errorf("ListOrgs(%s) len=%d want 0", tid, len(want))
			}
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("%s: ListOrgsAll len=%d ListOrgs len=%d", tid, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].Name != want[i].Name || got[i].TenantID != want[i].TenantID {
				t.Errorf("%s[%d]: All=%+v List=%+v", tid, i, got[i], want[i])
			}
		}
	}
}

func TestListOrgsAll_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	all, err := st.ListOrgsAll(context.Background())
	if err != nil {
		t.Fatalf("ListOrgsAll empty: %v", err)
	}
	if all == nil {
		t.Fatal("want non-nil empty map")
	}
	if len(all) != 0 {
		t.Fatalf("want empty map, got %d keys", len(all))
	}
}

func keysOf(m map[string][]Org) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
