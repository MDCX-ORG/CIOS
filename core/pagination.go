// Package core — pagination.go: shared page-size constants for the
// /v1 listing endpoints (assets, alarms, ...).
//
// These values are referenced by both the core HTTP handlers
// (assets.go, alarms.go) and the CLI dispatcher. Behaviour must stay
// identical to the literals that previously lived inline in each
// handler — same defaults, same cap — so PRMT-070 explicitly forbids
// changing the numeric values; only the indirection changes.
package core

import (
	"fmt"
	"strconv"
)

// DefaultPageSize is the page_size applied when a list request omits
// the query parameter. Mirrors M0 behaviour.
const DefaultPageSize = 100

// MaxPageSize is the hard cap on page_size (and the equivalent
// `limit` parameter on /v1/assets). Requests above this value are
// rejected with HTTP 400 on asset lists.
const MaxPageSize = 1000

// parseAdminPageSize parses page_size for L109 admin list endpoints
// (tenants / orgs / site-orgs / role-bindings). Default is MaxPageSize
// (not DefaultPageSize): those lists historically returned the full
// set, so defaulting to 1000 minimizes breakage while imposing a hard
// cap (PRMT-218). Values above MaxPageSize are clamped; <=0 is an error.
func parseAdminPageSize(raw string) (int, error) {
	if raw == "" {
		return MaxPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad page_size")
	}
	if n > MaxPageSize {
		n = MaxPageSize
	}
	return n, nil
}
