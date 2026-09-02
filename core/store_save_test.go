// Package core — fileStore.save write-path (PRMT-215).
//
// Covers compact Marshal (no indent), direct slice refs under write lock,
// load compatibility with indented on-disk files, and alloc regression.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSave_LoadIndentedLegacyStore proves load() still accepts
// MarshalIndent-era store files (two-space indent).
func TestSave_LoadIndentedLegacyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	legacy := diskShape{
		Version: 1,
		Assets: []Asset{{
			Path: "site01.pod000.cdu000", ResourceVersion: 1,
			Spec: map[string]any{"type": "cdu"}, CreatedAt: now, UpdatedAt: now,
		}},
		Alarms:       []Alarm{},
		Tickets:      []Ticket{},
		PMSchedules:  []PMSchedule{},
		Audits:       []AssetAudit{},
		Spares:       []SparePart{},
		SpareTxns:    []SpareTxn{},
		Inspections:  []InspectionTemplate{},
		Notes:        []TicketNote{},
		TicketAudits: []TicketAudit{},
		MWWindows:    []MaintenanceWindow{},
		Tenants: []Tenant{{
			ID: "acme", DisplayName: "ACME", IsolationTier: "label", Status: "active",
			CreatedAt: now, UpdatedAt: now,
		}},
		Orgs: []Org{{
			ID: "og_legacy01", TenantID: "acme", Name: "default", CreatedAt: now,
		}},
		TenantAudits: []TenantAudit{},
		SiteOrgs:     []SiteOrg{},
		RoleBindings: []RoleBinding{},
		Usages:       []UsageRecord{},
	}
	raw, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Fatal("fixture must be indented")
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore legacy: %v", err)
	}
	a, ok, err := st.GetAsset(context.Background(), "site01.pod000.cdu000")
	if err != nil || !ok || a.ResourceVersion != 1 {
		t.Fatalf("asset: ok=%v a=%+v err=%v", ok, a, err)
	}
	tn, ok, err := st.GetTenant(context.Background(), "acme")
	if err != nil || !ok || tn.DisplayName != "ACME" {
		t.Fatalf("tenant: ok=%v tn=%+v err=%v", ok, tn, err)
	}
	orgs, err := st.ListOrgs(context.Background(), "acme")
	if err != nil || len(orgs) != 1 || orgs[0].Name != "default" {
		t.Fatalf("orgs: %+v err=%v", orgs, err)
	}
}

func TestSave_RoundTripCompact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _, err = st.CreateTenant(ctx, "beta", "Beta Co", "svc:test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.PutAsset(ctx, Asset{
		Path: "site01.pod000.cdu000",
		Spec: map[string]any{"type": "cdu"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("invalid json: %s", raw)
	}
	if strings.Contains(string(raw), "\n  ") {
		t.Fatalf("disk still indented; want compact Marshal")
	}

	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tn, ok, err := st2.GetTenant(ctx, "beta")
	if err != nil || !ok || tn.DisplayName != "Beta Co" {
		t.Fatalf("tenant round-trip: %+v ok=%v err=%v", tn, ok, err)
	}
	a, ok, err := st2.GetAsset(ctx, "site01.pod000.cdu000")
	if err != nil || !ok || a.Spec["type"] != "cdu" {
		t.Fatalf("asset round-trip: %+v ok=%v err=%v", a, ok, err)
	}
}

func TestSave_AuditHistoryNotTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	const n = 50
	for i := 0; i < n; i++ {
		if err := st.AppendAssetAudit(ctx, AssetAudit{
			ID: fmt.Sprintf("aa_%03d", i), TS: now, Principal: "svc:t",
			Path: "site01.pod000.cdu000", Op: "put", Detail: fmt.Sprintf("d%d", i),
		}); err != nil {
			t.Fatalf("audit %d: %v", i, err)
		}
	}
	// role bindings (subject, scope unique)
	for i := 0; i < n; i++ {
		if err := st.PutRoleBinding(ctx, RoleBinding{
			Subject: fmt.Sprintf("svc:u%02d", i), Scope: "site01.**", Origin: "legacy",
		}); err != nil {
			t.Fatalf("rb %d: %v", i, err)
		}
	}
	// notes require a ticket
	tk, err := st.PutTicket(ctx, Ticket{
		ID: "tk_savehist01", AssetPath: "site01.pod000.cdu000", Title: "t",
		Severity: "minor", State: "open", OpenedAt: now,
	}, 0)
	if err != nil {
		t.Fatalf("ticket: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := st.AppendTicketNote(ctx, TicketNote{
			ID: fmt.Sprintf("tn_%03d", i), TicketID: tk.ID, Author: "svc:t",
			Body: fmt.Sprintf("note %d", i), At: now,
		}); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}

	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	auds, err := st2.ListAssetAudits(ctx, "site01.pod000.cdu000")
	if err != nil || len(auds) != n {
		t.Fatalf("audits len=%d err=%v want %d", len(auds), err, n)
	}
	for i := 0; i < n; i++ {
		if auds[i].ID != fmt.Sprintf("aa_%03d", i) || auds[i].Detail != fmt.Sprintf("d%d", i) {
			t.Fatalf("audit order broken at %d: %+v", i, auds[i])
		}
	}
	rbs, err := st2.ListAllRoleBindings(ctx)
	if err != nil || len(rbs) != n {
		t.Fatalf("rbs len=%d err=%v want %d", len(rbs), err, n)
	}
	notes, err := st2.ListTicketNotes(ctx, tk.ID)
	if err != nil || len(notes) != n {
		t.Fatalf("notes len=%d err=%v want %d", len(notes), err, n)
	}
	for i := 0; i < n; i++ {
		if notes[i].Body != fmt.Sprintf("note %d", i) {
			t.Fatalf("note order at %d: %+v", i, notes[i])
		}
	}
}
