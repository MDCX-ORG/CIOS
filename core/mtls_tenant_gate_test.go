package core

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/url"
	"testing"
)

func reqWithTenant(t *testing.T, tenant string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://x/v1/assets", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != "" {
		r.Header.Set(tenantHeaderName, tenant) // Set canonicalizes; map literal does not
	}
	return r
}

func TestGateTenantHeader_LabOff(t *testing.T) {
	SetTenantHeaderRequiresMTLSPeer(false)
	t.Cleanup(func() { SetTenantHeaderRequiresMTLSPeer(false) })
	if msg := gateTenantHeader(reqWithTenant(t, "acme")); msg != "" {
		t.Fatalf("lab should accept: %s", msg)
	}
}

func TestGateTenantHeader_NoHeaderAlwaysOK(t *testing.T) {
	SetTenantHeaderRequiresMTLSPeer(true)
	t.Cleanup(func() { SetTenantHeaderRequiresMTLSPeer(false) })
	if msg := gateTenantHeader(reqWithTenant(t, "")); msg != "" {
		t.Fatalf("no header should pass: %s", msg)
	}
}

func TestGateTenantHeader_RequireWithoutPeer(t *testing.T) {
	SetTenantHeaderRequiresMTLSPeer(true)
	t.Cleanup(func() { SetTenantHeaderRequiresMTLSPeer(false) })
	if msg := gateTenantHeader(reqWithTenant(t, "acme")); msg == "" {
		t.Fatal("expected reject without peer")
	}
}

func TestGateTenantHeader_RequireWithAPIGWPeer(t *testing.T) {
	SetTenantHeaderRequiresMTLSPeer(true)
	t.Cleanup(func() { SetTenantHeaderRequiresMTLSPeer(false) })
	u, _ := url.Parse("cios://sgp01/apigw")
	r := reqWithTenant(t, "acme")
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: "apigw"},
			URIs:    []*url.URL{u},
		}},
	}
	if msg := gateTenantHeader(r); msg != "" {
		t.Fatalf("apigw peer should accept: %s", msg)
	}
}
