// Package cli — inspection.go: `cios inspection list|get|create`
// against /v1/inspections (M2 E2.7 P561 / PRMT-049). Mirrors
// `cli/case.go` for the list/get surface (the inspection list
// endpoint has no pagination, so we use a direct GET + envelope
// decode rather than listAll). The create subcommand mirrors
// `cli/ticket.go`'s ticketOpenCmd body assembly (map[string]any
// with only the fields the API actually accepts).
//
// The `--interval` flag accepts a Go duration string ("24h",
// "168h" for 7 days). It is JSON-encoded as nanoseconds (the
// wire format of time.Duration in encoding/json), which matches
// `createInspectionRequest.Interval` on the server side.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// inspectionRow is the table row shape for the list endpoint.
// JSON tags mirror InspectionTemplate so json/yaml pass-through
// works. Interval is time.Duration so json/yaml emit it as a
// quoted nanosecond integer (server's wire format).
type inspectionRow struct {
	ID        string        `json:"id"`
	AssetPath string        `json:"asset_path"`
	Title     string        `json:"title"`
	Items     []string      `json:"items"`
	Interval  time.Duration `json:"interval"`
	NextDue   string        `json:"next_due"`
	Enabled   bool          `json:"enabled"`
}

type inspectionListResponse struct {
	Items []inspectionRow `json:"items"`
}

func inspectionCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios inspection <list|get|create> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return inspectionListCmd(g, args[1:], stdout, stderr)
	case "get":
		return inspectionGetCmd(g, args[1:], stdout, stderr)
	case "create":
		return inspectionCreateCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown inspection subcommand %q\n", args[0])
		return 2
	}
}

func inspectionListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspection list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filter := fs.String("filter", "", "cpath prefix filter (passed through to the server)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios inspection list [--filter PREFIX]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	if *filter != "" {
		q.Set("filter", *filter)
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/inspections", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got inspectionListResponse
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
		fmt.Fprintln(stderr, "no inspections")
		return 0
	}
	printInspectionTable(stdout, &got)
	return 0
}

func printInspectionTable(w io.Writer, r *inspectionListResponse) {
	fmt.Fprintln(w, "INSPECTIONS")
	fmt.Fprintf(w, "  %-21s  %-8s  %-7s  %-7s  %s\n", "NEXT DUE", "INTERVAL", "ITEMS", "ENABLED", "TITLE")
	for _, it := range r.Items {
		enabled := "yes"
		if !it.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(w, "  %-21s  %-8s  %-7s  %-7s  %s\n",
			formatRFC3339String(it.NextDue),
			formatInspectionInterval(it.Interval),
			strconv.Itoa(len(it.Items)),
			enabled,
			it.Title,
		)
	}
}

// formatInspectionInterval renders a Duration as a compact human
// shape (e.g. "24h", "168h", "30m") for the table view. Anything
// that doesn't fit a single unit falls back to the raw nanosecond
// count to stay unambiguous.
func formatInspectionInterval(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

func inspectionGetCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspection get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios inspection get <id>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios inspection get <id>")
		return 2
	}
	id := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/inspections/"+id, nil, nil)
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
	var got inspectionRow
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	printInspectionDetail(stdout, &got)
	return 0
}

func printInspectionDetail(w io.Writer, r *inspectionRow) {
	fmt.Fprintf(w, "ID           %s\n", r.ID)
	fmt.Fprintf(w, "ASSET        %s\n", r.AssetPath)
	fmt.Fprintf(w, "TITLE        %s\n", r.Title)
	fmt.Fprintf(w, "INTERVAL     %s\n", formatInspectionInterval(r.Interval))
	fmt.Fprintf(w, "NEXT DUE     %s\n", formatRFC3339String(r.NextDue))
	if len(r.Items) > 0 {
		fmt.Fprintln(w, "ITEMS")
		for _, item := range r.Items {
			fmt.Fprintf(w, "  - %s\n", item)
		}
	}
	if r.Enabled {
		fmt.Fprintln(w, "ENABLED      yes")
	} else {
		fmt.Fprintln(w, "ENABLED      no")
	}
}

func inspectionCreateCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspection create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asset := fs.String("asset", "", "asset path (required)")
	title := fs.String("title", "", "title (required)")
	interval := fs.String("interval", "", "interval duration (required; Go duration string like 24h, 168h)")
	items := fs.String("items", "", "comma-separated checklist items (optional)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios inspection create --asset <crn> --title <t> --interval <dur> [--items a,b,c]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *asset == "" || *title == "" || *interval == "" {
		fmt.Fprintln(stderr, "usage: cios inspection create --asset <crn> --title <t> --interval <dur> [--items a,b,c]")
		return 2
	}
	d, err := time.ParseDuration(*interval)
	if err != nil {
		fmt.Fprintf(stderr, "error: --interval: %s\n", err.Error())
		return 2
	}
	if d <= 0 {
		fmt.Fprintln(stderr, "error: --interval must be > 0")
		return 2
	}
	body := map[string]any{
		"asset_path": *asset,
		"title":      *title,
		"interval":   d,
	}
	if *items != "" {
		body["items"] = splitCSVItems(*items)
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/inspections", nil, body)
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
	var got inspectionRow
	if err := json.Unmarshal(respBody, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "created %s (next_due=%s, interval=%s)\n",
		got.ID, formatRFC3339String(got.NextDue), formatInspectionInterval(got.Interval))
	return 0
}

// splitCSVItems splits a comma-separated items list, trimming
// whitespace and dropping empty fragments. Mirrors how the
// server treats empty strings (the createInspectionRequest
// defaults Items to []string{} when nil, so an empty entry
// would be a no-op anyway, but trimming keeps the wire clean).
func splitCSVItems(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
