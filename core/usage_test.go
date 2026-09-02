// Package core — usage_test.go: unit tests for Measurement /
// UsageRecord compute + fileStore Upsert/List (PRMT-192 §4 / §7).
//
// Coverage:
//   - newUsageID shape (us_ + 16 base32)
//   - ComputeRackHourUsage: only type=rack && lifecycle=active
//   - ComputeEnergyUsage: sum kWh deltas in [start,end); skip non-kWh
//   - UpsertUsage assign id + replace + ListUsage filter/sort
//   - fileStore persist survives reload
package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewUsageID(t *testing.T) {
	id := newUsageID()
	if !strings.HasPrefix(id, "us_") {
		t.Fatalf("newUsageID() = %q, want us_ prefix", id)
	}
	// "us_" + 16 base32 chars (NoPadding) from 10 random bytes.
	body := strings.TrimPrefix(id, "us_")
	if len(body) != 16 {
		t.Fatalf("newUsageID body len = %d, want 16 (id=%q)", len(body), id)
	}
	for _, c := range body {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			t.Fatalf("newUsageID body has non-base32 char %q in %q", c, id)
		}
	}
}

func TestComputeRackHourUsage(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC) // 24h
	assets := []Asset{
		{Path: "sgp01.pod001.rack001", Spec: map[string]any{"type": "rack", "lifecycle": "active"}},
		{Path: "sgp01.pod001.rack002", Spec: map[string]any{"type": "rack", "lifecycle": "retired"}},
		{Path: "sgp01.pod001.cdu000", Spec: map[string]any{"type": "cdu", "lifecycle": "active"}},
		{Path: "sgp01.pod001.rack003", Spec: map[string]any{"type": "rack", "lifecycle": "active"}},
		{Path: "sgp01.pod001.rack004"}, // nil Spec
	}
	got := ComputeRackHourUsage(assets, start, end, UsageDaily)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 active racks; got %+v", len(got), got)
	}
	// Order follows asset input order: rack001 then rack003.
	wantPaths := []string{"sgp01.pod001.rack001", "sgp01.pod001.rack003"}
	for i, rec := range got {
		if rec.Kind != UsageKindRackHour {
			t.Errorf("[%d] kind = %q, want rack_hour", i, rec.Kind)
		}
		if rec.Unit != "h" {
			t.Errorf("[%d] unit = %q, want h", i, rec.Unit)
		}
		if rec.Quantity != 24 {
			t.Errorf("[%d] quantity = %v, want 24", i, rec.Quantity)
		}
		if rec.SiteID != "sgp01" {
			t.Errorf("[%d] site_id = %q, want sgp01", i, rec.SiteID)
		}
		if rec.TenantID != "" || rec.OrgID != "" {
			t.Errorf("[%d] tenant/org must be empty, got tenant=%q org=%q", i, rec.TenantID, rec.OrgID)
		}
		if rec.ID != "" {
			t.Errorf("[%d] id must be empty for caller Upsert, got %q", i, rec.ID)
		}
		if rec.AssetPath != wantPaths[i] {
			t.Errorf("[%d] asset_path = %q, want %q", i, rec.AssetPath, wantPaths[i])
		}
		if rec.Granularity != UsageDaily {
			t.Errorf("[%d] granularity = %q, want daily", i, rec.Granularity)
		}
		if !rec.PeriodStart.Equal(start) || !rec.PeriodEnd.Equal(end) {
			t.Errorf("[%d] period = [%v, %v], want [%v, %v]", i, rec.PeriodStart, rec.PeriodEnd, start, end)
		}
	}
}

func TestComputeEnergyUsage(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	pathA := "sgp01.pod001.pdu001"
	pathB := "sgp01.pod001.pdu002"
	ms := []Measurement{
		{AssetPath: pathA, Time: start.Add(-time.Hour), Quantity: 99, Unit: "kWh"},     // before window
		{AssetPath: pathA, Time: start, Quantity: 1.5, Unit: "kWh"},                    // in
		{AssetPath: pathA, Time: start.Add(2 * time.Hour), Quantity: 2.5, Unit: "kWh"}, // in
		{AssetPath: pathA, Time: end, Quantity: 50, Unit: "kWh"},                       // at end — excluded [start,end)
		{AssetPath: pathA, Time: start.Add(time.Hour), Quantity: 3, Unit: "MW"},        // non-kWh skip
		{AssetPath: pathB, Time: start.Add(time.Hour), Quantity: 4, Unit: "kWh"},       // in
		{AssetPath: pathB, Time: start.Add(3 * time.Hour), Quantity: 1, Unit: "kWh"},   // in
	}
	got := ComputeEnergyUsage(ms, start, end, UsageDaily)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 assets; got %+v", len(got), got)
	}
	// First-seen order: pathA then pathB.
	if got[0].AssetPath != pathA || got[0].Quantity != 4.0 {
		t.Errorf("pathA: got path=%q qty=%v, want qty=4 (1.5+2.5)", got[0].AssetPath, got[0].Quantity)
	}
	if got[1].AssetPath != pathB || got[1].Quantity != 5.0 {
		t.Errorf("pathB: got path=%q qty=%v, want qty=5 (4+1)", got[1].AssetPath, got[1].Quantity)
	}
	for i, rec := range got {
		if rec.Kind != UsageKindEnergy {
			t.Errorf("[%d] kind = %q, want energy", i, rec.Kind)
		}
		if rec.Unit != "kWh" {
			t.Errorf("[%d] unit = %q, want kWh", i, rec.Unit)
		}
		if rec.SiteID != "sgp01" {
			t.Errorf("[%d] site_id = %q, want sgp01", i, rec.SiteID)
		}
		if rec.ID != "" {
			t.Errorf("[%d] id must be empty", i)
		}
	}
}

func TestUsageStore_UpsertListFilterSort(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	// Upsert with empty ID → assign.
	r1, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, TenantID: "tn_a", SiteID: "sgp01",
		AssetPath: "sgp01.pod001.pdu001", PeriodStart: t0, PeriodEnd: t1,
		Granularity: UsageDaily, Quantity: 10, Unit: "kWh",
	})
	if err != nil {
		t.Fatalf("UpsertUsage r1: %v", err)
	}
	if !strings.HasPrefix(r1.ID, "us_") {
		t.Fatalf("assigned id = %q, want us_ prefix", r1.ID)
	}

	// Second record, earlier period for sort check + different kind.
	r2, err := st.UpsertUsage(ctx, UsageRecord{
		ID: "us_FIXEDID00000001", Kind: UsageKindRackHour, TenantID: "tn_a", SiteID: "sgp01",
		AssetPath: "sgp01.pod001.rack001", PeriodStart: t0.Add(-24 * time.Hour), PeriodEnd: t0,
		Granularity: UsageDaily, Quantity: 24, Unit: "h",
	})
	if err != nil {
		t.Fatalf("UpsertUsage r2: %v", err)
	}
	if r2.ID != "us_FIXEDID00000001" {
		t.Fatalf("r2 id = %q, want fixed", r2.ID)
	}

	// Replace same id.
	r2b, err := st.UpsertUsage(ctx, UsageRecord{
		ID: "us_FIXEDID00000001", Kind: UsageKindRackHour, TenantID: "tn_a", SiteID: "sgp01",
		AssetPath: "sgp01.pod001.rack001", PeriodStart: t0.Add(-24 * time.Hour), PeriodEnd: t0,
		Granularity: UsageDaily, Quantity: 24.5, Unit: "h",
	})
	if err != nil {
		t.Fatalf("UpsertUsage replace: %v", err)
	}
	if r2b.Quantity != 24.5 {
		t.Errorf("replace qty = %v, want 24.5", r2b.Quantity)
	}

	// Other tenant / site for filter exclusion.
	if _, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, TenantID: "tn_b", SiteID: "nrt01",
		AssetPath: "nrt01.pod001.pdu001", PeriodStart: t1, PeriodEnd: t2,
		Granularity: UsageMonthly, Quantity: 1, Unit: "kWh",
	}); err != nil {
		t.Fatalf("UpsertUsage r3: %v", err)
	}

	// List all for tenant tn_a: 2 records, sorted PeriodStart then AssetPath then Kind.
	all, err := st.ListUsage(ctx, UsageListFilter{TenantID: "tn_a"})
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListUsage tn_a len = %d, want 2; got %+v", len(all), all)
	}
	if all[0].ID != "us_FIXEDID00000001" || all[1].ID != r1.ID {
		t.Errorf("sort order ids = %q, %q; want fixed then %q", all[0].ID, all[1].ID, r1.ID)
	}
	if all[0].Quantity != 24.5 {
		t.Errorf("replaced record qty = %v, want 24.5", all[0].Quantity)
	}

	// Kind filter.
	energyOnly, err := st.ListUsage(ctx, UsageListFilter{Kind: UsageKindEnergy})
	if err != nil {
		t.Fatalf("ListUsage kind: %v", err)
	}
	if len(energyOnly) != 2 { // tn_a energy + tn_b energy
		t.Fatalf("energy kind len = %d, want 2", len(energyOnly))
	}

	// Site filter.
	sgp, err := st.ListUsage(ctx, UsageListFilter{SiteID: "sgp01"})
	if err != nil {
		t.Fatalf("ListUsage site: %v", err)
	}
	if len(sgp) != 2 {
		t.Fatalf("site sgp01 len = %d, want 2", len(sgp))
	}

	// Period overlap: window [t0, t1) overlaps r1 [t0,t1) and not r2 [t0-24h, t0)
	// r2.PeriodEnd == t0 so PeriodEnd > PeriodStartFilter is false when filter.PeriodStart==t0.
	overlap, err := st.ListUsage(ctx, UsageListFilter{
		TenantID: "tn_a", PeriodStart: t0, PeriodEnd: t1,
	})
	if err != nil {
		t.Fatalf("ListUsage period: %v", err)
	}
	if len(overlap) != 1 || overlap[0].ID != r1.ID {
		t.Fatalf("period overlap = %+v, want only r1", overlap)
	}

	// Empty list is non-nil.
	empty, err := st.ListUsage(ctx, UsageListFilter{TenantID: "tn_none"})
	if err != nil {
		t.Fatalf("ListUsage empty: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, want non-nil empty", empty)
	}
}

func TestUsageStore_PersistReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "store.json")
	st, err := NewFileStore(p)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rec, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, TenantID: "tn_persist", SiteID: "sgp01",
		AssetPath: "sgp01.pod001.pdu001", PeriodStart: start, PeriodEnd: end,
		Granularity: UsageDaily, Quantity: 12.5, Unit: "kWh",
	})
	if err != nil {
		t.Fatalf("UpsertUsage: %v", err)
	}

	// Reload from the same path.
	st2, err := NewFileStore(p)
	if err != nil {
		t.Fatalf("reload NewFileStore: %v", err)
	}
	got, err := st2.ListUsage(ctx, UsageListFilter{TenantID: "tn_persist"})
	if err != nil {
		t.Fatalf("ListUsage after reload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after reload len = %d, want 1", len(got))
	}
	if got[0].ID != rec.ID || got[0].Quantity != 12.5 || got[0].Kind != UsageKindEnergy {
		t.Errorf("reloaded = %+v, want id=%s qty=12.5 energy", got[0], rec.ID)
	}
}

func TestSiteIDFromPath(t *testing.T) {
	if g := siteIDFromPath("sgp01.pod001.rack001"); g != "sgp01" {
		t.Errorf("got %q, want sgp01", g)
	}
	if g := siteIDFromPath("lonely"); g != "lonely" {
		t.Errorf("got %q, want lonely", g)
	}
}

func TestUsageStore_NaturalKeyIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	r1, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, AssetPath: "sgp01.pod000", SiteID: "sgp01",
		PeriodStart: start, PeriodEnd: end, Granularity: UsageDaily,
		Quantity: 10, Unit: "kWh",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, AssetPath: "sgp01.pod000", SiteID: "sgp01",
		PeriodStart: start, PeriodEnd: end, Granularity: UsageDaily,
		Quantity: 99, Unit: "kWh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("natural key: id %q vs %q", r1.ID, r2.ID)
	}
	if r2.Quantity != 99 {
		t.Fatalf("qty = %v, want 99", r2.Quantity)
	}
	all, _ := st.ListUsage(ctx, UsageListFilter{})
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1", len(all))
	}
}

func TestEnrichUsageIdentity(t *testing.T) {
	st := seedSiteOrgStore(t)
	ctx := context.Background()
	if err := st.AttachSiteToOrg(ctx, "sgp01", "og_acme_eng", "svc:test"); err != nil {
		t.Fatal(err)
	}
	rec := UsageRecord{AssetPath: "sgp01.pod000.rack001", SiteID: "sgp01"}
	got, err := EnrichUsageIdentity(ctx, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	if got.OrgID != "og_acme_eng" || got.TenantID != "acme" {
		t.Fatalf("got org=%q tenant=%q", got.OrgID, got.TenantID)
	}
}

type captureSink struct {
	last UsageRecord
	n    int
}

func (c *captureSink) OnUsageUpserted(_ context.Context, rec UsageRecord) {
	c.n++
	c.last = rec
}

func TestUsageEventSink_OnCompute(t *testing.T) {
	// Sink is invoked from HTTP compute path — unit-check interface wiring.
	var c captureSink
	var sink UsageEventSink = &c
	sink.OnUsageUpserted(context.Background(), UsageRecord{ID: "us_TEST"})
	if c.n != 1 || c.last.ID != "us_TEST" {
		t.Fatalf("sink not called: %+v", c)
	}
	NoopUsageEventSink{}.OnUsageUpserted(context.Background(), UsageRecord{})
}

// TestUsageStore_PGParity exercises 018_usage.sql via NewPGStore.
// Skipped when CIOS_PG_DSN is unset.
func TestUsageStore_PGParity(t *testing.T) {
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		t.Skip("CIOS_PG_DSN not set - skipping PG usage parity test")
	}
	ctx := context.Background()
	st, err := NewPGStore(ctx, dsn, filepath.Join(moduleRoot(t), "migrations"))
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	r1, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, TenantID: "pg_t", SiteID: "sgp01",
		AssetPath: "sgp01.pod000", PeriodStart: start, PeriodEnd: end,
		Granularity: UsageDaily, Quantity: 1.5, Unit: "kWh",
	})
	if err != nil {
		t.Fatalf("UpsertUsage: %v", err)
	}
	r2, err := st.UpsertUsage(ctx, UsageRecord{
		Kind: UsageKindEnergy, TenantID: "pg_t", SiteID: "sgp01",
		AssetPath: "sgp01.pod000", PeriodStart: start, PeriodEnd: end,
		Granularity: UsageDaily, Quantity: 9.0, Unit: "kWh",
	})
	if err != nil {
		t.Fatalf("UpsertUsage2: %v", err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("natural key id mismatch %q %q", r1.ID, r2.ID)
	}
	if r2.Quantity != 9.0 {
		t.Fatalf("qty = %v", r2.Quantity)
	}
	list, err := st.ListUsage(ctx, UsageListFilter{TenantID: "pg_t", Kind: UsageKindEnergy})
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(list) < 1 {
		t.Fatal("empty list")
	}
}
