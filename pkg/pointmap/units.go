// Package pointmap — units.go: the unit conversion table that drives V2
// (and is also useful to the gateway at runtime). The table is loaded
// once from protocol/units.yaml plus any ext.d/*.yaml fragments
// (L54); subsequent lookups are O(1) and allocation-free.
package pointmap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Conv is the linear transform raw -> standard: standard = raw*Factor + Offset.
type Conv struct {
	Factor float64
	Offset float64
}

// Units is the in-memory form of protocol/units.yaml (+ ext.d fragments).
type Units struct {
	// std -> in -> Conv. Identical-unit conversions (in == std) are
	// resolved by CanConvert without consulting this map.
	accepts map[string]map[string]Conv
}

// LoadUnits reads dir/units.yaml and any ext.d/*.yaml fragments
// (L54), and returns the merged table. A fragment that re-defines a
// standard unit already in the core table or in an earlier fragment is
// a load error. The call is expected once at process start, so we
// trade memory for the O(1) lookup below.
func LoadUnits(dir string) (*Units, error) {
	core, err := readUnitsDoc(filepath.Join(dir, "units.yaml"))
	if err != nil {
		return nil, err
	}
	fragments, err := readUnitsFragments(filepath.Join(dir, "ext.d"))
	if err != nil {
		return nil, err
	}
	u := &Units{accepts: make(map[string]map[string]Conv, len(core.Units))}
	if err := mergeUnitsDoc(u, core, "units.yaml"); err != nil {
		return nil, err
	}
	for i, frag := range fragments {
		// The fragment file is the ith .yaml under ext.d/ (sorted);
		// we name it for error messages via a synthetic label.
		label := fmt.Sprintf("ext.d fragment #%d", i+1)
		if err := mergeUnitsDoc(u, frag, label); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// unitsDoc is the in-memory shape of a units-bearing YAML file (the
// core units.yaml or an ext.d fragment).
type unitsDoc struct {
	Units map[string]struct {
		Accepts map[string]struct {
			Factor float64 `yaml:"factor"`
			Offset float64 `yaml:"offset"`
		} `yaml:"accepts"`
	} `yaml:"units"`
}

func readUnitsDoc(path string) (unitsDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return unitsDoc{}, fmt.Errorf("pointmap: read %s: %w", path, err)
	}
	var doc unitsDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return unitsDoc{}, fmt.Errorf("pointmap: parse %s: %w", path, err)
	}
	return doc, nil
}

func readUnitsFragments(extDir string) ([]unitsDoc, error) {
	entries, err := os.ReadDir(extDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("pointmap: read %s: %w", extDir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	var docs []unitsDoc
	for _, name := range files {
		doc, err := readUnitsDoc(filepath.Join(extDir, name))
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// mergeUnitsDoc folds one unitsDoc into u. The label is used in error
// messages so callers can tell which file a conflict came from.
func mergeUnitsDoc(u *Units, doc unitsDoc, label string) error {
	for std, def := range doc.Units {
		if _, exists := u.accepts[std]; exists {
			return fmt.Errorf("pointmap: %s: standard unit %q already defined in core or earlier fragment", label, std)
		}
		row := make(map[string]Conv, len(def.Accepts))
		for in, c := range def.Accepts {
			row[in] = Conv{Factor: c.Factor, Offset: c.Offset}
		}
		u.accepts[std] = row
	}
	return nil
}

// CanConvert reports whether a raw value in unit `in` can be expressed as
// a standard value in unit `std`. The identity case (in == std) is always
// accepted with Factor=1, Offset=0. Unknown unit pairs return (_, false).
//
// The returned Conv is suitable for the standard value formula
// `std = raw*Factor + Offset` applied at the gateway.
func (u *Units) CanConvert(std, in string) (Conv, bool) {
	if std == in {
		return Conv{Factor: 1, Offset: 0}, true
	}
	if row, ok := u.accepts[std]; ok {
		if c, ok := row[in]; ok {
			return c, true
		}
	}
	return Conv{}, false
}
