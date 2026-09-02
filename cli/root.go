// Package cli — root.go: argument parsing, subcommand dispatch, and
// the top-level exit-code contract.
//
// Dispatch model (§4.1 / §5 MUST):
//  1. Parse global flags via a dedicated FlagSet ("-s/--server", "-o").
//  2. First non-flag arg picks the subcommand via a name → handler map.
//  3. Each handler owns its own FlagSet (no shared state, no globals).
//
// Main(args, stdin, stdout, stderr) is the only testable entry point.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// globalFlags carries the parsed global flags. They are passed
// explicitly into handlers (no globals).
type globalFlags struct {
	server string
	output string
}

// handlerFunc is the per-subcommand entry. It receives the post-flag
// positional args and may write to stdout/stderr. Its return is the
// process exit code.
type handlerFunc func(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int

// ExitCodeFor maps internal errors to the §4.2 exit codes. The CLI
// calls this from handlers so the contract lives in one place.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	// All command-implementation errors are mapped by the handlers:
	//   *NetError / *Problem → 1
	//   local flag/YAML/file → 2
	return 1
}

// Main is the entry point used by cmd/cios/main.go and tests. It
// returns an exit code; it never calls os.Exit itself.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Strip the program name: callers (cmd/cios) pass args[1:] but
	// tests pass the full os.Args. Detect and normalise.
	if len(args) > 0 && strings.HasSuffix(args[0], "cios") && !looksLikeFlag(args[0]) {
		// Be lenient: only strip when the first arg is a bare
		// non-flag, non-subcommand word. Otherwise leave it; the
		// caller chose to include it.
		if !isKnownSubcommand(args[0]) {
			args = args[1:]
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage())
		return 2
	}
	g, rest, code := parseGlobals(args, stderr)
	if code != 0 {
		return code
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, usage())
		return 2
	}
	name := rest[0]
	rest = rest[1:]
	h, ok := subcommands[name]
	if !ok {
		fmt.Fprintf(stderr, "error: unknown command %q\n%s\n", name, usage())
		return 2
	}
	return h(g, rest, stdin, stdout, stderr)
}

// parseGlobals parses global flags from args. We coalesce any
// "-s VALUE" / "-server VALUE" / "-o VALUE" pairs into the embedded
// "-x=VALUE" form first (so flag.Parse can't be confused by a value
// that happens to look like a subcommand name), then find the first
// non-flag arg as the subcommand.
func parseGlobals(args []string, stderr io.Writer) (globalFlags, []string, int) {
	// Step 1: coalesce global string-flag pairs.
	coalesced := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.Contains(a, "=") && (a == "-s" || a == "-server" || a == "-o") && i+1 < len(args) {
			coalesced = append(coalesced, a+"="+args[i+1])
			i++
			continue
		}
		coalesced = append(coalesced, a)
	}
	// Step 2: find first non-flag arg (the subcommand).
	cut := len(coalesced)
	for i, a := range coalesced {
		if strings.HasPrefix(a, "-") {
			continue
		}
		cut = i
		break
	}
	pre := coalesced[:cut]
	rest := coalesced[cut:]
	fs := flag.NewFlagSet("cios", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := globalFlags{}
	fs.StringVar(&g.server, "s", envOrServer("", os.Getenv("CIOS_SERVER")), "API base URL")
	fs.StringVar(&g.server, "server", envOrServer("", os.Getenv("CIOS_SERVER")), "API base URL")
	fs.StringVar(&g.output, "o", "table", "output mode: json|yaml|table")
	if err := fs.Parse(pre); err != nil {
		return g, nil, 2
	}
	switch g.output {
	case "json", "yaml", "table":
		// valid
	default:
		fmt.Fprintf(stderr, "error: invalid -o value %q (must be json, yaml, or table)\n%s\n", g.output, usage())
		return g, nil, 2
	}
	if g.server == "" {
		g.server = "http://127.0.0.1:8080"
	}
	return g, rest, 0
}

func envOrServer(def, env string) string {
	if env != "" {
		return env
	}
	return def
}

func looksLikeFlag(s string) bool { return strings.HasPrefix(s, "-") }

func isKnownSubcommand(s string) bool {
	_, ok := subcommands[s]
	return ok
}

// subcommands is the dispatch table (§5 MUST). Each handler carries
// its own FlagSet.
var subcommands = map[string]handlerFunc{
	"apply":       applyCmd,
	"delete":      deleteCmd,
	"asset":       assetCmd,
	"query":       queryCmd,
	"metric":      metricCmd,
	"alarm":       alarmCmd,
	"ticket":      ticketCmd,
	"report":      reportCmd,
	"case":        caseCmd,
	"spare":       spareCmd,
	"pm":          pmCmd,
	"inspection":  inspectionCmd,
	"maintenance": maintenanceCmd,
	"version":     versionCmd,
	"doctor":      doctorCmd,
}

func usage() string {
	return strings.TrimSpace(`usage: cios [-s URL] [-o json|yaml|table] <command> [...]

commands:
  apply -f <file.yaml>          PUT /v1/assets/{path}
  delete <path> [--cascade]     DELETE /v1/assets/{path}
  asset list [--type T] [--filter G] [--page-size N]
  asset get <path>              GET /v1/assets/{path}
  query <point-path>            GET /v1/points/{path}
  metric query <promql> [--time RFC3339]
  alarm list [--severity S] [--state S] [--filter G]
  ticket list|get|open|ack|resolve|close
  report ops [--since RFC3339] [--top N] [--filter G]
  case list [--filter G]
  spare list|get|adjust
  pm list|get|create
  inspection list|get|create
  maintenance upcoming [--before RFC3339] [--overdue]
  version                       print "cios dev"
  doctor [--json]               pre-prod readiness probe (read-only)`)
}

func versionCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios version") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintln(stdout, "cios dev")
	return 0
}
