// Package mtls provides shared TLS helpers for component mTLS (P793 / L34 / L104).
// Lab default is off; cloud profile uses ModeRequire.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Mode controls component mTLS boot posture.
type Mode string

const (
	ModeOff     Mode = "off"     // plain HTTP (lab / loopback)
	ModeRequire Mode = "require" // TLS + client certs required
)

// ParseMode maps env/flag values. Empty → off.
//
// Fail-closed (M4 F2 / P793): unrecognized values return an error so
// callers refuse boot instead of silently treating typos as ModeOff.
// Known synonyms only: off|0|false|no and require|on|1|true|yes.
func ParseMode(s string) (Mode, error) {
	raw := strings.TrimSpace(s)
	switch strings.ToLower(raw) {
	case "", "off", "0", "false", "no":
		return ModeOff, nil
	case "require", "on", "1", "true", "yes":
		return ModeRequire, nil
	default:
		return "", fmt.Errorf("mtls: unknown mode %q (want off|require)", raw)
	}
}

// Material is on-disk PEM paths for a component.
type Material struct {
	CertFile string
	KeyFile  string
	// ClientCAFile is the pool of CAs trusted for peer client certs (server side).
	// For clients, CAFile is the pool that verifies the server certificate.
	CAFile string
}

// ServerTLS builds a tls.Config for ListenAndServeTLS with client-cert required.
func ServerTLS(m Material) (*tls.Config, error) {
	if m.CertFile == "" || m.KeyFile == "" || m.CAFile == "" {
		return nil, fmt.Errorf("mtls: server requires cert, key, and client CA")
	}
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	pem, err := os.ReadFile(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: client CA has no valid certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLS builds a tls.Config for outbound HTTPS with client certificate.
func ClientTLS(m Material) (*tls.Config, error) {
	if m.CertFile == "" || m.KeyFile == "" || m.CAFile == "" {
		return nil, fmt.Errorf("mtls: client requires cert, key, and server CA")
	}
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client keypair: %w", err)
	}
	pem, err := os.ReadFile(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: server CA has no valid certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// PeerComponent returns a short component name from the verified peer certificate.
// Prefer URI SAN of form cios://site/component; else CN; else empty.
func PeerComponent(r *http.Request) string {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	c := r.TLS.PeerCertificates[0]
	for _, u := range c.URIs {
		if u == nil {
			continue
		}
		// cios://site/apigw → apigw
		if u.Scheme == "cios" {
			p := strings.Trim(u.Path, "/")
			if p == "" {
				p = strings.Trim(u.Host, "/")
			}
			if i := strings.LastIndex(p, "/"); i >= 0 {
				return p[i+1:]
			}
			if p != "" {
				return p
			}
		}
	}
	if c.Subject.CommonName != "" {
		// allow CN like "apigw" or "cios-apigw" or "site-apigw"
		cn := strings.ToLower(c.Subject.CommonName)
		if strings.Contains(cn, "apigw") {
			return "apigw"
		}
		if strings.Contains(cn, "core") {
			return "core"
		}
		return cn
	}
	return ""
}

// IsAPIGW reports whether the peer looks like the experience-layer gateway.
func IsAPIGW(component string) bool {
	c := strings.ToLower(strings.TrimSpace(component))
	return c == "apigw" || c == "cios-apigw" || strings.HasSuffix(c, "-apigw")
}
