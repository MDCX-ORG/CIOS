// Package core — vmquery.go: shared VictoriaMetrics/PromQL helpers.
//
// These helpers are used by the reconcile scanner and the usage
// scanner. They originated in the Capacity Engine (Commercial) and
// were factored out here so the open-source tree carries no
// dependency on that module.
package core

import (
	"strconv"
	"strings"
)

// defaultCapacityWindow is the trailing window for the P95 query
// when the client does not pass ?window=. 7d matches spec-002's
// "weekly capacity planning" default.
const defaultCapacityWindow = "7d"

// maxCapacityWindowDays is the soft cap on the trailing window in
// days (mirrors PRMT-040 §4 "configurable via ?window="). The cap
// keeps the VM query bounded (quantile_over_time over a year is
// expensive) and prevents accidental foot-guns from a typo'd
// window like "7y".
const maxCapacityWindowDays = 90

// validCapacityWindow enforces the trailing-window grammar
// (e.g. "7d", "12h", "30m"). Kept intentionally simple: at most a
// 3-digit number followed by one of d/h/m/s. Compound ranges
// ("1d12h") are rejected so a malformed input cannot silently
// degrade to "0d".
func validCapacityWindow(s string) bool {
	if len(s) < 2 || len(s) > 5 {
		return false
	}
	unit := s[len(s)-1]
	if unit != 'd' && unit != 'h' && unit != 'm' && unit != 's' {
		return false
	}
	num := s[:len(s)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return false
	}
	if unit == 'd' && n > maxCapacityWindowDays {
		return false
	}
	return true
}

// escapeLabelValue escapes a label value per the Prometheus
// exposition format: replace \, ", \n with their backslash-
// escaped form. The result is safe to splice into a `key="..."`
// context. cpath forbids double quotes and newlines inside
// segments, so the escape is defensive — exercised by the
// test that injects a synthetic backslash path through the
// store-side bypass — but cheap to apply unconditionally.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}
