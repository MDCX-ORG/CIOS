// Package cli — report.go: `cios report ops` (GET /v1/reports/ops).
//
// Mirrors cli/alarm.go in structure (flag set, response decode,
// mode-aware output). The report endpoint returns a single
// aggregated document, not a list, so the table mode prints a
// labelled summary instead of a tabular listing.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// reportOpsRow is the typed mirror of core.opsReportResponse so
// json/yaml modes pass the server bytes through verbatim.
type reportOpsRow struct {
	MTTRSeconds         *float64 `json:"mttr_seconds"`
	MeanResponseSeconds *float64 `json:"mean_response_seconds"`
	MTBFSeconds         *float64 `json:"mtbf_seconds"`
	TicketCounts        struct {
		ByState    map[string]int `json:"by_state"`
		BySeverity map[string]int `json:"by_severity"`
	} `json:"ticket_counts"`
	AlarmTop []struct {
		Path  string `json:"path"`
		Count int    `json:"count"`
	} `json:"alarm_top"`
	Window *struct {
		Since string `json:"since"`
	} `json:"window"`
}

func reportCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios report <ops|reconcile|generate>")
		return 2
	}
	switch args[0] {
	case "ops":
		return reportOpsCmd(g, args[1:], stdout, stderr)
	case "reconcile":
		// PRMT-050: CMDB-vs-telemetry reconciliation report.
		return reportReconcileCmd(g, args[1:], stdout, stderr)
	case "generate":
		// PRMT-042: on-demand HTML report generation. Pulls
		// /v1/reports/ops and renders to local --out (or stdout).
		return reportGenerateCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown report subcommand %q\n", args[0])
		return 2
	}
}

func reportOpsCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report ops", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "", "since timestamp (RFC3339)")
	top := fs.Int("top", 10, "alarm Top-N cap (1..100)")
	filter := fs.String("filter", "", "cpath glob")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios report ops [--since RFC3339] [--top N] [--filter G]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *top <= 0 || *top > 100 {
		fmt.Fprintln(stderr, "error: --top must be 1..100")
		return 2
	}
	q := url.Values{}
	if *since != "" {
		q.Set("since", *since)
	}
	q.Set("top", strconv.Itoa(*top))
	if *filter != "" {
		q.Set("filter", *filter)
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/reports/ops", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got reportOpsRow
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		// Pretty-print the raw decoded body so json/yaml modes are
		// useful for piping into jq / other tooling.
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	printReportOpsTable(stdout, &got)
	return 0
}

// printReportOpsTable writes a labelled summary suitable for
// interactive review. The layout is intentionally simple: a
// section per metric, then a short top-N table. No external deps.
func printReportOpsTable(w io.Writer, r *reportOpsRow) {
	fmt.Fprintln(w, "OPS REPORT")
	fmt.Fprintf(w, "  MTTR (mean resolve time) : %s\n", formatSecondsOrDash(r.MTTRSeconds))
	fmt.Fprintf(w, "  Mean response time       : %s\n", formatSecondsOrDash(r.MeanResponseSeconds))
	fmt.Fprintf(w, "  MTBF (per-asset adjacent): %s\n", formatSecondsOrDash(r.MTBFSeconds))
	if r.Window != nil && r.Window.Since != "" {
		if t, err := time.Parse(time.RFC3339, r.Window.Since); err == nil {
			fmt.Fprintf(w, "  Window (since)           : %s\n", t.UTC().Format(time.RFC3339))
		} else {
			fmt.Fprintf(w, "  Window (since)           : %s\n", r.Window.Since)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "TICKETS BY STATE")
	for _, st := range []string{"open", "acknowledged", "resolved", "closed"} {
		fmt.Fprintf(w, "  %-12s %d\n", st, r.TicketCounts.ByState[st])
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "TICKETS BY SEVERITY")
	for _, sev := range []string{"critical", "major", "minor", "info"} {
		fmt.Fprintf(w, "  %-12s %d\n", sev, r.TicketCounts.BySeverity[sev])
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ALARM TOP (firing, snapshot)")
	if len(r.AlarmTop) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	fmt.Fprintf(w, "  %-8s  %s\n", "COUNT", "PATH")
	for _, a := range r.AlarmTop {
		fmt.Fprintf(w, "  %-8d  %s\n", a.Count, a.Path)
	}
}

// formatSecondsOrDash prints a pointer-to-float seconds value as
// "Ns" (sub-second precision) or "-" when nil. Keeps the table
// compact and unambiguous (no NaN, no "0.000000" masking missing
// data).
func formatSecondsOrDash(p *float64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatFloat(*p, 'f', 3, 64) + "s"
}

// reportGenerateCmd implements `cios report generate [--type
// monthly|weekly|daily] [--out path]` (PRMT-042).
//
// Fetches /v1/reports/ops from the configured server and renders
// the response to a minimal local HTML document. The --type
// flag is accepted for forward-compat (the server returns a
// single ops view today; type-specific shaping is M3).
//
// Output: --out path writes to disk; empty --out writes to
// stdout (the same convention as `cios query`). No external
// template dependency — stdlib html/template only.
func reportGenerateCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	typ := fs.String("type", "monthly",
		"report type (monthly|weekly|daily) — affects only the title; data is /v1/reports/ops")
	out := fs.String("out", "", "output path; empty → stdout")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios report generate [--type TYPE] [--out PATH]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *typ {
	case "monthly", "weekly", "daily":
		// ok
	default:
		fmt.Fprintf(stderr, "error: --type must be monthly|weekly|daily\n")
		return 2
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/reports/ops", nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got reportOpsRow
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	html := renderReportHTML(&got, *typ)
	if *out == "" {
		_, err := stdout.Write(html)
		if err != nil {
			return 1
		}
		return 0
	}
	if err := os.WriteFile(*out, html, 0o644); err != nil {
		fmt.Fprintf(stderr, "error: write %s: %s\n", *out, err.Error())
		return 1
	}
	return 0
}

// renderReportHTML is the CLI-side mirror of core's renderer.
// Keeping a local copy avoids dragging core's full surface into
// the CLI binary (which only needs the report response shape).
// Visually consistent with the server's renderOpsHTML — same
// labels, same metric formatting.
func renderReportHTML(r *reportOpsRow, typ string) []byte {
	title := "CIOS " + typ + " Ops Report"
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>")
	b.WriteString(title)
	b.WriteString("</title></head><body>\n")
	b.WriteString("<h1>")
	b.WriteString(title)
	b.WriteString("</h1>\n")
	b.WriteString("<p>Generated at <strong>")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	b.WriteString("</strong></p>\n")
	b.WriteString("<h2>Key Metrics</h2><table border=\"1\">")
	b.WriteString("<tr><th>Metric</th><th>Value</th></tr>")
	b.WriteString("<tr><td>MTTR</td><td>")
	b.WriteString(formatSecondsOrDash(r.MTTRSeconds))
	b.WriteString("</td></tr>")
	b.WriteString("<tr><td>Mean response</td><td>")
	b.WriteString(formatSecondsOrDash(r.MeanResponseSeconds))
	b.WriteString("</td></tr>")
	b.WriteString("<tr><td>MTBF</td><td>")
	b.WriteString(formatSecondsOrDash(r.MTBFSeconds))
	b.WriteString("</td></tr></table>\n")
	b.WriteString("<h2>Ticket Counts</h2><p>by state: ")
	for _, st := range []string{"open", "acknowledged", "resolved", "closed"} {
		fmt.Fprintf(&b, "%s=%d ", st, r.TicketCounts.ByState[st])
	}
	b.WriteString("</p><p>by severity: ")
	for _, sv := range []string{"critical", "major", "minor", "info"} {
		fmt.Fprintf(&b, "%s=%d ", sv, r.TicketCounts.BySeverity[sv])
	}
	b.WriteString("</p>\n")
	b.WriteString("<h2>Alarm Top</h2>")
	if len(r.AlarmTop) == 0 {
		b.WriteString("<p>(none)</p>")
	} else {
		b.WriteString("<table border=\"1\"><tr><th>Count</th><th>Path</th></tr>")
		for _, a := range r.AlarmTop {
			fmt.Fprintf(&b, "<tr><td>%d</td><td>%s</td></tr>", a.Count, a.Path)
		}
		b.WriteString("</table>")
	}
	b.WriteString("\n</body></html>\n")
	return []byte(b.String())
}

// reportReconcileRow is the typed mirror of core.reconcileResponse
// so json/yaml modes pass the server bytes through verbatim.
type reportReconcileRow struct {
	Window            string `json:"window"`
	Degraded          bool   `json:"degraded"`
	OrphansRestricted bool   `json:"orphans_restricted"`
	Entries           []struct {
		Path             string `json:"path"`
		Lifecycle        string `json:"lifecycle"`
		State            string `json:"state"`
		TelemetryPresent bool   `json:"telemetry_present"`
		TelemetryUnknown bool   `json:"telemetry_unknown"`
	} `json:"entries"`
	Orphans []struct {
		Path string `json:"path"`
	} `json:"orphans"`
}

// reportReconcileCmd implements `cios report reconcile [--window 7d]`
// (PRMT-050). Fetches GET /v1/reports/reconcile and renders either
// raw JSON/YAML or a labelled table grouped by classification.
func reportReconcileCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	window := fs.String("window", "7d", "trailing window (e.g. 7d, 12h, 30m)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios report reconcile [--window WINDOW]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	q.Set("window", *window)
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/reports/reconcile", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got reportReconcileRow
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
	printReportReconcileTable(stdout, &got)
	return 0
}

// printReportReconcileTable writes a labelled summary suitable for
// interactive review. Layout: one section per classification (ok,
// registered_no_telemetry, orphans), each a compact table. The
// degraded flag is surfaced at the top so operators can tell at a
// glance whether the numbers are complete.
func printReportReconcileTable(w io.Writer, r *reportReconcileRow) {
	fmt.Fprintln(w, "RECONCILE REPORT")
	fmt.Fprintf(w, "  Window       : %s\n", r.Window)
	if r.Degraded {
		fmt.Fprintln(w, "  Degraded     : true  (telemetry side incomplete; some entries unknown)")
	} else {
		fmt.Fprintln(w, "  Degraded     : false")
	}
	if r.OrphansRestricted {
		fmt.Fprintln(w, "  Orphans      : (restricted — caller below operator role)")
	} else if len(r.Orphans) == 0 {
		fmt.Fprintln(w, "  Orphans      : (none)")
	} else {
		fmt.Fprintf(w, "  Orphans      : %d\n", len(r.Orphans))
	}

	// Bucketed listings.
	ok := make([]string, 0)
	noTel := make([]string, 0)
	unk := make([]string, 0)
	for _, e := range r.Entries {
		switch {
		case e.TelemetryUnknown:
			unk = append(unk, e.Path)
		case e.State == "ok":
			ok = append(ok, e.Path)
		default:
			noTel = append(noTel, e.Path)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "OK (registered + telemetry) : %d\n", len(ok))
	for _, p := range ok {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "REGISTERED, NO TELEMETRY : %d\n", len(noTel))
	for _, p := range noTel {
		fmt.Fprintf(w, "  %s\n", p)
	}
	if len(unk) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "UNKNOWN (VM failed) : %d\n", len(unk))
		for _, p := range unk {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}

	if !r.OrphansRestricted && len(r.Orphans) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "ORPHANS (telemetry, no asset) : %d\n", len(r.Orphans))
		for _, o := range r.Orphans {
			fmt.Fprintf(w, "  %s\n", o.Path)
		}
	}
}
