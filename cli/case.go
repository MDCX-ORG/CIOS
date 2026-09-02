// Package cli — case.go: `cios case list` (PRMT-044 / M2 E2.8
// P572, query-params + CSV export per PRMT-053). Mirrors
// cli/report.go's table layout: a labelled list of closed tickets
// — the KB seed for M4 AI training.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"
)

// caseRow is the typed mirror of listCasesResponse so json/yaml
// modes pass the server bytes through verbatim.
type caseRow struct {
	ID         string `json:"id"`
	AssetPath  string `json:"asset_path"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	State      string `json:"state"`
	OpenedAt   string `json:"opened_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	ClosedAt   string `json:"closed_at,omitempty"`
	Runbook    string `json:"runbook,omitempty"`
}

type casesListResponse struct {
	Items []caseRow `json:"items"`
}

func caseCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios case <list>")
		return 2
	}
	switch args[0] {
	case "list":
		return caseListCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown case subcommand %q\n", args[0])
		return 2
	}
}

func caseListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("case list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filter := fs.String("filter", "", "cpath glob (passed through to the server)")
	severity := fs.String("severity", "", "critical|major|minor|info")
	assetPrefix := fs.String("asset-prefix", "", "asset_path prefix match")
	since := fs.String("since", "", "RFC3339 lower bound on closed_at")
	until := fs.String("until", "", "RFC3339 upper bound on closed_at")
	limit := fs.Int("limit", 0, "max items to return (0 = server default)")
	csvOut := fs.Bool("csv", false, "emit CSV (text/csv) to stdout")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios case list [--filter GLOB] [--severity S] [--asset-prefix P] [--since T] [--until T] [--limit N] [--csv]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	if *filter != "" {
		q.Set("filter", *filter)
	}
	if *severity != "" {
		q.Set("severity", *severity)
	}
	if *assetPrefix != "" {
		q.Set("asset_prefix", *assetPrefix)
	}
	if *since != "" {
		if _, err := time.Parse(time.RFC3339, *since); err != nil {
			fmt.Fprintf(stderr, "error: --since: %s\n", err.Error())
			return 2
		}
		q.Set("since", *since)
	}
	if *until != "" {
		if _, err := time.Parse(time.RFC3339, *until); err != nil {
			fmt.Fprintf(stderr, "error: --until: %s\n", err.Error())
			return 2
		}
		q.Set("until", *until)
	}
	if *limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", *limit))
	}
	if *csvOut {
		q.Set("format", "csv")
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/cases", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	if *csvOut {
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	var got casesListResponse
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
	printCasesTable(stdout, &got)
	return 0
}

func printCasesTable(w io.Writer, r *casesListResponse) {
	if len(r.Items) == 0 {
		fmt.Fprintln(w, "CASES (closed tickets)")
		fmt.Fprintln(w, "  (none)")
		return
	}
	fmt.Fprintln(w, "CASES (closed tickets)")
	fmt.Fprintf(w, "  %-19s  %-7s  %-8s  %s\n", "CLOSED", "SEV", "ID", "TITLE")
	for _, c := range r.Items {
		closed := c.ClosedAt
		if closed == "" {
			closed = c.ResolvedAt
		}
		fmt.Fprintf(w, "  %-19s  %-7s  %-8s  %s\n",
			closed, c.Severity, shortID(c.ID), c.Title)
	}
}

// shortID trims a ticket id to its first 8 chars after the
// underscore prefix for compact display. The full id stays in
// the JSON output.
func shortID(id string) string {
	for i, r := range id {
		if r == '_' {
			if i+9 <= len(id) {
				return id[:i+9]
			}
			return id
		}
	}
	return id
}
