// Command cios-snmpsim runs a single-device SNMP v2c UDP simulator
// pre-loaded with an OID→int64 table from a -seed file. It is a
// stdlib-only wrapper around pkg/driver/snmpsim that adds three
// behaviours required by the M1 conformance toolchain:
//
//  1. An OID seed file parser: each non-comment, non-blank line is
//     "OID INT64VALUE", injected via SetOID before Start.
//  2. A loopback-only listen address guard (mirroring cios-modbussim).
//  3. A SIGINT/SIGTERM-driven graceful Stop via signal.NotifyContext.
//
// Wiring outside this command (the gateway + core + driver
// conformance suite) is intentionally out of scope; this binary is a
// process-level testbed, not a plant device.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/yurimeng/cios/pkg/driver/snmpsim"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:1161", "SNMP v2c UDP listen address (loopback only)")
	community := flag.String("community", "public", "Expected v2c community string")
	seed := flag.String("seed", "", "Optional seed file: each line is 'OID INT64VALUE'; '#' starts a comment")
	flag.Parse()

	if err := validateAddr(*addr); err != nil {
		log.Fatalf("cios-snmpsim: %v", err)
	}

	sim := snmpsim.New(snmpsim.Config{
		Addr:      *addr,
		Community: *community,
	})

	if *seed != "" {
		if err := loadSeed(sim, *seed); err != nil {
			log.Fatalf("cios-snmpsim: -seed: %v", err)
		}
	}

	listenAddr, err := sim.Start()
	if err != nil {
		log.Fatalf("cios-snmpsim: start: %v", err)
	}
	fmt.Fprintf(os.Stdout, "cios-snmpsim: listening on %s (community=%q)\n", listenAddr, *community)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	sim.Stop()
	fmt.Fprintln(os.Stdout, "cios-snmpsim: stopped")
}

// validateAddr enforces the loopback-only constraint. A production
// service would not be on 127.0.0.1; this binary is a testbed, so
// the guard mirrors cios-modbussim's behaviour.
func validateAddr(addr string) error {
	host, _, err := splitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse -addr: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("refuse non-loopback host %q (testbed only)", host)
	}
	return nil
}

// loadSeed reads -seed line-by-line. Each line is "OID INT64VALUE";
// leading/trailing whitespace is stripped; '#' starts a comment; blank
// lines are skipped. A malformed value aborts the whole load so the
// caller never sees a half-populated OID table.
func loadSeed(sim *snmpsim.Sim, path string) error {
	f, err := os.Open(path) // #nosec G304 — CLI tool, path supplied by operator
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		parts := strings.Fields(raw)
		if len(parts) != 2 {
			return fmt.Errorf("line %d: want 'OID VALUE', got %q", lineNum, raw)
		}
		oid := parts[0]
		val, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: parse value %q: %w", lineNum, parts[1], err)
		}
		sim.SetOID(oid, val)
	}
	return scanner.Err()
}

// splitHostPort is a tiny local copy: log.Fatalf only needs a single
// string and we don't want net.SplitHostPort's error semantics
// leaking into the message the operator reads.
func splitHostPort(s string) (string, string, error) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return s, "", nil
	}
	return s[:idx], s[idx+1:], nil
}
