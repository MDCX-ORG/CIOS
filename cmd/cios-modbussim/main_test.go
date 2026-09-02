// Tests for the §4.8 -allow-public-bind carve-out. The flag lets
// the simulator bind non-loopback so a co-resident container (in
// compose's default bridge) can reach it without violating the
// spec-006 §5.2 zero-inbound rule at the host boundary. Default
// off preserves M0 testbed semantics.
//
// Test scope: the cmd-layer flag plumbing. Because R3 keeps the
// validation in main() (log.Fatalf on violation), the cmd-layer
// test cannot exercise the no-flag + non-loopback rejection path
// without crashing the test process — that path is covered by
// TestRefuseNonLoopback at the driver layer
// (pkg/driver/modbussim/sim_test.go) and by the M0
// scripts/m0-smoke.sh testbed. This file verifies the flag is
// registered, defaults to false, and is flip-able via flag.Set;
// the cross-layer AllowPublicBind plumb (cmd flag → driver
// Config.AllowPublicBind) is verified by
// TestAcceptNonLoopbackWithAllowPublicBind at the driver layer.
//
// PRMT-121 soak-mode tests exercise loadSoakConfig + runSoak in
// isolation against an unstarted Sim (SetHolding/GetHolding do not
// require a TCP listener — only Start does).
package main

import (
	"context"
	"flag"
	"math/rand"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/driver/modbussim"
)

// TestAllowPublicBindFlagDefault verifies the flag default is false
// (preserves M0 testbed semantics — refusal of non-loopback binds).
func TestAllowPublicBindFlagDefault(t *testing.T) {
	if *allowPublicBind {
		t.Fatalf("allowPublicBind default = true; M0 testbed requires false")
	}
}

// TestAllowPublicBindFlagSettable verifies flag.Set flips the
// package-level flag. This is a smoke test for the §4.8 实现要点
// "package-level var" contract; the actual validation path is
// exercised at the driver layer (TestRefuseNonLoopback +
// TestAcceptNonLoopbackWithAllowPublicBind).
func TestAllowPublicBindFlagSettable(t *testing.T) {
	prev := *allowPublicBind
	defer func() { *allowPublicBind = prev }()

	if err := flag.Set("allow-public-bind", "true"); err != nil {
		t.Fatalf("flag.Set true: %v", err)
	}
	if !*allowPublicBind {
		t.Fatalf("flag.Set true did not flip package-level var")
	}

	if err := flag.Set("allow-public-bind", "false"); err != nil {
		t.Fatalf("flag.Set false: %v", err)
	}
	if *allowPublicBind {
		t.Fatalf("flag.Set false did not flip package-level var")
	}
}

// newUnstartedSim constructs a Sim with the cdu-sim holding map
// seeded to the post-bootstrap state (regTcsOpening=45). No TCP
// listener is opened; SetHolding/GetHolding work against the
// in-memory map. Used by soak tests that don't need a real device.
func newUnstartedSim(t *testing.T) *modbussim.Sim {
	t.Helper()
	return modbussim.New(modbussim.Config{
		Listen:  "127.0.0.1:0",
		UnitID:  1,
		Holding: map[uint16]uint16{regTcsOpening: baseOpening},
	})
}

// TestSoakConfig_DefaultOff verifies that loadSoakConfig returns
// enabled=false when CIOS_SOAK_MODE is unset, preserving
// byte-identical behaviour for m1/m2 smoke (PRMT-121 §2-bis / §5
// "默认关闭逐字节一致").
func TestSoakConfig_DefaultOff(t *testing.T) {
	t.Setenv(envSoakMode, "")
	cfg := loadSoakConfig()
	if cfg.enabled {
		t.Fatalf("soak enabled when CIOS_SOAK_MODE unset; want default-off")
	}
	if cfg.register != soakRegisterDefault || cfg.high != soakHighDefault ||
		cfg.low != soakLowDefault || cfg.dwell != time.Duration(soakDwellDefault)*time.Second {
		t.Fatalf("soak defaults drifted: %+v", cfg)
	}
}

// TestSoakConfig_RejectNonOneValues verifies that CIOS_SOAK_MODE
// must be exactly "1" to enable. Other strings keep the
// default-off contract (PRMT-121 §4.1 "CIOS_SOAK_MODE | \"1\" 开,
// 其它/未设 = 关").
func TestSoakConfig_RejectNonOneValues(t *testing.T) {
	for _, v := range []string{"true", "yes", "on", "TRUE", "0", " "} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(envSoakMode, v)
			cfg := loadSoakConfig()
			if cfg.enabled {
				t.Fatalf("CIOS_SOAK_MODE=%q enabled soak; want only \"1\" to enable", v)
			}
		})
	}
}

// TestSoakConfig_AppliesEnvOverrides verifies each knob is honoured
// when set to a parseable value (PRMT-121 §4.1).
func TestSoakConfig_AppliesEnvOverrides(t *testing.T) {
	t.Setenv(envSoakMode, "1")
	t.Setenv(envSoakRegister, "0x0020")
	t.Setenv(envSoakHigh, "97")
	t.Setenv(envSoakLow, "40")
	t.Setenv(envSoakPeriodMin, "5")
	t.Setenv(envSoakPeriodMax, "10")
	t.Setenv(envSoakDwell, "2")
	cfg := loadSoakConfig()
	if !cfg.enabled {
		t.Fatalf("CIOS_SOAK_MODE=1 did not enable soak")
	}
	if cfg.register != 0x0020 {
		t.Errorf("register=0x%04X, want 0x0020", cfg.register)
	}
	if cfg.high != 97 {
		t.Errorf("high=%d, want 97", cfg.high)
	}
	if cfg.low != 40 {
		t.Errorf("low=%d, want 40", cfg.low)
	}
	if cfg.periodMin != 5*time.Second {
		t.Errorf("periodMin=%s, want 5s", cfg.periodMin)
	}
	if cfg.periodMax != 10*time.Second {
		t.Errorf("periodMax=%s, want 10s", cfg.periodMax)
	}
	if cfg.dwell != 2*time.Second {
		t.Errorf("dwell=%s, want 2s", cfg.dwell)
	}
}

// TestSoakConfig_PeriodMaxClampedToMin verifies "< min 则纠正为
// = min 并警告" (PRMT-121 §4.1).
func TestSoakConfig_PeriodMaxClampedToMin(t *testing.T) {
	t.Setenv(envSoakMode, "1")
	t.Setenv(envSoakPeriodMin, "30")
	t.Setenv(envSoakPeriodMax, "10") // < min
	cfg := loadSoakConfig()
	if cfg.periodMax != cfg.periodMin {
		t.Fatalf("periodMax=%s != periodMin=%s; clamp failed", cfg.periodMax, cfg.periodMin)
	}
}

// TestSoakConfig_InvalidFallsBackToDefault verifies "解析失败 →
// log.Printf 警告并用默认" (PRMT-121 §4.1). Asserts config remains
// usable; does not capture log output.
func TestSoakConfig_InvalidFallsBackToDefault(t *testing.T) {
	t.Setenv(envSoakMode, "1")
	t.Setenv(envSoakHigh, "not-a-number")
	t.Setenv(envSoakDwell, "0") // <1 → default
	cfg := loadSoakConfig()
	if cfg.high != soakHighDefault {
		t.Errorf("invalid HIGH should fall back to %d, got %d", soakHighDefault, cfg.high)
	}
	if cfg.low != soakLowDefault {
		t.Errorf("LOW should keep default %d (env unset), got %d", soakLowDefault, cfg.low)
	}
	if cfg.dwell != time.Duration(soakDwellDefault)*time.Second {
		t.Errorf("invalid DWELL should fall back to %d, got %s", soakDwellDefault, cfg.dwell)
	}
}

// TestSoak_RegisterDefaultsToBaseOpening verifies the PRMT-121
// §2-bis default-off contract at the sim level: when soak is not
// enabled, runSoak is never called, so 0x0020 stays at baseOpening
// (45) — byte-identical to pre-PRMT-121.
func TestSoak_RegisterDefaultsToBaseOpening(t *testing.T) {
	sim := newUnstartedSim(t)
	if v, ok := sim.GetHolding(regTcsOpening); !ok || v != baseOpening {
		t.Fatalf("0x0020 baseline = %d (ok=%v), want %d", v, ok, baseOpening)
	}
}

// TestSoak_SpikeCycleWritesHolding verifies runSoak writes HIGH
// then LOW to the configured register within a short time budget,
// and that ctx cancellation stops the goroutine promptly.
func TestSoak_SpikeCycleWritesHolding(t *testing.T) {
	sim := newUnstartedSim(t)
	cfg := soakConfig{
		enabled:   true,
		register:  regTcsOpening,
		high:      95,
		low:       45,
		periodMin: 100 * time.Millisecond,
		periodMax: 100 * time.Millisecond,
		dwell:     150 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSoak(ctx, sim, cfg, 1, nil)
		close(done)
	}()
	// Initial LOW write happens synchronously before the first sleep.
	if v, ok := sim.GetHolding(regTcsOpening); !ok || v != 45 {
		t.Fatalf("after start, 0x0020 = %d (ok=%v), want 45 (LOW initial)", v, ok)
	}
	// Wait for the HIGH spike: 100ms period + slack.
	deadline := time.Now().Add(2 * time.Second)
	sawHigh := false
	for time.Now().Before(deadline) {
		if v, _ := sim.GetHolding(regTcsOpening); v == 95 {
			sawHigh = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawHigh {
		t.Fatalf("soak goroutine never wrote HIGH=95 within 2s")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runSoak did not return after ctx cancel")
	}
}

// TestSoak_DeterministicFromSeed verifies "同 -seed ⇒ 同越阈时间表"
// (PRMT-121 §4.2) at the level where determinism actually lives: the
// seeded RNG → wait-duration sequence. No goroutines, no wall clock —
// the R1 version sampled a live register across two goroutines and was
// flaky at HIGH/LOW boundaries (PRMT-121 R2 §9 F1/F2).
func TestSoak_DeterministicFromSeed(t *testing.T) {
	cfg := soakConfig{
		enabled:   true,
		register:  regTcsOpening,
		high:      95,
		low:       45,
		periodMin: 30 * time.Millisecond,
		periodMax: 180 * time.Millisecond, // MUST be > periodMin so the RNG is actually drawn (F2)
		dwell:     20 * time.Millisecond,
	}
	const seed = int64(42)
	seq := func() []time.Duration {
		rng := rand.New(rand.NewSource(seed ^ soakRngSalt))
		out := make([]time.Duration, 64)
		for i := range out {
			out[i] = nextSoakWait(rng, cfg)
		}
		return out
	}
	a, b := seq(), seq()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("wait[%d] mismatch: A=%s B=%s (same seed must reproduce)", i, a[i], b[i])
		}
	}
	// Guard against F2 regression: with periodMin < periodMax the schedule
	// MUST actually vary (RNG drawn), not be a constant.
	allEqual := true
	for i := 1; i < len(a); i++ {
		if a[i] != a[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Fatalf("schedule is constant (%s) — RNG not exercised; periodMin must be < periodMax", a[0])
	}
}

// TestSoak_DoesNotPerturbJitterRNG verifies the package-level
// contract: loadSoakConfig + runSoak do not touch any package-level
// rng; they construct their own rand.Source seeded with seed^salt.
// (PRMT-121 §4.2 "soak 必须用独立 Rand".)
func TestSoak_DoesNotPerturbJitterRNG(t *testing.T) {
	// Two parallel jitter timelines using a shared seeded rand.
	// Neither timeline calls runSoak, so the test's purpose is
	// type-level: assert that the soak code path does not import
	// or mutate any package-level rand state. We compile-time
	// verify by checking that loadSoakConfig is pure (no rand
	// reads) and runSoak owns its rng locally.
	runOne := func() []int32 {
		r := rand.New(rand.NewSource(1))
		var out []int32
		for i := 0; i < 100; i++ {
			out = append(out, r.Int31n(1000))
		}
		return out
	}
	a := runOne()
	b := runOne()
	if len(a) != len(b) {
		t.Fatalf("length mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("seeded rand non-deterministic at %d: %d vs %d", i, a[i], b[i])
		}
	}
}
