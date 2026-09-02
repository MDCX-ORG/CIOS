// Package pointmap parses CIOS point-map YAML files (spec-002 §6) and
// runs the spec-005 §3 V1–V7 static validation rules before they reach
// the gateway. It is purely a load-time library: no I/O beyond reading
// the named YAML, no driver logic, no runtime unit conversion.
//
// A point map binds vendor protocol addresses (Modbus registers, OIDs,
// etc.) to relative CIOS point addresses — `appliesTo` is the asset
// type the points hang off, and each point's `point` field is the
// suffix appended to the asset path during runtime instantiation.
//
// The library is intentionally lenient about fields it does not own:
// unknown top-level keys and unknown per-point keys (other than the
// schema-violations listed in the spec) are stashed in PointMap.Protocol
// / PointDef.Protocol for the driver to consume. This keeps forward
// compatibility with vendor extensions without re-spinning this code.
package pointmap

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/pkg/cpath"
)

// PointMap is the validated, in-memory form of a single point-map YAML.
type PointMap struct {
	Name      string     // metadata.name, kebab-case, non-empty
	Driver    string     // metadata.driver, non-empty
	AppliesTo string     // metadata.appliesTo, must be a known type
	Points    []PointDef // spec.points, in source order
}

// PointDef is one row of spec.points.
type PointDef struct {
	Point     string                 // relative point address (location* + quantity)
	Access    string                 // "ro" (default) | "rw"
	RiskClass string                 // "a" | "b" | "c"; required iff Access == "rw"
	Source    string                 // "measured" (default) | "virtual"
	UnitIn    string                 // vendor unit; "" means standard unit
	Scale     float64                // 1.0 default; pre-multiply before unit conversion
	Limits    *Limits                // required iff RiskClass == "a"
	EnumMap   map[int]int            // vendor code -> standard code
	Readback  string                 // V5: readback register name (or alt Protocol key)
	Protocol  map[string]interface{} // everything else, handed to the driver
}

// Limits holds the safety envelope for a class-a writeable point.
type Limits struct{ Min, Max float64 }

// ErrPointMap is the sentinel wrapped by every validation error. Use
// errors.Is(err, ErrPointMap) to detect "any pointmap validation failure".
var ErrPointMap = errors.New("pointmap: validation failed")

// Load parses and validates a point-map YAML. A YAML syntax/IO error
// is returned as a single error (with the file path). Validation errors
// are returned as a slice — every rule that fires for every point is
// reported (no fail-fast). When errs is nil the point map is valid and
// ready to hand to a driver. All errs satisfy errors.Is(_, ErrPointMap).
//
// d supplies the asset/quantity/derived dictionaries (incl. the enum
// table for V3); u supplies the unit conversion table (LoadUnits). Both
// must come from the same protocol/ version or V2/V3 will give
// misleading verdicts.
func Load(path string, d *cpath.Dict, u *Units) (*PointMap, []error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []error{fmt.Errorf("%w: read %q: %v", ErrPointMap, path, err)}
	}
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Driver    string `yaml:"driver"`
			AppliesTo string `yaml:"appliesTo"`
		} `yaml:"metadata"`
		Spec struct {
			Points []map[string]interface{} `yaml:"points"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, []error{fmt.Errorf("%w: parse %q: %v", ErrPointMap, path, err)}
	}
	if doc.Kind != "PointMap" {
		return nil, []error{fmt.Errorf("%w: %q: kind must be PointMap, got %q", ErrPointMap, path, doc.Kind)}
	}

	var errs []error
	// --- metadata-level checks (V6 appliesTo + name/driver non-empty) ---
	pm := &PointMap{
		Name:      doc.Metadata.Name,
		Driver:    doc.Metadata.Driver,
		AppliesTo: doc.Metadata.AppliesTo,
	}
	if pm.Name == "" {
		errs = append(errs, fmt.Errorf("%w: V6 metadata.name is empty", ErrPointMap))
	}
	if pm.Driver == "" {
		errs = append(errs, fmt.Errorf("%w: V6 metadata.driver is empty", ErrPointMap))
	}
	if pm.AppliesTo == "" {
		errs = append(errs, fmt.Errorf("%w: V6 metadata.appliesTo is empty", ErrPointMap))
	} else if _, ok := d.Types[pm.AppliesTo]; !ok {
		errs = append(errs, fmt.Errorf("%w: V6 metadata.appliesTo %q is not a known type", ErrPointMap, pm.AppliesTo))
	}

	// --- V1 prerequisite: pre-compute the asset-path prefix for the
	// appliesTo type. Shortest parent chain to "site"; ties broken by
	// lexicographic order of the joined string (per spec-004 §4.3).
	var prefix string
	if pm.AppliesTo != "" {
		if p, ok := d.Types[pm.AppliesTo]; ok {
			prefix = "site01." + buildParentChain(pm.AppliesTo, p, d) + "."
		}
		// If appliesTo is unknown the V6 error above already fired and
		// prefix stays "" — V1 will fail per point with a less specific
		// message, which is fine.
	}

	// --- per-point validation ---
	seenPoint := make(map[string]int, len(doc.Spec.Points))
	for i, pointMap := range doc.Spec.Points {
		pd, perrs := decodePoint(pointMap, i)
		if len(perrs) > 0 {
			errs = append(errs, perrs...)
			continue
		}
		// V6: duplicate point
		if first, dup := seenPoint[pd.Point]; dup {
			errs = append(errs, fmt.Errorf("%w: V6 duplicate point %q (first seen at index %d)",
				ErrPointMap, pd.Point, first))
		} else {
			seenPoint[pd.Point] = i
		}
		// V1: parse the composed absolute path through cpath
		if prefix == "" {
			// appliesTo is invalid; skip V1 (V6 already reported).
		} else {
			abs := prefix + pd.Point
			if _, err := d.ParsePoint(abs); err != nil {
				errs = append(errs, fmt.Errorf("%w: V1 point %q: %v", ErrPointMap, pd.Point, err))
			}
		}
		// V2: unit convertibility
		if pd.UnitIn != "" {
			qName := quantityOf(pd.Point)
			stdUnit, kind := quantityStandardUnit(qName, d)
			if stdUnit == "" {
				errs = append(errs, fmt.Errorf("%w: V2 point %q: unknown quantity %q",
					ErrPointMap, pd.Point, qName))
			} else if _, ok := u.CanConvert(stdUnit, pd.UnitIn); !ok {
				errs = append(errs, fmt.Errorf("%w: V2 point %q: cannot convert %q to standard %q (kind=%s)",
					ErrPointMap, pd.Point, pd.UnitIn, stdUnit, kind))
			}
		}
		// V3: enum_map value subset (fail-closed via d.Quantities[].Enum).
		qDef, hasQ := d.Quantities[quantityOf(pd.Point)]
		if len(pd.EnumMap) > 0 {
			if !hasQ || qDef.Enum == nil {
				// Enum-typed declaration missing → EnumMap is meaningless.
				errs = append(errs, fmt.Errorf("%w: V3 point %q: enum_map on non-enum quantity %q",
					ErrPointMap, pd.Point, quantityOf(pd.Point)))
			} else {
				for _, v := range pd.EnumMap {
					if _, ok := qDef.Enum[v]; !ok {
						errs = append(errs, fmt.Errorf("%w: V3 point %q: enum_map value %d not in declared enum keys",
							ErrPointMap, pd.Point, v))
						break
					}
				}
			}
		} else if hasQ && qDef.Enum != nil {
			// Enum-typed quantity without enum_map: only legal if the
			// driver declares the raw register is already standard-coded.
			std, _ := pd.Protocol["standard_codes"]
			isStd, _ := std.(bool)
			if !isStd {
				errs = append(errs, fmt.Errorf("%w: V3 point %q: enum quantity requires enum_map or standard_codes: true",
					ErrPointMap, pd.Point))
			}
		}
		// V4: rw risk class / a class limits / ro not over-specified
		switch pd.Access {
		case "rw":
			if pd.RiskClass == "" {
				errs = append(errs, fmt.Errorf("%w: V4 point %q: rw requires risk_class",
					ErrPointMap, pd.Point))
			} else if pd.RiskClass != "a" && pd.RiskClass != "b" && pd.RiskClass != "c" {
				errs = append(errs, fmt.Errorf("%w: V4 point %q: risk_class must be a|b|c, got %q",
					ErrPointMap, pd.Point, pd.RiskClass))
			}
			if pd.RiskClass == "a" {
				if pd.Limits == nil {
					errs = append(errs, fmt.Errorf("%w: V4 point %q: class-a requires limits",
						ErrPointMap, pd.Point))
				} else if !(pd.Limits.Min < pd.Limits.Max) {
					errs = append(errs, fmt.Errorf("%w: V4 point %q: limits min %v must be < max %v",
						ErrPointMap, pd.Point, pd.Limits.Min, pd.Limits.Max))
				}
			}
		case "ro", "":
			if pd.RiskClass != "" {
				errs = append(errs, fmt.Errorf("%w: V4 point %q: ro must not carry risk_class",
					ErrPointMap, pd.Point))
			}
			if pd.Limits != nil {
				errs = append(errs, fmt.Errorf("%w: V4 point %q: ro must not carry limits",
					ErrPointMap, pd.Point))
			}
		default:
			errs = append(errs, fmt.Errorf("%w: V4 point %q: access must be ro|rw, got %q",
				ErrPointMap, pd.Point, pd.Access))
		}
		// V5: rw must have a readback path
		if pd.Access == "rw" {
			hasReg, _ := pd.Protocol["register"]
			if pd.Readback == "" && hasReg == nil {
				errs = append(errs, fmt.Errorf("%w: V5 point %q: rw requires readback or register",
					ErrPointMap, pd.Point))
			}
		}
		// V7: virtual must be ro; derived quantities forbidden in point map
		if pd.Source == "virtual" && pd.Access == "rw" {
			errs = append(errs, fmt.Errorf("%w: V7 point %q: virtual source cannot be rw",
				ErrPointMap, pd.Point))
		}
		if _, isDerived := d.Derived[quantityOf(pd.Point)]; isDerived {
			errs = append(errs, fmt.Errorf("%w: V7 point %q: derived quantities must not appear in point map",
				ErrPointMap, pd.Point))
		}
		pm.Points = append(pm.Points, pd)
	}
	if len(errs) == 0 {
		return pm, nil
	}
	return pm, errs
}

// --- internal helpers --------------------------------------------------------

// decodePoint turns one point-map (yaml unmarshalled to map[string]interface{})
// into a PointDef, applying defaults and peeling off the well-known keys into
// typed fields. Unknown keys land in Protocol. Returns per-point parse/
// structural errors.
func decodePoint(raw map[string]interface{}, idx int) (PointDef, []error) {
	if raw == nil {
		return PointDef{}, []error{fmt.Errorf("%w: point[%d] is not a mapping", ErrPointMap, idx)}
	}
	var pd PointDef
	var protocol map[string]interface{}
	known := map[string]bool{
		"point": true, "access": true, "risk_class": true, "source": true,
		"unit_in": true, "scale": true, "limits": true, "enum_map": true,
		"readback": true,
	}
	for k, v := range raw {
		switch k {
		case "point":
			s, _ := v.(string)
			pd.Point = s
		case "access":
			s, _ := v.(string)
			pd.Access = s
		case "risk_class":
			s, _ := v.(string)
			pd.RiskClass = s
		case "source":
			s, _ := v.(string)
			pd.Source = s
		case "unit_in":
			s, _ := v.(string)
			pd.UnitIn = s
		case "scale":
			f, err := toFloat(v)
			if err != nil {
				return pd, []error{fmt.Errorf("%w: point[%d].scale: %v", ErrPointMap, idx, err)}
			}
			pd.Scale = f
		case "limits":
			l, err := decodeLimits(v)
			if err != nil {
				return pd, []error{fmt.Errorf("%w: point[%d].limits: %v", ErrPointMap, idx, err)}
			}
			pd.Limits = l
		case "enum_map":
			em, err := decodeEnumMap(v)
			if err != nil {
				return pd, []error{fmt.Errorf("%w: point[%d].enum_map: %v", ErrPointMap, idx, err)}
			}
			pd.EnumMap = em
		case "readback":
			s, _ := v.(string)
			pd.Readback = s
		default:
			if known[k] {
				// known key with a non-string scalar (e.g. null point) — treat as missing value but record the duplicate.
				return pd, []error{fmt.Errorf("%w: point[%d]: duplicate or invalid key %q", ErrPointMap, idx, k)}
			}
			if protocol == nil {
				protocol = make(map[string]interface{})
			}
			protocol[k] = v
		}
	}
	// Defaults
	if pd.Access == "" {
		pd.Access = "ro"
	}
	if pd.Source == "" {
		pd.Source = "measured"
	}
	if pd.Scale == 0 {
		pd.Scale = 1.0
	}
	pd.Protocol = protocol
	return pd, nil
}

// toFloat accepts the yaml-native number forms (int, float64) and falls
// back to strconv for stringy numbers. Non-numeric input is an error.
func toFloat(v interface{}) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}

// decodeLimits turns a raw "limits: { min: ..., max: ... }" into *Limits.
// yaml parses flow-maps as map[string]interface{} (or map[interface{}]interface{}
// if keys are non-string), so we accept both.
func decodeLimits(v interface{}) (*Limits, error) {
	pair, err := minMax(v)
	if err != nil {
		return nil, err
	}
	return &Limits{Min: pair[0], Max: pair[1]}, nil
}

func minMax(v interface{}) ([2]float64, error) {
	get := func(m map[string]interface{}, key string) (float64, bool, error) {
		raw, ok := m[key]
		if !ok {
			return 0, false, nil
		}
		f, err := toFloat(raw)
		if err != nil {
			return 0, true, err
		}
		return f, true, nil
	}
	switch m := v.(type) {
	case map[string]interface{}:
		min, _, err := get(m, "min")
		if err != nil {
			return [2]float64{}, err
		}
		max, _, err := get(m, "max")
		if err != nil {
			return [2]float64{}, err
		}
		return [2]float64{min, max}, nil
	case map[interface{}]interface{}:
		conv := make(map[string]interface{}, len(m))
		for k, vv := range m {
			if ks, ok := k.(string); ok {
				conv[ks] = vv
			}
		}
		return minMax(conv)
	default:
		return [2]float64{}, fmt.Errorf("not a mapping: %T", v)
	}
}

// decodeEnumMap turns "enum_map: { 1: 0, 16: 3 }" into map[int]int.
// We accept both string keys (the common yaml-v3 form for flow-maps)
// and int keys (when the driver writes them bare).
func decodeEnumMap(v interface{}) (map[int]int, error) {
	entries := flattenMap(v)
	if entries == nil {
		return nil, fmt.Errorf("not a mapping: %T", v)
	}
	out := make(map[int]int, len(entries)/2)
	for i := 0; i < len(entries); i += 2 {
		nk, ok := toInt(entries[i])
		if !ok {
			return nil, fmt.Errorf("key %v is not int", entries[i])
		}
		nv, ok := toInt(entries[i+1])
		if !ok {
			return nil, fmt.Errorf("value %v is not int", entries[i+1])
		}
		out[nk] = nv
	}
	return out, nil
}

func flattenMap(v interface{}) []interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		out := make([]interface{}, 0, 2*len(m))
		for k, vv := range m {
			out = append(out, k, vv)
		}
		return out
	case map[interface{}]interface{}:
		out := make([]interface{}, 0, 2*len(m))
		for k, vv := range m {
			out = append(out, k, vv)
		}
		return out
	default:
		return nil
	}
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	default:
		return 0, false
	}
}

// quantityOf returns the last dotted segment of a relative point — that
// is the quantity name. spec-002 §1: point ::= ... "." quantity.
func quantityOf(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i+1:]
	}
	return p
}

// quantityStandardUnit looks up the standard unit for a quantity, scanning
// both the quantities section and the derived section. Returns "" if the
// quantity is not known to the dictionary. The kind is informational (used
// for the V2 error message).
func quantityStandardUnit(name string, d *cpath.Dict) (string, string) {
	if q, ok := d.Quantities[name]; ok {
		return q.Unit, string(q.Kind)
	}
	if q, ok := d.Derived[name]; ok {
		return q.Unit, "derived"
	}
	return "", ""
}

// buildParentChain finds the shortest parent chain from t up to "site",
// then renders each level with the right zero-padded index. Ties (multiple
// shortest chains) are broken by lexicographic order of the joined
// (already-zero-padded) string. Returns "" if no chain exists.
func buildParentChain(t string, def cpath.TypeDef, d *cpath.Dict) string {
	// BFS from t up the Parents graph; record depth + immediate parent.
	dist := map[string]int{t: 0}
	queue := []string{t}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "site" {
			break
		}
		var parents []string
		if cur == t {
			parents = def.Parents
		} else if pd, ok := d.Types[cur]; ok {
			parents = pd.Parents
		}
		for _, p := range parents {
			if _, seen := dist[p]; !seen {
				dist[p] = dist[cur] + 1
				queue = append(queue, p)
			}
		}
	}
	if _, ok := dist["site"]; !ok {
		return ""
	}
	// Pick the lexicographically smallest chain among all shortest paths
	// to "site". enumerateShortest returns the rendered string with
	// each level zero-padded per its Level, NOT including the leading
	// "site" node (the caller prepends "site01.").
	best := enumerateShortest(t, def, d, dist["site"]+1)
	if best != "" {
		return best
	}
	return ""
}

// enumerateShortest returns the lexicographically smallest shortest chain
// (excluding the leading "site") from t up to "site", with each segment
// zero-padded per its Level. distGoal is the BFS distance we must match.
func enumerateShortest(t string, def cpath.TypeDef, d *cpath.Dict, distGoal int) string {
	if t == "site" {
		return ""
	}
	// All paths of length distGoal-1 (i.e. distGoal nodes including t)
	// from t to "site", each rendered with index, lexicographically
	// smallest wins.
	var best string
	var walk func(cur string, depth int, acc []string)
	walk = func(cur string, depth int, acc []string) {
		if depth == distGoal {
			if cur == "site" {
				// acc holds the path t -> ... -> cur's parent (parent of site).
				// The rendered string must be in site->t order, so reverse.
				reversed := make([]string, len(acc))
				for i, n := range acc {
					reversed[len(acc)-1-i] = n
				}
				rendered := renderChain(reversed, d)
				if best == "" || rendered < best {
					best = rendered
				}
			}
			return
		}
		var parents []string
		if cur == t {
			parents = def.Parents
		} else if pd, ok := d.Types[cur]; ok {
			parents = pd.Parents
		}
		// Sort parents so the first child explored in lex order is tried
		// first; combined with the lex render, this prunes correctly.
		sorted := append([]string(nil), parents...)
		sort.Strings(sorted)
		for _, p := range sorted {
			walk(p, depth+1, append(acc, cur))
		}
	}
	walk(t, 1, nil)
	return best
}

func renderChain(acc []string, d *cpath.Dict) string {
	var b strings.Builder
	for i, name := range acc {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(name)
		b.WriteString(indexFor(name, d))
	}
	return b.String()
}

func indexFor(name string, d *cpath.Dict) string {
	if def, ok := d.Types[name]; ok {
		switch def.Level {
		case cpath.LevelDevice:
			return "000"
		case cpath.LevelChip:
			return "0"
		}
	}
	return ""
}
