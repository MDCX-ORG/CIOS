// Sites handler: GET /api/sites is the first read-only aggregation
// route exposed by the experience-layer Gateway (spec-009 §7.1,
// PRMT-101 §2). It is a thin reverse-proxy over core /v1/sites —
// no business logic, no transformation beyond error mapping.
//
// PRMT-105: the handler now also forwards the verified caller
// identity (injected by AuthMiddleware into r.Context() via
// WithClaims) to core /v1 by calling Upstream.GetV1As. The Gateway
// does NOT inspect the claims to filter the response — per
// spec-009 §7.1 / L81 the resource scope authority is core's path-
// glob RBAC (L34/L50); the Gateway carries identity, never judges
// visibility. Do not add filtering, projection, or pagination
// logic here.
//
// PRMT-109: the handler now also dispatches by the verified
// tenant + isolation tier (L83). Tier 缺失/非法 → 403 (fail-closed,
// PRMT-109 §5). Otherwise the handler forwards via
// Upstream.GetV1AsTenant, which attaches the X-CIOS-Tenant
// propagation header (PRMT-109 §4). The actual enforcement is
// downstream: label tier injects `tenant="<id>"` into PromQL on
// metrics-bearing routes (a future PRMT), and row/db tier apply
// per-tenant predicates / database routing inside core (PRMT-109
// §10 Deferred). This handler does NOT inspect crn / path-globs —
// that remains core's job (L81).
package apigw

import (
	"errors"
	"net/http"

	"github.com/yurimeng/cios/pkg/tenant"
)

// handleSites serves GET /api/sites by reverse-calling
// core /v1/sites. Behaviour matrix (PRMT-101 §4 + §5 +
// PRMT-109 §5):
//
//	client | upstream / tier      | response
//	-------|----------------------|---------------------------------------
//	GET    | 2xx                  | 200 + verbatim body (Content-Type
//	         |                    | copied from upstream if present,
//	         |                    | else defaulted to application/json)
//	GET    | 4xx                  | 4xx + verbatim body (RFC7807 passthrough)
//	GET    | 5xx                  | 502 + RFC7807 "upstream-unavailable"
//	GET    | transport error      | 502 + RFC7807 "upstream-unavailable"
//	GET    | no claims in ctx     | 401 + RFC7807 "unauthorized"
//	         | (PRMT-105 §5)      | (theoretical; AuthMiddleware gates)
//	GET    | tenant/tier missing | 403 + RFC7807 "forbidden"
//	         | or invalid (PRMT-109)
//	non-GET| (n/a)                | 405 + RFC7807 "bad-request"
//
// The handler is intentionally minimal: PRMT-101 forbids any
// aggregation/transformation, PRMT-105 forbids any resource
// filtering, and PRMT-109 forbids any crn / path-glob logic on
// the Gateway (L81). Visibility is core /v1's job; tenant
// enforcement is core /v1's job (or, for label tier on metrics,
// the pkg/tenant InjectTenantLabel pass).
func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	// Method gate. We only allow GET on /api/sites in this batch
	// (PRMT-101 §5). Anything else is 405 with Allow: GET so
	// well-behaved clients can recover.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/sites only supports GET", r.URL.Path)
		return
	}

	// PRMT-105: recover the verified caller identity injected by
	// AuthMiddleware. AuthMiddleware already gates unauthenticated
	// requests with 401, so a missing-claims branch should be
	// unreachable in production — but PRMT-105 §5 says we MUST
	// fail closed (401) rather than silently forwarding an
	// anonymous request to /v1.
	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	// PRMT-109 §2 / §5: resolve (tenant, tier) up front and
	// fail closed on absence / invalidity. We do NOT inspect the
	// claims to filter the response (L81); we only confirm that
	// the tier claims are present so the upstream call below
	// gets its tenant propagation header. Tier-specific
	// enforcement happens in core (row/db) or in the PromQL
	// injector on metrics routes (label) — not here.
	_, _, tierOK := tenant.TenantFromClaims(claims)
	if !tierOK {
		WriteProblem(w, http.StatusForbidden,
			"forbidden", "tenant or isolation tier missing",
			"the verified token did not carry a usable tenant identity",
			r.URL.Path)
		return
	}

	rawToken, _ := RawTokenFrom(r.Context())
	status, body, contentType, err := s.up.GetV1AsTenant(r.Context(), claims, rawToken, "/v1/sites")
	if err != nil {
		// PRMT-109 §5: ErrTenantMissing is a caller-side signal,
		// not a transport failure — it should never reach this
		// line because we just resolved the tier above. If it
		// does (e.g. a future code path bypasses the resolve),
		// treat it the same as a 403 so the caller never sees
		// a tenant-less upstream call.
		if errors.Is(err, ErrTenantMissing) {
			WriteProblem(w, http.StatusForbidden,
				"forbidden", "tenant or isolation tier missing",
				"the verified token did not carry a usable tenant identity",
				r.URL.Path)
			return
		}
		// Transport-level failure (DNS, connection refused,
		// timeout, ctx cancelled). Per PRMT-101 §4 this maps to
		// RFC7807 502 upstream-unavailable. We do NOT include the
		// raw transport error in the public detail — that would
		// leak network topology to the caller. Operators can still
		// correlate via the server log (added in a follow-up
		// PRMT; PRMT-101 does not own request logging).
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/sites is not reachable", r.URL.Path)
		return
	}

	// Upstream responded. 5xx → translate to a Gateway-level 502 so
	// the caller always sees a single error shape. 4xx → pass
	// through (upstream already spoke RFC7807; respect it). 2xx →
	// pass body through unchanged.
	switch {
	case status >= 500:
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"core /v1/sites returned "+http.StatusText(status),
			r.URL.Path)
	default:
		// 2xx and 4xx: forward upstream response unchanged.
		// PRMT-115 §4: prefer the upstream Content-Type verbatim;
		// only fall back to a status-based default when the
		// upstream omitted the header (ContentType == "").
		//   - 4xx: upstream /v1 emits application/problem+json
		//     on every error path (spec-004 §4) — we keep the
		//     default for the empty-header case so a misbehaving
		//     upstream still surfaces the right shape.
		//   - 2xx: core /v1/sites returns JSON by default; a
		//     future non-JSON 2xx (e.g. text/csv) MUST be
		//     forwarded verbatim, not rewritten.
		ct := contentType
		if ct == "" {
			if status >= 400 {
				ct = "application/problem+json"
			} else {
				ct = "application/json"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}
