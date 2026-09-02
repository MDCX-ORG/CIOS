// Tests for cmd/cios-core/main.go. These cover the wiring layer
// (RBAC loader, store selection branch) without binding to the
// network or to a running PostgreSQL — the PG branch is asserted
// by call shape only (no real connect), per PRMT §5.
//
// PRMT-M1-Checkpoint-Fix-R1 §5.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yurimeng/cios/core"
)

// validRBACYAML is a 3-token, 3-role table. SHA256 digests are 64
// hex chars; the underlying values are not used by the loader
// (only their format) because the verifier stores the digest and
// never recomputes from plaintext. The package-level tests in
// core/auth_test.go own the real token↔digest round-trip.
const validRBACYAML = `tokens:
  - sha256: "0000000000000000000000000000000000000000000000000000000000000001"
    subject: "svc:grafana"
    role: "viewer"
    scopes:
      - "site01.**"
  - sha256: "0000000000000000000000000000000000000000000000000000000000000002"
    subject: "svc:cli"
    role: "operator"
    scopes:
      - "site01.pod002.**"
  - sha256: "0000000000000000000000000000000000000000000000000000000000000003"
    subject: "svc:admin"
    role: "admin"
    scopes: []
`

func writeTempRBAC(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rbac.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write rbac: %v", err)
	}
	return p
}

func TestLoadRBAC_OK_ThreeRoles(t *testing.T) {
	p := writeTempRBAC(t, validRBACYAML)
	st, err := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	auth, err := loadRBAC(p, st)
	if err != nil {
		t.Fatalf("loadRBAC: unexpected error: %v", err)
	}
	if auth == nil || auth.Verifier == nil {
		t.Fatalf("loadRBAC: nil auth or verifier")
	}
	// Round-trip: the digests we wrote should be accepted by the
	// verifier. The verifier is internal; we exercise it through
	// the public Verify surface.
	for _, hex := range []string{
		"0000000000000000000000000000000000000000000000000000000000000001",
		"0000000000000000000000000000000000000000000000000000000000000002",
		"0000000000000000000000000000000000000000000000000000000000000003",
	} {
		// Verify takes the *raw* token, not the digest; the
		// staticTokenVerifier hashes internally. We don't know
		// the raw value, so we can only check that some other
		// random string is rejected and our digest-shaped input
		// is treated as "unknown token" (not a panic / not a
		// successful lookup of the wrong entry).
		if _, err := auth.Verifier.Verify(hex); err == nil {
			t.Fatalf("Verify(%q): expected ErrUnauthorized, got nil", hex)
		} else if !errorsIsUnauthorized(err) {
			t.Fatalf("Verify(%q): error = %v, want ErrUnauthorized", hex, err)
		}
	}
}

func TestLoadRBAC_RejectsBadScope(t *testing.T) {
	// Pattern with a glob char ('[') that cpath rejects at compile
	// time. NewStaticTokenVerifier is the gate; the loader must
	// surface its error rather than swallow it.
	yaml := `tokens:
  - sha256: "0000000000000000000000000000000000000000000000000000000000000001"
    subject: "svc:x"
    role: "viewer"
    scopes:
      - "site01.[bad"
`
	p := writeTempRBAC(t, yaml)
	st, sErr := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if sErr != nil {
		t.Fatalf("openStore: %v", sErr)
	}
	_, err := loadRBAC(p, st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for bad scope, got nil")
	}
	if !strings.Contains(err.Error(), "scope") && !strings.Contains(err.Error(), "glob") {
		t.Logf("loadRBAC error (informational): %v", err)
	}
}

func TestLoadRBAC_RejectsBadRole(t *testing.T) {
	yaml := `tokens:
  - sha256: "0000000000000000000000000000000000000000000000000000000000000001"
    subject: "svc:x"
    role: "tenant"
    scopes:
      - "site01.**"
`
	p := writeTempRBAC(t, yaml)
	st, sErr := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if sErr != nil {
		t.Fatalf("openStore: %v", sErr)
	}
	_, err := loadRBAC(p, st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for unknown role, got nil")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Fatalf("loadRBAC: error %v should mention 'role'", err)
	}
}

func TestLoadRBAC_RejectsEmptySHA(t *testing.T) {
	yaml := `tokens:
  - sha256: ""
    subject: "svc:x"
    role: "viewer"
    scopes: ["**"]
`
	p := writeTempRBAC(t, yaml)
	st, sErr := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if sErr != nil {
		t.Fatalf("openStore: %v", sErr)
	}
	_, err := loadRBAC(p, st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for empty sha256, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("loadRBAC: error %v should mention 'sha256'", err)
	}
}

func TestLoadRBAC_RejectsEmptyFile(t *testing.T) {
	// No tokens at all → loader must refuse, never build an
	// empty verifier (which would accept *nothing* but also be
	// useless and surprising).
	p := writeTempRBAC(t, "tokens: []\n")
	st, sErr := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if sErr != nil {
		t.Fatalf("openStore: %v", sErr)
	}
	_, err := loadRBAC(p, st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for empty token list, got nil")
	}
}

func TestLoadRBAC_RejectsMissingFile(t *testing.T) {
	st, err := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	_, err = loadRBAC(filepath.Join(t.TempDir(), "does-not-exist.yaml"), st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for missing file, got nil")
	}
}

func TestLoadRBAC_RejectsGarbageYAML(t *testing.T) {
	p := writeTempRBAC(t, "::: this is not yaml :::\n")
	st, sErr := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if sErr != nil {
		t.Fatalf("openStore: %v", sErr)
	}
	_, err := loadRBAC(p, st)
	if err == nil {
		t.Fatalf("loadRBAC: expected error for garbage YAML, got nil")
	}
}

// TestStoreSelection_NoDSN_PicksFile is the "default = M0" guarantee
// the contract pins. We drive openStore() with empty dsn and
// assert it picks NewFileStore (i.e. returns a non-nil store
// without ever touching NewPGStore).
func TestStoreSelection_NoDSN_PicksFile(t *testing.T) {
	t.Setenv("CIOS_PG_DSN", "")
	st, err := openStore("", filepath.Join(t.TempDir(), "cios.json"), "")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if st == nil {
		t.Fatalf("openStore: nil store")
	}
	// Closing is a no-op on fileStore; safe to call to free the
	// underlying file handle in case the implementation grows
	// closer semantics later.
	type closer interface{ Close() error }
	if c, ok := st.(closer); ok {
		_ = c.Close()
	}
}

// TestStoreSelection_PathWithDSN_ReachesPG asserts the pg branch
// is taken when dsn is non-empty. We do not depend on a live PG:
// the dial will fail (no listener at the chosen port) and that
// error is the proof we reached NewPGStore.
func TestStoreSelection_PathWithDSN_ReachesPG(t *testing.T) {
	_, err := openStore(
		"postgres://user:pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=1",
		"ignored",
		"./migrations",
	)
	if err == nil {
		t.Fatalf("openStore: expected PG dial error, got nil")
	}
	// Differentiate: fileStore never returns a "dial" / "pgx" /
	// "connect" error — only the pg branch does. The wording is
	// unstable across pgx releases, so we match on a stable
	// substring set.
	if !isPGPathError(err) {
		t.Fatalf("openStore: error %v does not look like PG branch", err)
	}
}

// isPGPathError returns true if err looks like it came from
// the pg branch. NewPGStore wraps its errors as
// "core: pg store: <cause>"; NewFileStore never produces the
// "pg store" prefix. We also accept pgx / dial / connect /
// postgres substrings for the post-dial failure modes.
func isPGPathError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "pg store") {
		return true
	}
	low := strings.ToLower(s)
	for _, m := range []string{"pgx", "postgres", "dial", "connect_timeout", "connection refused"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func errorsIsUnauthorized(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unauthorized")
}

// TestRun_FailClosed_NoRBAC_NoAllowNoAuth pins PRMT-029 §2(B):
// running cios-core with neither -rbac nor -allow-no-auth must
// refuse to start. The error must name the missing flag so the
// operator knows what to do. The test stops at the fail-closed
// branch and never reaches the HTTP server (so we don't have to
// clean up a goroutine listening on a port).
func TestRun_FailClosed_NoRBAC_NoAllowNoAuth(t *testing.T) {
	// cpath.LoadDict needs the protocol/ dir; we use the in-repo
	// one. If it isn't there the test is structurally broken —
	// surface that, don't paper over it.
	if _, err := os.Stat("../../protocol"); err != nil {
		t.Fatalf("need protocol/ at repo root: %v", err)
	}
	err := run(runArgs{
		listen:      "127.0.0.1:0",
		storePath:   filepath.Join(t.TempDir(), "cios.json"),
		protocolDir: "../../protocol",
		vmURL:       "http://127.0.0.1:8428",
		rbacPath:    "",    // no RBAC file
		allowNoAuth: false, // no explicit override
	})
	if err == nil {
		t.Fatalf("run: expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to start") &&
		!strings.Contains(err.Error(), "-rbac") &&
		!strings.Contains(err.Error(), "-allow-no-auth") {
		t.Fatalf("run: error %q should mention -rbac or -allow-no-auth", err)
	}
}

// TestRun_AllowNoAuth_ReachesHTTPListen pins PRMT-029 §2(B):
// with -allow-no-auth set, run() must get past the fail-closed
// branch and reach the HTTP listen step. We grab a free port via
// net.Listen("tcp","127.0.0.1:0"), start run() in a goroutine on
// that port, then assert one of the two positive invariants:
// (i) the boot log line "listening on <addr>" appears, or
// (ii) GET /healthz returns 200. We then send SIGINT (which
// signal.NotifyContext inside run() handles) and wait for the
// goroutine to exit cleanly. Failing either invariant — or
// observing the fail-closed error — fails the test.
func TestRun_AllowNoAuth_ReachesHTTPListen(t *testing.T) {
	if _, err := os.Stat("../../protocol"); err != nil {
		t.Fatalf("need protocol/ at repo root: %v", err)
	}
	// PRMT-217: production builds refuse -allow-no-auth outright.
	if !core.LabBypassAvailable() {
		err := run(runArgs{
			listen:      "127.0.0.1:0",
			storePath:   filepath.Join(t.TempDir(), "cios.json"),
			protocolDir: "../../protocol",
			vmURL:       "http://127.0.0.1:8428",
			allowNoAuth: true,
		})
		if err == nil || !strings.Contains(err.Error(), "lab build") {
			t.Fatalf("prod -allow-no-auth: want refuse mentioning lab build, got %v", err)
		}
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	// Hand the listener to run(); close the one we used to pick
	// the port so run() can bind it without EADDRINUSE.
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	// Capture log output via log.SetOutput (the default logger is
	// what main.go's log.Printf writes to; redirecting os.Stderr
	// alone does not affect it). We scan line-by-line as bytes
	// arrive so the boot signal isn't lost when the test ends.
	logR, logW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origLogOut := log.Writer()
	log.SetOutput(logW)
	t.Cleanup(func() {
		log.SetOutput(origLogOut)
		_ = logW.Close()
		_ = logR.Close()
	})

	bootSeen := make(chan struct{}, 1)
	go func() {
		// Stream the pipe and signal as soon as we see the boot
		// line for our addr. bufio.Scanner over the pipe keeps
		// partial reads stitched into lines.
		scanner := bufio.NewScanner(logR)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "listening on "+addr) {
				select {
				case bootSeen <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(runArgs{
			listen:      addr,
			storePath:   filepath.Join(t.TempDir(), "cios.json"),
			protocolDir: "../../protocol",
			vmURL:       "http://127.0.0.1:8428",
			rbacPath:    "",
			allowNoAuth: true,
		})
	}()

	// Poll /healthz while waiting for the boot signal.
	deadline := time.Now().Add(5 * time.Second)
	hitHealth := false
	for time.Now().Before(deadline) {
		select {
		case <-bootSeen:
			hitHealth = true // boot log alone satisfies (i)
		default:
		}
		if hitHealth {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		resp, herr := http.DefaultClient.Do(req)
		cancel()
		if herr == nil {
			if resp.StatusCode == http.StatusOK {
				hitHealth = true
				_ = resp.Body.Close()
			} else {
				_ = resp.Body.Close()
			}
		}
		if hitHealth {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !hitHealth {
		// Send shutdown before failing so the goroutine can exit
		// cleanly instead of leaking past the test's t.Cleanup.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
		<-errCh
		t.Fatalf("run with -allow-no-auth: neither boot log nor /healthz seen within 5s")
	}

	// Trigger graceful shutdown via SIGINT (signal.NotifyContext
	// inside run() picks this up) and wait for the goroutine.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill SIGINT: %v", err)
	}
	select {
	case runErr := <-errCh:
		// Clean exit (nil) or http.ErrServerClosed-derived shutdown
		// are both acceptable; fail-closed error is not.
		if runErr != nil && strings.Contains(runErr.Error(), "refusing to start") {
			t.Fatalf("run with -allow-no-auth: unexpected fail-closed: %v", runErr)
		}
		if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) {
			// Boot-time errors other than fail-closed also fail the test.
			t.Fatalf("run with -allow-no-auth: unexpected run error: %v", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run with -allow-no-auth: did not exit within 10s after SIGINT")
	}
}

// Sanity guard: core.NewStaticTokenVerifier with a valid role
// must succeed. This pins the seam the loader depends on.
func TestCoreSeam_NewStaticTokenVerifier_OK(t *testing.T) {
	m := map[string]core.Principal{
		"abcd": {Subject: "x", Role: core.RoleViewer, Scopes: []string{"**"}},
	}
	if _, err := core.NewStaticTokenVerifier(m); err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
}

// --- PRMT-216: no-auth + public bind fail-closed -----------------------

func TestIsLoopbackHostPort(t *testing.T) {
	cases := []struct {
		addr string
		lo   bool
		err  bool
	}{
		{"127.0.0.1:8090", true, false},
		{"localhost:8090", true, false},
		{"[::1]:8090", true, false},
		{"0.0.0.0:8090", false, false},
		{":8090", false, false},
		{"192.168.1.1:8090", false, false},
		{"not-an-addr", false, true},
	}
	for _, tc := range cases {
		lo, err := isLoopbackHostPort(tc.addr)
		if tc.err {
			if err == nil {
				t.Errorf("%q: want err", tc.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", tc.addr, err)
			continue
		}
		if lo != tc.lo {
			t.Errorf("%q: lo=%v want %v", tc.addr, lo, tc.lo)
		}
	}
}

func TestValidatePprofLoopback_MessagesUnchanged(t *testing.T) {
	// PRMT-211 §4.6 wording must stay byte-stable for operators/tests.
	if err := validatePprofLoopback("127.0.0.1:6060"); err != nil {
		t.Fatalf("loopback: %v", err)
	}
	if err := validatePprofLoopback("0.0.0.0:6060"); err == nil || err.Error() != "pprof-addr must bind loopback" {
		t.Fatalf("0.0.0.0: got %v", err)
	}
	if err := validatePprofLoopback(":6060"); err == nil || err.Error() != "pprof-addr must bind loopback" {
		t.Fatalf(":6060: got %v", err)
	}
}

func TestRun_AllowNoAuth_PublicBind_Refused(t *testing.T) {
	if _, err := os.Stat("../../protocol"); err != nil {
		t.Fatalf("need protocol/: %v", err)
	}
	for _, listen := range []string{"0.0.0.0:18090", ":18091"} {
		err := run(runArgs{
			listen:      listen,
			storePath:   filepath.Join(t.TempDir(), "cios.json"),
			protocolDir: "../../protocol",
			vmURL:       "http://127.0.0.1:8428",
			allowNoAuth: true,
		})
		if err == nil {
			t.Fatalf("listen %q: expected refuse, got nil", listen)
		}
		// Prod: PRMT-217 lab-build gate fires first; lab: PRMT-216 public-bind gate.
		if !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("listen %q: error %q", listen, err)
		}
		if core.LabBypassAvailable() {
			if !strings.Contains(err.Error(), "-allow-public-bind") {
				t.Fatalf("listen %q: lab build should cite -allow-public-bind: %q", listen, err)
			}
		} else if !strings.Contains(err.Error(), "lab build") {
			t.Fatalf("listen %q: prod build should cite lab build: %q", listen, err)
		}
	}
}

func TestRun_AllowNoAuth_PublicBind_AllowedWithFlag(t *testing.T) {
	if _, err := os.Stat("../../protocol"); err != nil {
		t.Fatalf("need protocol/: %v", err)
	}
	if !core.LabBypassAvailable() {
		t.Skip("requires -tags lab (PRMT-217)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Bind 0.0.0.0 on an ephemeral port: grab free port then rebind via run.
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	addr := fmt.Sprintf("0.0.0.0:%d", port)

	logR, logW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := log.Writer()
	log.SetOutput(logW)
	t.Cleanup(func() {
		log.SetOutput(orig)
		_ = logW.Close()
		_ = logR.Close()
	})
	warnSeen := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(logR)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "-allow-public-bind") &&
				strings.Contains(sc.Text(), "platform admin") {
				select {
				case warnSeen <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(runArgs{
			listen:          addr,
			storePath:       filepath.Join(t.TempDir(), "cios.json"),
			protocolDir:     "../../protocol",
			vmURL:           "http://127.0.0.1:8428",
			allowNoAuth:     true,
			allowPublicBind: true,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		select {
		case <-warnSeen:
			ok = true
		default:
		}
		// Also accept listening via health on loopback of same port.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
		resp, herr := http.DefaultClient.Do(req)
		cancel()
		if herr == nil {
			if resp.StatusCode == http.StatusOK {
				ok = true
			}
			_ = resp.Body.Close()
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	runErr := <-errCh
	if !ok {
		t.Fatalf("did not observe WARN or health; runErr=%v", runErr)
	}
	if runErr != nil && strings.Contains(runErr.Error(), "refusing to start") {
		t.Fatalf("unexpected refuse: %v", runErr)
	}
}

func TestRun_RBAC_PublicBind_NoNoAuthGuard(t *testing.T) {
	// With -rbac and without -allow-no-auth the public-bind guard
	// must not fire (auth is present).
	if _, err := os.Stat("../../protocol"); err != nil {
		t.Fatalf("need protocol/: %v", err)
	}
	rbac := writeTempRBAC(t, validRBACYAML)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	addr := fmt.Sprintf("0.0.0.0:%d", port)

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(runArgs{
			listen:      addr,
			storePath:   filepath.Join(t.TempDir(), "cios.json"),
			protocolDir: "../../protocol",
			vmURL:       "http://127.0.0.1:8428",
			rbacPath:    rbac,
			allowNoAuth: false,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
		resp, herr := http.DefaultClient.Do(req)
		cancel()
		if herr == nil {
			_ = resp.Body.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	runErr := <-errCh
	if !up {
		t.Fatalf("rbac public bind did not start; err=%v", runErr)
	}
	if runErr != nil && strings.Contains(runErr.Error(), "refusing to start") &&
		strings.Contains(runErr.Error(), "-allow-public-bind") {
		t.Fatalf("guard should not apply without -allow-no-auth: %v", runErr)
	}
}
