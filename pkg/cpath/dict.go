// Package cpath implements the CIOS cascading-path scheme used to address
// assets and telemetry points (see spec-001 §2, spec-002 §1).
//
// Vocabulary is loaded at runtime from protocol/*.yaml so the dictionary
// can evolve without code changes. Per L54, the directory may also
// contain ext.d/*.yaml fragments that extend the core tables additively
// (no overrides, no edits — see spec-001 §4).
package cpath

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Level is the asset index formatting rule attached to a type.
type Level string

const (
	LevelDevice Level = "device" // 3-digit zero-padded, range 000-999
	LevelChip   Level = "chip"   // no leading zero, range 0-999
)

// TypeDef describes one entry in protocol/types.yaml.
type TypeDef struct {
	Parents       []string
	Level         Level
	Desc          string
	AttrsRequired []string
}

// QuantityDef describes one entry in protocol/quantities.yaml (quantities
// or derived section). Kind is empty for derived entries. Enum is the
// standard-code → label table for enum-typed quantities (e.g. status,
// leak); nil for non-enum quantities. Keys are always int (the standard
// codes the V3 subset check in pkg/pointmap runs against); values are
// the human-readable labels.
type QuantityDef struct {
	Unit string
	Kind string
	Host []string
	Enum map[int]string
}

// Dict is the in-memory protocol dictionary loaded by LoadDict.
type Dict struct {
	Types      map[string]TypeDef
	Quantities map[string]QuantityDef // quantities section
	Derived    map[string]QuantityDef // derived section
	Loops      map[string]bool        // locations.yaml loop keys (fws/tcs)
	Sides      map[string]bool        // locations.yaml side keys
	Phases     map[string]bool        // locations.yaml phase keys
	Domains    map[string][]string
	// Relations are topology edge verbs from types.yaml `relations:`
	// (spec-001 §7: feeds / cools / connects). Keys are the relation
	// names; value is true when present. PRMT-223 loads this so layout
	// validation and admin UI do not hardcode the vocabulary.
	Relations map[string]bool
}

// --- raw YAML shapes -------------------------------------------------------

type rawTypes struct {
	Version   string                            `yaml:"version"`
	Domains   map[string][]string               `yaml:"domains"`
	Types     map[string]map[string]interface{} `yaml:"types"`
	Lifecycle []string                          `yaml:"lifecycle"`
	Relations map[string]map[string]interface{} `yaml:"relations"`
}

type rawQuantities struct {
	Version    string                            `yaml:"version"`
	Quantities map[string]map[string]interface{} `yaml:"quantities"`
	Derived    map[string]map[string]interface{} `yaml:"derived"`
}

type rawLocations struct {
	Version string                            `yaml:"version"`
	Loop    map[string]map[string]interface{} `yaml:"loop"`
	Side    map[string]map[string]interface{} `yaml:"side"`
	Phase   map[string]map[string]interface{} `yaml:"phase"`
}

// --- ext.d fragment shapes (L54) --------------------------------------------
//
// A fragment is a YAML file under <dir>/ext.d/ with the same shape as the
// core tables. Any of the per-table maps may be absent; the fragment may
// also be empty aside from version. The L54 rule "fragments add only,
// never edit" is enforced here by addEntry (cpath's portion of it) and
// in tools/speccheck (the disjointness half).

// fragmentTypes mirrors rawTypes but only the fields ext.d fragments
// are allowed to define (L54 disallows fragment-level domains / lifecycle
// edits to the core vocabulary).
type fragmentTypes struct {
	Types map[string]map[string]interface{} `yaml:"types"`
}

// fragmentQuantities mirrors rawQuantities for the same reason.
type fragmentQuantities struct {
	Quantities map[string]map[string]interface{} `yaml:"quantities"`
	Derived    map[string]map[string]interface{} `yaml:"derived"`
}

// LoadDict reads types.yaml / quantities.yaml / locations.yaml under dir
// and then merges any ext.d/*.yaml fragments in lexicographic order
// (L54). Any missing file or parse error returns an error whose message
// names the file. A fragment that re-defines a name already present in
// the core table or in an earlier fragment is a load error (L54:
// fragments add only). Vocabulary disjointness across sections (e.g.
// "temp" appearing in both types and quantities) is the speccheck
// tool's job, not this package's: LoadDict trusts the dictionary.
func LoadDict(dir string) (*Dict, error) {
	t, err := loadRaw[rawTypes](filepath.Join(dir, "types.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s/types.yaml: %w", dir, err)
	}
	q, err := loadRaw[rawQuantities](filepath.Join(dir, "quantities.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s/quantities.yaml: %w", dir, err)
	}
	l, err := loadRaw[rawLocations](filepath.Join(dir, "locations.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s/locations.yaml: %w", dir, err)
	}

	// Fragment files in ext.d/, sorted by name. Missing dir → empty.
	fragTypes, fragQuant, err := loadDictFragments(dir)
	if err != nil {
		return nil, err
	}

	d := &Dict{
		Types:      make(map[string]TypeDef, len(t.Types)),
		Quantities: make(map[string]QuantityDef, len(q.Quantities)),
		Derived:    make(map[string]QuantityDef, len(q.Derived)),
		Loops:      keysToBool(l.Loop),
		Sides:      keysToBool(l.Side),
		Phases:     keysToBool(l.Phase),
		Domains:    t.Domains,
		Relations:  make(map[string]bool, len(t.Relations)),
	}
	for name := range t.Relations {
		d.Relations[name] = true
	}

	for name, def := range t.Types {
		parents, _ := def["parents"].([]interface{})
		level, _ := def["level"].(string)
		desc, _ := def["desc"].(string)
		attrs, _ := def["attrs_required"].([]interface{})
		d.Types[name] = TypeDef{
			Parents:       toStrings(parents),
			Level:         Level(level),
			Desc:          desc,
			AttrsRequired: toStrings(attrs),
		}
	}

	for name, def := range q.Quantities {
		unit, _ := def["unit"].(string)
		kind, _ := def["kind"].(string)
		d.Quantities[name] = QuantityDef{
			Unit: unit,
			Kind: kind,
			Enum: parseEnum(def["enum"]),
		}
	}

	for name, def := range q.Derived {
		unit, _ := def["unit"].(string)
		host, _ := def["host"].([]interface{})
		d.Derived[name] = QuantityDef{Unit: unit, Host: toStrings(host)}
	}

	// Merge fragments in order; conflicts surface immediately.
	for _, ft := range fragTypes {
		for name, def := range ft.Types {
			if _, exists := d.Types[name]; exists {
				return nil, fmt.Errorf("ext.d conflict: type %q already defined in core or earlier fragment", name)
			}
			parents, _ := def["parents"].([]interface{})
			level, _ := def["level"].(string)
			desc, _ := def["desc"].(string)
			attrs, _ := def["attrs_required"].([]interface{})
			d.Types[name] = TypeDef{
				Parents:       toStrings(parents),
				Level:         Level(level),
				Desc:          desc,
				AttrsRequired: toStrings(attrs),
			}
		}
	}
	for _, fq := range fragQuant {
		for name, def := range fq.Quantities {
			if _, exists := d.Quantities[name]; exists {
				return nil, fmt.Errorf("ext.d conflict: quantity %q already defined in core or earlier fragment", name)
			}
			unit, _ := def["unit"].(string)
			kind, _ := def["kind"].(string)
			d.Quantities[name] = QuantityDef{
				Unit: unit,
				Kind: kind,
				Enum: parseEnum(def["enum"]),
			}
		}
		for name, def := range fq.Derived {
			if _, exists := d.Derived[name]; exists {
				return nil, fmt.Errorf("ext.d conflict: derived %q already defined in core or earlier fragment", name)
			}
			unit, _ := def["unit"].(string)
			host, _ := def["host"].([]interface{})
			d.Derived[name] = QuantityDef{Unit: unit, Host: toStrings(host)}
		}
	}

	return d, nil
}

// loadDictFragments returns the ext.d fragment files, sorted by name.
// A missing or empty ext.d directory is not an error. Each fragment is
// expected to be either fragmentTypes (has types) or fragmentQuantities
// (has quantities / derived) or both. We probe both shapes for each
// file: this is cheap and avoids forcing the fragment author to declare
// a section header.
func loadDictFragments(dir string) ([]fragmentTypes, []fragmentQuantities, error) {
	extDir := filepath.Join(dir, "ext.d")
	entries, err := os.ReadDir(extDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", extDir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	var ts []fragmentTypes
	var qs []fragmentQuantities
	for _, name := range files {
		path := filepath.Join(extDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		var ft fragmentTypes
		if err := yaml.Unmarshal(raw, &ft); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		var fq fragmentQuantities
		if err := yaml.Unmarshal(raw, &fq); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		ts = append(ts, ft)
		qs = append(qs, fq)
	}
	return ts, qs, nil
}

func loadRaw[T any](path string) (*T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func keysToBool[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func toStrings(xs []interface{}) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseEnum turns the raw "enum: { <int>: <label>, ... }" sub-map of a
// quantity into a map[int]string. yaml.v3 unmarshals nested flow-maps
// with int-like keys as map[interface{}]interface{}, so we coerce both
// map shapes here. A missing or non-map input yields nil — the caller
// treats nil as "this quantity has no enum".
func parseEnum(raw interface{}) map[int]string {
	if raw == nil {
		return nil
	}
	var entries []interface{}
	switch m := raw.(type) {
	case map[string]interface{}:
		entries = make([]interface{}, 0, len(m))
		for k, v := range m {
			entries = append(entries, k, v)
		}
	case map[interface{}]interface{}:
		entries = make([]interface{}, 0, len(m))
		for k, v := range m {
			entries = append(entries, k, v)
		}
	default:
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	out := make(map[int]string, len(entries)/2)
	for i := 0; i < len(entries); i += 2 {
		ks, kok := entries[i].(string)
		if !kok {
			// yaml may parse "0" as int 0; coerce.
			if ki, kiok := entries[i].(int); kiok {
				ks = strconv.Itoa(ki)
			} else {
				continue
			}
		}
		n, err := strconv.Atoi(ks)
		if err != nil {
			// Skip non-int keys silently — the dictionary is trusted
			// and the speccheck tool would catch a structural error.
			continue
		}
		label, _ := entries[i+1].(string)
		out[n] = label
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
