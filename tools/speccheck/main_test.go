// Tests for the cardinality-budget check (T35, PRMT-069).
//
// Scope: loadCardinalityBudget + checkCardinalityBudget against a tmp
// protocol directory. The check is testable in isolation because
// checkCardinalityBudget already takes a parsed *typesFile and
// *quantitiesFile and a pre-built errs slice; the surrounding CLI
// plumbing is exercised by the existing `go run ./tools/speccheck
// ./protocol` smoke step in CI.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// minimalProtocol builds a tmp dir containing a tiny but valid core
// protocol (types + quantities), the optional pointmaps/ subdir, and
// the optional cardinality-budget.yaml. The two bool returns let the
// caller opt in/out of each artefact.
func minimalProtocol(t *testing.T) (dir string, types *typesFile, quantities *quantitiesFile) {
	t.Helper()
	dir = t.TempDir()

	typesYAML := `version: "0.6"
types:
  cdu:  { parents: [site], level: device }
  pdu:  { parents: [site], level: device }
  node: { parents: [site], level: device }
`
	if err := os.WriteFile(filepath.Join(dir, "types.yaml"), []byte(typesYAML), 0o644); err != nil {
		t.Fatalf("write types.yaml: %v", err)
	}

	quantitiesYAML := `version: "0.3"
quantities:
  temp: { unit: celsius, kind: gauge }
  flow: { unit: lpm, kind: gauge }
derived:
  cop:  { unit: ratio, host: [cdu] }
  pue:  { unit: ratio, host: [site] }
`
	if err := os.WriteFile(filepath.Join(dir, "quantities.yaml"), []byte(quantitiesYAML), 0o644); err != nil {
		t.Fatalf("write quantities.yaml: %v", err)
	}

	// locations.yaml is required by loadLocations even if we don't
	// assert against it.
	if err := os.WriteFile(filepath.Join(dir, "locations.yaml"),
		[]byte("version: \"0.1\"\nloop: {}\nside: {}\nphase: {}\n"), 0o644); err != nil {
		t.Fatalf("write locations.yaml: %v", err)
	}

	var err error
	types, err = loadTypes(filepath.Join(dir, "types.yaml"))
	if err != nil {
		t.Fatalf("loadTypes: %v", err)
	}
	quantities, err = loadQuantities(filepath.Join(dir, "quantities.yaml"))
	if err != nil {
		t.Fatalf("loadQuantities: %v", err)
	}
	return dir, types, quantities
}

func writePointMap(t *testing.T, dir, name, appliesTo string, n int) {
	t.Helper()
	pmDir := filepath.Join(dir, "pointmaps")
	if err := os.MkdirAll(pmDir, 0o755); err != nil {
		t.Fatalf("mkdir pointmaps: %v", err)
	}
	var body string
	body = "kind: PointMap\nmetadata:\n  name: " + name + "\n  driver: modbus\n  appliesTo: " + appliesTo + "\nspec:\n  points:\n"
	for i := 0; i < n; i++ {
		body += "    - point: temp\n      register: 0x000" + string(rune('0'+i)) + "\n"
	}
	if err := os.WriteFile(filepath.Join(pmDir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write pointmap: %v", err)
	}
}

func writeBudget(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "cardinality-budget.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write budget: %v", err)
	}
}

// TestCheckCardinality_BudgetAbsent covers the MUST: budget file
// missing → no errs appended, no hard error returned (i.e. the rest
// of the speccheck run keeps its exit-0 outcome).
func TestCheckCardinality_BudgetAbsent(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	writePointMap(t, dir, "cdu-pm", "cdu", 6)

	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("expected no hard error, got %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors with budget absent, got %v", errs)
	}
}

// TestCheckCardinality_UnderBudget covers: budget present, total
// estimate ≤ site_budget → no errs. With 6 cdu points + 1 derived
// (cop) = 7 cdu points/asset, 1 cdu asset × 7 = 7; budget 100 → OK.
func TestCheckCardinality_UnderBudget(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	writePointMap(t, dir, "cdu-pm", "cdu", 6)
	writeBudget(t, dir, `version: "0.1"
per_type_count:
  cdu: 1
  pdu: 2
site_budget: 100
`)

	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("expected no hard error, got %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors (under budget), got %v", errs)
	}
}

// TestCheckCardinality_OverBudget covers: budget present, total
// estimate > site_budget → errs appended (and no hard error — the
// budget-exceeded case is a validation failure surfaced through errs,
// not a parse failure).
//
// Numbers: cdu has 6 pointmap + 1 derived = 7 points/asset, 10 cdu
// assets → 70; pdu has 0 + 0 = 0; site_budget 50 → 70 > 50 → fail.
func TestCheckCardinality_OverBudget(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	writePointMap(t, dir, "cdu-pm", "cdu", 6)
	writeBudget(t, dir, `version: "0.1"
per_type_count:
  cdu: 10
  pdu: 0
site_budget: 50
`)

	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("expected no hard error (budget-exceeded is a validation err), got %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected at least one validation error (over budget)")
	}
	// First message is the headline; it should mention both numbers.
	got := errs[0]
	want1, want2 := "70", "50"
	if !contains(got, want1) || !contains(got, want2) {
		t.Errorf("expected headline to mention %s and %s, got %q", want1, want2, got)
	}
}

// TestCheckCardinality_EstimatorCorrect locks the arithmetic down:
// cdu = 3 pointmap + 1 derived(cop) = 4; pdu = 0; node = 2 pointmap + 0.
// With per_type_count {cdu: 5, pdu: 7, node: 3} and site_budget 100,
// expected total = 4*5 + 0*7 + 2*3 = 26. site_budget 25 → fail.
func TestCheckCardinality_EstimatorCorrect(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	writePointMap(t, dir, "cdu-pm", "cdu", 3)
	writePointMap(t, dir, "node-pm", "node", 2)
	writeBudget(t, dir, `version: "0.1"
per_type_count:
  cdu: 5
  pdu: 7
  node: 3
site_budget: 25
`)
	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected over-budget failure for total=26 > 25")
	}
	// Breakdown line should contain the per-type contributions.
	wantSubstr := "cdu"
	if !contains(joined(errs), wantSubstr) {
		t.Errorf("expected breakdown to mention %s, got %v", wantSubstr, errs)
	}
}

// TestCheckCardinality_PerTypeCountMissing covers: a type with
// non-zero point contribution but no per_type_count entry must NOT
// contribute to the total. Architect's per_type_count is authoritative
// for "how many of this type a site has" — absence means "we don't
// promise a count, so it doesn't count toward the cliff check".
func TestCheckCardinality_PerTypeCountMissing(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	// 5 node points, but no per_type_count.node → 0 contribution.
	writePointMap(t, dir, "node-pm", "node", 5)
	writeBudget(t, dir, `version: "0.1"
per_type_count:
  cdu: 1
site_budget: 1
`)
	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors (node has no per_type_count → not counted), got %v", errs)
	}
}

// TestCheckCardinality_DerivedOnly exercises the derived-quantity
// path with no pointmaps: 1 cop → cdu gets +1; 1 pue → site only.
// per_type_count.cdu=1, site_budget=2 → 1 ≤ 2 OK.
func TestCheckCardinality_DerivedOnly(t *testing.T) {
	dir, types, quantities := minimalProtocol(t)
	// No pointmaps/ at all.
	writeBudget(t, dir, `version: "0.1"
per_type_count:
  cdu: 1
site_budget: 2
`)
	var errs []string
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors (derived-only = 1 cdu point), got %v", errs)
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func joined(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + "\n"
	}
	return out
}

// --- PRMT-091: lifecycle + units checks -------------------------------------

// TestCheckLifecycle_OK covers the green path: a non-empty,
// deduplicated, lowercase list. The values themselves are arbitrary —
// speccheck does NOT hardcode the business set.
func TestCheckLifecycle_OK(t *testing.T) {
	tf := &typesFile{
		Lifecycle: []string{"planned", "active", "retired"},
	}
	if err := checkLifecycle(tf); err != nil {
		t.Fatalf("expected no error for valid lifecycle, got %v", err)
	}
}

// TestCheckLifecycle_Empty covers MUST: lifecycle must be non-empty
// when declared. A missing lifecycle field is also "empty" in the
// speccheck sense — the field is optional in the YAML schema, so a
// nil/zero-length slice is not an error UNLESS we decide to make it
// one. PRMT-091 says "if types.yaml declares lifecycle, the list must
// be non-empty" — so the empty list IS an error, but a missing field
// (nil slice) is also an empty list and is therefore flagged. The
// intent is "if you write the key, write something real".
func TestCheckLifecycle_Empty(t *testing.T) {
	tf := &typesFile{Lifecycle: []string{}}
	if err := checkLifecycle(tf); err == nil {
		t.Fatalf("expected error for empty lifecycle, got nil")
	}
}

// TestCheckLifecycle_Duplicate covers: a duplicated value must fail.
// The first occurrence is fine; the second one trips the check.
func TestCheckLifecycle_Duplicate(t *testing.T) {
	tf := &typesFile{
		Lifecycle: []string{"planned", "active", "planned"},
	}
	err := checkLifecycle(tf)
	if err == nil {
		t.Fatalf("expected error for duplicate lifecycle value, got nil")
	}
	if !contains(err.Error(), "planned") {
		t.Errorf("expected error to name the duplicated value, got %q", err.Error())
	}
}

// TestCheckLifecycle_BadFormat covers: values must match `^[a-z]+$`.
// Hyphens, underscores, digits, uppercase are all rejected — those
// would be vocabulary drift, not just shape drift.
func TestCheckLifecycle_BadFormat(t *testing.T) {
	cases := []string{
		"Active",             // uppercase
		"under-construction", // hyphen
		"phase_one",          // underscore
		"phase1",             // digit
		"",                   // empty
	}
	for _, c := range cases {
		tf := &typesFile{Lifecycle: []string{c}}
		if err := checkLifecycle(tf); err == nil {
			t.Errorf("expected error for lifecycle value %q, got nil", c)
		}
	}
}

// TestCheckUnitRefs_OK covers the green path: every quantity's unit
// resolves to either a canonical core key or an `accepts` alias.
func TestCheckUnitRefs_OK(t *testing.T) {
	q := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{
			"temp":  {"unit": "celsius"}, // canonical
			"power": {"unit": "kw"},      // alias of watt
		},
		Derived: map[string]map[string]interface{}{
			"heat": {"unit": "watt"}, // canonical
		},
	}
	units := map[string]struct{}{
		"celsius": {},
		"watt":    {},
		"kw":      {}, // alias
	}
	var errs []string
	checkUnitRefs(q, units, &errs)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

// TestCheckUnitRefs_Dangling covers MUST: a quantity naming a unit
// that exists nowhere in the unit set must fail.
func TestCheckUnitRefs_Dangling(t *testing.T) {
	q := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{
			"mystery": {"unit": "foounit"},
		},
	}
	units := map[string]struct{}{
		"celsius": {},
		"watt":    {},
	}
	var errs []string
	checkUnitRefs(q, units, &errs)
	if len(errs) == 0 {
		t.Fatalf("expected at least one dangling-reference error, got none")
	}
	got := joined(errs)
	if !contains(got, "foounit") {
		t.Errorf("expected error to mention the dangling unit, got %v", errs)
	}
}

// TestCheckUnitRefs_DanglingDerived covers the same check for the
// `derived:` block, which is structurally identical but lives in a
// separate map. A bug fix that only walked Quantities would miss
// derived quantities — this test pins both branches down.
func TestCheckUnitRefs_DanglingDerived(t *testing.T) {
	q := &quantitiesFile{
		Derived: map[string]map[string]interface{}{
			"bad": {"unit": "foounit"},
		},
	}
	units := map[string]struct{}{"celsius": {}}
	var errs []string
	checkUnitRefs(q, units, &errs)
	if len(errs) == 0 {
		t.Fatalf("expected dangling-reference error for derived, got none")
	}
	if !contains(errs[0], "derived") {
		t.Errorf("expected error to mention 'derived', got %q", errs[0])
	}
}

// TestCheckUnitRefs_MissingUnit covers: a quantity with no `unit:`
// field at all is silently skipped. spec-002 owns the rule that
// quantities must have units; this check only catches the explicit-
// but-broken case.
func TestCheckUnitRefs_MissingUnit(t *testing.T) {
	q := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{
			"no_unit_here": {"kind": "gauge"}, // no "unit" key
		},
	}
	units := map[string]struct{}{"celsius": {}}
	var errs []string
	checkUnitRefs(q, units, &errs)
	if len(errs) != 0 {
		t.Fatalf("expected missing-unit to be silently skipped, got %v", errs)
	}
}

// TestCheckUnitRefs_EmptyUnitTable covers: an empty units set means
// units.yaml is missing entirely. We cannot distinguish valid from
// dangling references in that case, so the check skips itself to
// avoid a false-positive flood.
func TestCheckUnitRefs_EmptyUnitTable(t *testing.T) {
	q := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{
			"anything": {"unit": "foounit"},
		},
	}
	var errs []string
	checkUnitRefs(q, map[string]struct{}{}, &errs)
	if len(errs) != 0 {
		t.Fatalf("expected empty-units-table to be a no-op, got %v", errs)
	}
}

// TestMergeFragment_UnitConflict covers MUST (8a): a fragment declaring
// a unit key that already exists in the core units must fail with a
// validation error, matching the add-only discipline for types /
// quantities / derived.
func TestMergeFragment_UnitConflict(t *testing.T) {
	types := &typesFile{Types: map[string]map[string]interface{}{}}
	quantities := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{},
		Derived:    map[string]map[string]interface{}{},
	}
	coreUnits := map[string]struct{}{
		"celsius": {},
		"watt":    {},
	}
	merged := map[string]struct{}{
		"celsius": {},
		"watt":    {},
	}
	frag := fragmentFile{
		Units: map[string]struct {
			Accepts map[string]struct {
				Factor float64 `yaml:"factor"`
				Offset float64 `yaml:"offset"`
			} `yaml:"accepts"`
		}{
			"celsius": {}, // conflict with core
		},
	}
	err := mergeFragment(types, quantities, frag, "frag.yaml", coreUnits, merged)
	if err == nil {
		t.Fatalf("expected unit conflict error, got nil")
	}
	if !contains(err.Error(), "celsius") {
		t.Errorf("expected error to name the conflicting unit, got %q", err.Error())
	}
}

// TestMergeFragment_UnitFragmentFragmentConflict covers: a fragment
// declaring a unit key that an earlier fragment already added must
// also fail. The discipline is add-only across the whole merge.
func TestMergeFragment_UnitFragmentFragmentConflict(t *testing.T) {
	types := &typesFile{Types: map[string]map[string]interface{}{}}
	quantities := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{},
		Derived:    map[string]map[string]interface{}{},
	}
	coreUnits := map[string]struct{}{"celsius": {}}
	merged := map[string]struct{}{
		"celsius":   {},
		"new_unit1": {}, // an earlier fragment already added this
	}
	frag := fragmentFile{
		Units: map[string]struct {
			Accepts map[string]struct {
				Factor float64 `yaml:"factor"`
				Offset float64 `yaml:"offset"`
			} `yaml:"accepts"`
		}{
			"new_unit1": {}, // conflict with earlier fragment
		},
	}
	err := mergeFragment(types, quantities, frag, "frag.yaml", coreUnits, merged)
	if err == nil {
		t.Fatalf("expected unit conflict (frag vs earlier frag), got nil")
	}
	if !contains(err.Error(), "new_unit1") {
		t.Errorf("expected error to name the conflicting unit, got %q", err.Error())
	}
}

// TestMergeFragment_UnitAddOK covers: a fragment declaring a brand-new
// unit (not in core, not in any earlier fragment) is accepted, and
// the merged set is updated so a later check can resolve it.
//
// Note: `merged` is the *canonical* set (used for the conflict check
// 8a); aliases are folded in separately by buildAllUnits. So this
// test only asserts on the canonical key, and the alias assertion
// lives in TestBuildAllUnits below.
func TestMergeFragment_UnitAddOK(t *testing.T) {
	types := &typesFile{Types: map[string]map[string]interface{}{}}
	quantities := &quantitiesFile{
		Quantities: map[string]map[string]interface{}{},
		Derived:    map[string]map[string]interface{}{},
	}
	coreUnits := map[string]struct{}{"celsius": {}}
	merged := map[string]struct{}{"celsius": {}}
	frag := fragmentFile{
		Units: map[string]struct {
			Accepts map[string]struct {
				Factor float64 `yaml:"factor"`
				Offset float64 `yaml:"offset"`
			} `yaml:"accepts"`
		}{
			"us_per_cm": {Accepts: map[string]struct {
				Factor float64 `yaml:"factor"`
				Offset float64 `yaml:"offset"`
			}{
				"ms_per_cm": {Factor: 1000.0, Offset: 0},
			}},
		},
	}
	if err := mergeFragment(types, quantities, frag, "frag.yaml", coreUnits, merged); err != nil {
		t.Fatalf("expected no error for add-only unit, got %v", err)
	}
	if _, ok := merged["us_per_cm"]; !ok {
		t.Errorf("expected merged set to contain new canonical unit us_per_cm, got %v", merged)
	}
	// The alias `ms_per_cm` is intentionally NOT in `merged` here —
	// it belongs to the all-units set built by buildAllUnits, which
	// the dangling-reference check consults separately.
	if _, ok := merged["ms_per_cm"]; ok {
		t.Errorf("expected merged (canonical) set NOT to contain alias ms_per_cm, got %v", merged)
	}
}

// TestBuildAllUnits covers: a fragment's unit declarations (canonical
// + accepts) fold into the core all-units set, so the post-merge
// dangling-reference check can resolve fragment-named aliases too.
func TestBuildAllUnits(t *testing.T) {
	coreAll := map[string]struct{}{
		"celsius": {},
		"kw":      {}, // alias of watt, already in coreAll
	}
	fragments := []fragmentFile{
		{
			Units: map[string]struct {
				Accepts map[string]struct {
					Factor float64 `yaml:"factor"`
					Offset float64 `yaml:"offset"`
				} `yaml:"accepts"`
			}{
				"us_per_cm": {Accepts: map[string]struct {
					Factor float64 `yaml:"factor"`
					Offset float64 `yaml:"offset"`
				}{
					"ms_per_cm": {Factor: 1000.0, Offset: 0},
				}},
			},
		},
	}
	got := buildAllUnits(coreAll, fragments)
	for _, want := range []string{"celsius", "kw", "us_per_cm", "ms_per_cm"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected all-units set to contain %q, got %v", want, got)
		}
	}
}

// --- end-to-end: run the full speccheck against tmp protocol dirs ----------
//
// These tests cover the orchestration glue that the unit tests above
// can't reach: that the new lifecycle and units checks run at the
// right point in main(), surface as the right exit code, and that a
// clean protocol really does produce exit 0.

// writeProtocolCore writes the three required core files plus an
// optional units.yaml into dir. It returns the canonical core unit
// names so the caller can assert against the output if needed.
func writeProtocolCore(t *testing.T, dir, typesYAML, quantitiesYAML, unitsYAML string) {
	t.Helper()
	files := map[string]string{
		"types.yaml":      typesYAML,
		"quantities.yaml": quantitiesYAML,
		"locations.yaml":  "version: \"0.1\"\nloop: {}\nside: {}\nphase: {}\n",
	}
	if unitsYAML != "" {
		files["units.yaml"] = unitsYAML
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// runMainInDir invokes main() with the given protocol dir. It
// captures stdout and stderr and returns the exit code. main() calls
// os.Exit directly, so we spawn it as a subprocess — the alternative
// would require refactoring main() to be testable, which the prompt
// forbids (surgical change to main.go only).
func runMainInDir(t *testing.T, dir string) (stdout, stderr string, exitCode int) {
	t.Helper()
	// The test binary is run from the repo root (cwd resets between
	// bash calls per the subagent contract), so the speccheck source
	// lives at a path relative to the repo root, not the test's pwd.
	// Resolve it from the test file's own location.
	pkgDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve test pkg dir: %v", err)
	}
	// Use `go run` against the local package so we get the just-edited
	// binary, not anything cached or installed.
	cmd := exec.Command("go", "run", ".", dir)
	cmd.Dir = pkgDir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	stdout = out.String()
	stderr = errOut.String()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("subprocess failed unexpectedly: %v", err)
		}
	}
	return stdout, stderr, exitCode
}

// TestSpeccheckEndToEnd_Clean covers the green path: a minimal
// protocol with a valid lifecycle and a units set that covers every
// quantity's reference (including via accepts aliases) must exit 0
// and report the expected counts.
func TestSpeccheckEndToEnd_Clean(t *testing.T) {
	if os.Getenv("CIOS_SPECCHECK_E2E") == "" {
		t.Skip("end-to-end test requires running the speccheck binary; set CIOS_SPECCHECK_E2E=1 to enable")
	}
	dir := t.TempDir()
	writeProtocolCore(t, dir,
		`version: "0.6"
types:
  cdu:  { parents: [site], level: device }
  pdu:  { parents: [site], level: device }
lifecycle: [planned, active, retired]
`,
		`version: "0.3"
quantities:
  temp:  { unit: celsius, kind: gauge }
  power: { unit: watt,    kind: gauge }
`,
		`version: "0.1"
units:
  celsius: { accepts: {} }
  watt:
    accepts:
      kw: { factor: 1000.0, offset: 0 }
`)

	stdout, stderr, code := runMainInDir(t, dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if !contains(stdout, "OK") {
		t.Errorf("expected OK in stdout, got %q", stdout)
	}
}

// TestSpeccheckEndToEnd_BadLifecycle covers: a protocol whose
// lifecycle value is malformed must fail with exit 1, and the error
// must mention the bad value.
func TestSpeccheckEndToEnd_BadLifecycle(t *testing.T) {
	if os.Getenv("CIOS_SPECCHECK_E2E") == "" {
		t.Skip("end-to-end test requires running the speccheck binary; set CIOS_SPECCHECK_E2E=1 to enable")
	}
	dir := t.TempDir()
	writeProtocolCore(t, dir,
		`version: "0.6"
types:
  cdu: { parents: [site], level: device }
lifecycle: [Active]
`,
		`version: "0.3"
quantities:
  temp: { unit: celsius, kind: gauge }
`,
		`version: "0.1"
units:
  celsius: { accepts: {} }
`)

	_, stderr, code := runMainInDir(t, dir)
	if code != 1 {
		t.Fatalf("expected exit 1 for bad lifecycle, got %d (stderr=%q)", code, stderr)
	}
	if !contains(stderr, "lifecycle") {
		t.Errorf("expected stderr to mention lifecycle, got %q", stderr)
	}
}

// TestSpeccheckEndToEnd_DanglingUnit covers: a quantity that names a
// unit which is not in units.yaml (canonical) and not in any accepts
// alias must fail with exit 1, and the error must name both the
// quantity and the missing unit.
func TestSpeccheckEndToEnd_DanglingUnit(t *testing.T) {
	if os.Getenv("CIOS_SPECCHECK_E2E") == "" {
		t.Skip("end-to-end test requires running the speccheck binary; set CIOS_SPECCHECK_E2E=1 to enable")
	}
	dir := t.TempDir()
	writeProtocolCore(t, dir,
		`version: "0.6"
types:
  cdu: { parents: [site], level: device }
lifecycle: [planned, active]
`,
		`version: "0.3"
quantities:
  mystery: { unit: foounit, kind: gauge }
`,
		`version: "0.1"
units:
  celsius: { accepts: {} }
`)

	_, stderr, code := runMainInDir(t, dir)
	if code != 1 {
		t.Fatalf("expected exit 1 for dangling unit, got %d (stderr=%q)", code, stderr)
	}
	if !contains(stderr, "foounit") {
		t.Errorf("expected stderr to mention the dangling unit, got %q", stderr)
	}
	if !contains(stderr, "mystery") {
		t.Errorf("expected stderr to mention the quantity, got %q", stderr)
	}
}
