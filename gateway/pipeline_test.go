package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/modbusbind"
	"github.com/yurimeng/cios/pkg/pointmap"
)

// testDict is the same minimal dict the promproj tests use, kept
// local so gateway tests don't need the real protocol/ directory.
// The pointmap pipeline tests are about the L1 conversion order
// (scale -> enum -> unit -> limits -> projection), not the
// dictionary itself, so a small focused dict is fine.
func testDict() *cpath.Dict {
	return &cpath.Dict{
		Types: map[string]cpath.TypeDef{
			"site": {Parents: nil, Level: cpath.LevelDevice},
			"pod":  {Parents: []string{"site"}, Level: cpath.LevelDevice},
			"cdu":  {Parents: []string{"pod"}, Level: cpath.LevelDevice},
		},
		Quantities: map[string]cpath.QuantityDef{
			"temp":    {Unit: "celsius", Kind: "gauge"},
			"flow":    {Unit: "lpm", Kind: "gauge"},
			"opening": {Unit: "percent", Kind: "gauge"},
			"status": {Unit: "enum", Kind: "gauge",
				Enum: map[int]string{0: "ok", 1: "warning", 2: "fault"}},
			"leak": {Unit: "enum", Kind: "gauge",
				Enum: map[int]string{0: "dry", 1: "leak"}},
		},
		Derived: map[string]cpath.QuantityDef{},
		Loops:   map[string]bool{"fws": true, "tcs": true},
		Sides:   map[string]bool{"supply": true, "return": true},
		Phases:  map[string]bool{},
		Domains: map[string][]string{"computing": {"pod"}, "cooling": {"cdu"}},
	}
}

// testUnits loads the real protocol/units.yaml so tests exercise the
// same conversion table the gateway uses at runtime. The dict
// (testDict above) is a small in-memory hand-built version, but
// the unit table is too involved to hand-build per-test without
// diverging from production.
func testUnits(t *testing.T) *pointmap.Units {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	u, err := pointmap.LoadUnits(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	return u
}

// moduleRoot walks up from the current working directory until it
// finds go.mod, then returns the directory containing it. This is
// stable under `go test ./gateway/`, `go test ./...`, and direct
// invocation — none of which guarantees a useful value from
// runtime.Caller.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// mkPM builds a minimal point map for one test. The relative point
// address is enough — the absolute path is reconstructed by the
// pipeline. The Protocol map is what the gateway reads.
//
// We default Scale to 1.0 here (matching pointmap.decodePoint) so a
// test that doesn't think about scale still gets a realistic
// pipeline. mkPM is a TEST helper; it must not lie about what
// pointmap.Load would produce.
func mkPM(appliesTo string, pts []pointmap.PointDef) *pointmap.PointMap {
	for i := range pts {
		if pts[i].Scale == 0 {
			pts[i].Scale = 1.0
		}
	}
	return &pointmap.PointMap{
		Name:      "test-pm",
		Driver:    "modbus",
		AppliesTo: appliesTo,
		Points:    pts,
	}
}

func sampleGood(point string, val float64) driver.Sample {
	return driver.Sample{Point: point, Value: val, Ts: time.UnixMilli(1), Quality: driver.QualityGood}
}

func sampleSuspect(point string, val float64) driver.Sample {
	return driver.Sample{Point: point, Value: val, Ts: time.UnixMilli(1), Quality: driver.QualitySuspect}
}

func TestPipeline_DecicelsiusToCelsius(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	pl, b, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	// Register and table must round-trip.
	if b.Register != 0x0010 || b.Table != "input" {
		t.Errorf("binding = %+v", b)
	}
	// Raw 235 decicelsius -> 23.5 celsius.
	line, err := pl.Convert(sampleGood("fws.supply.temp", 235))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, " 23.5 ") {
		t.Errorf("line missing value 23.5: %s", line)
	}
	if !strings.Contains(line, "cios_temp_celsius{") {
		t.Errorf("line missing metric cios_temp_celsius: %s", line)
	}
}

func TestPipeline_M3phToLpmWithScale(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.flow", UnitIn: "m3ph", Scale: 0.1,
			Protocol: map[string]interface{}{"register": 0x0012, "table": "input"}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	// Vendor sends a single uint16, gateway scales by 0.1 first
	// to get m3ph, then converts to lpm with the 16.666667 factor.
	// 100 raw -> 10 m3ph -> 166.66667 lpm.
	line, err := pl.Convert(sampleGood("fws.supply.flow", 100))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, " 166.66667 ") {
		t.Errorf("line missing value 166.66667: %s", line)
	}
}

func TestPipeline_EnumMapTranslatesAndKeepsQuality(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "status",
			Protocol: map[string]interface{}{"register": 0x0030, "table": "input"},
			EnumMap:  map[int]int{0: 0, 1: 1, 16: 2}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	// Vendor code 16 -> standard code 2.
	line, err := pl.Convert(sampleGood("status", 16))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, " 2 ") {
		t.Errorf("line missing mapped value 2: %s", line)
	}
	if !strings.Contains(line, `quality="good"`) {
		t.Errorf("line missing quality=good: %s", line)
	}
	if !strings.HasPrefix(line, "cios_status{") {
		t.Errorf("enum-typed quantity should drop _enum suffix: %s", line)
	}
}

func TestPipeline_EnumValueOutsideMapIsSuspect(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "leak",
			Protocol: map[string]interface{}{"register": 0x0031, "table": "input"},
			EnumMap:  map[int]int{0: 0, 1: 1}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	// Vendor code 99 is not in the enum_map — must downgrade to
	// suspect but still emit the line (L1).
	line, err := pl.Convert(sampleGood("leak", 99))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="suspect"`) {
		t.Errorf("line missing quality=suspect: %s", line)
	}
}

func TestPipeline_EnumNoMap_InsideStdVocabIsGood(t *testing.T) {
	// C-修复回归：quantity 字典里 leak 的标准枚举是 {0:"dry", 1:"leak"}。
	// 如果一个点位没声明 enum_map（vendor 直接发标准代码），
	// raw=1 应判为 good；这覆盖了 PRMT-009 stdSimConfig 的实际行为。
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "leak",
			Protocol: map[string]interface{}{"register": 0x0031, "table": "input"}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	line, err := pl.Convert(sampleGood("leak", 1))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="good"`) {
		t.Errorf("enum no-map, std vocab: line should be good, got: %s", line)
	}
}

func TestPipeline_EnumMapTranslatesToOutsideStdVocabIsSuspect(t *testing.T) {
	// C-修复回归：enum_map 把 vendor 16 翻成标准码 9，但 qDef.Enum
	// 字典里没有 9 → suspect（即使 enum_map 自身是合法映射）。
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "leak",
			Protocol: map[string]interface{}{"register": 0x0031, "table": "input"},
			EnumMap:  map[int]int{16: 9}}, // maps to a code NOT in {0,1}
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	line, err := pl.Convert(sampleGood("leak", 16))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="suspect"`) {
		t.Errorf("enum_map→std-outside-vocab: line should be suspect, got: %s", line)
	}
}

func TestPipeline_LimitsViolationIsSuspect(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "tcs.opening",
			Access: "rw", RiskClass: "a",
			Limits:   &pointmap.Limits{Min: 0, Max: 100},
			Protocol: map[string]interface{}{"register": 0x0020, "table": "holding"}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	// opening is percent, no unit_in, identity conv: raw == std.
	// 250 violates limits 0..100 -> suspect.
	line, err := pl.Convert(sampleGood("tcs.opening", 250))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="suspect"`) {
		t.Errorf("line missing quality=suspect: %s", line)
	}
	// Value 50 stays good.
	line, err = pl.Convert(sampleGood("tcs.opening", 50))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="good"`) {
		t.Errorf("line missing quality=good: %s", line)
	}
}

func TestPipeline_DriverSuspectPassesThrough(t *testing.T) {
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	pl, _, err := NewPipeline("site01.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	line, err := pl.Convert(sampleSuspect("fws.supply.temp", 0))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `quality="suspect"`) {
		t.Errorf("line missing quality=suspect: %s", line)
	}
}

func TestPipeline_AssetDrivenPath_NotZeroIndexed(t *testing.T) {
	// A-修复回归：原实现从 pm.AppliesTo 硬拼 "site01." + 全零索引
	// 链，与配置的 Device.Asset 无关。这条测试用非零索引、非
	// site01 站点验证：asset 完整地进入 path/标签。
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	pl, _, err := NewPipeline("site01.pod002.cdu001", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	line, err := pl.Convert(sampleGood("fws.supply.temp", 235))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, want := range []string{
		`site="site01"`,
		`pod="pod002"`,
		`cdu="cdu001"`,
		`path="site01.pod002.cdu001"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q: %s", want, line)
		}
	}
}

func TestPipeline_AssetDrivenPath_Site02(t *testing.T) {
	// A-修复回归：site 标签必须跟随配置而非硬编码 site01。
	d := testDict()
	u := testUnits(t)
	pm := mkPM("cdu", []pointmap.PointDef{
		{Point: "fws.supply.temp", UnitIn: "decicelsius",
			Protocol: map[string]interface{}{"register": 0x0010, "table": "input"}},
	})
	pl, _, err := NewPipeline("site02.pod000.cdu000", pm, pm.Points[0], d, u)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	line, err := pl.Convert(sampleGood("fws.supply.temp", 235))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(line, `site="site02"`) {
		t.Errorf("line missing site=site02: %s", line)
	}
	if !strings.Contains(line, `path="site02.pod000.cdu000"`) {
		t.Errorf("line missing path=site02...: %s", line)
	}
}

func TestBindingFromProtocol_TableValidation(t *testing.T) {
	cases := []struct {
		name    string
		proto   map[string]interface{}
		wantReg uint16
		wantTab string
		wantErr bool
	}{
		{"default holding", map[string]interface{}{"register": 100}, 100, "holding", false},
		{"input table", map[string]interface{}{"register": 100, "table": "input"}, 100, "input", false},
		{"bad table", map[string]interface{}{"register": 100, "table": "coils"}, 0, "", true},
		{"missing register", map[string]interface{}{"table": "holding"}, 0, "", true},
		{"string register", map[string]interface{}{"register": "30021"}, 30021, "holding", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pd := pointmap.PointDef{Protocol: tc.proto}
			b, err := modbusbind.BuildFromPointDef(pd)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if b.Register != tc.wantReg {
					t.Errorf("Register=%d want %d", b.Register, tc.wantReg)
				}
				if b.Table != tc.wantTab {
					t.Errorf("Table=%q want %q", b.Table, tc.wantTab)
				}
				if b.Writable {
					t.Errorf("Writable=true, want false (M1 will flip it)")
				}
			}
		})
	}
}
