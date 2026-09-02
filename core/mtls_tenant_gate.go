// mtls_tenant_gate.go — P793 H3: X-CIOS-Tenant accepted only from mTLS apigw peer
// when TenantHeaderRequiresMTLSPeer is enabled (CIOS_MTLS_MODE=require).
package core

import (
	"net/http"
	"sync/atomic"

	"github.com/yurimeng/cios/pkg/mtls"
)

// tenantHeaderRequiresPeer is 1 when cloud mTLS mode requires a verified
// apigw client cert before accepting X-CIOS-Tenant. Default 0 = lab.
var tenantHeaderRequiresPeer atomic.Uint32

// SetTenantHeaderRequiresMTLSPeer enables/disables the H3 peer gate.
// Called once at process boot from cmd/cios-core.
func SetTenantHeaderRequiresMTLSPeer(v bool) {
	if v {
		tenantHeaderRequiresPeer.Store(1)
	} else {
		tenantHeaderRequiresPeer.Store(0)
	}
}

// TenantHeaderRequiresMTLSPeer reports the gate state (tests).
func TenantHeaderRequiresMTLSPeer() bool {
	return tenantHeaderRequiresPeer.Load() == 1
}

// gateTenantHeader returns a non-empty problem detail when the tenant
// header must be rejected; empty string means accept.
func gateTenantHeader(r *http.Request) string {
	if tenantHeaderRequiresPeer.Load() == 0 {
		return ""
	}
	// No header → nothing to gate (callers without tenant still work).
	if r.Header.Get(tenantHeaderName) == "" {
		return ""
	}
	peer := mtls.PeerComponent(r)
	if mtls.IsAPIGW(peer) {
		return ""
	}
	if peer == "" {
		return "X-CIOS-Tenant requires verified mTLS peer (apigw); no client certificate on this connection"
	}
	return "X-CIOS-Tenant rejected: peer component " + peer + " is not apigw"
}

// tenantMTLSGateMW enforces H3 even when RBAC auth middleware is off
// (-allow-no-auth lab). Without this, SetTenantHeaderRequiresMTLSPeer
// only ran inside authMW and was a no-op under allow-no-auth.
type tenantMTLSGateMW struct {
	inner http.Handler
}

func newTenantMTLSGate(inner http.Handler) http.Handler {
	return &tenantMTLSGateMW{inner: inner}
}

func (m *tenantMTLSGateMW) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if msg := gateTenantHeader(r); msg != "" {
		rid := RequestIDFromContext(r.Context())
		writeProblem(w, http.StatusForbidden, "forbidden",
			"Forbidden", msg, r.URL.Path, rid)
		return
	}
	m.inner.ServeHTTP(w, r)
}
