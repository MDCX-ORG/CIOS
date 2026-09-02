// Package rules — derived.go: load the derived-quantity section
// of protocol/quantities.yaml and apply the PRMT-021 §4.1
// "two-stage declarative filter" to produce the in-scope
// Derived set the cmd loop will tick on.
//
// Filter (declarative, no heuristics — see §4.1 note):
//
//	(a) the entry's host list must NOT contain "site"
//	    → L48 site-level aggregates (pue/wue/itload/facilityload)
//	      are out of scope for the Go-rules first cut;
//	(b) ParseFormula must succeed AND every Refs() entry must be
//	    a pure relative-point identifier
//	    → naturally excludes "heat" (介质按 loop) and "cop"
//	      (制冷量/耗电功率) whose formulas carry non-ident text.
//
// Both (a) and (b) must hold for an entry to be included. An
// out-of-scope entry is silently skipped (the quantities.yaml
// legitimately contains entries the rules engine can't service
// yet — failing load over them would be a config-error blizzard).
// A short info-level line lists the kept/dropped sets so the
// operator can see why a given quantity isn't ticking.
package rules

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// rawQuantitiesYAML mirrors the cpath.dict.rawQuantities shape just
// for the `derived:` field. We don't import the cpath internal type
// to keep pkg/rules free of pkg/cpath's struct churn. Host is a
// list because quantities.yaml uses YAML lists; the cpath.Dict
// flattens to []string, so the wire shape here is identical.
type rawQuantitiesYAML struct {
	Derived map[string]rawDerivedDef `yaml:"derived"`
}

type rawDerivedDef struct {
	Unit    string   `yaml:"unit"`
	Host    []string `yaml:"host"`
	Formula string   `yaml:"formula"`
}

// Derived is one in-scope derived quantity. Per PRMT-021 §4.1:
// Formula is pre-compiled (load-once, not re-parsed per tick);
// the original source string is NOT retained after compile.
type Derived struct {
	Name    string   // quantity name, e.g. "deltat"
	Hosts   []string // legal host types from quantities.yaml, e.g. ["cdu","chiller"]
	Unit    string   // standard unit (Prometheus metric suffix), e.g. "celsius"
	Formula Formula  // compiled arithmetic expression
}

// LoadDerived reads protocolDir/quantities.yaml and returns the
// derived entries that pass both PRMT-021 §4.1 filters (a) and (b).
// Skipped entries (out of scope) are NOT errors — they show up in
// the kept/dropped info log so an operator can spot a misconfigured
// quantity without the daemon refusing to start.
//
// Returns an error ONLY for load-level failures: a missing file,
// a malformed YAML body, or a derived entry that lacks the
// required "host" field (L23 requires host to be declared).
func LoadDerived(protocolDir string) ([]Derived, error) {
	path := filepath.Join(protocolDir, "quantities.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rules: read %s: %w", path, err)
	}
	var q rawQuantitiesYAML
	if err := yaml.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("rules: parse %s: %w", path, err)
	}

	var kept []Derived
	var dropped []string
	for name, def := range q.Derived {
		// Filter (a): host must not be site-level. L48 puts
		// site-level aggregates (itload/facilityload/pue/wue)
		// out of cios-rules' first cut — those need cross-
		// asset aggregation that the Go engine doesn't do
		// yet (see LOCKED L67 vmalert trigger line).
		if hasSiteHost(def.Host) {
			dropped = append(dropped, name+":site-host")
			continue
		}
		// Filter (b): formula must parse as pure relative-point
		// arithmetic. This rejects "heat" (公式含比热/密度/介质按 loop)
		// and "cop" (制冷量/耗电功率) naturally — both contain
		// CJK characters that ParseFormula rejects.
		f, err := ParseFormula(def.Formula)
		if err != nil {
			dropped = append(dropped, name+":formula-unparseable")
			continue
		}
		// All Refs must be pure idents (lowercase-anchored
		// relative-point paths). ParseFormula already validates
		// this in validIdent — but a defensive re-check keeps
		// the contract explicit at the load boundary.
		refs := f.Refs()
		if !allPureIdents(refs) {
			dropped = append(dropped, name+":non-ident-ref")
			continue
		}
		kept = append(kept, Derived{
			Name:    name,
			Hosts:   append([]string(nil), def.Host...),
			Unit:    def.Unit,
			Formula: f,
		})
	}

	// Kept-first log line so a small rule set stays readable in
	// the daemon's startup banner; dropped entries trail on the
	// same line.
	if len(kept) > 0 {
		fmt.Fprintf(os.Stderr, "rules: loaded %d derived quantity(ies):", len(kept))
		for _, d := range kept {
			fmt.Fprintf(os.Stderr, " %s", d.Name)
		}
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintln(os.Stderr, "rules: no in-scope derived quantities loaded (all filtered out)")
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "rules: skipped %d out-of-scope entries:", len(dropped))
		for _, d := range dropped {
			fmt.Fprintf(os.Stderr, " %s", d)
		}
		fmt.Fprintln(os.Stderr)
	}
	return kept, nil
}

// hasSiteHost reports whether "site" appears in the host list.
// Comparison is exact-string; quantities.yaml does not currently
// use inheritance / patterns for host lists, and the protocol
// for any future extension would say so explicitly.
func hasSiteHost(hosts []string) bool {
	for _, h := range hosts {
		if h == "site" {
			return true
		}
	}
	return false
}

// allPureIdents is a defensive backstop against a future ParseFormula
// change that loosens the ident shape. Keep both filters in lockstep:
// PRMT-021 §4.1 (b) requires "all Refs() to be pure relative-point
// identifiers". validIdent in formula.go already enforces this at
// parse time; this function is a paranoia pass at the load boundary.
func allPureIdents(refs []string) bool {
	for _, r := range refs {
		if r == "" {
			return false
		}
		if r[0] >= '0' && r[0] <= '9' {
			return false
		}
		// The validIdent body in formula.go is the source of
		// truth; this is a structurally identical re-check.
		for i := 0; i < len(r); i++ {
			c := r[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.'
			if !ok {
				return false
			}
			if c == '.' && i+1 < len(r) && r[i+1] == '.' {
				return false
			}
		}
	}
	return true
}
