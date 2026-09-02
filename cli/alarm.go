// Package cli — alarm.go: `cios alarm list` (GET /v1/alarms).
// Auto-paginates with the same hard cap as asset.go.
package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// alarmRow is one row of the alarm listing.
type alarmRow struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Severity string `json:"severity"`
	State    string `json:"state"`
	Summary  string `json:"summary"`
	Since    string `json:"since"`
}

func alarmCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios alarm <list>")
		return 2
	}
	switch args[0] {
	case "list":
		return alarmListCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown alarm subcommand %q\n", args[0])
		return 2
	}
}

func alarmListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("alarm list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	severity := fs.String("severity", "", "severity filter (critical|major|minor|info)")
	state := fs.String("state", "", "state filter (firing|acked|resolved)")
	filter := fs.String("filter", "", "cpath glob")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios alarm list [--severity S] [--state S] [--filter G]") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(defaultPageSize))
	if *severity != "" {
		q.Set("severity", *severity)
	}
	if *state != "" {
		q.Set("state", *state)
	}
	if *filter != "" {
		q.Set("filter", *filter)
	}
	c := NewClient(g.server)
	items, status, err := listAll[alarmRow](c, "/v1/alarms", q)
	if err != nil {
		// Pagination overflow OR mid-loop error — surface both via the
		// §5 MUST stderr format.
		if err.Error() == "pagination overflow (>100 pages)" {
			fmt.Fprintf(stderr, "error: %s\n", err.Error())
			return 1
		}
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		fmt.Fprintf(stderr, "error: http %d\n", status)
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		if err := Print(stdout, g.output, items, TableSpec{}); err != nil {
			fmt.Fprintln(stderr, "error: "+err.Error())
			return 2
		}
		return 0
	}
	if len(items) == 0 {
		fmt.Fprintln(stderr, "no alarms")
		return 0
	}
	rows := make([]any, 0, len(items))
	for _, a := range items {
		rows = append(rows, a)
	}
	tbl := TableSpec{
		Columns: []string{"SEVERITY", "STATE", "PATH", "SINCE", "SUMMARY"},
		Row: func(v any) []string {
			a := v.(alarmRow)
			return []string{
				a.Severity,
				a.State,
				a.Path,
				formatRFC3339String(a.Since),
				a.Summary,
			}
		},
	}
	if err := Print(stdout, g.output, rows, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}
