package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

func usageTestDict(t *testing.T) *cpath.Dict {
	t.Helper()
	d, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	return d
}

func TestPreviousUTCDay(t *testing.T) {
	now := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)
	start, end := previousUTCDay(now)
	if !start.Equal(time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v", end)
	}
}

func TestScanUsageTick_RackHour(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Seed active rack.
	if _, err := st.PutAsset(ctx, Asset{
		Path: "sgp01.pod000.rack001",
		Spec: map[string]any{"type": "rack", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	var sink captureSink
	srv := NewServer(st, usageTestDict(t), "") // empty vm → no energy
	srv.usageSink = &sink

	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	srv.scanUsageTick(ctx, now)

	// Daily + monthly rack_hour rows for the same rack.
	list, err := st.ListUsage(ctx, UsageListFilter{Kind: UsageKindRackHour})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("rack rows = %d, want 2 (daily+monthly)", len(list))
	}
	var daily, monthly *UsageRecord
	for i := range list {
		switch list[i].Granularity {
		case UsageDaily:
			daily = &list[i]
		case UsageMonthly:
			monthly = &list[i]
		}
	}
	if daily == nil || monthly == nil {
		t.Fatalf("missing grain: daily=%v monthly=%v list=%+v", daily, monthly, list)
	}
	if daily.Quantity != 24 {
		t.Fatalf("daily qty = %v, want 24h", daily.Quantity)
	}
	// June 2026 = 30 days → 720h
	if monthly.Quantity != 720 {
		t.Fatalf("monthly qty = %v, want 720h", monthly.Quantity)
	}
	if sink.n < 2 {
		t.Fatalf("sink calls = %d, want ≥2", sink.n)
	}
}

func TestFetchEnergyMeasurements_FromVM(t *testing.T) {
	// Fake VM returns increase = 12.5 kWh.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{"value": []any{1.0, "12.5"}},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dir := t.TempDir()
	st, _ := NewFileStore(filepath.Join(dir, "s.json"))
	srv := NewServer(st, usageTestDict(t), ts.URL)
	assets := []Asset{{Path: "sgp01.meter000", Spec: map[string]any{"type": "meter"}}}
	start := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	ms := srv.fetchEnergyMeasurements(context.Background(), assets, start, end)
	if len(ms) != 1 || ms[0].Quantity != 12.5 || ms[0].Unit != "kWh" {
		t.Fatalf("measurements = %+v", ms)
	}
}

type mockJSPub struct {
	mu   sync.Mutex
	subj string
	body []byte
	n    int
}

func (m *mockJSPub) Publish(subject string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.subj = subject
	m.body = append([]byte(nil), data...)
	return nil
}

func TestNATSUsageEventSink(t *testing.T) {
	pub := &mockJSPub{}
	sink := NATSUsageEventSink{Pub: pub}
	sink.OnUsageUpserted(context.Background(), UsageRecord{ID: "us_X", Kind: UsageKindEnergy, Quantity: 1})
	if pub.n != 1 || pub.subj != "cios.usage.upserted" {
		t.Fatalf("pub = %+v", pub)
	}
	var rec UsageRecord
	if err := json.Unmarshal(pub.body, &rec); err != nil || rec.ID != "us_X" {
		t.Fatalf("body = %s err=%v", pub.body, err)
	}
}

func TestScanUsageTick_MonthlyTimezone(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.PutAsset(ctx, Asset{
		Path: "sgp01",
		Spec: map[string]any{"type": "site", "timezone": "Asia/Singapore"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutAsset(ctx, Asset{
		Path: "sgp01.pod000.rack001",
		Spec: map[string]any{"type": "rack", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st, usageTestDict(t), "")
	// Mid-March SGT → previous month = February (28d in 2026).
	loc, _ := time.LoadLocation("Asia/Singapore")
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, loc)
	srv.scanUsageTick(ctx, now)
	list, err := st.ListUsage(ctx, UsageListFilter{Kind: UsageKindRackHour, Granularity: UsageMonthly})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("monthly rows = %d want 1; all=%+v", len(list), list)
	}
	// Feb 2026 = 28 days → 672h
	if list[0].Quantity != 672 {
		t.Fatalf("qty = %v want 672", list[0].Quantity)
	}
	wantStart := time.Date(2026, 2, 1, 0, 0, 0, 0, loc)
	if !list[0].PeriodStart.Equal(wantStart) {
		t.Fatalf("period_start = %v want %v", list[0].PeriodStart, wantStart)
	}
}
