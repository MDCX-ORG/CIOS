//go:build !race

// Alloc regression for fileStore.save (PRMT-215 §5.2 #5).
// Excluded under -race: the race detector inflates AllocedBytesPerOp
// (~2×) so the 60%-of-baseline gate is only meaningful without -race.
package core

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSave_AllocRegression_2000Entities(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	fs := st.(*fileStore)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	fs.mu.Lock()
	for i := 0; i < 2000; i++ {
		tid := fmt.Sprintf("t%04d", i)
		fs.tenants[tid] = Tenant{
			ID: tid, DisplayName: tid, IsolationTier: "label", Status: "active",
			CreatedAt: now, UpdatedAt: now,
		}
		oid := fmt.Sprintf("og_%04dxxxxxxxx", i)
		fs.orgs[oid] = Org{ID: oid, TenantID: tid, Name: "default", CreatedAt: now}
		fs.audits = append(fs.audits, AssetAudit{
			ID: fmt.Sprintf("aa_%04d", i), Path: "p", Op: "put", TS: now,
		})
	}
	if err := fs.save(); err != nil {
		fs.mu.Unlock()
		t.Fatal(err)
	}
	// Hold lock for the whole benchmark: save requires write lock.
	res := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := fs.save(); err != nil {
				b.Fatal(err)
			}
		}
	})
	fs.mu.Unlock()
	// Pre-change first run of BenchmarkFileStoreSave_Baseline2000:
	// 4558418 B/op (also 4513552 / 4647913). Gate: after < 60%.
	const baselineBPerOp int64 = 4_558_418
	got := res.AllocedBytesPerOp()
	limit := baselineBPerOp * 60 / 100
	if got > limit {
		t.Errorf("save AllocedBytesPerOp=%d want <= %d (60%% of baseline %d)", got, limit, baselineBPerOp)
	}
	t.Logf("save AllocedBytesPerOp=%d N=%d baseline=%d limit60=%d", got, res.N, baselineBPerOp, limit)
}
