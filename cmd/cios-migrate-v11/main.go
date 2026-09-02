// Command cios-migrate-v11 — PRMT-186 v1.1 one-shot migration
// + dual-grammar report tool (L101 D4, spec-001 v1.1 §5bis.2,
// spec-004 v1.1 §6bis).
//
// Two subcommands (no daemon):
//
//	migrate
//	  cios-migrate-v11 migrate [-store PATH] [-pg-dsn DSN]
//	                            [-audit-sink PATH]
//	                            [-principal ID]
//	  Idempotently:
//	    (1) ensure a `default` org per tenant (185, ErrOrgNameConflict
//	        swallowed);
//	    (2) backfill sites to their tenant's `default` org (189);
//	    (3) rewrite legacy-origin RoleBinding rows to
//	        crn-form-under-`org/default` (190 + 190-bis);
//	    (4) append one pre/post diff per rewrite to the
//	        MigrationAuditSink (NOT tenant_audit — PRMT-186 §0.8).
//	  Prints a JSON MigrateReport and exits 0 on success, non-zero
//	  on error.
//
//	report
//	  cios-migrate-v11 report [-days N]
//	  Reads the in-process legacyScopeUses counter (190's
//	  legacyScopeUses, exposed via core.LegacyScopeUses()) and
//	  prints whether the §6bis "N consecutive days zero legacy use"
//	  closure criterion is met. NEVER flips the closure flag
//	  (R6 — human-only via 190-bis config).
//
// All flags match the cios-core precedent (-store / -pg-dsn /
// -migrations / -principal) plus -audit-sink for the migration
// audit log path. Exit codes follow the cmd/scene-prune-gen
// pattern: 2 = usage / flag parse, 1 = runtime error, 0 = ok.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/yurimeng/cios/core"
)

const (
	exitOK          = 0
	exitRuntime     = 1
	exitUsage       = 2
	defaultSinkPath = "./v11-migration-audit.jsonl"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the package-level entry; tests exercise it directly.
func run(args []string, stdout, stderr *os.File) int {
	if len(args) < 1 {
		fmt.Fprintf(stderr, "usage:\n  %s migrate [-store PATH] [-pg-dsn DSN] [-audit-sink PATH] [-principal ID]\n  %s report [-days N]\n", ownName(args), ownName(args))
		return exitUsage
	}
	switch args[0] {
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	case "report":
		return runReport(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintf(stdout, "usage:\n  %s migrate [-store PATH] [-pg-dsn DSN] [-audit-sink PATH] [-principal ID]\n  %s report [-days N]\n", ownName(args), ownName(args))
		return exitOK
	default:
		fmt.Fprintf(stderr, "%s: unknown subcommand %q (want migrate|report)\n", ownName(args), args[0])
		return exitUsage
	}
}

// ownName recovers the binary name from the command line. We
// default to "cios-migrate-v11" if no os.Args is available.
func ownName(_ []string) string {
	if len(os.Args) > 0 {
		return filepathBase(os.Args[0])
	}
	return "cios-migrate-v11"
}

// filepathBase is filepath.Base isolated so we don't pull in
// path/filepath just for one call.
func filepathBase(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}

// runMigrate is the `migrate` subcommand implementation.
func runMigrate(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("cios-migrate-v11 migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storePath := fs.String("store", "cios-core.json", "JSON file backing the store (used when no -pg-dsn / CIOS_PG_DSN)")
	pgDSN := fs.String("pg-dsn", "", "PostgreSQL DSN; empty → env CIOS_PG_DSN; still empty → fileStore")
	migrations := fs.String("migrations", "migrations", "migrations directory (PG mode)")
	auditSinkPath := fs.String("audit-sink", defaultSinkPath, "migration-audit JSONL output path (append-only, not tenant_audit)")
	principal := fs.String("principal", "system:migrate-v11", "principal recorded on the audit rows and default-org CreateOrg")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	dsn := strings.TrimSpace(*pgDSN)
	if dsn == "" {
		dsn = strings.TrimSpace(envLookup("CIOS_PG_DSN"))
	}

	st, err := openStoreCLI(dsn, *storePath, *migrations)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: open store: %v\n", err)
		return exitRuntime
	}

	sinkFile, err := core.OpenMigrationAuditFile(*auditSinkPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: open audit sink: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = sinkFile.Close() }()
	sink := core.NewJSONLMigrationAuditSink(sinkFile)

	rep, err := core.MigrateV11(context.Background(), st, *principal, sink)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return exitRuntime
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintf(stderr, "migrate: encode report: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// runReport is the `report` subcommand implementation.
func runReport(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("cios-migrate-v11 report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	days := fs.Int("days", 30, "window size for the §6bis 'N consecutive days zero legacy use' criterion (advisory; counter is in-process)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	r, err := core.ReportLegacyUse(context.Background(), *days)
	if err != nil {
		fmt.Fprintf(stderr, "report: %v\n", err)
		return exitRuntime
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintf(stderr, "report: encode: %v\n", err)
		return exitRuntime
	}
	// Print the human-readable closure-ready line on stderr so it
	// is greppable independent of the JSON.
	var status string
	if r.ClosureReady {
		status = "CLOSURE-READY: yes"
	} else {
		status = "CLOSURE-READY: no"
	}
	fmt.Fprintf(stderr, "%s (uses=%d, flag_open=%t, historical_evidence=%t)\n",
		status, r.LegacyScopeUsesNow, r.ClosureFlagOpen, r.HistoricalEvidence)
	fmt.Fprintf(stderr, "note: %s\n", r.Note)
	return exitOK
}

// openStoreCLI mirrors cmd/cios-core's openStore (datasource
// precedence: -pg-dsn > CIOS_PG_DSN > fileStore). We duplicate
// rather than import so the migrate-v11 binary stays under the
// PRMT-186 §3 whitelist (no cmd/cios-core import).
func openStoreCLI(dsn, storePath, migrations string) (core.Store, error) {
	if dsn != "" {
		return core.NewPGStore(context.Background(), dsn, migrations)
	}
	return core.NewFileStore(storePath)
}

// envLookup is os.Getenv. We isolate it so a future test can
// inject a fixture without polluting the process env.
func envLookup(k string) string { return os.Getenv(k) }
