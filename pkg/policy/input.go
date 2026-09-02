// Package policy is the experience-layer Policy Decision Point
// (PDP) used by the API Gateway (spec-009 §7.1, PRMT-104).
//
// pkg/policy is strictly CONTEXTUAL — it judges realm/action/MFA/
// time, never resource scope. Resource-scope authority (which
// asset branches a caller can touch) is the exclusive domain of
// core /v1's existing rules, enforced by PRMT-105. The PDP merely
// consumes the verified token's scope as opaque input.
//
// The boundary is enforced two ways:
//   - pkg/policy/Input.Path is treated as a CONTEXTUAL attribute
//     only; it is NOT consulted when computing allow/deny.
//   - policy/rego/context.rego contains no matching logic on the
//     path field. The acceptance grep in PRMT-104 §7 pins this;
//     the file must stay path-free.
package policy

import "time"

// Input is the PDP's decision input. Field semantics (PRMT-104 §4):
//
//   - Realm  : token realm ("ops" | "customer"). The PDP verifies
//     the realm is compatible with the surface the request is
//     hitting — the gateway path prefix already encodes the
//     surface, so this is the cross-product we authorise.
//   - Action : "read" | "write". The gateway maps r.Method:
//     GET → read, everything else → write (PRMT-104 §4; this is a
//     static, conservative mapping and will be refined when SSE /
//     write paths come online).
//   - Method : original HTTP verb. Carried for logging / future
//     refinement; not used in allow/deny by the sample Rego.
//   - Path   : request path, carried as context only. The PDP does
//     NOT use it for resource-scope decisions — that is core's
//     job (PRMT-105).
//   - MFA    : whether the verified token asserts MFA was
//     performed. Sensitive actions (e.g. write) require MFA per
//     the sample Rego; we leave the hook so PRMT-108 (CLI bearer
//     via STS) can flip it on without changing the contract.
//   - Time   : decision time. The sample Rego treats off-hour
//     access as a deny signal so this hook is observable in tests.
//   - Scope  : token scope, copied verbatim from sts.Verify. The
//     PDP treats it as opaque input; the sample Rego does NOT use
//     it to authorise access — that is core RBAC's job.
//   - Org    : Organization the identity belongs to under the
//     tenant (L84 / D35; PRMT-110 §4). Carried so the PDP can
//     authorise the site-switch context. Empty = "no org claim";
//     the Rego rule below treats that as fail-closed for site
//     switching (PRMT-110 §5).
//   - Sites : site codes (L36) the identity can reach under the
//     named Org. The PDP compares TargetSite against this set
//     (PRMT-110 §4: "TargetSite ∈ Sites"). Resource scope inside
//     a site remains core RBAC's job (L81 red line); this set is
//     strictly a cross-site group membership, never a crn-scope
//     claim.
//   - TargetSite : the site the request is targeting, taken from
//     the request (path / query). When non-empty, the PDP
//     requires it to be a member of Sites (R6 site switcher,
//     PRMT-110 §1). An empty TargetSite is treated as "no site
//     switch requested" so the rule does not over-deny global
//     reads; callers that need strict scoping must set it
//     explicitly.
type Input struct {
	Realm      string    `json:"realm"`
	Action     string    `json:"action"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	MFA        bool      `json:"mfa"`
	Time       time.Time `json:"time"`
	Scope      []string  `json:"scope"`
	Org        string    `json:"org,omitempty"`
	Sites      []string  `json:"sites,omitempty"`
	TargetSite string    `json:"target_site,omitempty"`
}
