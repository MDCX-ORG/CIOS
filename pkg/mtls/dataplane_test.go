package mtls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPGDSNApplyTLS_URL(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	// OutboundTLS needs valid PEM; PGDSNApplyTLS only stats the file.
	got, err := PGDSNApplyTLS("postgres://u:p@db:5432/cios?sslmode=disable", ca, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sslmode=verify-full") {
		t.Fatalf("sslmode not forced: %s", got)
	}
	if !strings.Contains(got, "sslrootcert=") {
		t.Fatalf("missing rootcert: %s", got)
	}
	if strings.Contains(got, "sslmode=disable") {
		t.Fatalf("disable survived: %s", got)
	}
}

func TestPGDSNApplyTLS_Keyword(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(ca, []byte("x"), 0o600)
	got, err := PGDSNApplyTLS("host=localhost user=cios dbname=cios sslmode=disable", ca, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sslmode=verify-full") || strings.Contains(got, "sslmode=disable") {
		t.Fatalf("got %q", got)
	}
}

func TestPGDSNApplyTLS_EmptyCANoop(t *testing.T) {
	in := "postgres://localhost/cios"
	got, err := PGDSNApplyTLS(in, "", "", "")
	if err != nil || got != in {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestRequireHTTPS(t *testing.T) {
	if err := RequireHTTPS("https://vm:8428", "vm"); err != nil {
		t.Fatal(err)
	}
	if err := RequireHTTPS("http://vm:8428", "vm"); err == nil {
		t.Fatal("expected error for http")
	}
	if err := RequireHTTPS("", "vm"); err != nil {
		t.Fatal(err)
	}
}

func TestHasSecurePG(t *testing.T) {
	if !HasSecurePG("postgres://x?sslmode=verify-full") {
		t.Fatal("verify-full")
	}
	if HasSecurePG("postgres://x?sslmode=disable") {
		t.Fatal("disable should be insecure")
	}
}

func TestOutboundTLS_CAOnly(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := mustSelfSignedCA(t)
	caPath := writePEM(t, dir, "ca.pem", "CERTIFICATE", caCert.Raw)
	_ = caKey
	cfg, err := OutboundTLS(caPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil || len(cfg.Certificates) != 0 {
		t.Fatal("want RootCAs only")
	}
}
