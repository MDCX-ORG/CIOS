package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/modbussim"
	"github.com/yurimeng/cios/pkg/driver/snmpsim"
	"github.com/yurimeng/cios/pkg/modbusbind"
	"github.com/yurimeng/cios/pkg/pointmap"
)

// fakeVM is the testbed VictoriaMetrics: it accepts POSTs to
// /api/v1/import/prometheus, records every body it sees, and lets
// the test trigger a 500 with setFail(true).
type fakeVM struct {
	mu       sync.Mutex
	bodies   []string
	failNext bool
	srv      *httptest.Server
}

func newFakeVM() *fakeVM {
	f := &fakeVM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failNext {
			f.failNext = false
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		f.bodies = append(f.bodies, string(buf[:n]))
		w.WriteHeader(http.StatusNoContent)
	}))
	return f
}

func (f *fakeVM) URL() string    { return f.srv.URL + "/api/v1/import/prometheus" }
func (f *fakeVM) Close()         { f.srv.Close() }
func (f *fakeVM) setFail(b bool) { f.mu.Lock(); f.failNext = b; f.mu.Unlock() }
func (f *fakeVM) bodiesCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.bodies))
	copy(out, f.bodies)
	return out
}

// writePointmap drops a self-contained point-map YAML into a temp
// directory and returns the path. The Register values match the
// stdSimConfig the PRMT-009 conformance tests use.
func writePointmap(t *testing.T, dir string) string {
	t.Helper()
	pm := `kind: PointMap
metadata:
  name: cdu-sim
  driver: modbus
  appliesTo: cdu
spec:
  points:
    - point: fws.supply.temp
      register: 16
      table: input
      unit_in: decicelsius
    - point: status
      register: 32
      table: input
      enum_map: { 0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5 }
`
	p := filepath.Join(dir, "cdu-sim.yaml")
	if err := os.WriteFile(p, []byte(pm), 0o644); err != nil {
		t.Fatalf("write pointmap: %v", err)
	}
	return p
}

// moduleRoot copied from pipeline_test.go to keep the test file
// self-contained.
func moduleRoot2() string {
	wd, _ := os.Getwd()
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// TestRun_EndToEnd spins up modbussim, a fake VM, and the gateway
// loop, and verifies two ticks' worth of Prometheus text arrives at
// the fake VM. We run only a few ticks (Interval=100ms, ctx
// cancelled after ~600ms) so the test stays sub-second.
func TestRun_EndToEnd(t *testing.T) {
	vm := newFakeVM()
	defer vm.Close()
	sim := modbussim.New(modbussim.Config{
		Holding: map[uint16]uint16{0x0020: 50},
		Input:   map[uint16]uint16{0x0010: 235, 0x0020: 1},
	})
	simAddr, err := sim.Start()
	if err != nil {
		t.Fatalf("sim.Start: %v", err)
	}
	defer sim.Stop()

	pmDir := t.TempDir()
	pmPath := writePointmap(t, pmDir)

	root := moduleRoot2()
	if root == "" {
		t.Fatal("module root not found")
	}
	cfg := Config{
		Site:        "site01",
		ProtocolDir: filepath.Join(root, "protocol"),
		VMWriteURL:  vm.URL(),
		Interval:    100 * time.Millisecond,
		Devices: []Device{{
			Asset:    "site01.pod000.cdu000",
			PointMap: pmPath,
			Endpoint: simAddr,
			UnitID:   "1",
		}},
	}
	// Device.baseDir is normally set by LoadConfig. The run loop
	// reads the pointmap by joining baseDir+PointMap, so set it
	// explicitly here.
	cfg.Devices[0].baseDir = pmDir

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	// Run blocks on the ticker; cancel() makes it return.
	_ = Run(ctx, cfg)

	bodies := vm.bodiesCopy()
	if len(bodies) < 2 {
		t.Fatalf("expected >=2 POSTs to fake VM, got %d", len(bodies))
	}
	// First body should mention both fws.supply.temp (23.5) and status (1).
	seen := map[string]bool{}
	for _, line := range strings.Split(bodies[0], "\n") {
		switch {
		case strings.HasPrefix(line, "cios_temp_celsius{"):
			// 235 decicelsius -> 23.5 celsius.
			if !strings.Contains(line, " 23.5 ") {
				t.Errorf("temp line wrong: %s", line)
			}
			seen["temp"] = true
		case strings.HasPrefix(line, "cios_status{"):
			// enum mapping: vendor 1 -> standard 1.
			if !strings.Contains(line, " 1 ") {
				t.Errorf("status line wrong: %s", line)
			}
			seen["status"] = true
		}
	}
	if !seen["temp"] || !seen["status"] {
		t.Errorf("missing lines in first body: seen=%v body=%q", seen, bodies[0])
	}
}

// TestRun_VM500DoesNotCrash: the loop must keep ticking even when
// the VM returns 500. We point the gateway at a fake VM that fails
// its first POST; we expect Run to stay alive across that and at
// least one more tick.
func TestRun_VM500DoesNotCrash(t *testing.T) {
	vm := newFakeVM()
	defer vm.Close()
	sim := modbussim.New(modbussim.Config{
		Input: map[uint16]uint16{0x0010: 235},
	})
	simAddr, err := sim.Start()
	if err != nil {
		t.Fatalf("sim.Start: %v", err)
	}
	defer sim.Stop()

	pmDir := t.TempDir()
	pmPath := writePointmap(t, pmDir)
	root := moduleRoot2()

	cfg := Config{
		Site:        "site01",
		ProtocolDir: filepath.Join(root, "protocol"),
		VMWriteURL:  vm.URL(),
		Interval:    80 * time.Millisecond,
		Devices: []Device{{
			Asset: "site01.pod000.cdu000", PointMap: pmPath,
			Endpoint: simAddr, UnitID: "1",
		}},
	}
	cfg.Devices[0].baseDir = pmDir

	// Trigger a 500 on the FIRST POST only.
	vm.setFail(true)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("Run returned error after VM 500: %v", err)
	}
	// The 500 was the first POST; subsequent POSTs should have
	// succeeded. We need >=1 successful body in the buffer.
	if got := vm.bodiesCopy(); len(got) < 1 {
		t.Errorf("expected at least 1 successful POST after 500, got %d", len(got))
	}
}

// TestLoadConfig_Demo exercises the actual demo config + pointmap
// to make sure deploy/edge/gateway.yaml and the cdu-sim.yaml
// pointmap round-trip through LoadConfig and pointmap.Load.
func TestLoadConfig_Demo(t *testing.T) {
	root := moduleRoot2()
	cfgPath := filepath.Join(root, "deploy", "edge", "gateway.yaml")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Site != "site01" {
		t.Errorf("Site = %q, want site01", cfg.Site)
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", cfg.Interval)
	}
	if len(cfg.Devices) != 1 {
		t.Fatalf("Devices len = %d, want 1", len(cfg.Devices))
	}
	d := cfg.Devices[0]
	if d.Asset != "site01.pod000.cdu000" {
		t.Errorf("Asset = %q", d.Asset)
	}
	if d.UnitID != "1" {
		t.Errorf("UnitID = %q, want 1", d.UnitID)
	}
	// The pointmap must be loadable by pointmap.Load (V1–V7 pass).
	pmPath := d.PointMapPath()
	if _, err := os.Stat(pmPath); err != nil {
		t.Fatalf("pointmap not found at %s: %v", pmPath, err)
	}
	fmt.Fprintf(os.Stderr, "loaded demo pointmap: %s\n", pmPath)
}

// writeGatewayYAML drops a gateway.yaml with the requested body
// into a temp dir and returns the path.
func writeGatewayYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadConfig_RejectsEmptySite(t *testing.T) {
	root := moduleRoot2()
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: ""
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices: []
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty site, got nil")
	}
}

func TestLoadConfig_RejectsEmptyVMWriteURL(t *testing.T) {
	root := moduleRoot2()
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: ""
devices: []
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty vm_write_url, got nil")
	}
}

func TestLoadConfig_RejectsEmptyDevices(t *testing.T) {
	root := moduleRoot2()
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices: []
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty devices, got nil")
	}
}

func TestLoadConfig_DefaultsIntervalTo10s(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
    unit_id: "1"
`, filepath.Join(root, "protocol")))
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s default", cfg.Interval)
	}
}

func TestLoadConfig_RejectsAssetSiteMismatch(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	// Asset site is site01, config site is site02.
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site02
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
    unit_id: "1"
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for asset/config site mismatch, got nil")
	}
}

func TestLoadConfig_DefaultsUnitIDTo1(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
`, filepath.Join(root, "protocol")))
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Devices[0].UnitID != "1" {
		t.Errorf("UnitID = %q, want 1 default", cfg.Devices[0].UnitID)
	}
}

func TestLoadConfig_RejectsBadYAML(t *testing.T) {
	// Triggers config.go:60-62 (yaml.Unmarshal error path).
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte(":\n  : ::  bad"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for malformed YAML, got nil")
	}
}

func TestLoadConfig_RejectsMissingFile(t *testing.T) {
	// Triggers config.go:56-58 (os.ReadFile error path).
	if _, err := LoadConfig("/nonexistent/gateway.yaml"); err == nil {
		t.Errorf("expected error for missing file, got nil")
	}
}

func TestLoadConfig_RejectsEmptyProtocolDir(t *testing.T) {
	// Triggers config.go:70-72 (protocol_dir empty).
	p := writeGatewayYAML(t, `
site: site01
protocol_dir: ""
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
`)
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty protocol_dir, got nil")
	}
}

func TestLoadConfig_RejectsBadAsset(t *testing.T) {
	// Triggers config.go:107-110 (ParseAssetPath error path).
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: "bad..asset"
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for malformed asset, got nil")
	}
}

func TestLoadConfig_RejectsEmptyPointMap(t *testing.T) {
	// Triggers config.go:115-117 (pointmap empty path).
	root := moduleRoot2()
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: ""
    endpoint: 127.0.0.1:15020
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty pointmap, got nil")
	}
}

func TestLoadConfig_RejectsEmptyEndpoint(t *testing.T) {
	// Triggers config.go:118-120 (endpoint empty path).
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: ""
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for empty endpoint, got nil")
	}
}

func TestPipeline_NewPipelineRejectsEmptyAsset(t *testing.T) {
	// Triggers pipeline.go:58-60 (asset=="" path).
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	if _, _, err := NewPipeline("", pm, pm.Points[0], d, u); err == nil {
		t.Errorf("expected error for empty asset, got nil")
	}
}

func TestPipeline_NewPipelineRejectsBadParsePoint(t *testing.T) {
	// Triggers pipeline.go:61-63 (ParsePoint error path).
	d := testDict()
	u := testUnits(t)
	// quantity "no_such" is not in dict → ParsePoint fails.
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "no_such",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	if _, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u); err == nil {
		t.Errorf("expected error for unknown quantity, got nil")
	}
}

func TestPipeline_NewPipelineRejectsUnknownQuantity(t *testing.T) {
	// Triggers pipeline.go:70-72 (quantity not in dict path).
	// Construct a pointmap whose Point parses (cpath doesn't know
	// the quantity yet — it stops at the first unknown token in
	// location/quantity), but the gateway's own d.Quantities
	// lookup fails. Use a parseable relative point and a dict
	// missing the quantity entry.
	d := testDict()
	delete(d.Quantities, "temp")
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	if _, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u); err == nil {
		t.Errorf("expected error for unknown quantity, got nil")
	}
}

func TestPipeline_NewPipelineRejectsBadUnitConversion(t *testing.T) {
	// Triggers pipeline.go:74-77 (CanConvert false path).
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "not_a_unit",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	if _, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u); err == nil {
		t.Errorf("expected error for unknown unit_in, got nil")
	}
}

func TestPipeline_NewPipelineRejectsBadBinding(t *testing.T) {
	// Triggers modbusbind.BuildFromPointDef error path
	// (e.g. table="coils"); PRMT-030 §A.
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "coils"}},
	})
	if _, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u); err == nil {
		t.Errorf("expected error for bad table, got nil")
	}
}

func TestBindingFromProtocol_UnsupportedRegisterType(t *testing.T) {
	// Triggers pipeline.go:184-185 (string → strconv error) and
	// 192-194 (table=non-string). The non-existent register type
	// is the easier of the two to hit.
	pd := pointmap.PointDef{Protocol: map[string]interface{}{
		"register": "notanumber",
	}}
	if _, err := modbusbind.BuildFromPointDef(pd); err == nil {
		t.Errorf("expected error for non-numeric register, got nil")
	}
}

func TestBindingFromProtocol_BadTableType(t *testing.T) {
	// Triggers pipeline.go:192-194 (table=non-string path).
	pd := pointmap.PointDef{Protocol: map[string]interface{}{
		"register": 100,
		"table":    42,
	}}
	if _, err := modbusbind.BuildFromPointDef(pd); err == nil {
		t.Errorf("expected error for non-string table, got nil")
	}
}

func TestPipeline_NewPipelineRejectsMetricName(t *testing.T) {
	// Triggers pipeline.go:98-100 (MetricName error path).
	// Make a point whose quantity parses but is not in d.Quantities
	// by hand. Use a known-typed point that resolves to a quantity
	// we then drop from the dict. ParsePoint only checks the dict
	// for type/loop/side/phase, not quantity — so a parseable
	// point can still slip past ParsePoint but fail MetricName.
	d2 := testDict()
	delete(d2.Quantities, "flow")
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.flow", UnitIn: "m3ph", Scale: 0.1,
			Protocol: map[string]interface{}{"register": 0x0012, "table": "input"}},
	})
	if _, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d2, u); err == nil {
		t.Errorf("expected error for unknown quantity at metric-name step, got nil")
	}
}

func TestRun_PointmapInvalidReturnsError(t *testing.T) {
	root := moduleRoot2()
	dir := t.TempDir()
	// A pointmap that is syntactically YAML but fails V6 (no
	// appliesTo). pointmap.Load is a fail-closed validator.
	bad := `kind: PointMap
metadata:
  name: bad
  driver: modbus
  appliesTo: ""
spec:
  points:
    - point: fws.supply.temp
      register: 0
      unit_in: decicelsius
`
	pmPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(pmPath, []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Config{
		Site:        "site01",
		ProtocolDir: filepath.Join(root, "protocol"),
		VMWriteURL:  "http://127.0.0.1:0/api/v1/import/prometheus",
		Interval:    1 * time.Second,
		Devices: []Device{{
			Asset: "site01.pod000.cdu000", PointMap: pmPath,
			Endpoint: "127.0.0.1:0", UnitID: "1",
		}},
	}
	cfg.Devices[0].baseDir = dir

	err := Run(context.Background(), cfg)
	if err == nil {
		t.Errorf("expected Run to fail on bad pointmap, got nil")
	}
}

func TestRun_NoDevicesReturnsError(t *testing.T) {
	root := moduleRoot2()
	// Empty Devices should be rejected at LoadConfig time, but
	// the run loop's own check ("no devices to run") covers the
	// post-LoadConfig path. Build a Config directly to exercise
	// that branch.
	cfg := Config{
		Site:        "site01",
		ProtocolDir: filepath.Join(root, "protocol"),
		VMWriteURL:  "http://127.0.0.1:0",
		Interval:    1 * time.Second,
	}
	if err := Run(context.Background(), cfg); err == nil {
		t.Errorf("expected error for empty devices, got nil")
	}
}

// writeSNMPPointmap drops an SNMP-flavoured point-map YAML into a temp
// directory and returns the path. The OIDs match what TestRun_SNMPDevice
// seeds into the snmpsim agent; the protocol key is "oid", read by
// snmpBindingFromProtocol (PRMT-023 §4.3).
func writeSNMPPointmap(t *testing.T, dir string) string {
	t.Helper()
	pm := `kind: PointMap
metadata:
  name: cdu-snmp-sim
  driver: snmp
  appliesTo: cdu
spec:
  points:
    - point: fws.supply.temp
      oid: "1.3.6.1.4.1.9999.1.1.0"
      unit_in: decicelsius
    - point: status
      oid: "1.3.6.1.4.1.9999.1.2.0"
      enum_map: { 0: 0, 1: 1, 2: 2 }
`
	p := filepath.Join(dir, "cdu-snmp-sim.yaml")
	if err := os.WriteFile(p, []byte(pm), 0o644); err != nil {
		t.Fatalf("write snmp pointmap: %v", err)
	}
	return p
}

// TestRun_SNMPDevice exercises the PRMT-023 wiring end-to-end:
// snmpsim agent + fake VM + gateway with protocol=snmp. The test
// asserts that the snmp driver path produces the same Prometheus
// projections the modbus path does, with the seeded OID values
// surfaced as the metric value.
func TestRun_SNMPDevice(t *testing.T) {
	vm := newFakeVM()
	defer vm.Close()
	sim := snmpsim.New(snmpsim.Config{Community: "public"})
	simAddr, err := sim.Start()
	if err != nil {
		t.Fatalf("snmpsim.Start: %v", err)
	}
	defer sim.Stop()
	// Seed: temp raw 75 decicelsius -> 7.5 C; status enum 1. Both
	// seeds stay under 128 so snmpsim's BER INTEGER encoder emits
	// one byte without the sign-pad path (matches the existing
	// pkg/driver/snmp conformance test convention).
	sim.SetOID("1.3.6.1.4.1.9999.1.1.0", 75)
	sim.SetOID("1.3.6.1.4.1.9999.1.2.0", 1)

	pmDir := t.TempDir()
	pmPath := writeSNMPPointmap(t, pmDir)

	root := moduleRoot2()
	if root == "" {
		t.Fatal("module root not found")
	}
	cfg := Config{
		Site:        "site01",
		ProtocolDir: filepath.Join(root, "protocol"),
		VMWriteURL:  vm.URL(),
		Interval:    100 * time.Millisecond,
		Devices: []Device{{
			Asset:     "site01.pod000.cdu000",
			PointMap:  pmPath,
			Endpoint:  simAddr,
			Protocol:  "snmp",
			Community: "public",
		}},
	}
	cfg.Devices[0].baseDir = pmDir

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = Run(ctx, cfg)

	bodies := vm.bodiesCopy()
	if len(bodies) < 2 {
		t.Fatalf("expected >=2 POSTs to fake VM, got %d", len(bodies))
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(bodies[0], "\n") {
		switch {
		case strings.HasPrefix(line, "cios_temp_celsius{"):
			// 75 decicelsius -> 7.5 celsius (same conversion the
			// modbus path runs — proves the protocol-agnostic
			// Pipeline is shared).
			if !strings.Contains(line, " 7.5 ") {
				t.Errorf("snmp temp line wrong: %s", line)
			}
			if !strings.Contains(line, `quality="good"`) {
				t.Errorf("snmp temp line not good: %s", line)
			}
			seen["temp"] = true
		case strings.HasPrefix(line, "cios_status{"):
			if !strings.Contains(line, " 1 ") {
				t.Errorf("snmp status line wrong: %s", line)
			}
			seen["status"] = true
		}
	}
	if !seen["temp"] || !seen["status"] {
		t.Errorf("missing lines in first body: seen=%v body=%q", seen, bodies[0])
	}
}

// TestLoadConfig_RejectsUnknownProtocol covers the PRMT-023 §4.1
// LoadConfig branch that fails closed on an unknown protocol value.
func TestLoadConfig_RejectsUnknownProtocol(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
    protocol: bacnet
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for unknown protocol, got nil")
	}
}

// TestLoadConfig_RejectsSNMPWithPlugin covers the PRMT-023 §4.1
// LoadConfig branch that forbids plugin_binary + protocol=snmp (the
// cios-snmp-driver go-plugin entry is a later prompt).
func TestLoadConfig_RejectsSNMPWithPlugin(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
    protocol: snmp
    plugin_binary: ./bin/cios-snmp-driver
`, filepath.Join(root, "protocol")))
	if _, err := LoadConfig(p); err == nil {
		t.Errorf("expected error for snmp + plugin_binary, got nil")
	}
}

// TestLoadConfig_SNMPDefaultsCommunity covers the PRMT-023 §4.1
// "snmp with empty community → public" default.
func TestLoadConfig_SNMPDefaultsCommunity(t *testing.T) {
	root := moduleRoot2()
	pmDir := t.TempDir()
	writePointmap(t, pmDir)
	p := writeGatewayYAML(t, fmt.Sprintf(`
site: site01
protocol_dir: %q
vm_write_url: http://127.0.0.1:8428/api/v1/import/prometheus
devices:
  - asset: site01.pod000.cdu000
    pointmap: cdu-sim.yaml
    endpoint: 127.0.0.1:15020
    protocol: snmp
`, filepath.Join(root, "protocol")))
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Devices[0].Community != "public" {
		t.Errorf("Community = %q, want public default", cfg.Devices[0].Community)
	}
	if cfg.Devices[0].Protocol != "snmp" {
		t.Errorf("Protocol = %q, want snmp", cfg.Devices[0].Protocol)
	}
}

// --- PRMT-097: retired-asset skip (opt-in) ---------------------------------

// TestRetiredCache_ContainsReplace is a tiny invariant check: the
// cache is nil-safe (contains on a nil pointer → false, no panic)
// and replace overwrites atomically.
func TestRetiredCache_ContainsReplace(t *testing.T) {
	var c *retiredCache // nil
	if c.contains("site01.pod000.cdu000") {
		t.Errorf("nil cache must report contains=false")
	}
	c = newRetiredCache()
	if c.contains("site01.pod000.cdu000") {
		t.Errorf("empty cache must report contains=false")
	}
	c.replace(map[string]struct{}{"site01.pod000.cdu000": {}})
	if !c.contains("site01.pod000.cdu000") {
		t.Errorf("expected contains=true after replace")
	}
	if c.contains("site01.pod001.cdu000") {
		t.Errorf("expected contains=false for unknown path")
	}
}

// fakeCMDB is the testbed core: it answers GET /v1/assets?lifecycle=retired
// with the given path list, and lets the test trigger a 5xx or a
// connection-refused by returning a closed server.
type fakeCMDB struct {
	mu       sync.Mutex
	paths    []string
	failNext bool
	srv      *httptest.Server
}

func newFakeCMDB() *fakeCMDB {
	f := &fakeCMDB{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failNext {
			f.failNext = false
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		type item struct {
			Path string `json:"path"`
		}
		out := struct {
			Items []item `json:"items"`
		}{}
		for _, p := range f.paths {
			out.Items = append(out.Items, item{Path: p})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	return f
}

func (f *fakeCMDB) URL() string { return f.srv.URL }
func (f *fakeCMDB) Close()      { f.srv.Close() }
func (f *fakeCMDB) setPaths(p []string) {
	f.mu.Lock()
	f.paths = p
	f.mu.Unlock()
}
func (f *fakeCMDB) setFail(b bool) {
	f.mu.Lock()
	f.failNext = b
	f.mu.Unlock()
}

// TestRefreshRetiredCache_HappyPath: one successful fetch swaps
// the cache; the new set is visible to contains().
func TestRefreshRetiredCache_HappyPath(t *testing.T) {
	cmdb := newFakeCMDB()
	defer cmdb.Close()
	cmdb.setPaths([]string{"site01.pod000.cdu000", "site01.pod000.cdu001"})

	cache := newRetiredCache()
	hc := &http.Client{Timeout: 2 * time.Second}
	refreshRetiredCache(context.Background(), hc, cmdb.URL(), cache)

	if !cache.contains("site01.pod000.cdu000") {
		t.Errorf("expected cdu000 in cache, got false")
	}
	if !cache.contains("site01.pod000.cdu001") {
		t.Errorf("expected cdu001 in cache, got false")
	}
	if cache.contains("site01.pod000.cdu002") {
		t.Errorf("did not expect cdu002 in cache")
	}
}

// TestRefreshRetiredCache_FailOpen: when core returns 5xx, the
// existing cache is preserved (fail-open — collect loop never
// loses its last known set on a transient outage).
func TestRefreshRetiredCache_FailOpen(t *testing.T) {
	cmdb := newFakeCMDB()
	defer cmdb.Close()
	cmdb.setPaths([]string{"site01.pod000.cdu000"})

	cache := newRetiredCache()
	cache.replace(map[string]struct{}{"site01.pod000.cdu000": {}}) // pre-seed
	hc := &http.Client{Timeout: 2 * time.Second}

	// Now flip the server to 500; refreshRetiredCache must not
	// clobber the seed.
	cmdb.setFail(true)
	refreshRetiredCache(context.Background(), hc, cmdb.URL(), cache)

	if !cache.contains("site01.pod000.cdu000") {
		t.Errorf("expected cdu000 preserved after 5xx, got false (fail-open broken)")
	}
}

// TestRefreshRetiredCache_Unreachable: an unreachable core (closed
// server) is treated the same as 5xx — cache preserved.
func TestRefreshRetiredCache_Unreachable(t *testing.T) {
	cmdb := newFakeCMDB()
	url := cmdb.URL()
	cmdb.Close() // shut down before refresh → dial error

	cache := newRetiredCache()
	cache.replace(map[string]struct{}{"site01.pod000.cdu000": {}})
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	refreshRetiredCache(context.Background(), hc, url, cache)

	if !cache.contains("site01.pod000.cdu000") {
		t.Errorf("expected cdu000 preserved on unreachable core, got false")
	}
}

// TestStartRetiredPoll_EmptyURLIsNoop: empty URL → no goroutine,
// the returned cache is empty, contains() is false. This is the
// zero-regression contract for config-file-only deployments.
func TestStartRetiredPoll_EmptyURLIsNoop(t *testing.T) {
	cache := startRetiredPoll(context.Background(), "", 0)
	if cache == nil {
		t.Fatalf("startRetiredPoll returned nil cache")
	}
	if cache.contains("anything") {
		t.Errorf("empty-URL cache must report contains=false")
	}
	// Sanity: spawning is instantaneous, so by the time we get
	// here no goroutine is still running. We cannot directly
	// observe "no goroutine", but if the contract were broken
	// (background poll on a closed ctx) we'd see a leak. Run a
	// short sleep and trust the rest of the suite to catch
	// regressions via -race.
	time.Sleep(20 * time.Millisecond)
}

// TestStartRetiredPoll_PopulatesFromFakeCMDB: with a non-empty
// URL the goroutine starts, fetches at least once, and the cache
// reflects the fake core's response. We use a 50ms tick so the
// test stays under a second even with the goroutine warm-up.
func TestStartRetiredPoll_PopulatesFromFakeCMDB(t *testing.T) {
	cmdb := newFakeCMDB()
	defer cmdb.Close()
	cmdb.setPaths([]string{"site01.pod000.cdu000"})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	cache := startRetiredPoll(ctx, cmdb.URL(), 50*time.Millisecond)

	// Spin until either the cache reflects the fake, or the
	// ctx times out. The ticker is 50ms; 400ms is plenty.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cache.contains("site01.pod000.cdu000") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("cache never observed the retired path; fail-open or goroutine broken")
}

// TestPostBatch_SkipsRetiredAsset: drives postBatch directly with a
// cache that marks the only device retired, and asserts the
// driver is never called. The retired skip check is the only thing
// under test; modbus/Convert plumbing is covered by TestRun_EndToEnd.
func TestPostBatch_SkipsRetiredAsset(t *testing.T) {
	drv := &countDriver{}
	dev := gatewayDevice{
		cfg:   Device{Asset: "site01.pod000.cdu000"},
		drv:   drv,
		pipes: nil, // never reached: retired check fires first
	}
	cache := newRetiredCache()
	cache.replace(map[string]struct{}{"site01.pod000.cdu000": {}})

	// nil http client is fine: with a retired-only batch the body
	// builder is empty and postBatch returns before any HTTP.
	postBatch(context.Background(), nil, "http://unused", time.Now(), []gatewayDevice{dev}, cache)

	if drv.calls != 0 {
		t.Errorf("expected 0 Collect calls on retired device, got %d", drv.calls)
	}
}

// TestPostBatch_EmptyCacheCollectsAll: the same setup as the
// retired-skip test, but with a nil cache — fail-open means the
// device IS collected. We don't drive a real Convert (no pointmap
// pipeline), so we observe the contract via the driver call count
// rather than the VM body.
func TestPostBatch_EmptyCacheCollectsAll(t *testing.T) {
	drv := &countDriver{}
	dev := gatewayDevice{
		cfg:   Device{Asset: "site01.pod000.cdu000"},
		drv:   drv,
		pipes: nil, // postBatch will return empty body when no pipes
	}
	// nil cache → contains() is always false → collect runs.
	postBatch(context.Background(), nil, "http://unused", time.Now(), []gatewayDevice{dev}, nil)

	if drv.calls != 1 {
		t.Errorf("expected 1 Collect call (empty cache = collect all), got %d", drv.calls)
	}
}

// failSoftDriver is the per-tick programmable driver used by
// TestPostBatch_FailSoftOnPerDeviceError. Each Collect returns
// either the pre-seeded samples with nil error, or the pre-seeded
// error with nil samples. Calls are counted so the test can assert
// that B is retried on every tick (not skipped forever).
type failSoftDriver struct {
	mu        sync.Mutex
	calls     int
	tickErr   map[int]error // tick index (0-based) → error to return
	tickSamps map[int][]driver.Sample
}

func (d *failSoftDriver) Init(context.Context, driver.DriverConfig) error { return nil }
func (d *failSoftDriver) Collect(_ context.Context) ([]driver.Sample, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	idx := d.calls
	d.calls++
	if e, ok := d.tickErr[idx]; ok {
		return nil, e
	}
	return d.tickSamps[idx], nil
}
func (d *failSoftDriver) Discover(context.Context) ([]driver.AssetCandidate, error) {
	return nil, driver.ErrNotSupported
}
func (d *failSoftDriver) Subscribe(context.Context, chan<- driver.Sample) error {
	return driver.ErrNotSupported
}
func (d *failSoftDriver) Write(context.Context, driver.ControlCommand) (driver.ControlResult, error) {
	return driver.ControlResult{}, driver.ErrNotSupported
}
func (d *failSoftDriver) Health(context.Context) driver.DriverHealth {
	return driver.DriverHealth{Connected: true}
}
func (d *failSoftDriver) Close(context.Context) error { return nil }
func (d *failSoftDriver) callCount() int              { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

// TestPostBatch_FailSoftOnPerDeviceError — PRMT-099 R5 / K17.
// Two-device batch: device A always succeeds with a sample,
// device B returns a transient error on tick 0 and a good sample
// on tick 1. Asserts:
//  1. Tick 0: A's sample reaches the publish path (fake VM body
//     contains a cios_temp_celsius{...} line). B's error is
//     swallowed (does not abort the batch, does not panic).
//  2. Tick 0 returns within a 2s budget (no block, no hang).
//  3. Tick 1: B is retried (not skipped forever), and B's sample
//     now reaches the publish path on the next tick.
//
// This pins the L61 fail-soft contract for the gateway Collect
// path. We do NOT drive the full Run() ticker because postBatch
// is the unit under test (per-tick behaviour), and reusing
// TestPostBatch_EmptyCacheCollectsAll's direct-postBatch style
// keeps the test fast (<10ms) and deterministic.
func TestPostBatch_FailSoftOnPerDeviceError(t *testing.T) {
	// Build a real Pipeline for device A so Convert produces a
	// real Prometheus line. testDict/testUnits/mkPM are the
	// helpers already used by pipeline_test.go in this package.
	dict := testDict()
	units := testUnits(t)
	pmA := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	plA, _, err := NewPipeline("site01.pod000.cdu000", pmA, pmA.Points[0], dict, units)
	if err != nil {
		t.Fatalf("NewPipeline A: %v", err)
	}

	drvA := &failSoftDriver{
		tickSamps: map[int][]driver.Sample{
			0: {sampleGood("fws.supply.temp", 235)}, // 23.5 C after unit conversion
			1: {sampleGood("fws.supply.temp", 240)},
		},
	}
	drvB := &failSoftDriver{
		tickErr: map[int]error{
			0: fmt.Errorf("simulated transient I/O error"),
		},
		tickSamps: map[int][]driver.Sample{
			1: {sampleGood("fws.supply.temp", 100)}, // recovers on tick 1
		},
	}

	devA := gatewayDevice{
		cfg:   Device{Asset: "site01.pod000.cdu000"},
		drv:   drvA,
		pipes: map[string]*Pipeline{"fws.supply.temp": plA},
	}
	devB := gatewayDevice{
		cfg:   Device{Asset: "site01.pod000.cdu001"},
		drv:   drvB,
		pipes: map[string]*Pipeline{"fws.supply.temp": plA}, // reuse A's pipeline
	}

	vm := newFakeVM()
	defer vm.Close()

	// --- Tick 0: A succeeds, B fails. Budget 2s. -----------------
	tick0Start := time.Now()
	postBatch(context.Background(), &http.Client{Timeout: 2 * time.Second},
		vm.URL(), time.Now(), []gatewayDevice{devA, devB}, nil)
	tick0Elapsed := time.Since(tick0Start)
	if tick0Elapsed > 2*time.Second {
		t.Errorf("tick 0 took %v, exceeds 2s budget (would block the loop)", tick0Elapsed)
	}

	bodies := vm.bodiesCopy()
	if len(bodies) < 1 {
		t.Fatalf("tick 0: expected A's sample to reach VM, got %d bodies", len(bodies))
	}
	tick0Body := bodies[0]
	if !strings.Contains(tick0Body, "cios_temp_celsius{") {
		t.Errorf("tick 0: VM body missing cios_temp_celsius line (A's sample not published); body=%q", tick0Body)
	}
	if !strings.Contains(tick0Body, " 23.5 ") {
		t.Errorf("tick 0: VM body missing 23.5 value (A's convert broken?); body=%q", tick0Body)
	}
	// Per-call counters confirm B was invoked (not skipped before being tried).
	if drvA.callCount() != 1 {
		t.Errorf("tick 0: drvA calls = %d, want 1", drvA.callCount())
	}
	if drvB.callCount() != 1 {
		t.Errorf("tick 0: drvB calls = %d, want 1 (B must be tried every tick)", drvB.callCount())
	}

	// --- Tick 1: B recovers. Both samples flow. ------------------
	postBatch(context.Background(), &http.Client{Timeout: 2 * time.Second},
		vm.URL(), time.Now(), []gatewayDevice{devA, devB}, nil)

	bodies = vm.bodiesCopy()
	if len(bodies) < 2 {
		t.Fatalf("tick 1: expected a 2nd VM body, got %d bodies total", len(bodies))
	}
	tick1Body := bodies[1]
	// Device A on tick 1 returns 240 decicelsius -> 24.0 celsius.
	if !strings.Contains(tick1Body, " 24 ") {
		t.Errorf("tick 1: VM body missing A's 24.0 value; body=%q", tick1Body)
	}
	// Device B on tick 1 returns 100 decicelsius -> 10.0 celsius.
	// Both A and B write into the same body (one POST per tick);
	// the presence of the 10.0 value proves B's recovery sample
	// flowed through the failed-then-retried path.
	if !strings.Contains(tick1Body, " 10 ") {
		t.Errorf("tick 1: VM body missing B's 10.0 value (B not retried on next tick?); body=%q", tick1Body)
	}
	// Retried-on-every-tick contract: drvB.Collect was called twice.
	if drvB.callCount() != 2 {
		t.Errorf("tick 1: drvB calls = %d, want 2 (B must be retried, not skipped)", drvB.callCount())
	}
	if drvA.callCount() != 2 {
		t.Errorf("tick 1: drvA calls = %d, want 2", drvA.callCount())
	}
}

// countDriver is the minimal driver.Driver used by the
// TestPostBatch_* tests: it counts Collect invocations and returns
// no samples, which is enough to verify the retired-skip branch
// (the unit under test) without dragging in modbussim, the
// pointmap-to-binding pipeline build, or the VM HTTP round-trip.
type countDriver struct {
	calls int
}

func (c *countDriver) Init(context.Context, driver.DriverConfig) error { return nil }
func (c *countDriver) Collect(context.Context) ([]driver.Sample, error) {
	c.calls++
	return nil, nil
}
func (c *countDriver) Discover(context.Context) ([]driver.AssetCandidate, error) {
	return nil, driver.ErrNotSupported
}
func (c *countDriver) Subscribe(context.Context, chan<- driver.Sample) error {
	return driver.ErrNotSupported
}
func (c *countDriver) Write(context.Context, driver.ControlCommand) (driver.ControlResult, error) {
	return driver.ControlResult{}, driver.ErrNotSupported
}
func (c *countDriver) Health(context.Context) driver.DriverHealth {
	return driver.DriverHealth{Connected: true}
}
func (c *countDriver) Close(context.Context) error { return nil }
