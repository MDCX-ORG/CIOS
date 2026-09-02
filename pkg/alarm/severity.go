// Package alarm — severity.go: the canonical severity whitelist
// from spec-003 §2. Both pkg/alarm/rule.go (rule loader) and
// core/alarms.go (HTTP filter) used to keep their own copies
// "by convention only"; PRMT-030 §A collapses them onto this
// single source. Adding a new severity requires editing spec-003
// first — this file is downstream of that change, not upstream of it.
package alarm

// AllowedSeverities is the spec-003 §2 severity set. The map-of-
// empty-struct form gives O(1) membership tests without pulling in
// a generics dependency, matching the prior duplicates' style.
var AllowedSeverities = map[string]struct{}{
	"critical": {},
	"major":    {},
	"minor":    {},
	"info":     {},
}
