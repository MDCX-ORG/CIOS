// Package tenant: tenant-isolation tier resolution and enforcement
// at the experience-layer Gateway. PRMT-109 §4.
//
// Scope (PRMT-109 §0, §1, §2):
//   - Resolve the per-tenant isolation_tier (db | row | label) from
//     the verified STS claims.
//   - Label tier: inject a `tenant="<id>"` label into every PromQL
//     selector that crosses the Gateway. This is the execution path
//     for tier=label — the tier maps to L53 (PromQL label-level
//     tenant isolation).
//   - Row/db tier: emit a tenant propagation header alongside the
//     bearer so core /v1 can apply row-level predicates (RLS) or
//     per-tenant database routing. The Gateway itself does NOT
//     inspect asset-resource scopes (L81 red line) and does NOT
//     decide per-row visibility (L83 / spec-001/spec-004 deferred).
//
// Hard non-goals (PRMT-109 §6):
//   - No third-party PromQL parser; the label injector uses a
//     hand-rolled, conservative selector scanner and fails closed
//     on anything ambiguous.
//   - No resource-scope logic. The tenant label is a tenant
//     identity tag, not an authorisation scope.
package tenant

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yurimeng/cios/pkg/sts"
)

// Tier is the per-tenant isolation depth (L83). The wire form is the
// short token "db" / "row" / "label"; ParseTier is the only sanctioned
// entry point so a typo never silently degrades to a weaker tier.
type Tier string

const (
	// TierDB is the strongest tier — VIP / regulated tenants land on a
	// per-tenant database or schema. The Gateway does NOT pick the
	// database (spec-001/spec-004 deferred, §10 of PRMT-109); it only
	// attaches the tenant id so core can route.
	TierDB Tier = "db"

	// TierRow is the middle tier — row-level predicates (e.g. PG RLS)
	// on a shared schema. As with TierDB, Gateway only propagates the
	// tenant id.
	TierRow Tier = "row"

	// TierLabel is the default tier — tenant isolation is enforced by
	// injecting a tenant="<id>" label into every PromQL selector.
	// This tier is what L53 reserved PromQL label space for.
	TierLabel Tier = "label"
)

// ErrInvalidTier is returned by ParseTier for any string outside the
// {db, row, label} allowlist. The HTTP handler maps this to 403
// (PRMT-109 §5 fail-closed). Case sensitivity is deliberate: the
// upstream tenant record is the single source of truth and emits
// the canonical lowercase form; a mixed-case value is almost
// certainly a client bug or a downgrade attempt.
var ErrInvalidTier = errors.New("tenant: invalid isolation tier")

// ParseTier validates s against the {db, row, label} allowlist.
// Empty input is rejected (PRMT-109 §5: tier 缺失/非法 → 403).
//
// Matching is exact and case-sensitive: "DB" / "Row" / "label " are
// rejected. The upstream tenant record is the single source of
// truth and emits the canonical lowercase form; a mixed-case or
// whitespace-padded value is almost certainly a client bug or a
// downgrade attempt and must fail closed.
func ParseTier(s string) (Tier, error) {
	switch s {
	case "db":
		return TierDB, nil
	case "row":
		return TierRow, nil
	case "label":
		return TierLabel, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidTier, s)
	}
}

// TenantFromClaims extracts the tenant id and isolation tier from
// a verified TokenClaims (PRMT-109 §4). Returns (id, tier, true)
// only when BOTH Tenant and IsolationTier are present AND the
// tier parses; otherwise (zero, zero, false).
//
// ok=false is the caller's signal to fail closed. The handler
// translates this to 403 — never to "anonymous tenant allowed"
// (PRMT-109 §6).
//
// TenantFromClaims does NOT consult any external store (e.g. a
// tenants table). The token is the source of truth in this round;
// a future PRMT can layer in a refresh / lookup, but spec-004 §6
// reserves the claims as the carrier (L83 §3 "tenant_id 传播").
func TenantFromClaims(c sts.TokenClaims) (id string, tier Tier, ok bool) {
	id = strings.TrimSpace(c.Tenant)
	if id == "" {
		return "", "", false
	}
	t, err := ParseTier(c.IsolationTier)
	if err != nil {
		return "", "", false
	}
	return id, t, true
}
