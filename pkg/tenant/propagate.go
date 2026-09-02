// Tenant propagation header for the row/db tiers. PRMT-109 §4 / §2.
//
// For tier=row and tier=db, the Gateway does NOT execute the
// isolation itself (row-level RLS and per-tenant database routing
// are core concerns, deferred to spec-001/spec-004升版 per
// PRMT-109 §10). The Gateway's only job is to forward the
// tenant identity to core /v1 so core can apply the correct
// row predicate or pick the right database.
//
// We carry the tenant id in a dedicated HTTP header rather than
// overloading Authorization: Bearer. The bearer is the caller's
// identity (sub); the tenant id is the per-tenant data scope —
// the two are independent and can change independently. Mixing
// them in one header would also force core's RBAC to parse a
// composite token, breaking spec-004 §6's "Authorization: Bearer
// <sub>" shape (PRMT-105).
//
// Header name is fixed here. spec-004 §6 does NOT yet enumerate
// tenant headers (PRMT-109 §10 Deferred); we pick a stable,
// namespaced name and document the choice. A future PRMT that
// re-writes the spec header registry MUST update this constant
// alongside the registry entry — there's only one site in the
// codebase that needs to move.
package tenant

// TenantHeaderName is the HTTP header used to propagate the tenant
// identity from the Gateway to core /v1 for row/db tiers (PRMT-109
// §4 TenantPropagationHeader). The X-CIOS- prefix mirrors the
// existing spec-004 convention for CIOS-internal headers (none yet
// shipped, but the namespace is reserved by spec-006 §5 for
// infrastructure metadata).
const TenantHeaderName = "X-CIOS-Tenant"

// TenantPropagationHeader returns the (name, value) pair to attach
// to outbound /v1 calls when the resolved tier is row or db
// (PRMT-109 §4). label-tier callers do not call this — they use
// InjectTenantLabel instead.
//
// tenantID is taken verbatim (no escaping): HTTP header values are
// opaque to the transport layer and core is expected to treat the
// value as a tenant identifier (not as text to splice into a
// query). TenantFromClaims rejects empty ids upstream, so the
// caller is guaranteed a non-empty string here.
func TenantPropagationHeader(tenantID string) (name, value string) {
	return TenantHeaderName, tenantID
}
