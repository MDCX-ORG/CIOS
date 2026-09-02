// pkg/tenant/org.go — Org claim resolution and site-set
// membership check. PRMT-110 §4.
//
// Scope (PRMT-110 §1, §2):
//   - Pull the identity's org + reachable-site set out of the
//     verified STS claims (L84 / D35: tenant → Org → site).
//   - Provide a tiny "is target site in the identity's site set"
//     helper so the gateway (R6 site switcher) and the PDP
//     (policy/rego/context.rego) can share one source of truth.
//
// Hard non-goals (PRMT-110 §6, §3):
//   - No resource-scope logic. The PDP / gateway do not look at
//     resource sub-trees here (L81 red line). pkg/tenant only
//     deals with the org's site membership.
//   - No org-table lookup. The token claim is the carrier
//     (L83 §3 / spec-004 §6 reserved); a future PRMT can layer
//     in a refresh / lookup, but spec-001 / spec-004 upgrades
//     are §10 deferred.
//   - No third-party deps. Mirrors PRMT-109's discipline.
package tenant

import (
	"strings"

	"github.com/yurimeng/cios/pkg/sts"
)

// OrgFromClaims extracts the identity's Organization and the
// site set reachable under that Org from a verified TokenClaims
// (PRMT-110 §4). Returns (org, sites, true) only when BOTH Org
// and Sites are present and non-empty; otherwise ("", nil,
// false).
//
// ok=false is the caller's signal to fail closed. The gateway /
// PDP MUST treat that as "no site switching allowed" — never
// as "anonymous org allowed" (PRMT-110 §5).
//
// Whitespace-only entries in Sites are dropped (defence in depth
// against upstream typos that would otherwise expand the
// reachable set). A Sites list composed entirely of whitespace
// entries collapses to ok=false.
//
// OrgFromClaims does NOT consult any external store. The token
// is the source of truth in this round, mirroring
// TenantFromClaims (PRMT-109 §4).
func OrgFromClaims(c sts.TokenClaims) (org string, sites []string, ok bool) {
	org = strings.TrimSpace(c.Org)
	if org == "" {
		return "", nil, false
	}
	if len(c.Sites) == 0 {
		return "", nil, false
	}
	cleaned := make([]string, 0, len(c.Sites))
	seen := make(map[string]struct{}, len(c.Sites))
	for _, s := range c.Sites {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return "", nil, false
	}
	return org, cleaned, true
}

// SiteAllowed reports whether target is in the identity's
// reachable site set. An empty target or an empty sites slice
// yields false (PRMT-110 §5 fail-closed: missing site set must
// NOT widen to "all sites allowed"). Whitespace-only target is
// normalised via TrimSpace; the comparison is exact and
// case-sensitive (L36 site codes are the canonical form).
func SiteAllowed(sites []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	if len(sites) == 0 {
		return false
	}
	for _, s := range sites {
		if s == target {
			return true
		}
	}
	return false
}
