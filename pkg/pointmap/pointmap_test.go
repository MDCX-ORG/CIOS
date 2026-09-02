package pointmap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/cpath"
)

const protoDir = "../../protocol"

func loadDict(t *testing.T) *cpath.Dict {
	t.Helper()
	d, err := cpath.LoadDict(protoDir)
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	return d
}

func loadUnits(t *testing.T) *Units {
	t.Helper()
	u, err := LoadUnits(protoDir)
	if err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	return u
}

// --- 合法样例：cdu-sim-m1 通过校验且字段逐项断言 -------------------

func TestLoadValid(t *testing.T) {
	d := loadDict(t)
	u := loadUnits(t)
	pm, errs := Load(filepath.Join("testdata", "cdu-sim-m1.yaml"), d, u)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got %d: %v", len(errs), errs)
	}
	if pm == nil {
		t.Fatal("pm is nil")
	}
	if pm.Name != "cdu-sim-m1" {
		t.Errorf("Name = %q", pm.Name)
	}
	if pm.Driver != "modbus-sim" {
		t.Errorf("Driver = %q", pm.Driver)
	}
	if pm.AppliesTo != "cdu" {
		t.Errorf("AppliesTo = %q", pm.AppliesTo)
	}
	if len(pm.Points) != 6 {
		t.Fatalf("len(Points) = %d, want 6", len(pm.Points))
	}

	// fws.supply.flow: register/type/scale preserved; Access/Scale/UnitIn defaults
	p0 := pm.Points[0]
	if p0.Point != "fws.supply.flow" {
		t.Errorf("Points[0].Point = %q", p0.Point)
	}
	if p0.Access != "ro" {
		t.Errorf("Points[0].Access default = %q, want ro", p0.Access)
	}
	if p0.Scale != 0.1 {
		t.Errorf("Points[0].Scale = %v, want 0.1", p0.Scale)
	}
	if p0.UnitIn != "m3ph" {
		t.Errorf("Points[0].UnitIn = %q", p0.UnitIn)
	}
	if reg, _ := p0.Protocol["register"].(int); reg != 30021 {
		t.Errorf("Points[0].Protocol[register] = %v, want 30021", p0.Protocol["register"])
	}
	if typ, _ := p0.Protocol["type"].(string); typ != "holding, float32, be" {
		t.Errorf("Points[0].Protocol[type] = %q", typ)
	}

	// fws.return.temp: no scale -> default 1.0
	p2 := pm.Points[2]
	if p2.Scale != 1.0 {
		t.Errorf("Points[2].Scale default = %v, want 1.0", p2.Scale)
	}

	// tcs.opening: rw class-b with limits (P722 / L108 secondary valve)
	p3 := pm.Points[3]
	if p3.Access != "rw" {
		t.Errorf("Points[3].Access = %q", p3.Access)
	}
	if p3.RiskClass != "b" {
		t.Errorf("Points[3].RiskClass = %q", p3.RiskClass)
	}
	if p3.Limits == nil || p3.Limits.Min != 0 || p3.Limits.Max != 100 {
		t.Errorf("Points[3].Limits = %+v", p3.Limits)
	}

	// status: enum_map preserved as int keys
	p4 := pm.Points[4]
	if got := p4.EnumMap[1]; got != 0 {
		t.Errorf("EnumMap[1] = %d, want 0", got)
	}
	if got := p4.EnumMap[16]; got != 3 {
		t.Errorf("EnumMap[16] = %d, want 3", got)
	}
}

// --- 7 条 V 规则各 1 条非法样例 -------------------------------------

func TestLoadInvalidV1(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v1-missing-side.yaml", d, u)
	assertHasRule(t, errs, "V1")
}

func TestLoadInvalidV2(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v2-bad-unit.yaml", d, u)
	assertHasRule(t, errs, "V2")
}

func TestLoadInvalidV3(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v3-bad-enum.yaml", d, u)
	assertHasRule(t, errs, "V3")
}

func TestLoadInvalidV4(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v4-rw-no-risk.yaml", d, u)
	assertHasRule(t, errs, "V4")
}

func TestLoadInvalidV5(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v5-rw-no-readback.yaml", d, u)
	assertHasRule(t, errs, "V5")
}

func TestLoadInvalidV6(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v6-dup-point.yaml", d, u)
	assertHasRule(t, errs, "V6")
}

func TestLoadInvalidV7(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/v7-derived.yaml", d, u)
	assertHasRule(t, errs, "V7")
}

// --- 多错样例：≥3 条不同规则 ---------------------------------------

func TestLoadMultiErrors(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("testdata/invalid/multi.yaml", d, u)
	if len(errs) < 3 {
		t.Fatalf("expected >=3 errors, got %d: %v", len(errs), errs)
	}
	got := map[string]bool{}
	for _, e := range errs {
		if !errors.Is(e, ErrPointMap) {
			t.Errorf("err %v is not ErrPointMap", e)
		}
		for _, r := range []string{"V1", "V2", "V3", "V4", "V5", "V6", "V7"} {
			if strings.Contains(e.Error(), r+" ") {
				got[r] = true
			}
		}
	}
	for _, r := range []string{"V1", "V5", "V7"} {
		if !got[r] {
			t.Errorf("expected %s among multi errors, got rules %v", r, got)
		}
	}
}

// --- Load error paths: kind mismatch / read error / decode error / apply V2/V3/V4-V7 sub-paths ---

func TestLoadKindMismatch(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	bad := writeYAML(t, "tmp-kind.yaml", `
kind: NotPointMap
metadata:
  name: bad-kind
  driver: d
  appliesTo: cdu
spec:
  points: []
`)
	_, errs := Load(bad, d, u)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "kind must be PointMap") {
		t.Errorf("err = %v, want kind mismatch", errs[0])
	}
}

func TestLoadReadError(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	_, errs := Load("/nonexistent/pointmap.yaml", d, u)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "read") {
		t.Errorf("err = %v, want read error", errs[0])
	}
}

func TestLoadScaleParseError(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	bad := writeYAML(t, "tmp-scale.yaml", `
kind: PointMap
metadata:
  name: bad-scale
  driver: d
  appliesTo: cdu
spec:
  points:
    - point: fws.supply.flow
      register: 1
      scale: not-a-number
`)
	_, errs := Load(bad, d, u)
	if len(errs) == 0 {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(errs[0].Error(), "scale") {
		t.Errorf("err = %v, want mention of scale", errs[0])
	}
}

func TestLoadAppliesToUnknownType(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	bad := writeYAML(t, "tmp-unknown.yaml", `
kind: PointMap
metadata:
  name: unknown-type
  driver: d
  appliesTo: nosuchtype
spec:
  points:
    - point: fws.supply.flow
      register: 1
`)
	_, errs := Load(bad, d, u)
	assertHasRule(t, errs, "V6")
}

func TestLoadAppliesToEmptyName(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	bad := writeYAML(t, "tmp-noname.yaml", `
kind: PointMap
metadata:
  name: ""
  driver: d
  appliesTo: cdu
spec:
  points: []
`)
	_, errs := Load(bad, d, u)
	assertHasRule(t, errs, "V6")
}

func TestQuantityStandardUnitDerived(t *testing.T) {
	d := loadDict(t)
	std, kind := quantityStandardUnit("pue", d)
	if std == "" || kind != "derived" {
		t.Errorf("pue: got (%q,%q), want non-empty std and kind=derived", std, kind)
	}
	std, kind = quantityStandardUnit("nosuchqty", d)
	if std != "" {
		t.Errorf("nosuchqty: got std=%q, want empty", std)
	}
}

func TestIndexForUnknownType(t *testing.T) {
	d := loadDict(t)
	if got := indexFor("nosuchtype", d); got != "" {
		t.Errorf("indexFor(unknown) = %q, want empty", got)
	}
}

func TestLoadUnitsReadError(t *testing.T) {
	_, err := LoadUnits("/nonexistent/dir")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// --- R1 regression lock: Load behaves identically for a pointmap YAML
// copied to a directory outside the repo tree. Before R1, the V3 enum
// check used findQuantitiesYAML to walk up from the YAML's path looking
// for protocol/quantities.yaml, so a pointmap file living in /tmp
// (or, in production, a vendor model package directory) would silently
// lose its V3 enforcement. With Enum now in cpath.Dict, the validation
// outcome must depend only on the Dict handed in — not on filesystem
// search. We exercise both the green path (cdu-sim-m1) and a V3
// violation (v3-bad-enum) to pin the behavior in both directions.

func TestLoadFromOutsideRepo(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)

	cases := []struct {
		name        string
		src         string // file under pkg/pointmap/testdata
		wantErrRule string // "" → expect no errors
	}{
		{"valid", "cdu-sim-m1.yaml", ""},
		{"v3-bad-enum", "invalid/v3-bad-enum.yaml", "V3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Copy the source YAML out of the repo (t.TempDir lives
			// under the test runner's tmp, well outside the repo).
			src, err := os.ReadFile(filepath.Join("testdata", c.src))
			if err != nil {
				t.Fatalf("read src: %v", err)
			}
			out := filepath.Join(t.TempDir(), c.src)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(out, src, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, errs := Load(out, d, u)
			if c.wantErrRule == "" {
				if len(errs) > 0 {
					t.Errorf("expected no errors, got %d: %v", len(errs), errs)
				}
				return
			}
			assertHasRule(t, errs, c.wantErrRule)
		})
	}
}

// --- V1 合成路径：cdu / cell 两种父链 -------------------------------

// We can't directly observe the absolute path from the public API, so we
// drive Load on a minimal point map whose V1 violation would only fire
// if the prefix were wrong. cell requires 4 levels (bess > battery >
// cell); cdu requires 2 (pod > cdu). Easiest: load a YAML that succeeds
// V1 only if the prefix is right, then mutate the point to something
// syntactically valid only with the correct prefix. We exercise both
// chains indirectly through TestLoadValidPrefixes below, using a tiny
// custom point map.
func TestLoadValidPrefixes(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)

	// appliesTo=cdu: prefix site01.pod000.cdu000.
	//   point: fws.supply.flow → site01.pod000.cdu000.fws.supply.flow
	//   = valid. point: fws.temp → V1 fail (L47).
	cduOK := writeYAML(t, "tmp-cdu-ok.yaml", `
kind: PointMap
metadata:
  name: cdu-ok
  driver: d
  appliesTo: cdu
spec:
  points:
    - point: fws.supply.flow
      register: 1
`)
	_, errs := Load(cduOK, d, u)
	if len(errs) > 0 {
		t.Errorf("cdu prefix flow: unexpected errors %v", errs)
	}
	cduBad := writeYAML(t, "tmp-cdu-bad.yaml", `
kind: PointMap
metadata:
  name: cdu-bad
  driver: d
  appliesTo: cdu
spec:
  points:
    - point: fws.temp
      register: 1
`)
	_, errs = Load(cduBad, d, u)
	assertHasRule(t, errs, "V1")

	// appliesTo=cell: prefix site01.bess000.battery000.cell0.
	//   point: resistance → site01.bess000.battery000.cell0.resistance
	cellOK := writeYAML(t, "tmp-cell-ok.yaml", `
kind: PointMap
metadata:
  name: cell-ok
  driver: d
  appliesTo: cell
spec:
  points:
    - point: resistance
      register: 1
`)
	_, errs = Load(cellOK, d, u)
	if len(errs) > 0 {
		t.Errorf("cell prefix resistance: unexpected errors %v", errs)
	}
}

// --- Coverage helpers: exercise the leaf helpers (toInt, minMax,
// flattenMap, toFloat) on non-map / non-int inputs that Load only
// touches on the error path. These tests pin the public contract
// of the helpers and lift coverage above the 85% gate.

func TestToInt(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int
		ok   bool
	}{
		{int(1), 1, true},
		{int64(2), 2, true},
		{float64(3.0), 3, true},
		{float64(1.5), 1, true}, // truncates toward zero (Go int conversion)
		{"4", 4, true},
		{"abc", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("toInt(%v) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestToFloat(t *testing.T) {
	if _, err := toFloat(float64(1.5)); err != nil {
		t.Errorf("toFloat(float64) err: %v", err)
	}
	if _, err := toFloat(int(2)); err != nil {
		t.Errorf("toFloat(int) err: %v", err)
	}
	if _, err := toFloat(int64(3)); err != nil {
		t.Errorf("toFloat(int64) err: %v", err)
	}
	if _, err := toFloat("4.5"); err != nil {
		t.Errorf("toFloat(string) err: %v", err)
	}
	if _, err := toFloat(nil); err == nil {
		t.Error("toFloat(nil) expected error, got nil")
	}
	if _, err := toFloat(true); err == nil {
		t.Error("toFloat(bool) expected error, got nil")
	}
}

func TestFlattenMap(t *testing.T) {
	// map[string]interface{} form
	got := flattenMap(map[string]interface{}{"a": 1, "b": 2})
	if len(got) != 4 {
		t.Errorf("flattenMap[string]I] = %d entries, want 4", len(got))
	}
	// map[interface{}]interface{} form (yaml's int-keyed flow-map shape)
	got = flattenMap(map[interface{}]interface{}{"a": 1, "b": 2})
	if len(got) != 4 {
		t.Errorf("flattenMap[interface{}] = %d entries, want 4", len(got))
	}
	// Non-map input → nil
	if got := flattenMap("not a map"); got != nil {
		t.Errorf("flattenMap(string) = %v, want nil", got)
	}
}

func TestMinMaxInvalidKey(t *testing.T) {
	// limits with a non-numeric min triggers toFloat's error path.
	_, err := minMax(map[string]interface{}{"min": "abc", "max": 10})
	if err == nil {
		t.Error("minMax(invalid min) expected error, got nil")
	}
	// Non-mapping input → error
	_, err = minMax("not a map")
	if err == nil {
		t.Error("minMax(string) expected error, got nil")
	}
}

func TestDecodeEnumMapInvalid(t *testing.T) {
	// Non-int value triggers toInt failure
	_, err := decodeEnumMap(map[string]interface{}{"1": "abc"})
	if err == nil {
		t.Error("decodeEnumMap(non-int value) expected error, got nil")
	}
	// Non-int key triggers toInt failure
	_, err = decodeEnumMap(map[string]interface{}{"abc": 1})
	if err == nil {
		t.Error("decodeEnumMap(non-int key) expected error, got nil")
	}
	// Non-map input → error
	_, err = decodeEnumMap("not a map")
	if err == nil {
		t.Error("decodeEnumMap(string) expected error, got nil")
	}
}

func TestLoadLimitsParseError(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	bad := writeYAML(t, "tmp-bad-limits.yaml", `
kind: PointMap
metadata:
  name: bad-limits
  driver: d
  appliesTo: cdu
spec:
  points:
    - point: tcs.opening
      register: 1
      access: rw
      risk_class: a
      limits: { min: "abc", max: 100 }
`)
	_, errs := Load(bad, d, u)
	if len(errs) == 0 {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(errs[0].Error(), "limits") {
		t.Errorf("err = %v, want mention of limits", errs[0])
	}
}

func TestLoadEnumMapParseError(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	// Non-int value in enum_map — the toInt path returns an error.
	bad := writeYAML(t, "tmp-bad-enum.yaml", `
kind: PointMap
metadata:
  name: bad-enum
  driver: d
  appliesTo: cdu
spec:
  points:
    - point: status
      register: 1
      enum_map: { 1: "ok" }
`)
	_, errs := Load(bad, d, u)
	if len(errs) == 0 {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(errs[0].Error(), "enum_map") {
		t.Errorf("err = %v, want mention of enum_map", errs[0])
	}
}

func TestLoadPointMissing(t *testing.T) {
	d, u := loadDict(t), loadUnits(t)
	// A point entry with no "point" key still parses but V1 fails on the
	// empty string. Pin the behavior (V1 fires, Point is empty).
	bad := writeYAML(t, "tmp-nopoint.yaml", `
kind: PointMap
metadata:
  name: nopoint
  driver: d
  appliesTo: cdu
spec:
  points:
    - register: 1
`)
	_, errs := Load(bad, d, u)
	if len(errs) == 0 {
		t.Fatal("expected V1 error, got nil")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "V1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected V1 error, got %v", errs)
	}
}

func assertHasRule(t *testing.T, errs []error, rule string) {
	t.Helper()
	if len(errs) == 0 {
		t.Fatalf("%s: no errors reported, expected at least one", rule)
	}
	// Match the rule tag followed by a separator (space or colon) — the
	// exact message format varies per rule (e.g. "V1 point", "V6 metadata",
	// "V6 duplicate point"). The contract is "message contains 'V<N>'".
	marker := rule + " "
	for _, e := range errs {
		if !errors.Is(e, ErrPointMap) {
			t.Errorf("%s: err %v is not ErrPointMap", rule, e)
		}
		if strings.Contains(e.Error(), marker) {
			return
		}
	}
	t.Errorf("%s: no error contains %q, got %v", rule, marker, errs)
}

func writeYAML(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write tmp yaml: %v", err)
	}
	return p
}
