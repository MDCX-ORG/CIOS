// Package core — vmquery_test.go: shared PromQL test helpers.
//
// parseAssetPathMatcher was factored out of the Capacity Engine
// test suite (Commercial) so the open-source reconcile / usage
// tests keep their fake-VM matcher parser.
package core

import "strings"

// parseAssetPathMatcher extracts the asset_path label value(s)
// from a PromQL query, supporting both the per-asset exact form
// (asset_path="X") and the PRMT-086 batch regex form
// (asset_path=~"^(X|Y|Z)$"). Returns the list of paths the
// matcher asks for (in the un-escaped form the production code
// supplied, e.g. `site01.pod000.cdu000` even when the wire form
// has `\.`). The fake-VM helpers in this file use it to dispatch
// a single sample per path so the production fetcher's per-path
// map gets populated correctly. The `^(` / `)$` anchors are
// stripped and `\.` → `.` is undone before splitting on `|`.
func parseAssetPathMatcher(q string) []string {
	// Try the exact matcher first (per-asset / legacy sinks).
	if i := strings.Index(q, `asset_path="`); i >= 0 {
		rest := q[i+len(`asset_path="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return nil
		}
		return []string{rest[:j]}
	}
	// Batch regex matcher. The body is the alternation between
	// the first closing `"` and the next `"`. Strip the
	// `^(` / `)$` anchors the production matcher wraps around
	// the alternation (regex-safe matcher), unescape `\.` →
	// `.` per part (regexp.QuoteMeta on `.`), and split on `|`.
	if i := strings.Index(q, `asset_path=~"`); i >= 0 {
		rest := q[i+len(`asset_path=~"`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return nil
		}
		body := rest[:j]
		body = strings.TrimPrefix(body, "^(")
		body = strings.TrimSuffix(body, ")$")
		if body == "" {
			return nil
		}
		parts := strings.Split(body, "|")
		for k, p := range parts {
			parts[k] = strings.ReplaceAll(p, `\.`, ".")
		}
		return parts
	}
	return nil
}
