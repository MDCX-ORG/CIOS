// Package cli — maintenance.go: `cios maintenance upcoming` against
// /v1/maintenance/upcoming (M2 E2.4 + E2.7 / PRMT-058). Mirrors the
// query-arg pattern of `cli/report.go`'s reportOpsCmd (RFC3339
// before, optional --overdue bool) so the CLI shape matches the
// server's filter contract.
//
// `cios maintenance window list|create|delete` against
// /v1/maintenance/windows (M2 E2.4 / PRMT-096) lives in
// maintenance_window.go so this file's scope (upcoming only) is
// unchanged from M2 batch 1.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"
)

// maintenanceRow is the table row shape for the upcoming endpoint.
// JSON tags mirror maintenanceUpcomingItem so json/yaml pass-through
// works without an extra decode step on the server side.
type maintenanceRow struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	AssetPath string    `json:"asset_path"`
	Title     string    `json:"title"`
	NextDue   time.Time `json:"next_due"`
	Overdue   bool      `json:"overdue"`
}

type maintenanceUpcomingResponse struct {
	Items []maintenanceRow `json:"items"`
}

func maintenanceCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios maintenance <upcoming|window> ...")
		return 2
	}
	switch args[0] {
	case "upcoming":
		return maintenanceUpcomingCmd(g, args[1:], stdout, stderr)
	case "window":
		return maintenanceWindowCmd(g, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown maintenance subcommand %q\n", args[0])
		return 2
	}
}

// maintenanceUpcomingCmd fetches /v1/maintenance/upcoming with the
// optional --before (RFC3339) and --overdue flags. Server-side
// filter + sort + per-item scope; CLI is a thin pass-through.
func maintenanceUpcomingCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance upcoming", flag.ContinueOnError)
	fs.SetOutput(stderr)
	before := fs.String("before", "", "RFC3339 cutoff; include only items with next_due <= before")
	overdue := fs.Bool("overdue", false, "include only overdue items (next_due < now)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios maintenance upcoming [--before RFC3339] [--overdue]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	if *before != "" {
		if _, err := time.Parse(time.RFC3339, *before); err != nil {
			fmt.Fprintf(stderr, "error: --before must be RFC3339: %s\n", err.Error())
			return 2
		}
		q.Set("before", *before)
	}
	if *overdue {
		q.Set("overdue", "true")
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/maintenance/upcoming", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got maintenanceUpcomingResponse
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	if len(got.Items) == 0 {
		fmt.Fprintln(stderr, "no upcoming maintenance")
		return 0
	}
	printMaintenanceTable(stdout, &got)
	return 0
}

func printMaintenanceTable(w io.Writer, r *maintenanceUpcomingResponse) {
	fmt.Fprintln(w, "UPCOMING MAINTENANCE")
	fmt.Fprintf(w, "  %-21s  %-7s  %-11s  %s\n", "NEXT DUE", "OVERDUE", "KIND", "TITLE")
	for _, it := range r.Items {
		overdue := "no"
		if it.Overdue {
			overdue = "yes"
		}
		fmt.Fprintf(w, "  %-21s  %-7s  %-11s  %s\n",
			formatTime(it.NextDue),
			overdue,
			it.Kind,
			it.Title,
		)
	}
}
