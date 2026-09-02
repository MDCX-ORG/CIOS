// Package rules — compute.go: the per-bucket arithmetic reduction
// that turns a (host instance × location-prefix × input map) into
// one derived-point value.
//
// Compute is the simplest piece of the rules pipeline. It is pure:
// no I/O, no clock, no global state. The cmd loop:
//
//  1. discovers what relative-point names exist in VM for a
//     given Derived;
//  2. suffix-matches each one against d.Formula.Refs() to find
//     the location prefix (the part before the ref) — see
//     PRMT-021 §4.4 (b);
//  3. groups by (assetPath, locPrefix) into buckets;
//  4. for each bucket that has every ref present, calls
//     Compute(d, assetPath, locPrefix, inputs).
//
// The OLD design had Compute also responsible for "do the ref
// names share a common prefix? if not, error out" — that was
// fragile (e.g. "return.temp - supply.temp" has the empty string
// as common prefix, which would have rejected the spec-002 §9
// example). PRMT-021 §4.3 / §1.3 move the prefix responsibility
// to the discovery/bucketing step, so Compute is a straight
// arithmetic reduction. **No "prefix mismatch" logic** lives here.
package rules

import "strings"

// Compute evaluates a single (host instance × location-prefix)
// bucket of a derived quantity.
//
//   - d: the in-scope Derived definition (already compiled, already
//     confirmed in scope by LoadDerived).
//   - assetPath: the absolute host instance path, e.g.
//     "sgp01.pod002.cdu000". This is the FULL point path the
//     promtext row will eventually carry in its `path` label
//     (see cmd/cios-rules).
//   - locPrefix: the location prefix discovered by the cmd loop's
//     suffix-matching step (PRMT-021 §4.4 b), e.g. "fws" for
//     a cdu's primary water-supply loop. May be "" when the
//     formula's ref is already at asset level (no fold
//     dimension).
//   - inputs: the formula's Refs() mapped to their current
//     numeric values. Keys are the ref names with the locPrefix
//     stripped (i.e. "return.temp", not "fws.return.temp").
//
// Returns the derived point's full address (assetPath + locPrefix
// + d.Name joined by dots) and its computed value. A missing ref
// produces ErrMissingInput; the caller is expected to skip the
// bucket and continue (per §2-bis #3 — never emit a NaN/zero).
//
// Errors from the underlying formula (e.g. division by zero)
// propagate unchanged. The caller logs and skips the bucket.
func Compute(d Derived, assetPath, locPrefix string, inputs map[string]float64) (pointPath string, value float64, err error) {
	v, err := d.Formula.Eval(inputs)
	if err != nil {
		// Pass ErrMissingInput through unchanged so the caller's
		// errors.Is check works. Other errors (div-by-zero etc.)
		// carry their own context.
		return "", 0, err
	}
	// Address join: non-empty segments in [assetPath, locPrefix, d.Name]
	// joined by '.'. Spec-002 §9 mandates the three-segment form
	// for non-site derived points; an empty locPrefix (no fold
	// dimension) collapses to assetPath.d.Name, which is still
	// a valid spec-002 address.
	parts := []string{assetPath}
	if locPrefix != "" {
		parts = append(parts, locPrefix)
	}
	parts = append(parts, d.Name)
	pointPath = strings.Join(parts, ".")
	return pointPath, v, nil
}
