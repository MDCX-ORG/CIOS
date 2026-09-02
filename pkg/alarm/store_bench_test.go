package alarm

// PRMT-099 R10 (K7-sub): collapse pkg/alarm.OpenTicket from 3 RTT
// (ticket-existence probe + maintenance-window probe + INSERT) to a
// single CTE round-trip.
//
// The benchmark issues N OpenTicket calls against the canonical
// "no existing non-closed ticket for this alarm_id, no active
// maintenance window for this asset" path so the INSERT runs on
// every iteration (existing-probe misses because alarm_id is unique
// per i; mw-probe misses because no maintenance_windows rows are
// seeded). p99 is computed from per-iteration wall-clock samples
// and reported alongside the framework's mean.
//
// Gated on CIOS_PG_DSN — the 3-RTT claim is meaningless without a
// live DB. PRMT-099 §0 makes this mandatory.
//
// R10d note (PRMT-099 R10c 复核五轮 Finding R10c.D): the previous
// `i%32` AssetPath produced only 32 distinct alarm_ids, so after
// i≥32 the existing-probe hit and the bench measured the existing-
// skip path (~99.6% of b.N=7600) rather than the INSERT path the
// optimisation targets. Replaced `i%32` with `i` so every iteration
// runs INSERT; cleanup still wipes the deterministic asset_path
// prefix on test exit. CTE code unchanged — R10.1 priority
// semantics are unaffected.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

func newBenchAlarmStore(b *testing.B) (*Store, func()) {
	b.Helper()
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		b.Skip("CIOS_PG_DSN not set — R10 requires a live PG (PRMT-099 §0)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := NewStore(ctx, dsn)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	wdCtx := context.Background()
	// Cleanup: wipe any tickets the bench created. Tickets use the
	// alarm_id as the dedup key, and our bench derives alarm_id from
	// rule "cdu-fws-deltat-low" + asset_path "sgp01.pod002.cdu%03d".
	// The id column has a tk_ prefix and is regenerated per call,
	// so we delete by the deterministic asset_path prefix instead.
	b.Cleanup(func() {
		_, _ = st.pool.Exec(wdCtx, "DELETE FROM tickets WHERE asset_path LIKE 'sgp01.pod002.cdu%'")
		st.Close()
	})
	return st, nil
}

// BenchmarkOpenTicket_HotPath measures the canonical path that PRMT-096
// explicitly cites as the 3-RTT cost driver:
//   - no existing non-closed ticket for this alarm
//   - no active maintenance window suppressing this asset
//   - INSERT must run
//
// which is what an alarm storm under healthy inventory looks like. The
// suppression path (a hit on probe 2) is faster, not slower, so this is
// the worst case for OpenTicket and the right thing to optimise.
//
// R10d note (PRMT-099 R10c 复核五轮 Finding R10c.D): every iteration
// must reach the INSERT branch for the measurement to be evidence of
// the 3-RTT→1-RTT optimisation. AssetPath is built from `i` (unique
// per iteration) rather than `i%32` (32 distinct values; after i≥32
// the existing-probe would hit and the bench would measure the
// existing-skip path instead, which was the R10c.D finding).
func BenchmarkOpenTicket_HotPath(b *testing.B) {
	st, _ := newBenchAlarmStore(b)
	ctx := context.Background()
	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := Event{
			RuleName:  "cdu-fws-deltat-low",
			AssetPath: fmt.Sprintf("sgp01.pod002.cdu%03d", i),
			Severity:  "major",
			Summary:   "bench alarm",
		}
		start := time.Now()
		if err := st.OpenTicket(ctx, ev); err != nil {
			b.Fatalf("OpenTicket %d: %v", i, err)
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
