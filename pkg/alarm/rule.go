// Package alarm — rule.go: AlarmRule YAML type + loader.
//
// One AlarmRule applies to one asset type (types.yaml entry) and
// expresses a firing condition on a per-instance snapshot. Loader
// validation enforces the four invariants PRMT-020 §4.1 calls out:
// kind, name uniqueness, appliesTo membership in the dict, severity
// ∈ {critical|major|minor|info}, expr parseable. Hysteresis defaults
// to zero (spec-003 §3 "缺省 0，方向由比较符自动推导").
package alarm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/pkg/cpath"
)

// AlarmRule mirrors the YAML shape of spec-003 §3. Expr is filled
// by LoadRules after parsing spec.expr; the engine reads it without
// re-parsing on every Observe call.
type AlarmRule struct {
	Kind     string       `yaml:"kind"`
	Metadata ruleMetadata `yaml:"metadata"`
	Spec     ruleSpec     `yaml:"spec"`
	Expr     Expr         `yaml:"-"`
}

type ruleMetadata struct {
	Name      string `yaml:"name"`
	AppliesTo string `yaml:"appliesTo"`
}

type ruleSpec struct {
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Severity    string            `yaml:"severity"`
	Hysteresis  float64           `yaml:"hysteresis"`
	Annotations map[string]string `yaml:"annotations"`

	// ForDuration is populated by LoadRules; the YAML tag is on For
	// (string) because yaml.v3 does not auto-decode duration strings.
	ForDuration time.Duration `yaml:"-"`
}

// reRuleName pins the "kebab-case" shape spec-003 §3 calls out. The
// rule is intentionally strict (no underscores, no leading digit) so
// the rule name is safe to put in NATS subjects and CE `data.rule`
// without further escaping.
var reRuleName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// LoadRules reads every *.yaml under dir (non-recursive) and parses
// them as AlarmRule documents. Each rule is validated against dict:
// Kind=="AlarmRule", Name matches kebab-case + is unique across the
// directory, AppliesTo ∈ dict.Types, Severity ∈ 4-value whitelist,
// For parses as a Go duration, Expr compiles via ParseExpr.
//
// A bad rule is fatal: LoadRules returns the first error wrapped with
// the file name so the operator can locate it. The decision to abort
// (rather than skip and continue) is deliberate — a partially-loaded
// rule set would silently drop alerts, which is worse than crashing.
func LoadRules(dir string, dict *cpath.Dict) ([]AlarmRule, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read rules dir %q: %w", dir, err)
	}
	if dict == nil {
		return nil, errors.New("alarm: LoadRules requires a non-nil dict")
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	seen := map[string]string{} // name → file (for dup detection)
	out := make([]AlarmRule, 0, len(files))
	for _, name := range files {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: read: %w", name, err)
		}
		var r AlarmRule
		if err := yaml.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: parse yaml: %w", name, err)
		}
		if err := validateRule(&r, dict, name, seen); err != nil {
			return nil, err
		}
		// Parse duration into the typed field; leave it zero if the
		// YAML omits `for` (spec-003 §3 doesn't mandate one and the
		// engine treats zero as "fire on first sample").
		if strings.TrimSpace(r.Spec.For) != "" {
			d, err := time.ParseDuration(r.Spec.For)
			if err != nil {
				return nil, fmt.Errorf("%s: spec.for %q: %w", name, r.Spec.For, err)
			}
			r.Spec.ForDuration = d
		}
		// Compile expr.
		e, err := ParseExpr(r.Spec.Expr)
		if err != nil {
			return nil, fmt.Errorf("%s: spec.expr: %w", name, err)
		}
		r.Expr = e
		out = append(out, r)
	}
	return out, nil
}

// validateRule runs the four invariant checks from PRMT-020 §4.1 and
// records the rule's name in `seen` so a duplicate name later in the
// directory is rejected.
func validateRule(r *AlarmRule, dict *cpath.Dict, file string, seen map[string]string) error {
	if r.Kind != "AlarmRule" {
		return fmt.Errorf("%s: kind=%q, want %q", file, r.Kind, "AlarmRule")
	}
	name := r.Metadata.Name
	if !reRuleName.MatchString(name) {
		return fmt.Errorf("%s: metadata.name=%q is not kebab-case", file, name)
	}
	if prev, dup := seen[name]; dup {
		return fmt.Errorf("%s: metadata.name=%q duplicates %s", file, name, prev)
	}
	if _, ok := dict.Types[r.Metadata.AppliesTo]; !ok {
		return fmt.Errorf("%s: metadata.appliesTo=%q not in dict.Types", file, r.Metadata.AppliesTo)
	}
	if _, ok := AllowedSeverities[r.Spec.Severity]; !ok {
		return fmt.Errorf("%s: spec.severity=%q not in {critical,major,minor,info}", file, r.Spec.Severity)
	}
	if strings.TrimSpace(r.Spec.Expr) == "" {
		return fmt.Errorf("%s: spec.expr is empty", file)
	}
	if r.Spec.Hysteresis < 0 {
		return fmt.Errorf("%s: spec.hysteresis=%g must be >= 0", file, r.Spec.Hysteresis)
	}
	seen[name] = file
	return nil
}
