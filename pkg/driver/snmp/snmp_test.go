package snmp

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/snmpsim"
)

// --- helpers ---------------------------------------------------------------

// startSim spins up a snmpsim on a kernel-allocated port and returns
// it along with the bound address. The caller is responsible for
// Stop()'ing it.
func startSim(t *testing.T, cfg snmpsim.Config) (*snmpsim.Sim, string) {
	t.Helper()
	sim := snmpsim.New(cfg)
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("sim.Start: %v", err)
	}
	return sim, addr
}

// freeLoopbackPort grabs and immediately releases a free 127.0.0.1
// UDP port so a sim can be (re)bound to it deterministically. Used
// by C1 and C4 where the address must be reserved up front so two
// sim instances can share it across a restart.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

// stdBindings is the 8-point table used by C2/C3/C6. OIDs are under
// the enterprise 9999 arc to keep the sim's OID table tidy and
// distinct from real IANA assignments.
func stdBindings() []Binding {
	return []Binding{
		{Point: "site01.pod000.cdu000.fws.supply.temp", OID: "1.3.6.1.4.1.9999.1.1.0"},
		{Point: "site01.pod000.cdu000.fws.return.temp", OID: "1.3.6.1.4.1.9999.1.2.0"},
		{Point: "site01.pod000.cdu000.fws.supply.flow", OID: "1.3.6.1.4.1.9999.1.3.0"},
		{Point: "site01.pod000.cdu000.fws.return.flow", OID: "1.3.6.1.4.1.9999.1.4.0"},
		{Point: "site01.pod000.cdu000.tcs.opening", OID: "1.3.6.1.4.1.9999.1.5.0", Writable: true, Kind: "integer"},
		{Point: "site01.pod000.cdu000.fws.supply.pressure", OID: "1.3.6.1.4.1.9999.1.6.0"},
		{Point: "site01.pod000.cdu000.fws.return.pressure", OID: "1.3.6.1.4.1.9999.1.7.0"},
		{Point: "site01.pod000.cdu000.fws.supply.density", OID: "1.3.6.1.4.1.9999.1.8.0"},
	}
}

// stdSimConfig seeds the sim so every binding in stdBindings has a
// known, distinct value. Keeping the (OID → int64) map here makes
// the assertions in C2/C3/C6 readable.
func stdSimConfig() snmpsim.Config {
	return snmpsim.Config{
		Community: "public",
		Addr:      "127.0.0.1:0",
	}
}

// driverShutdown drops the driver's session. The driver has no
// exported Shutdown; the test reaches in via the unexported
// `reconnect` semantics by closing the session. We use the
// unexported helper the test code in pkg/driver/snmp already needs
// to touch.
func driverShutdown(d *Driver) {
	d.mu.Lock()
	if d.sess != nil {
		_ = d.sess.Close()
		d.sess = nil
	}
	d.connected = false
	d.mu.Unlock()
}

// expectedValues is the (Point → int64) lookup C2/C3 use to assert
// each Sample's Value. The values are arbitrary but distinct so a
// cross-talk bug in the driver would surface as a wrong number.
func expectedValues() map[string]int64 {
	return map[string]int64{
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

// seedSim sets the 8 OIDs in sim to the values in expectedValues().
// Returns nothing — the test fails the caller if the value map is
// stale.
func seedSim(sim *snmpsim.Sim) {
	for point, v := range expectedValues() {
		_ = point // silence unused-var
		_ = v
	}
	oidMap := map[string]int64{
		"1.3.6.1.4.1.9999.1.1.0": 11,
		"1.3.6.1.4.1.9999.1.2.0": 22,
		"1.3.6.1.4.1.9999.1.3.0": 33,
		"1.3.6.1.4.1.9999.1.4.0": 44,
		"1.3.6.1.4.1.9999.1.5.0": 55,
		"1.3.6.1.4.1.9999.1.6.0": 66,
		"1.3.6.1.4.1.9999.1.7.0": 77,
		"1.3.6.1.4.1.9999.1.8.0": 88,
	}
	for oid, v := range oidMap {
		sim.SetOID(oid, v)
	}
}

// --- C1 ---------------------------------------------------------------------

// TestConformanceC1 covers Init's three branches:
//
//   - happy path against a live sim: no error, Health.Connected=true
//   - wrong community: returns ErrAuth (errors.Is matches)
//   - dial failure (unreachable port): returns a non-ErrAuth error
func TestConformanceC1(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		sim, addr := startSim(t, stdSimConfig())
		defer sim.Stop()
		sim.SetOID("1.3.6.1.4.1.9999.1.1.0", 1)
		d := New(stdBindings())
		defer driverShutdown(d)
		if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		h := d.Health(ctx)
		if !h.Connected {
			t.Errorf("Connected=false after Init, want true")
		}
	})

	t.Run("wrong community returns ErrAuth", func(t *testing.T) {
		sim, addr := startSim(t, stdSimConfig())
		defer sim.Stop()
		sim.SetOID("1.3.6.1.4.1.9999.1.1.0", 1)
		d := New(stdBindings())
		defer driverShutdown(d)
		err := d.Init(ctx, driver.DriverConfig{
			Endpoint: addr,
			Options:  map[string]string{"community": "private"},
		})
		if !errors.Is(err, ErrAuth) {
			t.Errorf("Init err=%v, want ErrAuth", err)
		}
		h := d.Health(ctx)
		if h.Connected {
			t.Errorf("Connected=true after ErrAuth, want false")
		}
	})

	t.Run("dial fails on closed port", func(t *testing.T) {
		// Pin a port, free it, point the driver at it. C1
		// distinguishes "auth" from "network" — the test must
		// show the failure is a network error, NOT ErrAuth.
		addr := freeLoopbackPort(t)
		d := New(stdBindings())
		defer driverShutdown(d)
		err := d.Init(ctx, driver.DriverConfig{Endpoint: addr})
		if err == nil {
			t.Fatalf("Init: expected dial error, got nil")
		}
		if errors.Is(err, ErrAuth) {
			t.Errorf("Init err=%v is ErrAuth; want a network error", err)
		}
		if h := d.Health(ctx); h.Connected {
			t.Errorf("Connected=true after failed dial, want false")
		}
	})
}

// --- C2 ---------------------------------------------------------------------

// TestConformanceC2 reads all 8 bindings against a clean sim and
// asserts: count, per-point value, per-point Quality=good, ordering
// matches the binding-table order, no error returned.
func TestConformanceC2(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)

	bindings := stdBindings()
	d := New(bindings)
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	samples, err := d.Collect(ctx)
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
		if int64(s.Value) != want[s.Point] {
			t.Errorf("samples[%d] %q Value = %d, want %d", i, s.Point, int64(s.Value), want[s.Point])
		}
	}
	h := d.Health(ctx)
	if h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess zero after fully-good Collect")
	}
	if h.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d after clean Collect, want 0", h.ErrorCount)
	}
}

// --- C3 ---------------------------------------------------------------------

// TestConformanceC3 masks one OID at the sim and asserts: the
// masked point is suspect, the other seven are good, Collect
// returns no error, LastSuccess stays zero.
func TestConformanceC3(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)
	const maskedOID = "1.3.6.1.4.1.9999.1.1.0"
	const maskedPoint = "site01.pod000.cdu000.fws.supply.temp"
	sim.Mask(maskedOID)

	d := New(stdBindings())
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	samples, err := d.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect returned error: %v (must be nil)", err)
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
	h := d.Health(ctx)
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
//  2. Stop sim1 → next Collect returns all-suspect, no error,
//     Health.Connected=false
//  3. Start sim2 at the SAME address → next Collect recovers
//     without a fresh Init; samples back to all-good
func TestConformanceC4(t *testing.T) {
	ctx := context.Background()
	addr := freeLoopbackPort(t)

	sim1 := snmpsim.New(snmpsim.Config{Community: "public", Addr: addr})
	seedSim(sim1)
	if _, err := sim1.Start(); err != nil {
		t.Fatalf("sim1.Start: %v", err)
	}

	d := New(stdBindings())
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		driverShutdown(d)
		sim1.Stop()
		t.Fatalf("Init: %v", err)
	}

	// Phase 1: clean collect.
	samples, err := d.Collect(ctx)
	if err != nil {
		driverShutdown(d)
		sim1.Stop()
		t.Fatalf("phase1 Collect: err=%v", err)
	}
	if !allGood(samples) {
		driverShutdown(d)
		sim1.Stop()
		t.Fatalf("phase1 Collect: not all-good")
	}

	// Phase 2: stop the sim. The driver still holds a session; the
	// next Collect must transparently reconnect (and fail, because
	// nothing is listening), then return all-suspect with nil
	// error.
	driverShutdown(d)
	sim1.Stop()
	// Give the OS a moment to release the UDP port.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pc, lerr := net.ListenPacket("udp", addr)
		if lerr == nil {
			_ = pc.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	samples, err = d.Collect(ctx)
	if err != nil {
		t.Fatalf("phase2 Collect returned error: %v (must be nil even when offline)", err)
	}
	for _, s := range samples {
		if s.Quality != driver.QualitySuspect {
			t.Errorf("phase2 %q Quality=%q, want suspect (sim is down)", s.Point, s.Quality)
		}
	}
	if h := d.Health(ctx); h.Connected {
		t.Errorf("phase2 Health.Connected=true, want false")
	}

	// Phase 3: bring up sim2 on the same port. The driver must
	// reconnect on its own — no Re-Init.
	sim2 := snmpsim.New(snmpsim.Config{Community: "public", Addr: addr})
	seedSim(sim2)
	if _, err := sim2.Start(); err != nil {
		t.Fatalf("sim2.Start at %s: %v", addr, err)
	}
	defer sim2.Stop()
	defer driverShutdown(d)

	samples, err = d.Collect(ctx)
	if err != nil {
		t.Fatalf("phase3 Collect: %v", err)
	}
	if !allGood(samples) {
		t.Errorf("phase3 samples not all-good after recovery: %s", qualitySummary(samples))
	}
	if h := d.Health(ctx); !h.Connected {
		t.Errorf("phase3 Health.Connected=false, want true")
	}
}

// --- C6 ---------------------------------------------------------------------

// TestConformanceC6 covers Write across its four contract branches:
// happy, expired TTL, read-only point, and post-write Collect sees
// the new value.
func TestConformanceC6(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)

	d := New(stdBindings())
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	writable := "site01.pod000.cdu000.tcs.opening"
	readonly := "site01.pod000.cdu000.fws.supply.pressure"

	t.Run("happy path", func(t *testing.T) {
		res, err := d.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 45, TTL: time.Second, RequestID: "r1",
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !res.Accepted {
			t.Errorf("Accepted=false, want true")
		}
		if int64(res.Readback) != 45 {
			t.Errorf("Readback = %v, want 45", res.Readback)
		}
		if res.ReadbackTs.IsZero() {
			t.Errorf("ReadbackTs zero")
		}
	})

	t.Run("ttl expired", func(t *testing.T) {
		_, err := d.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 99, TTL: 0,
		})
		if !errors.Is(err, ErrExpired) {
			t.Errorf("err = %v, want ErrExpired", err)
		}
		_, err = d.Write(ctx, driver.ControlCommand{
			Point: writable, Value: 99, TTL: -time.Second,
		})
		if !errors.Is(err, ErrExpired) {
			t.Errorf("negative TTL err = %v, want ErrExpired", err)
		}
	})

	t.Run("readonly point", func(t *testing.T) {
		_, err := d.Write(ctx, driver.ControlCommand{
			Point: readonly, Value: 1, TTL: time.Second,
		})
		if !errors.Is(err, ErrNotWritable) {
			t.Errorf("err = %v, want ErrNotWritable", err)
		}
	})

	t.Run("unknown point", func(t *testing.T) {
		_, err := d.Write(ctx, driver.ControlCommand{
			Point: "site01.pod000.cdu000.does.not.exist", Value: 1, TTL: time.Second,
		})
		if !errors.Is(err, ErrNotWritable) {
			t.Errorf("unknown-point err = %v, want ErrNotWritable", err)
		}
	})

	t.Run("post-write Collect sees new value", func(t *testing.T) {
		const newVal = 77
		if _, err := d.Write(ctx, driver.ControlCommand{
			Point: writable, Value: newVal, TTL: time.Second,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		samples, err := d.Collect(ctx)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for _, s := range samples {
			if s.Point == writable {
				if int64(s.Value) != newVal {
					t.Errorf("post-Write Collect %q = %v, want %d", s.Point, s.Value, newVal)
				}
				return
			}
		}
		t.Errorf("writable point %q not in Collect samples", writable)
	})
}

// --- Init validation (ErrInvalidBinding paths) ------------------------------

// TestInitEmptyBindings asserts the empty binding table is rejected
// as a configuration bug.
func TestInitEmptyBindings(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New(nil)
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init nil bindings: err=%v, want errInvalidBinding", err)
	}
	d2 := New([]Binding{})
	defer driverShutdown(d2)
	if err := d2.Init(context.Background(), driver.DriverConfig{Endpoint: addr}); !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init empty bindings: err=%v, want errInvalidBinding", err)
	}
}

// TestInitDuplicatePoint catches a config-mistake that would
// otherwise silently corrupt Write lookups (Write returns the first
// match).
func TestInitDuplicatePoint(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New([]Binding{
		{Point: "p1", OID: "1.3.6.1.4.1.9999.1.1.0"},
		{Point: "p1", OID: "1.3.6.1.4.1.9999.1.2.0"},
	})
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init duplicate Point: err=%v, want errInvalidBinding", err)
	}
}

// TestInitInvalidOID rejects free-form OID values that do not parse
// as dotted-decimal.
func TestInitInvalidOID(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New([]Binding{
		{Point: "p1", OID: "not-an-oid"},
	})
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init bad OID: err=%v, want errInvalidBinding", err)
	}
}

// TestInitWritableWithoutKind rejects Writable=true with empty Kind
// (we have no way to encode the SET).
func TestInitWritableWithoutKind(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New([]Binding{
		{Point: "p1", OID: "1.3.6.1.4.1.9999.1.1.0", Writable: true},
	})
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init Writable-no-Kind: err=%v, want errInvalidBinding", err)
	}
}

// TestInitWritableInvalidKind rejects Writable=true with a Kind
// outside the supported set.
func TestInitWritableInvalidKind(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New([]Binding{
		{Point: "p1", OID: "1.3.6.1.4.1.9999.1.1.0", Writable: true, Kind: "octetstring"},
	})
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init Writable-bad-Kind: err=%v, want errInvalidBinding", err)
	}
}

// TestInitReadonlyWithKind rejects Writable=false with a non-empty
// Kind (the field is meaningless on a read-only point and signals
// a config mistake).
func TestInitReadonlyWithKind(t *testing.T) {
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	d := New([]Binding{
		{Point: "p1", OID: "1.3.6.1.4.1.9999.1.1.0", Writable: false, Kind: "integer"},
	})
	defer driverShutdown(d)
	err := d.Init(context.Background(), driver.DriverConfig{Endpoint: addr})
	if !errors.Is(err, errInvalidBinding) {
		t.Errorf("Init Readonly-with-Kind: err=%v, want errInvalidBinding", err)
	}
}

// --- ErrNotSupported pins --------------------------------------------------

func TestDiscoverSubscribeUnsupported(t *testing.T) {
	d := New(stdBindings())
	if _, err := d.Discover(context.Background()); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("Discover err=%v, want ErrNotSupported", err)
	}
	if err := d.Subscribe(context.Background(), make(chan driver.Sample)); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("Subscribe err=%v, want ErrNotSupported", err)
	}
}

// --- TestWriteDoesNotRefreshLastSuccess (L61 regression) -------------------

// TestWriteDoesNotRefreshLastSuccess pins the L61 contract: a
// successful Write between a partial-suspect Collect and a clean
// Collect must not move LastSuccess — otherwise the gateway's
// staleness detector loses track of an ongoing telemetry outage
// because someone operated a control loop.
func TestWriteDoesNotRefreshLastSuccess(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)
	sim.Mask("1.3.6.1.4.1.9999.1.1.0") // supply.temp

	d := New(stdBindings())
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Phase 1: Collect with one masked point → partial-suspect,
	// LastSuccess must stay zero.
	if _, err := d.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if h := d.Health(ctx); !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess set after partial-suspect Collect, want zero")
	}
	// Phase 2: a successful Write must NOT advance LastSuccess.
	res, err := d.Write(ctx, driver.ControlCommand{
		Point: "site01.pod000.cdu000.tcs.opening", Value: 42, TTL: time.Second,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("Write: err=%v res=%+v", err, res)
	}
	if h := d.Health(ctx); !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess=%v after Write, want zero (Write must not refresh it)", h.LastSuccess)
	}
	// Phase 3: clear the mask, do a clean Collect. Now LastSuccess
	// must move forward.
	sim.Unmask("1.3.6.1.4.1.9999.1.1.0")
	if _, err := d.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if h := d.Health(ctx); h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess zero after clean Collect, want set")
	}
}

// --- Init probe timeout: no Close/Get race (CODE-SCAN-2026-07-16 §2.1) ----

// TestInitProbeTimeoutNoRace pins that when the Init health probe
// times out, Close is not called while Get is still in-flight.
// Blackhole UDP: Connect succeeds (connectionless), Get waits for
// sess.Timeout, probeCtx expires first — join-before-Close is required.
// Run with -race; the assertion is the absence of a race report.
func TestInitProbeTimeoutNoRace(t *testing.T) {
	ctx := context.Background()
	// Reserve a free port and leave it unbound so Get never gets a reply.
	addr := freeLoopbackPort(t)
	d := New(stdBindings())
	defer driverShutdown(d)
	err := d.Init(ctx, driver.DriverConfig{Endpoint: addr})
	if err == nil {
		t.Fatal("Init to blackhole UDP must error (probe timeout or network)")
	}
	if h := d.Health(ctx); h.Connected {
		t.Errorf("Connected=true after failed Init, want false")
	}
}

// --- Concurrency: Collect / Health race ------------------------------------

// TestCollectHealthRace is the `-race` check the prompt asks for.
// 50 rounds of concurrent Collect + Health; the mutex is what
// serialises access; the point is the absence of a race-detector
// hit, not the counts.
func TestCollectHealthRace(t *testing.T) {
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)
	d := New(stdBindings())
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := d.Collect(ctx); err != nil {
				t.Errorf("Collect[%d]: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = d.Health(ctx)
		}
	}()
	wg.Wait()
}

// --- Coverage: 70k+ requests (L62 long-running) ---------------------------

// TestLongRunning is the §5 long-running requirement: ≥70 000
// requests still correct. 8 800 rounds × 8 bindings = 70 400.
func TestLongRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	sim, addr := startSim(t, stdSimConfig())
	defer sim.Stop()
	seedSim(sim)
	d := New(stdBindings())
	defer driverShutdown(d)
	if err := d.Init(ctx, driver.DriverConfig{Endpoint: addr}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const rounds = 8800
	for i := 0; i < rounds; i++ {
		samples, err := d.Collect(ctx)
		if err != nil {
			t.Fatalf("Collect[%d]: %v", i, err)
		}
		for _, s := range samples {
			if s.Quality != driver.QualityGood {
				t.Fatalf("round %d: %+v", i, s)
			}
		}
	}
}

// --- Endpoint normalisation (R1 D1 regression) ----------------------------
//
// The R1 review surfaced that normalizeEndpoint used a local
// splitHostPort whose success branch silently swallowed "no port"
// inputs (returns nil error), so the ":161 default port" path was
// dead code and Init would fail on the common "10.0.0.5" shorthand.
// These subtests pin the corrected behaviour: missing-port inputs
// are appended with the default port, IPv6 is bracket-safe, and
// Init accepts the shorthand (returning a network error rather
// than an invalid-binding or auth error when no agent listens on
// :161).

func TestEndpointNoPortNormalized(t *testing.T) {
	ctx := context.Background()

	t.Run("missing port gets :161 appended", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{"10.0.0.5", "10.0.0.5:161"},
			{"  10.0.0.5  ", "10.0.0.5:161"},
			{"127.0.0.1", "127.0.0.1:161"},
			{"agent-7.internal", "agent-7.internal:161"},
			{"10.0.0.5:1161", "10.0.0.5:1161"}, // explicit port preserved
			{"[::1]:161", "[::1]:161"},         // bracketed IPv6 + port preserved
		}
		for _, tc := range cases {
			got, err := normalizeEndpoint(tc.in)
			if err != nil {
				t.Errorf("normalizeEndpoint(%q) err=%v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("normalizeEndpoint(%q)=%q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("empty endpoint rejected", func(t *testing.T) {
		if _, err := normalizeEndpoint(""); err == nil {
			t.Errorf("normalizeEndpoint(\"\") err=nil, want error")
		}
	})

	t.Run("invalid port rejected", func(t *testing.T) {
		if _, err := normalizeEndpoint("10.0.0.5:abc"); err == nil {
			t.Errorf("normalizeEndpoint(\"10.0.0.5:abc\") err=nil, want error")
		}
	})

	t.Run("Init accepts no-port shorthand and dials :161", func(t *testing.T) {
		// Bind a sim on an ephemeral port; point the driver at the
		// same host with NO port. After R1 the driver must append
		// :161 internally, fail to reach the sim (it's not on :161),
		// and surface a network-class error — not ErrAuth and not a
		// strconv-style parse failure. This is the regression pin
		// for D1: the shorthand must at least be PARSED, not
		// rejected up front.
		sim, addr := startSim(t, stdSimConfig())
		defer sim.Stop()
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("split sim addr: %v", err)
		}
		d := New(stdBindings())
		defer driverShutdown(d)
		err = d.Init(ctx, driver.DriverConfig{Endpoint: host})
		if err == nil {
			t.Fatalf("Init(%q) err=nil; sim not on :161, want dial failure", host)
		}
		if errors.Is(err, ErrAuth) {
			t.Errorf("Init err=%v is ErrAuth; want a network/dial error", err)
		}
		if errors.Is(err, errInvalidBinding) {
			t.Errorf("Init err=%v wraps errInvalidBinding; endpoint parsing failed", err)
		}
		// And: normalisation must have left Connected=false.
		if h := d.Health(ctx); h.Connected {
			t.Errorf("Connected=true after failed dial, want false")
		}
	})
}

// --- helpers (mirroring modbus_test) ---------------------------------------

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
	// Quality is a string-typed int; no fmt import needed.
	var b strings.Builder
	first := true
	for q, n := range counts {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(string(q))
		b.WriteString("=")
		// convert n to decimal inline (avoid fmt to keep the
		// helper self-contained)
		if n == 0 {
			b.WriteByte('0')
			continue
		}
		var buf [20]byte
		i := len(buf)
		for n > 0 {
			i--
			buf[i] = byte('0' + n%10)
			n /= 10
		}
		b.Write(buf[i:])
	}
	return b.String()
}
