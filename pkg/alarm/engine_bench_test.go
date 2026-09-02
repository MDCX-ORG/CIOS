// Package alarm — engine_bench_test.go: PRMT-099 K10 benchmark.
//
// Target: alarm.Observe per sample < 1ms AND steady-state zero
// allocations. Steady state = no transitions firing/resolved, so the
// engine walks the rule loop, looks up the instance, evaluates the
// expr, and returns nil — exactly the common production path.
//
// Scale: 10 rules × 100 assets. The snapshot map (all keys referenced
// by all rules) and `now` are built ONCE outside the inner loop; the
// loop body does only one Observe per iteration. We cycle through the
// 100 assets so the instance map is fully populated by the time the
// timed region starts, exercising the steady-state lookup path
// (no getOrCreate allocation).
package alarm

import (
	"testing"
	"time"
)

// makeBenchRules builds a small heterogeneous rule set that exercises
// the steady-state tick path: simple cmp, hysteresis cmp, and a
// multi-ref arithmetic rule. AppliesTo is "cdu" for all so a single
// snapshot satisfies every rule's key set (PRMT-099 R1 AppliesTo
// filter still applies — the bench assetType is "cdu").
func makeBenchRules(b *testing.B) []AlarmRule {
	b.Helper()
	// 10 rules: mix of single-cmp + multi-ref to reflect production shape.
	specs := []struct {
		name, expr string
		sev        string
		hys        float64
	}{
		{"r01-status-fault", "status == 3", "critical", 0},
		{"r02-deltat-low", "fws.deltat < 4", "minor", 0},
		{"r03-deltat-high", "fws.deltat > 10", "major", 0.5},
		{"r04-supply-flow", "fws.supply.flow > 100", "major", 0},
		{"r05-return-temp", "fws.return.temp > 35", "minor", 1},
		{"r06-sup-return", "fws.supply.temp - fws.return.temp > 5", "minor", 0},
		{"r07-leak-detect", "leak == 1", "critical", 0},
		{"r08-pressure-low", "fws.pressure < 2", "major", 0.2},
		{"r09-pump-power", "pump.power > 90", "info", 0},
		{"r10-cdu-load", "fws.supply.flow * fws.supply.temp > 1000", "info", 0},
	}
	out := make([]AlarmRule, len(specs))
	for i, s := range specs {
		e, err := ParseExpr(s.expr)
		if err != nil {
			b.Fatalf("ParseExpr(%q): %v", s.expr, err)
		}
		out[i] = AlarmRule{
			Kind: "AlarmRule",
			Metadata: ruleMetadata{
				Name:      s.name,
				AppliesTo: "cdu",
			},
			Spec: ruleSpec{
				Severity:    s.sev,
				Hysteresis:  s.hys,
				ForDuration: 0,
				Expr:        s.expr,
				Annotations: map[string]string{"summary": "bench: " + s.name},
			},
			Expr: e,
		}
	}
	return out
}

// BenchmarkObserveSteadyState measures per-Observe cost with the
// engine in steady state (no transitions). Allocations tracked via
// `-benchmem`. Snapshot map and `now` are built once outside b.N;
// each iteration does exactly one Observe call so per-op ns and
// allocs/op are directly comparable to the K10 target.
func BenchmarkObserveSteadyState(b *testing.B) {
	const numAssets = 100
	rules := makeBenchRules(b)
	eng := NewEngine(rules)

	// Snapshot values chosen so exprs are SATISFIED → engine fires on
	// the first tick per asset and dedups thereafter. Steady state
	// begins after the warm-up loop below.
	snap := map[string]float64{
		"status":          3,
		"fws.deltat":      3,
		"fws.supply.flow": 200,
		"fws.return.temp": 36,
		"fws.supply.temp": 30,
		"leak":            1,
		"fws.pressure":    1,
		"pump.power":      95,
	}
	now := time.Now()

	// Warm up: fire each asset once so the instances map is fully
	// populated. Without this, the first b.N iterations would include
	// getOrCreate map insertion (which would inflate allocs/op and
	// not represent the steady-state cost).
	for i := 0; i < numAssets; i++ {
		asset := assetPathForBench(i)
		_ = eng.Observe(asset, "cdu", snap, now)
	}

	// Pre-build assets slice outside the timed region.
	assets := make([]string, numAssets)
	for i := range assets {
		assets[i] = assetPathForBench(i)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eng.Observe(assets[i%numAssets], "cdu", snap, now)
	}
}

// assetPathForBench returns a deterministic cdu-shaped asset path for
// the bench. Kept simple so the steady-state path through buildPointPath
// (which concatenates assetPath + "." + first ref) stays representative
// of production-shape strings.
func assetPathForBench(i int) string {
	return "sgp01.pod002.cdu" + pad3(i)
}

// pad3 renders i as 3-digit zero-padded (000..099) for asset naming
// that matches the project cpath convention.
func pad3(i int) string {
	const digits = "0123456789"
	d2 := digits[(i/10)%10]
	d3 := digits[i%10]
	return string([]byte{digits[i/100], d2, d3})
}
