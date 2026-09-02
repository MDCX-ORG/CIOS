package core

// PRMT-099 R8 (K1): pgxpool acquire p99 < 10ms on a hot pool.
//
// Runs 10_000 acquire/release on a pool that is already warmed up
// (production path). p99 is computed by hand (the bench's reported
// ns/op is the mean; we want the tail, which is what a real K1 miss
// looks like under alarm storms).
//
// Gated on CIOS_PG_DSN so a CI run without PG stays green.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func newBenchPGPool(b *testing.B) (*pgStore, func()) {
	b.Helper()
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		b.Skip("CIOS_PG_DSN not set — R8 requires a live PG (PRMT-099 §0)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := moduleRoot(&testing.T{})
	migs := filepath.Join(root, "migrations")
	st, err := NewPGStore(ctx, dsn, migs)
	if err != nil {
		b.Fatalf("NewPGStore: %v", err)
	}
	pgs, ok := st.(*pgStore)
	if !ok {
		b.Fatalf("expected *pgStore, got %T", st)
	}
	// Warm the pool: force a few round-trips so the first measured
	// Acquire is on a hot path (pool + socket + statement cache all
	// initialised).
	for i := 0; i < 8; i++ {
		c, err := pgs.pool.Acquire(ctx)
		if err != nil {
			b.Fatalf("warm acquire: %v", err)
		}
		_ = c.Ping(ctx)
		c.Release()
	}
	return pgs, func() { pgs.pool.Close() }
}

// BenchmarkPGPool_AcquireRelease is the K1 measure. b.N acquires are
// timed end-to-end; the bench itself reports mean ns/op. A separate
// helper computes p99 so the result can be cross-checked against the
// 10ms target.
func BenchmarkPGPool_AcquireRelease(b *testing.B) {
	pgs, done := newBenchPGPool(b)
	defer done()
	ctx := context.Background()

	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		c, err := pgs.pool.Acquire(ctx)
		if err != nil {
			b.Fatalf("acquire %d: %v", i, err)
		}
		c.Release()
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()

	// Surface p50/p99 inline so a single `go test -bench` output
	// row carries the tail number an operator cares about. Mean is
	// already reported by the framework.
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)*50/100]
	p99 := samples[len(samples)*99/100]
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns")
}

// PRMT-099 R9 (K2): PG single-row read/write p99 < 10ms.
//
// Each iteration: PutTicket (new id, expectVersion=0, the canonical
// INSERT...ON CONFLICT path) + GetTicket. Both calls go through the
// production pgStore.Store wrappers so the bench measures the real
// hot path, not a stub. p99 is computed and reported alongside.
func BenchmarkPGStore_PutGetTicket(b *testing.B) {
	pgs, done := newBenchPGPool(b)
	defer done()
	ctx := context.Background()
	// Clean up BENCH-* rows after the bench so a follow-up
	// `go test ./core/` (TestPG_PutTicketUpsertAndList, file/PG
	// ordering) sees an empty tickets table again.
	b.Cleanup(func() {
		_, _ = pgs.pool.Exec(ctx, "DELETE FROM tickets WHERE id LIKE 'BENCH-%'")
	})

	rng := rand.New(rand.NewSource(1)) // deterministic, no syscall
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	samples := make([]time.Duration, 0, b.N)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("BENCH-%08d", i)
		tk := Ticket{
			ID:        id,
			AssetPath: "site01.pod000.cdu000",
			Title:     "bench ticket " + id,
			Severity:  "major",
			State:     "open",
			OpenedAt:  base.Add(time.Duration(rng.Intn(60)) * time.Second),
		}
		start := time.Now()
		if _, err := pgs.PutTicket(ctx, tk, 0); err != nil {
			b.Fatalf("PutTicket %d: %v", i, err)
		}
		if _, _, err := pgs.GetTicket(ctx, id); err != nil {
			b.Fatalf("GetTicket %d: %v", i, err)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)*50/100]
	p99 := samples[len(samples)*99/100]
	b.ReportMetric(float64(p50.Microseconds()), "p50-us")
	b.ReportMetric(float64(p99.Microseconds()), "p99-us")
}
