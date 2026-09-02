package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/modbussim"
)

// --- helpers ---------------------------------------------------------------

// startSim spins up a modbussim on a kernel-allocated port and returns
// it along with the bound address. The caller is responsible for
// Stop()'ing it.
func startSim(t *testing.T, cfg modbussim.Config) (*modbussim.Sim, string) {
	t.Helper()
	sim := modbussim.New(cfg)
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("sim.Start: %v", err)
	}
	return sim, addr
}

// freeLoopbackPort grabs and immediately releases a free 127.0.0.1
// port so a sim can be (re)bound to it deterministically. Used by C4
// where two sim instances must share an address across a restart.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// stdBindings is the 8-point mixed holding/input table used by C2/C3.
// Eight is the minimum that exercises both tables and lets C3 mask one
// while still proving "the rest = 7 good".
func stdBindings() []Binding {
	return []Binding{
		{Point: "site01.pod000.cdu000.fws.supply.temp", Table: "holding", Register: 0x0010},
		{Point: "site01.pod000.cdu000.fws.return.temp", Table: "holding", Register: 0x0011},
		{Point: "site01.pod000.cdu000.fws.supply.flow", Table: "holding", Register: 0x0012},
		{Point: "site01.pod000.cdu000.fws.return.flow", Table: "holding", Register: 0x0013},
		{Point: "site01.pod000.cdu000.tcs.opening", Table: "holding", Register: 0x0020, Writable: true},
		{Point: "site01.pod000.cdu000.fws.supply.pressure", Table: "input", Register: 0x0030},
		{Point: "site01.pod000.cdu000.fws.return.pressure", Table: "input", Register: 0x0031},
		{Point: "site01.pod000.cdu000.fws.supply.density", Table: "input", Register: 0x0032},
	}
}

// stdSimConfig seeds modbussim so every binding in stdBindings has a
// known, distinct value. Keeping the mapping inline (point→register→
// value) makes the assertions in C2 trivially readable.
func stdSimConfig() modbussim.Config {
	return modbussim.Config{
		Holding: map[uint16]uint16{
			0x0010: 11,
			0x0011: 22,
			0x0012: 33,
			0x0013: 44,
			0x0020: 55,
		},
		Input: map[uint16]uint16{
			0x0030: 66,
			0x0031: 77,
			0x0032: 88,
		},
	}
}

// expectedValues mirrors stdSimConfig in point-name space: the
// (Point → uint16 register value) lookup C2 / C3 use to assert each
// Sample's Value.
func expectedValues() map[string]uint16 {
	return map[string]uint16{
		"site01.pod000.cdu000.fws.supply.temp":     11,
		"site01.pod000.cdu000.fws.return.temp":     22,
		"site01.pod000.cdu000.fws.supply.flow":     33,
		"site01.pod000.cdu000.fws.return.flow":     44,
		"site01.pod000.cdu000.tcs.opening":         55,
		"site01.pod000.cdu000.fws.supply.pressure": 66,
		"site01.pod000.cdu000.fws.return.pressure": 77,
		"site01.pod000.cdu000.fws.supply.density":  88,
	}
}

// driverShutdown closes the driver's underlying socket. We schedule
// it as a Cleanup hook BEFORE sim.Stop to dodge a latent
// modbussim race: Sim.Stop's "Range and close all live conns" can
// miss a connection that was Store'd by acceptLoop just after Range
// completed, leaving the handle goroutine blocked in readFull and
// Sim.Stop blocked on wg.Wait. Telling the driver to close its end
// first guarantees EOF on the sim side regardless of that race.
// modbussim itself is read-only in PRMT-009; this is the test-side
// fix. The connection-loss happens before sim.Stop returns, which
// is consistent with the driver's fail-soft contract.
func driverShutdown(drv *Driver) {
	drv.mu.Lock()
	drv.closeConn()
	drv.mu.Unlock()
}

// --- C1 ---------------------------------------------------------------------

// TestConformanceC1 covers Init's three branches:
//
//   - happy path against a live sim: no error, Health.Connected=true
//   - dial failure: error returned, the driver still holds endpoint
//     state, Health.Connected=false
//   - binding validation: input+Writable=true is rejected BEFORE any
//     dial is attempted (verified by pointing at a closed port and
//     observing no network error in the chain)
func TestConformanceC1(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		sim, addr := startSim(t, stdSimConfig())
		defer sim.Stop()
		drv := New(stdBindings())
		defer driverShutdown(drv)
		if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		h := drv.Health(ctx)
		if !h.Connected {
			t.Errorf("Connected=false after Init, want true")
		}
	})

	t.Run("dial fails on closed port", func(t *testing.T) {
		// Pin a port, free it, dial it — guaranteed nobody is
		// listening. (Race: another process could grab it. The
		// 127.0.0.1-only constraint of the simulator and the
		// short-lived ephemeral range make this acceptable in
		// practice; if it ever flakes we can retry-loop.)
		addr := freeLoopbackPort(t)
		drv := New(stdBindings())
		err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr})
		if err == nil {
			t.Fatalf("Init: expected dial error, got nil")
		}
		if !strings.Contains(err.Error(), "dial") {
			t.Errorf("Init error = %v, want dial-level error", err)
		}
		h := drv.Health(ctx)
		if h.Connected {
			t.Errorf("Connected=true after failed dial, want false")
		}
	})

	t.Run("input+writable rejected without dial", func(t *testing.T) {
		// Endpoint is unreachable. The point of the test: validation
		// short-circuits before any I/O, so the error is the binding
		// error, NOT a dial error.
		addr := freeLoopbackPort(t)
		drv := New([]Binding{
			{Point: "p1", Table: "input", Register: 1, Writable: true},
		})
		err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr})
		if err == nil {
			t.Fatalf("Init: expected binding error, got nil")
		}
		if !errors.Is(err, errInvalidBinding) {
			t.Errorf("Init error = %v, want errInvalidBinding", err)
		}
		if strings.Contains(err.Error(), "dial") {
			t.Errorf("Init error contains 'dial' (%v) — validation should run before dial", err)
		}
	})
}

// --- C2 ---------------------------------------------------------------------

// TestConformanceC2 reads all 8 bindings against a clean sim and
// asserts: count, per-point value, per-point Quality=good, ordering
// matches the binding-table order. Ordering matters because the
// gateway pipes samples to VictoriaMetrics tag-by-point, but having
// the driver preserve binding order also makes humans' lives easier.
func TestConformanceC2(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()

	bindings := stdBindings()
	drv := New(bindings)
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	samples, err := drv.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != len(bindings) {
		t.Fatalf("len(samples) = %d, want %d", len(samples), len(bindings))
	}
	want := expectedValues()
	for i, s := range samples {
		if s.Point != bindings[i].Point {
			t.Errorf("samples[%d].Point = %q, want %q (order must match binding order)", i, s.Point, bindings[i].Point)
		}
		if s.Quality != driver.QualityGood {
			t.Errorf("samples[%d] %q Quality = %q, want good", i, s.Point, s.Quality)
		}
		if got := uint16(s.Value); got != want[s.Point] {
			t.Errorf("samples[%d] %q Value = %d, want %d", i, s.Point, got, want[s.Point])
		}
	}
	h := drv.Health(ctx)
	if h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess zero after fully-good Collect")
	}
	if h.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d after clean Collect, want 0", h.ErrorCount)
	}
}

// --- C3 ---------------------------------------------------------------------

// TestConformanceC3 masks one register at the sim and asserts: the
// masked point is suspect, the other seven are good, Collect returns
// no error. This is the fail-soft contract: device-level trouble
// degrades the point's quality, never the whole round.
func TestConformanceC3(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()

	// Mask the first holding register (0x0010 → supply.temp).
	const maskedReg = 0x0010
	maskedPoint := "site01.pod000.cdu000.fws.supply.temp"
	sim.Mask(maskedReg)

	drv := New(stdBindings())
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	samples, err := drv.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect returned error: %v (must be nil; device failures go to Quality)", err)
	}
	if len(samples) != 8 {
		t.Fatalf("len(samples) = %d, want 8", len(samples))
	}
	good, suspect := 0, 0
	for _, s := range samples {
		switch s.Quality {
		case driver.QualityGood:
			good++
		case driver.QualitySuspect:
			suspect++
			if s.Point != maskedPoint {
				t.Errorf("unexpected suspect point %q, only %q should be masked", s.Point, maskedPoint)
			}
		default:
			t.Errorf("unexpected Quality %q on %q", s.Quality, s.Point)
		}
	}
	if good != 7 || suspect != 1 {
		t.Errorf("good=%d suspect=%d, want good=7 suspect=1", good, suspect)
	}
	// A partially-suspect round must NOT advance LastSuccess.
	h := drv.Health(ctx)
	if !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess set after partial-suspect round, want zero")
	}
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount = 0 after masked point, want >0")
	}
}

// --- C4 ---------------------------------------------------------------------

// TestConformanceC4 verifies survive-the-disconnect behaviour:
//
//  1. Init against sim1, do one good Collect
//  2. Stop sim1 → Collect during outage returns all-suspect, no
//     error, Health.Connected=false
//  3. Start sim2 at the SAME address → next Collect recovers without
//     a fresh Init; samples back to all-good; Health.Connected=true
//
// The "same address" requirement is what makes this non-trivial; we
// reserve a port up front and hand it to both sim instances.
func TestConformanceC4(t *testing.T) {
	ctx := context.Background()
	addr := freeLoopbackPort(t)

	sim1 := modbussim.New(modbussim.Config{
		Listen:  addr,
		Holding: stdSimConfig().Holding,
		Input:   stdSimConfig().Input,
	})
	if _, err := sim1.Start(); err != nil {
		t.Fatalf("sim1.Start: %v", err)
	}

	drv := New(stdBindings())
	defer driverShutdown(drv) // LIFO: runs before sim2.Stop below
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		driverShutdown(drv)
		sim1.Stop()
		t.Fatalf("Init: %v", err)
	}

	// Phase 1: clean collect.
	samples, err := drv.Collect(ctx)
	if err != nil || !allGood(samples) {
		driverShutdown(drv)
		sim1.Stop()
		t.Fatalf("phase1 Collect: err=%v allGood=%v", err, allGood(samples))
	}

	// Phase 2: stop the sim. We want the driver to see the dead
	// connection during the next round. driverShutdown first
	// guarantees sim1.Stop never blocks on a connection acceptLoop
	// Store'd just after Sim.Stop's Range() ran (modbussim race;
	// driver-side workaround per PRMT-009 file-whitelist).
	driverShutdown(drv)
	sim1.Stop()

	// Give the OS a moment to fully release the port. macOS in
	// particular can hold it briefly; without this the sim2 listen
	// occasionally races.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ln, lerr := net.Listen("tcp", addr)
		if lerr == nil {
			_ = ln.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	samples, err = drv.Collect(ctx)
	if err != nil {
		t.Fatalf("phase2 Collect returned error: %v (must be nil even when offline)", err)
	}
	for _, s := range samples {
		if s.Quality != driver.QualitySuspect {
			t.Errorf("phase2 %q Quality=%q, want suspect (sim is down)", s.Point, s.Quality)
		}
	}
	if h := drv.Health(ctx); h.Connected {
		t.Errorf("phase2 Health.Connected=true, want false")
	}

	// Phase 3: bring up sim2 on the same port. The driver must
	// reconnect on its own — no Re-Init.
	sim2 := modbussim.New(modbussim.Config{
		Listen:  addr,
		Holding: stdSimConfig().Holding,
		Input:   stdSimConfig().Input,
	})
	if _, err := sim2.Start(); err != nil {
		t.Fatalf("sim2.Start at %s: %v", addr, err)
	}
	defer sim2.Stop()

	samples, err = drv.Collect(ctx)
	if err != nil {
		t.Fatalf("phase3 Collect: %v", err)
	}
	if !allGood(samples) {
		t.Errorf("phase3 samples not all-good after recovery: %s", qualitySummary(samples))
	}
	if h := drv.Health(ctx); !h.Connected {
		t.Errorf("phase3 Health.Connected=false, want true")
	}
}

func allGood(samples []driver.Sample) bool {
	for _, s := range samples {
		if s.Quality != driver.QualityGood {
			return false
		}
	}
	return true
}

func qualitySummary(samples []driver.Sample) string {
	counts := map[driver.Quality]int{}
	for _, s := range samples {
		counts[s.Quality]++
	}
	return fmt.Sprintf("%v", counts)
}

// --- C6 ---------------------------------------------------------------------

// TestConformanceC6 covers Write across its four contract branches:
// happy, expired TTL, read-only point, and post-write Collect sees
// the new value. The fourth one closes the loop: spec-002 §8 says Set
// is judged successful only when telemetry confirms — the driver
// must make that confirmation possible.
func TestConformanceC6(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()

	drv := New(stdBindings())
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	writable := "site01.pod000.cdu000.tcs.opening"
	readonly := "site01.pod000.cdu000.fws.supply.pressure"

	t.Run("happy path", func(t *testing.T) {
		res, err := drv.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 45, TTL: time.Second, RequestID: "r1",
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !res.Accepted {
			t.Errorf("Accepted=false, want true")
		}
		if res.Readback != 45 {
			t.Errorf("Readback = %v, want 45", res.Readback)
		}
		if res.ReadbackTs.IsZero() {
			t.Errorf("ReadbackTs zero")
		}
	})

	t.Run("ttl expired", func(t *testing.T) {
		_, err := drv.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 99, TTL: 0,
		})
		if !errors.Is(err, ErrExpired) {
			t.Errorf("err = %v, want ErrExpired", err)
		}
		// Negative TTL is the same class.
		_, err = drv.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 99, TTL: -time.Second,
		})
		if !errors.Is(err, ErrExpired) {
			t.Errorf("negative TTL err = %v, want ErrExpired", err)
		}
	})

	t.Run("readonly point", func(t *testing.T) {
		_, err := drv.Write(ctx, driver.ControlCommand{
			Point: readonly, Value: 1, TTL: time.Second,
		})
		if !errors.Is(err, ErrNotWritable) {
			t.Errorf("err = %v, want ErrNotWritable", err)
		}
	})

	t.Run("unknown point", func(t *testing.T) {
		_, err := drv.Write(ctx, driver.ControlCommand{
			Point: "site01.pod000.cdu000.does.not.exist", Value: 1, TTL: time.Second,
		})
		if !errors.Is(err, ErrNotWritable) {
			t.Errorf("unknown-point err = %v, want ErrNotWritable", err)
		}
	})

	t.Run("post-write Collect sees new value", func(t *testing.T) {
		// Write a distinctive value, then poll the whole table and
		// confirm the writable point reads back the new number.
		const newVal = 77
		if _, err := drv.Write(ctx, driver.ControlCommand{
			Point: writable, Value: newVal, TTL: time.Second,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		samples, err := drv.Collect(ctx)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for _, s := range samples {
			if s.Point == writable {
				if uint16(s.Value) != newVal {
					t.Errorf("post-Write Collect %q = %v, want %d", s.Point, s.Value, newVal)
				}
				return
			}
		}
		t.Errorf("writable point %q not in Collect samples", writable)
	})
}

// --- Supplementary unit tests (MUST §5 line 4) ----------------------------

// TestTxIDWrapAround forces the internal counter near uint16 max so a
// burst of Collect calls crosses the 0xFFFF → 0x0000 boundary, and
// confirms there is no off-by-one or mismatch panic. We can't read
// the counter back, but we can do a few thousand requests and check
// they all succeed.
func TestTxIDWrapAround(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, modbussim.Config{
		Holding: map[uint16]uint16{0x0001: 0xCAFE},
	})
	defer sim.Stop()

	drv := New([]Binding{{Point: "p1", Table: "holding", Register: 0x0001}})
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Push the internal counter to a value that guarantees the wrap
	// happens within the loop budget.
	drv.mu.Lock()
	drv.txID = 0xFFF0
	drv.mu.Unlock()

	const rounds = 80 // straddles 0xFFFF → 0x0000
	for i := 0; i < rounds; i++ {
		samples, err := drv.Collect(ctx)
		if err != nil {
			t.Fatalf("Collect[%d]: %v", i, err)
		}
		if len(samples) != 1 || samples[0].Quality != driver.QualityGood {
			t.Fatalf("Collect[%d] samples=%+v", i, samples)
		}
	}

	// And the harder requirement from §5: ≥70000 requests still
	// correct. Reset to mid-range and run a bigger loop with multiple
	// bindings to keep wall-clock reasonable.
	drv.mu.Lock()
	drv.txID = 0
	drv.mu.Unlock()
	big := make([]Binding, 10)
	for i := range big {
		big[i] = Binding{Point: fmt.Sprintf("p%d", i), Table: "holding", Register: 0x0001}
	}
	drv2 := New(big)
	defer driverShutdown(drv2)
	if err := drv2.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init drv2: %v", err)
	}
	// 8k rounds × 10 bindings = 80k requests, comfortably past 70k.
	for i := 0; i < 8000; i++ {
		samples, err := drv2.Collect(ctx)
		if err != nil {
			t.Fatalf("Collect drv2[%d]: %v", i, err)
		}
		for _, s := range samples {
			if s.Quality != driver.QualityGood {
				t.Fatalf("drv2 round %d: %+v", i, s)
			}
		}
	}
}

// TestInitInvalidUnitID asserts non-numeric unit_id values surface
// as Init errors. The driver must not silently coerce or default —
// the gateway is sending us configuration and a typo should be loud.
func TestInitInvalidUnitID(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	drv := New(stdBindings())
	defer driverShutdown(drv)
	err := drv.Init(context.Background(), driver.DriverConfig{
		Endpoint: addr,
		Options:  map[string]string{"unit_id": "abc"},
	})
	if err == nil {
		t.Fatalf("Init with unit_id=abc: expected error")
	}
	if !strings.Contains(err.Error(), "unit_id") {
		t.Errorf("error %q does not mention unit_id", err)
	}
}

// TestInitEmptyBindings is the other Init failure path called out
// in §5: an empty binding table is a configuration bug, not an
// "I have nothing to poll" success.
func TestInitEmptyBindings(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	drv := New(nil)
	defer driverShutdown(drv)
	err := drv.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init nil bindings: err=%v, want errInvalidBinding", err)
	}
	drv2 := New([]Binding{})
	defer driverShutdown(drv2)
	if err := drv2.Init(context.Background(), driver.DriverConfig{Endpoint: addr}); !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init empty bindings: err=%v, want errInvalidBinding", err)
	}
}

// TestInitDuplicatePoint catches a config-mistake that would otherwise
// silently corrupt Write lookups (Write returns the first match).
func TestInitDuplicatePoint(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	drv := New([]Binding{
		{Point: "p1", Table: "holding", Register: 1},
		{Point: "p1", Table: "holding", Register: 2},
	})
	defer driverShutdown(drv)
	err := drv.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init duplicate Point: err=%v, want errInvalidBinding", err)
	}
}

// TestInitInvalidTable rejects free-form Table values.
func TestInitInvalidTable(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	drv := New([]Binding{
		{Point: "p1", Table: "coils", Register: 1},
	})
	defer driverShutdown(drv)
	err := drv.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init bad Table: err=%v, want errInvalidBinding", err)
	}
}

// TestDiscoverSubscribeUnsupported pins the two optional methods to
// the sentinel — drivers above this one rely on errors.Is matching.
func TestDiscoverSubscribeUnsupported(t *testing.T) {
	drv := New(stdBindings())
	_, err := drv.Discover(context.Background())
	if !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("Discover err=%v, want ErrNotSupported", err)
	}
	if err := drv.Subscribe(context.Background(), make(chan driver.Sample)); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("Subscribe err=%v, want ErrNotSupported", err)
	}
}

// --- Concurrency (MUST §5 line 5) ------------------------------------------

// TestCollectHealthRace runs Collect and Health concurrently for 50
// rounds. The mutex is what serialises access; this test is the
// `-race` check the prompt asks for. The point is not the counts but
// the absence of a race-detector hit.
func TestCollectHealthRace(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	drv := New(stdBindings())
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := drv.Collect(ctx); err != nil {
				t.Errorf("Collect[%d]: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = drv.Health(ctx)
		}
	}()
	wg.Wait()
}

// TestWriteWithoutInit makes sure Write does not panic on a fresh
// Driver: the binding lookup happens before any connection touch, so
// an unknown point returns ErrNotWritable cleanly.
func TestWriteWithoutInit(t *testing.T) {
	drv := New(stdBindings())
	_, err := drv.Write(context.Background(), driver.ControlCommand{
		Point: "site01.pod000.cdu000.does.not.exist", Value: 1, TTL: time.Second,
	})
	if !errors.Is(err, ErrNotWritable) {
		t.Errorf("err=%v, want ErrNotWritable", err)
	}
}

// --- error-path tests (drive coverage of connection-loss branches) --------

// TestWriteReconnectsAfterDrop covers the path in Write that calls
// reconnect when d.conn is nil at entry. We force that state by doing
// a Collect against a dead sim (which closes the conn) and then
// Write against a fresh sim at the same port.
func TestWriteReconnectsAfterDrop(t *testing.T) {
	ctx := context.Background()
	addr := freeLoopbackPort(t)

	sim1 := modbussim.New(modbussim.Config{
		Listen: addr, Holding: stdSimConfig().Holding, Input: stdSimConfig().Input,
	})
	if _, err := sim1.Start(); err != nil {
		t.Fatalf("sim1.Start: %v", err)
	}

	drv := New(stdBindings())
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		driverShutdown(drv)
		sim1.Stop()
		t.Fatalf("Init: %v", err)
	}
	// Drop the conn by stopping sim and forcing a Collect.
	// driverShutdown first dodges the modbussim Stop race (see
	// driverShutdown comment); the test goes on to verify the driver
	// rebuilds its conn from scratch via Write.
	driverShutdown(drv)
	sim1.Stop()
	// Wait for port release before bringing up sim2.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ln, lerr := net.Listen("tcp", addr)
		if lerr == nil {
			_ = ln.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _ = drv.Collect(ctx) // expected: all-suspect, conn nil afterward

	sim2 := modbussim.New(modbussim.Config{
		Listen: addr, Holding: stdSimConfig().Holding, Input: stdSimConfig().Input,
	})
	if _, err := sim2.Start(); err != nil {
		t.Fatalf("sim2.Start: %v", err)
	}
	defer sim2.Stop()
	defer driverShutdown(drv) // LIFO: runs before sim2.Stop

	// Write must reconnect on its own and succeed.
	res, err := drv.Write(ctx, driver.ControlCommand{
		Point: "site01.pod000.cdu000.tcs.opening", Value: 33, TTL: time.Second,
	})
	if err != nil {
		t.Fatalf("Write after reconnect: %v", err)
	}
	if !res.Accepted || res.Readback != 33 {
		t.Errorf("Write res = %+v, want Accepted=true Readback=33", res)
	}
}

// TestWriteOnReadonlyRegister exercises the modbus-exception branch
// inside Write: we mask the writable register at the sim, so FC6
// returns exception 0x02. The driver must surface Accepted=false +
// non-nil error WITHOUT closing the connection (exceptions are
// app-level, not transport-level).
func TestWriteOnMaskedRegister(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()

	drv := New(stdBindings())
	defer driverShutdown(drv) // LIFO: runs before sim.Stop — dodges modbussim Stop race
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	sim.Mask(0x0020) // tcs.opening register
	res, err := drv.Write(ctx, driver.ControlCommand{
		Point: "site01.pod000.cdu000.tcs.opening", Value: 1, TTL: time.Second,
	})
	if err == nil {
		t.Fatalf("Write on masked register: expected error")
	}
	if res.Accepted {
		t.Errorf("Accepted=true on masked write, want false")
	}
	// Connection should still be alive (exception, not loss): a
	// follow-up Collect succeeds on the unmasked points.
	sim.Unmask(0x0020)
	samples, cerr := drv.Collect(ctx)
	if cerr != nil {
		t.Fatalf("Collect after exception: %v", cerr)
	}
	if !allGood(samples) {
		t.Errorf("post-exception Collect not all-good: %s", qualitySummary(samples))
	}
}

// TestWriteDoesNotRefreshLastSuccess pins the §4.2 contract: LastSuccess
// is the timestamp of the most recent fully-good Collect round and
// only that. A successful Write between a masked-Collect and a
// recovery-Collect must not move LastSuccess — otherwise the
// gateway's staleness detector loses track of an ongoing telemetry
// outage simply because someone operated a control loop.
func TestWriteDoesNotRefreshLastSuccess(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()

	drv := New(stdBindings())
	defer driverShutdown(drv)
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Phase 1: a Collect round with one masked point → partially
	// suspect, LastSuccess must stay zero.
	sim.Mask(0x0010) // supply.temp
	if _, err := drv.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if h := drv.Health(ctx); !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess set after partial-suspect Collect, want zero")
	}
	// Phase 2: a successful Write on the writable point must NOT
	// advance LastSuccess (regression guard for the §4.2 contract).
	res, err := drv.Write(ctx, driver.ControlCommand{
		Point: "site01.pod000.cdu000.tcs.opening", Value: 42, TTL: time.Second,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("Write: err=%v res=%+v", err, res)
	}
	if h := drv.Health(ctx); !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess=%v after Write, want zero (Write must not refresh it)", h.LastSuccess)
	}
	// Phase 3: clear the mask, do a clean Collect. Now LastSuccess
	// must move forward.
	sim.Unmask(0x0010)
	if _, err := drv.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if h := drv.Health(ctx); h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess zero after clean Collect, want set")
	}
}

// --- roundTrip error frames (drive coverage of the framing branches) -----

// badServer is a minimal TCP listener that runs a per-connection
// handler under the caller's control. Used to feed roundTrip the
// pathological responses real sims never produce.
type badServer struct {
	ln      net.Listener
	addr    string
	handler func(net.Conn)
	wg      sync.WaitGroup

	mu    sync.Mutex
	conns []net.Conn
}

func newBadServer(t *testing.T, handler func(net.Conn)) *badServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("badServer listen: %v", err)
	}
	s := &badServer{ln: ln, addr: ln.Addr().String(), handler: handler}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.wg.Add(1)
			go func(c net.Conn) {
				defer s.wg.Done()
				defer c.Close()
				handler(c)
			}(c)
		}
	}()
	return s
}

// Stop closes the listener AND every accepted connection. Closing
// only the listener leaves handler goroutines blocked in ReadFull
// against an idle client (the driver may be done with the
// connection but holds it open for the next request); wg.Wait
// would never complete. Closing each connection forces the per-conn
// goroutine out of its read.
func (s *badServer) Stop() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
	s.wg.Wait()
}

// readReq drains one MBAP frame (header + body) and returns the
// transaction id. Used by handlers that need to echo a custom reply
// with the correct txid (or deliberately the wrong one).
func readReq(c net.Conn) (txID uint16, body []byte, err error) {
	var hdr [6]byte
	if _, err = io.ReadFull(c, hdr[:]); err != nil {
		return
	}
	txID = binary.BigEndian.Uint16(hdr[0:2])
	length := binary.BigEndian.Uint16(hdr[4:6])
	body = make([]byte, length)
	_, err = io.ReadFull(c, body)
	return
}

// runRoundTripCase wires a Driver up to a badServer, runs one
// Collect, and returns the resulting Health snapshot. The bad server
// only sends one bad frame then closes the socket; the driver's
// in-round reconnect succeeds (the listener is still up), so
// Health.Connected is true at the end. The per-point Quality
// (checked by the caller) and the accumulated ErrorCount are what
// distinguish "this round saw a problem" from a clean round.
func runRoundTripCase(t *testing.T, handler func(net.Conn)) driver.DriverHealth {
	t.Helper()
	srv := newBadServer(t, handler)
	defer srv.Stop()
	drv := New([]Binding{{Point: "p1", Table: "holding", Register: 0x0001}})
	if err := drv.Init(context.Background(), driver.DriverConfig{Endpoint: srv.addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	samples, _ := drv.Collect(context.Background())
	h := drv.Health(context.Background())
	// Sanity: the per-point sample is suspect in every badServer case.
	if len(samples) != 1 || samples[0].Quality != driver.QualitySuspect {
		t.Errorf("badServer Collect: samples=%+v, want one suspect", samples)
	}
	return h
}

func TestRoundTripBadProtoID(t *testing.T) {
	h := runRoundTripCase(t, func(c net.Conn) {
		txID, _, err := readReq(c)
		if err != nil {
			return
		}
		// Reply with proto id = 1 (must be 0).
		resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x01, 0x00, 0x04, 0x01, 0x03, 0x02, 0x00, 0x2A}
		_, _ = c.Write(resp)
	})
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount=0 after bad proto id")
	}
}

func TestRoundTripTxIDMismatch(t *testing.T) {
	h := runRoundTripCase(t, func(c net.Conn) {
		_, _, err := readReq(c)
		if err != nil {
			return
		}
		// Reply with txid 0xBEEF regardless of what the client sent.
		resp := []byte{0xBE, 0xEF, 0x00, 0x00, 0x00, 0x04, 0x01, 0x03, 0x02, 0x00, 0x2A}
		_, _ = c.Write(resp)
	})
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount=0 after txid mismatch")
	}
}

func TestRoundTripShortReadResponse(t *testing.T) {
	// Reply with a well-framed FC3 response that claims qty=1 but
	// supplies zero data bytes — len(respPDU) = 2 → driver hits the
	// `len(resp) < 4` branch in Collect.
	h := runRoundTripCase(t, func(c net.Conn) {
		txID, _, err := readReq(c)
		if err != nil {
			return
		}
		// MBAP: txid, proto=0, len=3 (unit+fc+bytecount). Body: unit, fc=0x03, bytecount=0.
		resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x00, 0x00, 0x03, 0x01, 0x03, 0x00}
		_, _ = c.Write(resp)
	})
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount=0 after short response")
	}
}

func TestRoundTripTruncatedException(t *testing.T) {
	// Reply with FC|0x80 but no exception-code byte → triggers the
	// "truncated exception PDU" branch.
	h := runRoundTripCase(t, func(c net.Conn) {
		txID, _, err := readReq(c)
		if err != nil {
			return
		}
		// length = 2: unit + FC(exception) but no exc code.
		resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x00, 0x00, 0x02, 0x01, 0x83}
		_, _ = c.Write(resp)
	})
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount=0 after truncated exception")
	}
}

func TestRoundTripExceptionKeepsConnection(t *testing.T) {
	// Well-formed exception 0x02 — connection MUST survive (this is
	// the "masked register" path conceptually). We use the bad server
	// instead of modbussim to isolate the connection-state assertion.
	srv := newBadServer(t, func(c net.Conn) {
		for {
			txID, _, err := readReq(c)
			if err != nil {
				return
			}
			resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x00, 0x00, 0x03, 0x01, 0x83, 0x02}
			if _, err := c.Write(resp); err != nil {
				return
			}
		}
	})
	defer srv.Stop()
	drv := New([]Binding{{Point: "p1", Table: "holding", Register: 0x0001}})
	if err := drv.Init(context.Background(), driver.DriverConfig{Endpoint: srv.addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := drv.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	h := drv.Health(context.Background())
	if !h.Connected {
		t.Errorf("Connected=false after app-level exception, want true")
	}
	if h.ErrorCount == 0 {
		t.Errorf("ErrorCount=0 after exception")
	}
}

// TestCollectMidRoundReconnectFailure drives the path where the
// connection dies in the middle of a multi-point round AND the
// in-round reconnect attempt also fails: the remaining bindings must
// come back as suspect and Collect must return nil error.
func TestCollectMidRoundReconnectFailure(t *testing.T) {
	ctx := context.Background()
	// Bad server that answers the first request normally then
	// closes the socket on the second.
	addr := ""
	first := true
	var mu sync.Mutex
	srv := newBadServer(t, func(c net.Conn) {
		for {
			txID, body, err := readReq(c)
			if err != nil {
				return
			}
			// body = unitID + PDU; PDU first byte = FC.
			if len(body) < 2 {
				return
			}
			mu.Lock()
			isFirst := first
			first = false
			mu.Unlock()
			if !isFirst {
				return // close socket
			}
			fc := body[1]
			resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x00, 0x00, 0x05, 0x01, fc, 0x02, 0x00, 0x2A}
			if _, err := c.Write(resp); err != nil {
				return
			}
		}
	})
	addr = srv.addr
	defer srv.Stop()

	drv := New([]Binding{
		{Point: "p1", Table: "holding", Register: 0x0001},
		{Point: "p2", Table: "holding", Register: 0x0002},
		{Point: "p3", Table: "holding", Register: 0x0003},
	})
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	samples, err := drv.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v (must be nil)", err)
	}
	if len(samples) != 3 {
		t.Fatalf("len=%d, want 3", len(samples))
	}
	// p1 good, p2 + p3 suspect (mid-round drop, no reconnect possible
	// because the bad server cycles a new closed connection each time).
	if samples[0].Quality != driver.QualityGood {
		t.Errorf("p1 Quality=%q, want good", samples[0].Quality)
	}
	if samples[1].Quality != driver.QualitySuspect {
		t.Errorf("p2 Quality=%q, want suspect", samples[1].Quality)
	}
	if samples[2].Quality != driver.QualitySuspect {
		t.Errorf("p3 Quality=%q, want suspect", samples[2].Quality)
	}
}

// TestReconnectEmptyEndpoint sanity-checks the no-endpoint reconnect
// branch — reachable only if a caller pokes the internal state, but
// the branch exists for defence in depth.
func TestReconnectEmptyEndpoint(t *testing.T) {
	drv := New(stdBindings())
	// Force into "no endpoint" state with a live conn-nil.
	drv.mu.Lock()
	drv.endpoint = ""
	drv.conn = nil
	drv.mu.Unlock()
	samples, err := drv.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, s := range samples {
		if s.Quality != driver.QualitySuspect {
			t.Errorf("Quality %q, want suspect", s.Quality)
		}
	}
}

// TestCollectMidRoundReconnectFillsSuspect drives the in-round
// reconnect failure path. The bad server replies normally to the
// first request and then drops its connection; on the second
// binding the driver roundTrips into a dead socket, falls into the
// "default" branch of Collect, closes the conn, and tries to
// reconnect. We poison d.endpoint to a closed port right before the
// in-round reconnect runs, so reconnect fails and fillSuspect must
// mark bindings p2 and p3 suspect. This is the only path that hits
// fillSuspect, so coverage depends on it.
func TestCollectMidRoundReconnectFillsSuspect(t *testing.T) {
	ctx := context.Background()
	count := 0
	var mu sync.Mutex
	srv := newBadServer(t, func(c net.Conn) {
		for {
			txID, body, err := readReq(c)
			if err != nil {
				return
			}
			if len(body) < 2 {
				return
			}
			mu.Lock()
			count++
			n := count
			mu.Unlock()
			if n == 1 {
				// First request: reply normally so p1 reads good.
				fc := body[1]
				resp := []byte{byte(txID >> 8), byte(txID), 0x00, 0x00, 0x00, 0x05, 0x01, fc, 0x02, 0x00, 0x2A}
				if _, err := c.Write(resp); err != nil {
					return
				}
				continue
			}
			// Second connection (after the in-round reconnect
			// succeeds against the still-listening bad server):
			// no reply, just close. This makes p2's roundTrip
			// fail at the read side.
			return
		}
	})
	addr := srv.addr

	drv := New([]Binding{
		{Point: "p1", Table: "holding", Register: 0x0001},
		{Point: "p2", Table: "holding", Register: 0x0002},
		{Point: "p3", Table: "holding", Register: 0x0003},
	})
	if err := drv.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Replace the endpoint with a dead port BEFORE the in-round
	// reconnect runs. The driver holds a live conn to the bad
	// server for p1; when p2 roundTrips, the server will close →
	// driver closes the conn, tries to reconnect — and that
	// reconnect attempt goes to the dead port we just swapped in,
	// failing. fillSuspect must then mark p2 and p3 suspect.
	dead := freeLoopbackPort(t)
	drv.mu.Lock()
	drv.endpoint = dead
	drv.mu.Unlock()
	defer srv.Stop()

	samples, err := drv.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v (must be nil)", err)
	}
	if len(samples) != 3 {
		t.Fatalf("len=%d, want 3", len(samples))
	}
	if samples[0].Quality != driver.QualityGood {
		t.Errorf("p1 Quality=%q, want good", samples[0].Quality)
	}
	if samples[1].Quality != driver.QualitySuspect {
		t.Errorf("p2 Quality=%q, want suspect (fillSuspect path)", samples[1].Quality)
	}
	if samples[2].Quality != driver.QualitySuspect {
		t.Errorf("p3 Quality=%q, want suspect (fillSuspect path)", samples[2].Quality)
	}
}
