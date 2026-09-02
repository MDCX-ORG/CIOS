// Package cli — pm.go: `cios pm list|get|create` against
// /v1/pm/schedules (M2 E2.4 P531 / PRMT-043). Mirrors `cli/case.go`
// for the list/get surface (the PM list endpoint has no
// pagination, so we use a direct GET + envelope decode rather
// than listAll). The create subcommand mirrors `cli/ticket.go`'s
// ticketOpenCmd body assembly (map[string]any with only the
// fields the API actually accepts).
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// pmRow is the table row shape for the list endpoint. JSON tags
// mirror PMSchedule so json/yaml pass-through works.
type pmRow struct {
	ID           string `json:"id"`
	AssetPath    string `json:"asset_path"`
	Kind         string `json:"kind"`
	IntervalDays int    `json:"interval_days"`
	NextDue      string `json:"next_due"`
	Title        string `json:"title"`
	Severity     string `json:"severity"`
	Enabled      bool   `json:"enabled"`
}

type pmListResponse struct {
	Items []pmRow `json:"items"`
}

func pmCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios pm <list|get|create> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return pmListCmd(g, args[1:], stdout, stderr)
	case "get":
		return pmGetCmd(g, args[1:], stdout, stderr)
	case "create":
		return pmCreateCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown pm subcommand %q\n", args[0])
		return 2
	}
}

func pmListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pm list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filter := fs.String("filter", "", "cpath prefix filter (passed through to the server)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios pm list [--filter PREFIX]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	if *filter != "" {
		q.Set("filter", *filter)
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/pm/schedules", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got pmListResponse
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
		fmt.Fprintln(stderr, "no pm schedules")
		return 0
	}
	printPMTable(stdout, &got)
	return 0
}

func printPMTable(w io.Writer, r *pmListResponse) {
	fmt.Fprintln(w, "PM SCHEDULES")
	fmt.Fprintf(w, "  %-21s  %-7s  %-7s  %-7s  %s\n", "NEXT DUE", "INTERVAL", "SEV", "ENABLED", "TITLE")
	for _, p := range r.Items {
		enabled := "yes"
		if !p.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(w, "  %-21s  %-7s  %-7s  %-7s  %s\n",
			formatRFC3339String(p.NextDue),
			strconv.Itoa(p.IntervalDays)+"d",
			p.Severity,
			enabled,
			p.Title,
		)
	}
}

func pmGetCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pm get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios pm get <id>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios pm get <id>")
		return 2
	}
	id := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/pm/schedules/"+id, nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	var got pmRow
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	printPMDetail(stdout, &got)
	return 0
}

func printPMDetail(w io.Writer, r *pmRow) {
	fmt.Fprintf(w, "ID           %s\n", r.ID)
	fmt.Fprintf(w, "ASSET        %s\n", r.AssetPath)
	fmt.Fprintf(w, "KIND         %s\n", r.Kind)
	fmt.Fprintf(w, "INTERVAL     %d days\n", r.IntervalDays)
	fmt.Fprintf(w, "NEXT DUE     %s\n", formatRFC3339String(r.NextDue))
	fmt.Fprintf(w, "TITLE        %s\n", r.Title)
	fmt.Fprintf(w, "SEVERITY     %s\n", r.Severity)
	if r.Enabled {
		fmt.Fprintln(w, "ENABLED      yes")
	} else {
		fmt.Fprintln(w, "ENABLED      no")
	}
}

func pmCreateCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pm create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asset := fs.String("asset", "", "asset path (required)")
	title := fs.String("title", "", "title (required)")
	severity := fs.String("severity", "", "severity (required; critical|major|minor|info)")
	intervalDays := fs.Int("interval-days", 0, "interval in days (required; > 0)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios pm create --asset <crn> --title <t> --severity <s> --interval-days <n>")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *asset == "" || *title == "" || *severity == "" || *intervalDays <= 0 {
		fmt.Fprintln(stderr, "usage: cios pm create --asset <crn> --title <t> --severity <s> --interval-days <n>")
		return 2
	}
	body := map[string]any{
		"asset_path":    *asset,
		"title":         *title,
		"severity":      *severity,
		"interval_days": *intervalDays,
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/pm/schedules", nil, body)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(respBody)
		if err != nil {
			return 1
		}
		return 0
	}
	var got pmRow
	if err := json.Unmarshal(respBody, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "created %s (next_due=%s, interval=%dd)\n",
		got.ID, formatRFC3339String(got.NextDue), got.IntervalDays)
	return 0
}
