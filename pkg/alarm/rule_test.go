package alarm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/alarm/testdata"
)

// writeRule drops a single rule YAML file into a tmp dir and returns
// the dir path. The caller is responsible for cleanup.
func writeRule(t *testing.T, name, body string) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return d
}

// spec003Example is the verbatim spec-003 §3 example, lifted into a
// single-file fixture so the loader test doubles as a sanity check
// on the YAML shape the rest of the project will use.
const spec003Example = `kind: AlarmRule
metadata:
  name: cdu-fws-deltat-low
  appliesTo: cdu
spec:
  expr: "fws.deltat < 4"
  for: 5m
  severity: minor
  hysteresis: 0.5
  annotations:
    summary: "CDU 一次侧温差过低"
    runbook: rb/cdu-deltat-low
`

func TestLoadRules_SpecExample(t *testing.T) {
	dir := writeRule(t, "cdu-deltat.yaml", spec003Example)
	rules, err := LoadRules(dir, testdata.LoadDict())
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("len(rules)=%d, want 1", len(rules))
	}
	r := rules[0]
	if r.Metadata.Name != "cdu-fws-deltat-low" {
		t.Fatalf("name=%q", r.Metadata.Name)
	}
	if r.Metadata.AppliesTo != "cdu" {
		t.Fatalf("appliesTo=%q", r.Metadata.AppliesTo)
	}
	if r.Spec.Severity != "minor" {
		t.Fatalf("severity=%q", r.Spec.Severity)
	}
	if r.Spec.Hysteresis != 0.5 {
		t.Fatalf("hysteresis=%g", r.Spec.Hysteresis)
	}
	if r.Spec.ForDuration.String() != "5m0s" {
		t.Fatalf("for=%v", r.Spec.ForDuration)
	}
	if r.Spec.Annotations["summary"] != "CDU 一次侧温差过低" {
		t.Fatalf("annotations=%v", r.Spec.Annotations)
	}
	if r.Expr == nil {
		t.Fatal("Expr not compiled")
	}
	// Quick eval to prove Expr is wired.
	ok, err := r.Expr.Eval(map[string]float64{"fws.deltat": 3})
	if err != nil || !ok {
		t.Fatalf("expr eval (3): ok=%v err=%v", ok, err)
	}
}

func TestLoadRules_StatusEqualsFault(t *testing.T) {
	// spec-003 §3: expr: "status == 3" — recommended per-type.
	dir := writeRule(t, "cdu-status-fault.yaml", `kind: AlarmRule
metadata:
  name: cdu-status-fault
  appliesTo: cdu
spec:
  expr: "status == 3"
  for: 30s
  severity: critical
  annotations:
    summary: "CDU fault"
`)
	rules, err := LoadRules(dir, testdata.LoadDict())
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if rules[0].Spec.Severity != "critical" {
		t.Fatalf("severity=%q", rules[0].Spec.Severity)
	}
}

// ---- rejections ------------------------------------------------------------

func TestLoadRules_BadKind(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: NotAlarmRule
metadata: {name: foo, appliesTo: cdu}
spec: {expr: "x == 1", severity: minor}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("want kind error, got %v", err)
	}
}

func TestLoadRules_BadName(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: "BadName", appliesTo: cdu}
spec: {expr: "x == 1", severity: minor}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "kebab-case") {
		t.Fatalf("want kebab-case error, got %v", err)
	}
}

func TestLoadRules_AppliesToNotInDict(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: foo, appliesTo: nonsense}
spec: {expr: "x == 1", severity: minor}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "appliesTo") {
		t.Fatalf("want appliesTo error, got %v", err)
	}
}

func TestLoadRules_BadSeverity(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: foo, appliesTo: cdu}
spec: {expr: "x == 1", severity: banana}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "severity") {
		t.Fatalf("want severity error, got %v", err)
	}
}

func TestLoadRules_ExprUnparseable(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: foo, appliesTo: cdu}
spec: {expr: "x > >", severity: minor}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "expr") {
		t.Fatalf("want expr error, got %v", err)
	}
}

func TestLoadRules_DuplicateName(t *testing.T) {
	d := t.TempDir()
	for _, fn := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(d, fn), []byte(`kind: AlarmRule
metadata: {name: dup, appliesTo: cdu}
spec: {expr: "x == 1", severity: minor}
`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := LoadRules(d, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("want duplicates error, got %v", err)
	}
}

func TestLoadRules_BadDuration(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: foo, appliesTo: cdu}
spec: {expr: "x == 1", severity: minor, for: "five minutes"}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "for") {
		t.Fatalf("want for error, got %v", err)
	}
}

func TestLoadRules_NegativeHysteresis(t *testing.T) {
	dir := writeRule(t, "bad.yaml", `kind: AlarmRule
metadata: {name: foo, appliesTo: cdu}
spec: {expr: "x == 1", severity: minor, hysteresis: -1}
`)
	_, err := LoadRules(dir, testdata.LoadDict())
	if err == nil || !strings.Contains(err.Error(), "hysteresis") {
		t.Fatalf("want hysteresis error, got %v", err)
	}
}

func TestLoadRules_EmptyDir(t *testing.T) {
	d := t.TempDir()
	rules, err := LoadRules(d, testdata.LoadDict())
	if err != nil {
		t.Fatalf("empty dir should load zero rules, got %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("len=%d want 0", len(rules))
	}
}

func TestLoadRules_NilDict(t *testing.T) {
	d := t.TempDir()
	if _, err := LoadRules(d, nil); err == nil {
		t.Fatal("nil dict should error")
	}
}

func TestLoadRules_MultipleRules_KeepsOrder(t *testing.T) {
	d := t.TempDir()
	for _, fn := range []string{"a.yaml", "b.yaml", "c.yaml"} {
		os.WriteFile(filepath.Join(d, fn), []byte(`kind: AlarmRule
metadata: {name: `+fn[:1]+`, appliesTo: cdu}
spec: {expr: "x == 1", severity: minor}
`), 0o644)
	}
	rules, err := LoadRules(d, testdata.LoadDict())
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len=%d", len(rules))
	}
	// Files are sorted lexicographically: a < b < c.
	want := []string{"a", "b", "c"}
	for i, r := range rules {
		if r.Metadata.Name != want[i] {
			t.Fatalf("[%d].name=%q want %q", i, r.Metadata.Name, want[i])
		}
	}
}
