// dataplane.go — P793 Phase 3: product-native TLS for PG / NATS / HTTPS
// upstreams (VM, control). Prefer server CA verify (+ optional client
// cert) over inventing a second component mTLS hop.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// OutboundTLS builds a client tls.Config that verifies the server with
// CAFile. Optional CertFile+KeyFile enable mutual TLS to the data-plane
// peer (Postgres/NATS/HTTPS). CAFile is required; empty → error.
func OutboundTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	caFile = strings.TrimSpace(caFile)
	if caFile == "" {
		return nil, fmt.Errorf("mtls: outbound TLS requires CA file")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: CA has no valid certificates")
	}
	cfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("mtls: outbound client cert requires both cert and key")
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("mtls: load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// PGDSNApplyTLS injects sslmode=verify-full + sslrootcert (+ optional
// client cert) into a Postgres DSN URL or libpq keyword string.
// Empty caFile → dsn returned unchanged (lab plain TCP).
//
// Existing sslmode / sslrootcert query params are overwritten so a
// mis-set "sslmode=disable" cannot silently bypass Phase 3 require.
func PGDSNApplyTLS(dsn, caFile, certFile, keyFile string) (string, error) {
	caFile = strings.TrimSpace(caFile)
	if caFile == "" {
		return dsn, nil
	}
	if _, err := os.Stat(caFile); err != nil {
		return "", fmt.Errorf("mtls: pg CA: %w", err)
	}
	// URL form: postgres://… or postgresql://…
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("mtls: parse pg dsn: %w", err)
		}
		q := u.Query()
		q.Set("sslmode", "verify-full")
		q.Set("sslrootcert", caFile)
		if c := strings.TrimSpace(certFile); c != "" {
			if k := strings.TrimSpace(keyFile); k == "" {
				return "", fmt.Errorf("mtls: pg client cert requires key")
			}
			q.Set("sslcert", c)
			q.Set("sslkey", strings.TrimSpace(keyFile))
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Keyword form: host=… user=… (space-separated). Strip prior ssl*
	// keys then append.
	parts := strings.Fields(dsn)
	out := make([]string, 0, len(parts)+4)
	for _, p := range parts {
		low := strings.ToLower(p)
		if strings.HasPrefix(low, "sslmode=") ||
			strings.HasPrefix(low, "sslrootcert=") ||
			strings.HasPrefix(low, "sslcert=") ||
			strings.HasPrefix(low, "sslkey=") {
			continue
		}
		out = append(out, p)
	}
	out = append(out, "sslmode=verify-full", "sslrootcert="+caFile)
	if c := strings.TrimSpace(certFile); c != "" {
		if k := strings.TrimSpace(keyFile); k == "" {
			return "", fmt.Errorf("mtls: pg client cert requires key")
		}
		out = append(out, "sslcert="+c, "sslkey="+strings.TrimSpace(keyFile))
	}
	return strings.Join(out, " "), nil
}

// RequireHTTPS returns an error if rawURL is empty or uses a non-TLS scheme.
// Used when CIOS_DATA_PLANE_TLS=require for VM / control base URLs.
func RequireHTTPS(rawURL, name string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("mtls: %s url: %w", name, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("mtls: data-plane require: %s must be https (got %q)", name, u.Scheme)
	}
	return nil
}

// HasSecurePG reports whether dsn already requests TLS (sslmode not
// disable/allow/prefer empty). Used for require-mode boot checks when
// operator put sslmode in DSN without CA helper.
func HasSecurePG(dsn string) bool {
	low := strings.ToLower(dsn)
	if strings.Contains(low, "sslmode=verify-full") ||
		strings.Contains(low, "sslmode=verify-ca") ||
		strings.Contains(low, "sslmode=require") {
		return true
	}
	return false
}
