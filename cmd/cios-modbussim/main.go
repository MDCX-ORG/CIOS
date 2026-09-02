// Command cios-modbussim runs a single-device Modbus TCP simulator pre-loaded
// with the cdu-sim point map (deploy/edge/pointmaps/cdu-sim.yaml) for the M0
// end-to-end demo. It is a stdlib-only wrapper around pkg/driver/modbussim
// that adds two behaviours required by the demo:
//
//  1. A every-2-second jitter loop that perturbs the supply.temp / return.temp
//     and supply.flow input registers by ±2% so Grafana time-series panels
//     show motion instead of a flat line.
//  2. A SIGINT/SIGTERM-driven graceful Stop via signal.NotifyContext.
//
// Wiring outside this command (the gateway + core + CLI demo) is intentionally
// out of scope; this binary is a process-level testbed, not a plant device.
//
// PRMT-121 (soak mode, default off): when CIOS_SOAK_MODE=1, an additional
// goroutine drives regTcsOpening (0x0020, tcs.opening) between LOW and HIGH
// on a seeded-random cadence so the §M2-1 soak harness can exercise
// firing→open→close cycles repeatedly. The gate is env-only — default-off
// means byte-identical behaviour for m1/m2 smoke.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yurimeng/cios/pkg/driver/modbussim"
)

// Register addresses and baseline values MUST match
// deploy/edge/pointmaps/cdu-sim.yaml (PRMT-010). The gateway's pointmap
// loader relies on the same numbers.
const (
	regSupplyTemp = 0x0010 // decicelsius
	regReturnTemp = 0x0011 // decicelsius
	regSupplyFlow = 0x0012 // 0.1 m3ph (raw value 1200 → 120.0 m3/h)
	regTcsOpening = 0x0020 // holding, 0-100
	regStatus     = 0x0030 // enum_map vendor code
	regLeak       = 0x0031 // enum 0/1

	baseSupplyTemp = 235 // 23.5 °C
	baseReturnTemp = 198 // 19.8 °C
	baseSupplyFlow = 1200
	baseOpening    = 45
	baseStatus     = 1
	baseLeak       = 0

	jitterPeriod = 2 * time.Second
	jitterPct    = 0.02 // ±2%
)

// soakConfig holds the PRMT-121 soak-mode knobs. All fields have
// sane defaults and are overridable via env (see loadSoakConfig).
// Default-on = false; sim behaves byte-identically to pre-PRMT-121
// unless CIOS_SOAK_MODE=1 is set.
type soakConfig struct {
	enabled   bool
	register  uint16
	high      uint16
	low       uint16
	periodMin time.Duration
	periodMax time.Duration
	dwell     time.Duration
}

// Soak mode env knobs (PRMT-121 §4.1). Defaults match the prompt:
// register=0x0020 (tcs.opening), HIGH=95 (> rule threshold 90),
// LOW=45 (= baseOpening), period [60s, 180s], dwell=15s.
const (
	envSoakMode      = "CIOS_SOAK_MODE"
	envSoakRegister  = "CIOS_SOAK_REGISTER"
	envSoakHigh      = "CIOS_SOAK_HIGH"
	envSoakLow       = "CIOS_SOAK_LOW"
	envSoakPeriodMin = "CIOS_SOAK_PERIOD_MIN_S"
	envSoakPeriodMax = "CIOS_SOAK_PERIOD_MAX_S"
	envSoakDwell     = "CIOS_SOAK_DWELL_S"

	soakRegisterDefault  = uint16(0x0020)
	soakHighDefault      = uint16(95)
	soakLowDefault       = uint16(45)
	soakPeriodMinDefault = 60
	soakPeriodMaxDefault = 180
	soakDwellDefault     = 15
)

// soakRngSalt is XOR'd with the user's -seed flag when seeding the
// soak goroutine's RNG, so the soak timeline is reproducible from
// -seed but does not perturb the jitter RNG sequence (PRMT-121 §4.2).
const soakRngSalt int64 = 0x50414B

// loadSoakConfig reads the seven env vars, applying defaults on
// missing or invalid values. Parse failures log a warning and fall
// back to default — never fatal (PRMT-121 §4.1 "解析失败 → log.Printf
// 警告并用默认，不得 fatal"). Caller should gate on cfg.enabled.
func loadSoakConfig() soakConfig {
	cfg := soakConfig{
		register:  soakRegisterDefault,
		high:      soakHighDefault,
		low:       soakLowDefault,
		periodMin: time.Duration(soakPeriodMinDefault) * time.Second,
		periodMax: time.Duration(soakPeriodMaxDefault) * time.Second,
		dwell:     time.Duration(soakDwellDefault) * time.Second,
	}
	if v, ok := os.LookupEnv(envSoakMode); ok && v == "1" {
		cfg.enabled = true
	} else {
		// Default off: any value other than "1" (or unset) disables
		// the soak goroutine entirely — byte-identical behaviour.
		return cfg
	}
	if v, ok := os.LookupEnv(envSoakRegister); ok {
		if n, err := strconv.ParseUint(v, 0, 16); err == nil {
			cfg.register = uint16(n)
		} else {
			log.Printf("soak: %s=%q invalid (%v); using default 0x%04X", envSoakRegister, v, err, cfg.register)
		}
	}
	if v, ok := os.LookupEnv(envSoakHigh); ok {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.high = uint16(n)
		} else {
			log.Printf("soak: %s=%q invalid (%v); using default %d", envSoakHigh, v, err, cfg.high)
		}
	}
	if v, ok := os.LookupEnv(envSoakLow); ok {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.low = uint16(n)
		} else {
			log.Printf("soak: %s=%q invalid (%v); using default %d", envSoakLow, v, err, cfg.low)
		}
	}
	periodMin := soakPeriodMinDefault
	if v, ok := os.LookupEnv(envSoakPeriodMin); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			periodMin = n
		} else {
			log.Printf("soak: %s=%q invalid (<1 or non-int); using default %d", envSoakPeriodMin, v, periodMin)
		}
	}
	periodMax := soakPeriodMaxDefault
	if v, ok := os.LookupEnv(envSoakPeriodMax); ok {
		if n, err := strconv.Atoi(v); err == nil {
			periodMax = n
		} else {
			log.Printf("soak: %s=%q invalid (non-int); using default %d", envSoakPeriodMax, v, periodMax)
		}
	}
	// Enforce periodMax >= periodMin: PRMT-121 §4.1 "< min 则纠正为 = min 并警告".
	if periodMax < periodMin {
		log.Printf("soak: %s=%d < %s=%d; clamping max to min", envSoakPeriodMax, periodMax, envSoakPeriodMin, periodMin)
		periodMax = periodMin
	}
	cfg.periodMin = time.Duration(periodMin) * time.Second
	cfg.periodMax = time.Duration(periodMax) * time.Second
	if v, ok := os.LookupEnv(envSoakDwell); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			cfg.dwell = time.Duration(n) * time.Second
		} else {
			log.Printf("soak: %s=%q invalid (<1 or non-int); using default %ds", envSoakDwell, v, soakDwellDefault)
		}
	}
	// Sanity: HIGH must exceed the rule threshold (90) for firing;
	// LOW must stay at or below baseOpening (45) so the rule recovers.
	if cfg.high <= 90 {
		log.Printf("soak: HIGH=%d does not exceed rule threshold 90; rule will never fire", cfg.high)
	}
	if cfg.low > 45 {
		log.Printf("soak: LOW=%d exceeds baseOpening 45; rule may not recover", cfg.low)
	}
	return cfg
}

// nextSoakWait returns the next inter-spike wait. Pure: depends only on
// rng + cfg, no clock/goroutine state. Extracted from runSoak so seed
// determinism is unit-testable without sampling a live register across
// goroutines (PRMT-121 R2; cadence value/behaviour unchanged).
func nextSoakWait(rng *rand.Rand, cfg soakConfig) time.Duration {
	span := int64(cfg.periodMax - cfg.periodMin)
	if span <= 0 {
		return cfg.periodMin
	}
	return cfg.periodMin + time.Duration(rng.Int63n(span+1))
}

// runSoak drives cfg.register between cfg.low and cfg.high on a
// seeded-random cadence, respecting ctx for clean shutdown. Uses
// its own RNG (seed ^ soakRngSalt) so the jitter RNG sequence is
// not perturbed (PRMT-121 §4.2).
func runSoak(ctx context.Context, sim *modbussim.Sim, cfg soakConfig, seed int64, logSink *log.Logger) {
	if logSink == nil {
		logSink = log.Default()
	}
	rng := rand.New(rand.NewSource(seed ^ soakRngSalt))
	// Start at LOW so the register reflects its baseOpening baseline
	// before the first spike (PRMT-121 §4.2 "起始: sim.SetHolding(REGISTER, LOW)").
	sim.SetHolding(cfg.register, cfg.low)
	logSink.Printf("soak: enabled register=0x%04X low=%d high=%d period=[%s,%s] dwell=%s seed=%d",
		cfg.register, cfg.low, cfg.high, cfg.periodMin, cfg.periodMax, cfg.dwell, seed)
	for {
		// Random wait in [periodMin, periodMax].
		wait := nextSoakWait(rng, cfg)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		sim.SetHolding(cfg.register, cfg.high)
		select {
		case <-ctx.Done():
			// Leave register HIGH on shutdown — baseOpening is the
			// initial state, not a contract for post-shutdown reads.
			return
		case <-time.After(cfg.dwell):
		}
		sim.SetHolding(cfg.register, cfg.low)
	}
}

// allowPublicBind lifts the loopback-only guard in main() so the
// simulator can serve a co-resident process in a compose network
// (e.g. cios-gateway reaching cios-modbussim across the bridge).
// Default false preserves M0 / testbed semantics. Package-level so
// main_test.go can flip it without mutating flag.CommandLine.
var allowPublicBind = flag.Bool("allow-public-bind", false,
	"allow -listen on non-loopback addresses (compose/cross-container; default off keeps M0 testbed semantics)")

func main() {
	listen := flag.String("listen", "127.0.0.1:15020", "Modbus TCP listen address (loopback only)")
	unit := flag.Uint("unit", 1, "Modbus unit id echoed in responses")
	seed := flag.Int64("seed", 1, "RNG seed for jitter (deterministic by default)")
	flag.Parse()

	host, port, err := splitHostPort(*listen)
	if err != nil {
		log.Fatalf("cios-modbussim: parse -listen: %v", err)
	}
	bind := joinHostPort(host, port)
	if !*allowPublicBind {
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			log.Fatalf("cios-modbussim: refuse non-loopback host %q (testbed only)", host)
		}
	}

	holding := map[uint16]uint16{
		regTcsOpening: baseOpening,
	}
	input := map[uint16]uint16{
		regSupplyTemp: baseSupplyTemp,
		regReturnTemp: baseReturnTemp,
		regSupplyFlow: baseSupplyFlow,
		regStatus:     baseStatus,
		regLeak:       baseLeak,
	}

	sim := modbussim.New(modbussim.Config{
		Listen:          bind,
		UnitID:          byte(*unit),
		Holding:         holding,
		Input:           input,
		AllowPublicBind: *allowPublicBind,
	})

	addr, err := sim.Start()
	if err != nil {
		log.Fatalf("cios-modbussim: start: %v", err)
	}
	prefix := ""
	if *allowPublicBind && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		prefix = "non-loopback bind enabled (-allow-public-bind); "
	}
	fmt.Fprintf(os.Stdout, "cios-modbussim: listening on %s%s (unit=%d)\n", prefix, addr, *unit)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rng := rand.New(rand.NewSource(*seed))

	// PRMT-121 soak goroutine (default off). Runs in parallel with
	// the jitter loop and shares ctx for clean shutdown.
	soakCfg := loadSoakConfig()
	if soakCfg.enabled {
		go runSoak(ctx, sim, soakCfg, *seed, log.Default())
	}

	// Jitter loop: writes back to the input map every 2s. The jitter is
	// multiplicative, clamped to a sane range so we never produce an
	// out-of-band reading.
	tick := time.NewTicker(jitterPeriod)
	defer tick.Stop()

	jitter := func(base uint16) uint16 {
		span := int32(float64(base) * jitterPct)
		if span < 1 {
			span = 1
		}
		delta := rng.Int31n(2*span+1) - span // [-span, +span]
		v := int32(base) + delta
		if v < 0 {
			v = 0
		}
		if v > 0xFFFF {
			v = 0xFFFF
		}
		return uint16(v)
	}

	for {
		select {
		case <-ctx.Done():
			sim.Stop()
			fmt.Fprintln(os.Stdout, "cios-modbussim: stopped")
			return
		case <-tick.C:
			sim.SetInput(regSupplyTemp, jitter(baseSupplyTemp))
			sim.SetInput(regReturnTemp, jitter(baseReturnTemp))
			sim.SetInput(regSupplyFlow, jitter(baseSupplyFlow))
		}
	}
}

// splitHostPort / joinHostPort are tiny wrappers kept local so this command
// can stay stdlib-only without pulling in net.SplitHostPort's error semantics
// into log.Fatalf callers (we want a single error string).
func splitHostPort(s string) (host, port string, err error) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("missing port in %q", s)
}

func joinHostPort(host, port string) string {
	return host + ":" + port
}
