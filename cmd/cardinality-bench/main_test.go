package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSeries_TargetSeriesCountHonored(t *testing.T) {
	// Cardinality-budget.yaml yields ~8 distinct type keys when
	// parseable; the harness slices to <=8, so buildSeries is
	// exercised with 4 types here for arithmetic clarity.
	types := []string{"gpu", "node", "cell", "rack"}
	tenants := []string{"t0", "t1"}
	for _, target := range []int{100, 1000, 10000, 30000} {
		got := activeSeriesFor(target, tenants, types)
		// We require EXACTLY target (rounding rule below absorbs
		// the last-type remainder into the level total).
		if got != target {
			t.Fatalf("target=%d: got %d series (off by %d)", target, got, got-target)
		}
		// Also assert the text-exposition stream is well-formed:
		// one non-empty line per series, every line carries tenant.
		b := buildSeries(target, tenants, types)
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) != got {
			t.Fatalf("target=%d: payload has %d lines, expected %d", target, len(lines), got)
		}
		for i, ln := range lines {
			if !strings.Contains(ln, `tenant="t`) {
				t.Fatalf("target=%d line %d missing tenant label: %q", target, i, ln)
			}
		}
	}
}

func TestBuildSeries_RequiresAtLeastOneTenant(t *testing.T) {
	if b := buildSeries(100, nil, []string{"gpu"}); b != nil {
		t.Fatalf("nil tenants should yield nil, got %d bytes", len(b))
	}
}

func TestBuildSeries_VocabularyHonored(t *testing.T) {
	// PRMT-183 §4: metric stems use cardinality-budget type keys;
	// no invented metric names outside the protocol.
	allowedTypes := map[string]bool{"gpu": true, "node": true}
	b := buildSeries(20, []string{"t0"}, []string{"gpu", "node"})
	s := string(b)
	if !strings.Contains(s, `cios_gpu_point`) {
		t.Fatalf("expected cios_gpu_point stem; payload: %q", s)
	}
	if !strings.Contains(s, `cios_node_point`) {
		t.Fatalf("expected cios_node_point stem; payload: %q", s)
	}
	if strings.Contains(s, "cios_pump_point") {
		t.Fatalf("forbidden stem present: %q", s)
	}
	_ = allowedTypes
}

func TestPercentile_KnownValues(t *testing.T) {
	// 1..100 inclusive -> p50 = 50.5 (interpolated), p95 ≈ 95.05,
	// p99 ≈ 99.01. We assert within ±2 to keep the test robust to
	// the linear-interpolation rule (PRMT-183 §4 says
	// "linear interpolation between adjacent ranks").
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Microsecond
	}
	p50 := percentile(samples, 50)
	p95 := percentile(samples, 95)
	p99 := percentile(samples, 99)
	if p50 < 50*time.Microsecond || p50 > 51*time.Microsecond {
		t.Fatalf("p50 out of band: %s", p50)
	}
	if p95 < 94*time.Microsecond || p95 > 96*time.Microsecond {
		t.Fatalf("p95 out of band: %s", p95)
	}
	if p99 < 98*time.Microsecond || p99 > 100*time.Microsecond {
		t.Fatalf("p99 out of band: %s", p99)
	}
}

func TestPercentile_Empty(t *testing.T) {
	if d := percentile(nil, 50); d != 0 {
		t.Fatalf("empty percentile should be 0, got %s", d)
	}
}

func TestRecommendThreshold_PicksSmallestCrossing(t *testing.T) {
	// Baseline p95 = 10ms; degrade-factor = 3.0 → limit = 30ms.
	per := map[int]time.Duration{
		10000:  10 * time.Millisecond, // baseline
		30000:  20 * time.Millisecond, // under
		100000: 35 * time.Millisecond, // crosses → smallest crossing
		300000: 80 * time.Millisecond,
	}
	got := recommendThreshold(per, 10*time.Millisecond, 3.0)
	if got != 100000 {
		t.Fatalf("expected 100000, got %d", got)
	}
}

func TestRecommendThreshold_NoCrossingReturnsZero(t *testing.T) {
	per := map[int]time.Duration{
		10000: 10 * time.Millisecond,
		30000: 15 * time.Millisecond,
	}
	if got := recommendThreshold(per, 10*time.Millisecond, 3.0); got != 0 {
		t.Fatalf("expected 0 (no crossing), got %d", got)
	}
}
