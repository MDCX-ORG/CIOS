// speccheck validates the CIOS protocol dictionary files, including
// any ext.d/*.yaml fragments (L54, spec-001 §4).
//
// Usage: speccheck <protocol_dir>
//
// Exit codes:
//
//	0 = all checks passed
//	1 = validation failure (details on stderr)
//	2 = file read / parse error
//
// Checks performed (spec-001 §1-§4, spec-006 §1, L54):
//  1. types.yaml / quantities.yaml / locations.yaml parse as YAML.
//  2. Vocabulary disjointness:
//     types ∩ (quantities ∪ derived) = ∅
//     types ∩ locations          = ∅
//     quantities ∩ locations     = ∅
//  3. Every parents reference in types.yaml resolves to a type or to "site".
//  4. Every top-level type listed in domains exists in types.
//  5. ext.d fragments are well-formed (YAML parses; only allowed top-level
//     keys: version, types, quantities, derived, units) and add-only
//     (no re-definition of names already in core or an earlier fragment).
//  6. Cardinality budget (T35, PRMT-069): Σ(per-type points × per_type_count)
//     ≤ site_budget. Reads protocol/cardinality-budget.yaml; if absent,
//     prints "budget file absent, skipped" and the check is a no-op
//     (exit 0). Per-type point count = pointmap points in
//     protocol/pointmaps/*.yaml (grouped by metadata.appliesTo) plus
//     derived quantities whose host list contains the type.
//  7. Lifecycle set (PRMT-091): if types.yaml declares lifecycle, the
//     list must be non-empty, deduplicated, and every value must match
//     `^[a-z]+$` (per spec-008 §13.1 vocabulary convention; speccheck
//     does NOT hardcode the business set — it only checks shape).
//  8. Units (PRMT-091): if units.yaml is present, it must parse and
//     every unit key declared in a fragment must NOT collide with a
//     core unit key (L54 add-only applies to units too). After all
//     fragments merge, every quantity's `unit` reference must resolve
//     to a unit in the merged (core + fragment) unit set.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- core table shapes -------------------------------------------------------

type typesFile struct {
	Version   string                            `yaml:"version"`
	Domains   map[string][]string               `yaml:"domains"`
	Types     map[string]map[string]interface{} `yaml:"types"`
	Lifecycle []string                          `yaml:"lifecycle"`
}

type quantitiesFile struct {
	Version    string                            `yaml:"version"`
	Quantities map[string]map[string]interface{} `yaml:"quantities"`
	Derived    map[string]map[string]interface{} `yaml:"derived"`
}

type locationsFile struct {
	Version string                            `yaml:"version"`
	Loop    map[string]map[string]interface{} `yaml:"loop"`
	Side    map[string]map[string]interface{} `yaml:"side"`
	Phase   map[string]map[string]interface{} `yaml:"phase"`
}

type unitsFile struct {
	Units map[string]struct {
		Accepts map[string]struct {
			Factor float64 `yaml:"factor"`
			Offset float64 `yaml:"offset"`
		} `yaml:"accepts"`
	} `yaml:"units"`
}

// --- fragment shape (L54) ----------------------------------------------------
//
// An ext.d fragment may have any subset of {version, types, quantities,
// derived, units}. We probe each allowed top-level key by unmarshalling
// into a permissive structure; this keeps the parser schema in sync
// with the per-section core parsers (no second source of truth).

type fragmentFile struct {
	Version    string                            `yaml:"version"`
	Types      map[string]map[string]interface{} `yaml:"types"`
	Quantities map[string]map[string]interface{} `yaml:"quantities"`
	Derived    map[string]map[string]interface{} `yaml:"derived"`
	Units      map[string]struct {
		Accepts map[string]struct {
			Factor float64 `yaml:"factor"`
			Offset float64 `yaml:"offset"`
		} `yaml:"accepts"`
	} `yaml:"units"`
}

// allowedFragmentKeys is the closed set of top-level keys a fragment
// may declare (L54). Anything else is a hard error.
var allowedFragmentKeys = map[string]bool{
	"version":    true,
	"types":      true,
	"quantities": true,
	"derived":    true,
	"units":      true,
}

// --- cardinality budget (T35, PRMT-069) --------------------------------------

// cardinalityBudgetFile is the optional sidecar under protocol/. Its
// absence is not an error — the check is a no-op in that case (the
// architect will add the file when fleet rollout is ready).
type cardinalityBudgetFile struct {
	Version      string         `yaml:"version"`
	PerTypeCount map[string]int `yaml:"per_type_count"`
	SiteBudget   int            `yaml:"site_budget"`
}

// pointMapFile is a minimal reader for protocol/pointmaps/*.yaml. Only
// the fields needed for cardinality counting are decoded; V1–V7
// validation lives in pkg/pointmap.Load and is not duplicated here.
type pointMapFile struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		AppliesTo string `yaml:"appliesTo"`
	} `yaml:"metadata"`
	Spec struct {
		Points []map[string]interface{} `yaml:"points"`
	} `yaml:"spec"`
}

// --- main --------------------------------------------------------------------

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: speccheck <protocol_dir>\n")
		os.Exit(2)
	}
	dir := os.Args[1]

	typesPath := filepath.Join(dir, "types.yaml")
	quantitiesPath := filepath.Join(dir, "quantities.yaml")
	locationsPath := filepath.Join(dir, "locations.yaml")
	unitsPath := filepath.Join(dir, "units.yaml")

	types, err := loadTypes(typesPath)
	if err != nil {
		fail("read " + typesPath + ": " + err.Error())
	}
	quantities, err := loadQuantities(quantitiesPath)
	if err != nil {
		fail("read " + quantitiesPath + ": " + err.Error())
	}
	locations, err := loadLocations(locationsPath)
	if err != nil {
		fail("read " + locationsPath + ": " + err.Error())
	}
	coreCanonicalUnits, coreAllUnits, err := loadUnits(unitsPath)
	if err != nil {
		fail("read " + unitsPath + ": " + err.Error())
	}

	// 7. lifecycle shape (PRMT-091). Run before fragment merge so
	// fragments inherit a valid vocabulary. Only the shape is checked;
	// speccheck deliberately does NOT hardcode the business set
	// (planned/installed/active/maintenance/retired from spec-008 §13.1)
	// to avoid coupling the CI tool to the lifecycle vocabulary — a
	// future state set change should not require a speccheck change.
	if err := checkLifecycle(types); err != nil {
		fmt.Fprintln(os.Stderr, "speccheck: "+err.Error())
		os.Exit(1)
	}

	// Fragments: sorted by file name, merged into the core tables in
	// order. Conflicts surface as check-5 errors (validation, exit 1).
	fragments, fragmentFiles, err := loadFragments(filepath.Join(dir, "ext.d"))
	if err != nil {
		fail(err.Error())
	}
	// Track fragment-declared units separately so the post-merge
	// dangling-reference check (check 8b) can see the full unit set.
	// mergedUnits starts with the canonical core units; fragment
	// units are added during mergeFragment. The check 8b lookup also
	// accepts aliases of any canonical unit (core or fragment).
	mergedCanonicalUnits := make(map[string]struct{}, len(coreCanonicalUnits))
	for k := range coreCanonicalUnits {
		mergedCanonicalUnits[k] = struct{}{}
	}
	for i, frag := range fragments {
		path := filepath.Join(dir, "ext.d", fragmentFiles[i])
		if err := mergeFragment(types, quantities, frag, path, coreCanonicalUnits, mergedCanonicalUnits); err != nil {
			// check-5/8a: validation failure, not parse failure
			fmt.Fprintln(os.Stderr, "speccheck: "+err.Error())
			os.Exit(1)
		}
	}

	var errs []string

	// 2. vocabulary disjointness (post-merge, so fragments participate)
	quantNames := keys(quantities.Quantities)
	derivedNames := keys(quantities.Derived)
	locNames := locationKeys(locations)
	typeNames := keys(types.Types)

	if xs := intersect(typeNames, union(quantNames, derivedNames)); len(xs) > 0 {
		errs = append(errs, fmt.Sprintf("conflict between types and quantities: %s", formatList(xs)))
	}
	if xs := intersect(typeNames, locNames); len(xs) > 0 {
		errs = append(errs, fmt.Sprintf("conflict between types and locations: %s", formatList(xs)))
	}
	if xs := intersect(quantNames, locNames); len(xs) > 0 {
		errs = append(errs, fmt.Sprintf("conflict between quantities and locations: %s", formatList(xs)))
	}

	// 3. parents references resolve to a type or to "site"
	for name, def := range types.Types {
		parents, ok := def["parents"].([]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("type %q: parents missing or not a list", name))
			continue
		}
		for _, p := range parents {
			pName, ok := p.(string)
			if !ok {
				errs = append(errs, fmt.Sprintf("type %q: parent entry is not a string", name))
				continue
			}
			if pName == "site" {
				continue
			}
			if _, found := types.Types[pName]; !found {
				errs = append(errs, fmt.Sprintf("type %q: parent %q is not a known type (and not \"site\")", name, pName))
			}
		}
	}

	// 4. domains top-level types exist
	for domain, names := range types.Domains {
		for _, n := range names {
			if _, found := types.Types[n]; !found {
				errs = append(errs, fmt.Sprintf("domain %q: top-level type %q is not in types", domain, n))
			}
		}
	}

	// 8b. units: every quantity's `unit` reference must resolve to a
	// unit in the merged (core + fragment) unit set. A quantity with
	// no `unit` field is skipped silently — some quantities (e.g.
	// pure enum/derived) may legitimately omit a unit; spec-002 owns
	// the rule. We only catch the case where a value is declared but
	// points to nothing.
	//
	// The lookup accepts both canonical unit keys AND any alias
	// declared under `accepts` (gateway normalizes aliases to the
	// canonical form at ingest, so a quantity that names an alias is
	// still semantically pinned to a defined unit).
	checkUnitRefs(quantities, buildAllUnits(coreAllUnits, fragments), &errs)

	// 6. cardinality budget check (T35, PRMT-069). Skipped silently
	// (exit 0) if the budget file is absent — the architect owns that
	// file and may not have added it yet.
	if err := checkCardinalityBudget(dir, types, quantities, &errs); err != nil {
		fmt.Fprintln(os.Stderr, "speccheck: "+err.Error())
		os.Exit(2)
	}

	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "speccheck: "+e)
		}
		os.Exit(1)
	}

	fmt.Printf("speccheck: %d types, %d quantities, %d locations, %d ext fragments — OK\n",
		len(typeNames), len(quantNames)+len(derivedNames), len(locNames), len(fragments))
}

// --- core loaders ------------------------------------------------------------

func loadTypes(path string) (*typesFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f typesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func loadQuantities(path string) (*quantitiesFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f quantitiesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func loadLocations(path string) (*locationsFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f locationsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// loadUnits reads protocol/units.yaml and returns (canonical, all) unit
// sets, where:
//
//   - canonical = the top-level keys of units.yaml (the standard units
//     that quantities/derived SHOULD name; fragment unit declarations
//     are also added here during merge, and used for the conflict
//     check 8a),
//   - all = canonical ∪ every `accepts` alias under each canonical key
//     (used for the dangling-reference check 8b — a quantity naming an
//     alias is still semantically pinned to a defined unit; the gateway
//     normalizes aliases to the canonical form at ingest).
//
// Rationale: per the spec-002 §3 convention, the standard unit is the
// identity (factor 1, offset 0) and accepts are linear transformations
// of it. A quantity's `unit:` value should be the standard unit, but
// naming an alias is technically valid input (the driver may report
// "kw" while the quantity is still semantically "watt"). A reference
// that resolves to a canonical key OR to any accepts alias is a hit.
//
// A missing units.yaml returns empty sets: speccheck then reports the
// file as absent via the merge check (the fragment's "unit_in must be
// in units.yaml" rule owns that gate at gateway runtime). speccheck
// itself skips check 8b on empty `all` because no references could
// resolve — appending "every reference is dangling" would be a
// false positive on a deliberately-minimal protocol.
func loadUnits(path string) (canonical, all map[string]struct{}, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, map[string]struct{}{}, nil
		}
		return nil, nil, err
	}
	var f unitsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, nil, err
	}
	canonical = make(map[string]struct{}, len(f.Units))
	all = make(map[string]struct{}, len(f.Units))
	for k, def := range f.Units {
		canonical[k] = struct{}{}
		all[k] = struct{}{}
		for a := range def.Accepts {
			all[a] = struct{}{}
		}
	}
	return canonical, all, nil
}

// --- fragment loader + merger (L54 check 5) ---------------------------------

// loadFragments returns the parsed fragments under extDir, sorted by
// file name, and the matching file name list. A missing or empty
// ext.d directory is not an error.
func loadFragments(extDir string) ([]fragmentFile, []string, error) {
	entries, err := os.ReadDir(extDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", extDir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	var frags []fragmentFile
	for _, name := range files {
		path := filepath.Join(extDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		// Schema check: only allowed top-level keys. Read into a generic
		// node so we can enumerate keys without committing to the typed
		// struct (which would silently ignore extras).
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		// yaml.Unmarshal into a *yaml.Node wraps the document in a
		// DocumentNode; the actual top-level mapping is its first child.
		top := &node
		if top.Kind == yaml.DocumentNode && len(top.Content) > 0 {
			top = top.Content[0]
		}
		if top.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("parse %s: top-level must be a mapping", path)
		}
		for i := 0; i < len(top.Content); i += 2 {
			k := top.Content[i].Value
			if !allowedFragmentKeys[k] {
				return nil, nil, fmt.Errorf("%s: unknown top-level key %q (allowed: version, types, quantities, derived, units)", path, k)
			}
		}
		var f fragmentFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		frags = append(frags, f)
	}
	return frags, files, nil
}

// mergeFragment folds one fragment into the core tables, enforcing
// add-only semantics. Conflicts become validation errors (exit 1).
//
// coreUnits and mergedUnits carry the canonical unit set (PRMT-091
// check 8a): a fragment must not redeclare a unit key already in the
// core. mergedUnits is updated in-place so the post-merge dangling-
// reference check (check 8b) sees every new unit.
func mergeFragment(types *typesFile, quantities *quantitiesFile, frag fragmentFile, path string, coreUnits, mergedUnits map[string]struct{}) error {
	for name := range frag.Types {
		if _, exists := types.Types[name]; exists {
			return fmt.Errorf("%s: type %q already defined in core or earlier fragment", path, name)
		}
	}
	for name := range frag.Quantities {
		if _, exists := quantities.Quantities[name]; exists {
			return fmt.Errorf("%s: quantity %q already defined in core or earlier fragment", path, name)
		}
	}
	for name := range frag.Derived {
		if _, exists := quantities.Derived[name]; exists {
			return fmt.Errorf("%s: derived %q already defined in core or earlier fragment", path, name)
		}
	}
	for name := range frag.Units {
		if _, exists := coreUnits[name]; exists {
			return fmt.Errorf("%s: unit %q already defined in core units", path, name)
		}
		if _, exists := mergedUnits[name]; exists {
			return fmt.Errorf("%s: unit %q already defined in an earlier fragment", path, name)
		}
	}
	// Apply — after the conflict check, so we never half-merge.
	for name, def := range frag.Types {
		types.Types[name] = def
	}
	for name, def := range frag.Quantities {
		quantities.Quantities[name] = def
	}
	for name, def := range frag.Derived {
		quantities.Derived[name] = def
	}
	for name := range frag.Units {
		mergedUnits[name] = struct{}{}
	}
	return nil
}

// --- cardinality budget (T35, PRMT-069, check 6) ----------------------------

// checkCardinalityBudget estimates per-site point count from
// (a) pointmap files in protocol/pointmaps/ (grouped by metadata.appliesTo)
// (b) derived quantities from quantities.yaml, distributed to each type
// in their host list.
//
// If protocol/cardinality-budget.yaml is missing, the check is a
// no-op (architect has not provided a budget yet — T35 is fleet-prep).
// If present, Σ(per_type_count × points) must be ≤ site_budget;
// otherwise an error is appended and main exits 1.
//
// A non-nil return is a hard parse/read error (exit 2), distinct from
// a budget-exceeded validation error (exit 1).
func checkCardinalityBudget(dir string, types *typesFile, quantities *quantitiesFile, errs *[]string) error {
	budgetPath := filepath.Join(dir, "cardinality-budget.yaml")
	raw, err := os.ReadFile(budgetPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("speccheck: cardinality: budget file absent, skipped")
			return nil
		}
		return fmt.Errorf("read %s: %w", budgetPath, err)
	}
	var b cardinalityBudgetFile
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("parse %s: %w", budgetPath, err)
	}
	if b.SiteBudget <= 0 {
		return fmt.Errorf("%s: site_budget must be a positive integer", budgetPath)
	}

	// Per-type point count from the protocol's own pointmaps.
	pmCounts, err := loadPointMapCounts(filepath.Join(dir, "pointmaps"), types)
	if err != nil {
		return err
	}
	// Derived points belong to every type in their `host` list. A
	// derived point is one logical point per asset instance of a host
	// type, so it counts once toward that type's per-instance total.
	deriveCounts := derivedPerType(quantities)

	// Merge: a type may have both pointmap and derived contributions.
	perType := mergeCounts(pmCounts, deriveCounts)

	// Build a stable, sorted breakdown. Only types with non-zero count
	// are listed — keeps the message small for the common case.
	names := make([]string, 0, len(perType))
	for n := range perType {
		names = append(names, n)
	}
	sort.Strings(names)

	var total int
	for _, n := range names {
		if c, ok := b.PerTypeCount[n]; ok {
			total += perType[n] * c
		}
	}
	if total > b.SiteBudget {
		*errs = append(*errs, fmt.Sprintf(
			"cardinality: estimated %d points > site_budget %d (per-type contribution below)",
			total, b.SiteBudget,
		))
		for _, n := range names {
			c, hasCount := b.PerTypeCount[n]
			if !hasCount {
				continue
			}
			*errs = append(*errs, fmt.Sprintf(
				"  %-20s %d points/asset × %d assets = %d",
				n, perType[n], c, perType[n]*c,
			))
		}
		return nil
	}

	fmt.Printf("speccheck: cardinality: estimated %d points ≤ site_budget %d — OK\n", total, b.SiteBudget)
	for _, n := range names {
		c, hasCount := b.PerTypeCount[n]
		if !hasCount {
			continue
		}
		if perType[n] == 0 {
			continue
		}
		fmt.Printf("speccheck: cardinality:   %-20s %d points/asset × %d assets = %d\n",
			n, perType[n], c, perType[n]*c)
	}
	return nil
}

// loadPointMapCounts sums spec.points across every yaml under dir,
// keyed by metadata.appliesTo. A missing or empty dir is not an error
// (no pointmaps yet → 0 contribution). A pointmap with an unknown
// appliesTo is silently skipped (per the speccheck policy of being
// permissive on dict membership; pkg/pointmap.Load enforces the strict
// V6 rule at parse time).
func loadPointMapCounts(dir string, types *typesFile) (map[string]int, error) {
	out := map[string]int{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	for _, name := range files {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var pm pointMapFile
		if err := yaml.Unmarshal(raw, &pm); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if pm.Metadata.AppliesTo == "" {
			continue
		}
		if _, known := types.Types[pm.Metadata.AppliesTo]; !known {
			continue
		}
		out[pm.Metadata.AppliesTo] += len(pm.Spec.Points)
	}
	return out, nil
}

// derivedPerType returns the per-type count of derived points. Each
// derived quantity contributes 1 point to every type listed in its
// `host` array (recording rules instantiate it per asset instance).
func derivedPerType(q *quantitiesFile) map[string]int {
	out := map[string]int{}
	for _, def := range q.Derived {
		hosts, _ := def["host"].([]interface{})
		for _, h := range hosts {
			name, ok := h.(string)
			if !ok {
				continue
			}
			out[name]++
		}
	}
	return out
}

// mergeCounts sums two count maps. Missing keys are treated as 0.
func mergeCounts(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

// --- helpers -----------------------------------------------------------------

// checkLifecycle validates the shape of types.Lifecycle (PRMT-091
// check 7). It is intentionally content-free: speccheck only checks
// (a) non-empty, (b) no duplicates, (c) every value matches `^[a-z]+$`.
//
// The business set (planned/installed/active/maintenance/retired from
// spec-008 §13.1) is NOT hardcoded — the §7 Implementation Notes call
// this out as a deliberate decision: coupling the CI tool to a
// specific lifecycle vocabulary would force a speccheck change every
// time the spec adds or renames a state, and would be out of scope for
// a "shape validator". A future spec change can add a stricter check
// via a separate, opt-in validation hook.
func checkLifecycle(types *typesFile) error {
	if len(types.Lifecycle) == 0 {
		return fmt.Errorf("types.yaml: lifecycle is empty (declare at least one state)")
	}
	seen := make(map[string]struct{}, len(types.Lifecycle))
	for _, v := range types.Lifecycle {
		if v == "" {
			return fmt.Errorf("types.yaml: lifecycle contains an empty entry")
		}
		if _, dup := seen[v]; dup {
			return fmt.Errorf("types.yaml: lifecycle value %q is duplicated", v)
		}
		seen[v] = struct{}{}
		if !lifecycleValueRe.MatchString(v) {
			return fmt.Errorf("types.yaml: lifecycle value %q does not match %s", v, lifecycleValueRe.String())
		}
	}
	return nil
}

// lifecycleValueRe enforces the spec-008 §13.1 vocabulary convention
// of single-word, lowercase ASCII names (e.g. "planned", "active",
// "maintenance"). Underscores, hyphens, digits, and uppercase are all
// rejected — those would be vocabulary drift, not just shape drift.
var lifecycleValueRe = regexp.MustCompile(`^[a-z]+$`)

// checkUnitRefs walks every quantity (regular + derived) and appends
// a validation error for any `unit:` value that does not resolve to a
// key in the merged unit set (PRMT-091 check 8b). Quantities with no
// `unit` field are skipped — a quantity without a unit is meaningless
// to spec-002 but the spec owns that rule; this check only catches
// the explicit-but-broken case (dangling pointer).
//
// `units` should be the *all* set (canonical + accepts aliases) so
// that a quantity naming a gateway-recognized alias is not flagged.
func checkUnitRefs(q *quantitiesFile, units map[string]struct{}, errs *[]string) {
	if len(units) == 0 {
		// Empty lookup table means units.yaml is missing entirely.
		// We cannot distinguish "valid" from "dangling" references in
		// that case, so we skip the check — flagging every reference
		// as dangling would be a false positive on a deliberately
		// minimal protocol.
		return
	}
	for name, def := range q.Quantities {
		ref, ok := def["unit"].(string)
		if !ok || ref == "" {
			continue
		}
		if _, found := units[ref]; !found {
			*errs = append(*errs, fmt.Sprintf("quantity %q: unit %q is not defined in units.yaml (or any ext.d fragment)", name, ref))
		}
	}
	for name, def := range q.Derived {
		ref, ok := def["unit"].(string)
		if !ok || ref == "" {
			continue
		}
		if _, found := units[ref]; !found {
			*errs = append(*errs, fmt.Sprintf("derived %q: unit %q is not defined in units.yaml (or any ext.d fragment)", name, ref))
		}
	}
}

// buildAllUnits folds the per-fragment unit declarations into the core
// all-units set (canonical + aliases). Used for the check 8b lookup
// so fragment aliases are equally resolvable as core aliases.
func buildAllUnits(coreAll map[string]struct{}, fragments []fragmentFile) map[string]struct{} {
	out := make(map[string]struct{}, len(coreAll))
	for k := range coreAll {
		out[k] = struct{}{}
	}
	for _, frag := range fragments {
		for k, def := range frag.Units {
			out[k] = struct{}{}
			for a := range def.Accepts {
				out[a] = struct{}{}
			}
		}
	}
	return out
}

func locationKeys(l *locationsFile) []string {
	seen := map[string]struct{}{}
	for k := range l.Loop {
		seen[k] = struct{}{}
	}
	for k := range l.Side {
		seen[k] = struct{}{}
	}
	for k := range l.Phase {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func keys(m map[string]map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, x := range b {
		seen[x] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func intersect(a, b []string) []string {
	set := map[string]struct{}{}
	for _, x := range b {
		set[x] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, x := range a {
		if _, ok := set[x]; ok {
			if _, dup := seen[x]; !dup {
				seen[x] = struct{}{}
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}

func formatList(xs []string) string {
	return "[" + strings.Join(xs, ", ") + "]"
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "speccheck: "+msg)
	os.Exit(2)
}
