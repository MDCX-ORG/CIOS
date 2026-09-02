package mtls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	m, err := ParseMode("")
	if err != nil || m != ModeOff {
		t.Fatalf("empty → off, got %q err=%v", m, err)
	}
	m, err = ParseMode("require")
	if err != nil || m != ModeRequire {
		t.Fatalf("require, got %q err=%v", m, err)
	}
	m, err = ParseMode("REQUIRE")
	if err != nil || m != ModeRequire {
		t.Fatalf("REQUIRE, got %q err=%v", m, err)
	}
	if _, err := ParseMode("requrie"); err == nil {
		t.Fatal("typo must fail closed")
	}
	if _, err := ParseMode("tls"); err == nil {
		t.Fatal("unknown must fail closed")
	}
}

func TestPeerComponent_URIAndCN(t *testing.T) {
	uri, _ := url.Parse("cios://sgp01/apigw")
	cert := &x509.Certificate{
		Subject: pkix.Name{CommonName: "other"},
		URIs:    []*url.URL{uri},
	}
	r := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}}
	if got := PeerComponent(r); got != "apigw" {
		t.Fatalf("URI peer = %q", got)
	}
	r2 := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
		Subject: pkix.Name{CommonName: "cios-apigw"},
	}}}}
	if got := PeerComponent(r2); got != "apigw" {
		t.Fatalf("CN peer = %q", got)
	}
	if !IsAPIGW("apigw") || IsAPIGW("core") {
		t.Fatal("IsAPIGW")
	}
}

func TestServerAndClientTLS_RoundTripFiles(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := mustSelfSignedCA(t)
	srvCert, srvKey := mustLeaf(t, caCert, caKey, "core", "cios://sgp01/core")
	cliCert, cliKey := mustLeaf(t, caCert, caKey, "apigw", "cios://sgp01/apigw")

	caPath := writePEM(t, dir, "ca.pem", "CERTIFICATE", caCert.Raw)
	sc := writePEM(t, dir, "core.pem", "CERTIFICATE", srvCert.Raw)
	sk := writeKey(t, dir, "core.key", srvKey)
	cc := writePEM(t, dir, "apigw.pem", "CERTIFICATE", cliCert.Raw)
	ck := writeKey(t, dir, "apigw.key", cliKey)

	st, err := ServerTLS(Material{CertFile: sc, KeyFile: sk, CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	if st.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("client auth")
	}
	ct, err := ClientTLS(Material{CertFile: cc, KeyFile: ck, CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(ct.Certificates) != 1 {
		t.Fatal("client cert")
	}
}

func mustSelfSignedCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cios-dev-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func mustLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, cn, uri string) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeKey(t *testing.T, dir, name string, key *rsa.PrivateKey) string {
	t.Helper()
	p := filepath.Join(dir, name)
	b := x509.MarshalPKCS1PrivateKey(key)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: b}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
