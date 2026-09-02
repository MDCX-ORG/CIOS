// Package core — report_gen.go: HTML rendering + scheduled report
// writes (M2 E2.6 P552 / §M2-4).
//
// Wraps the existing opsReportResponse (PRMT-037) in a minimal
// HTML document and writes it to disk on a configurable interval.
// The scheduler is opt-in: empty -report-dir disables it, so the
// default behaviour is unchanged (no goroutine, no files).
//
// Failure handling: every write is fail-soft. A bad disk path or
// transient write error is logged and the loop continues — the
// next tick retries. This mirrors RunSLAScanner's contract so
// M3 can introduce leader election across the board without
// changing this file's behaviour.
//
// Scope: the scheduler reads every ticket and alarm — it is a
// site-wide operations report, not per-tenant. Per-tenant
// reports are M3. The comment on RunReportScheduler records the
// trade-off so future readers do not "fix" it without checking
// the prompt.
package core

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// opsReportHTML is the template body. Kept as a package-level
// constant so a single template.Parse call at startup builds it
// (no per-render Parse cost). The placeholder sections for
// availability / PUE trend are explicit "M2.x 待接 VM 趋势"
// strings, not speculative numbers — spec-008 v0.3 Q11.
const opsReportHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>CIOS Ops Report — {{.GeneratedAt}}</title>
<style>
body{font-family:sans-serif;margin:2em;color:#222}
h1{border-bottom:2px solid #444}
table{border-collapse:collapse;margin:1em 0}
td,th{border:1px solid #aaa;padding:.4em .8em;text-align:left}
.metric{font-weight:600}
.placeholder{color:#888;font-style:italic}
</style>
</head>
<body>
<h1>CIOS Operations Report</h1>
<p>Generated at <span class="metric">{{.GeneratedAt}}</span></p>

<h2>Key Metrics</h2>
<table>
<tr><th>Metric</th><th>Value</th></tr>
<tr><td>MTTR (mean resolve)</td><td>{{.MTTR}}</td></tr>
<tr><td>Mean response time</td><td>{{.MeanResp}}</td></tr>
<tr><td>MTBF (per-asset)</td><td>{{.MTBF}}</td></tr>
</table>

<h2>Ticket Counts by State</h2>
<table>
<tr><th>State</th><th>Count</th></tr>
{{range $k, $v := .ByState}}<tr><td>{{$k}}</td><td>{{$v}}</td></tr>{{end}}
</table>

<h2>Ticket Counts by Severity</h2>
<table>
<tr><th>Severity</th><th>Count</th></tr>
{{range $k, $v := .BySeverity}}<tr><td>{{$k}}</td><td>{{$v}}</td></tr>{{end}}
</table>

<h2>Alarm Top (firing snapshot)</h2>
{{if .AlarmTop}}
<table>
<tr><th>Count</th><th>Path</th></tr>
{{range .AlarmTop}}<tr><td>{{.Count}}</td><td>{{.Path}}</td></tr>{{end}}
</table>
{{else}}
<p>(none)</p>
{{end}}

<h2>Pipeline Gaps (DATA-RESILIENCE G6)</h2>
{{if .PipelineGaps}}
<p>Firing alarms whose summary indicates telemetry silence (rule pipeline-gap or equivalent).</p>
<table>
<tr><th>Path</th><th>Since</th><th>Summary</th></tr>
{{range .PipelineGaps}}<tr><td>{{.Path}}</td><td>{{.Since}}</td><td>{{.Summary}}</td></tr>{{end}}
</table>
{{else}}
<p>(none firing)</p>
{{end}}

<h2>Availability / PUE Trend</h2>
<p class="placeholder">M2.x 待接 VM 趋势 — not yet implemented.</p>

</body>
</html>
`

// opsHTMLData is the view-model fed to opsReportHTML. Flattening
// pointer-to-float into "-" / formatted string here keeps the
// template free of conditional formatting logic.
type opsHTMLPipelineGap struct {
	Path    string
	Since   string
	Summary string
}

type opsHTMLData struct {
	GeneratedAt  string
	MTTR         string
	MeanResp     string
	MTBF         string
	ByState      map[string]int
	BySeverity   map[string]int
	AlarmTop     []opsReportAlarmTopItem
	PipelineGaps []opsHTMLPipelineGap
}

// renderOpsHTML converts an opsReportResponse into a minimal
// HTML document. Pure function — no Store access, no clock — so
// the unit test can pin the generatedAt timestamp and assert
// field-by-field. The template is parsed once at package init
// (parseError is checked at init; a bad template is a programmer
// error, not a runtime concern).
var opsHTMLTmpl = template.Must(template.New("ops").Parse(opsReportHTML))

// renderOpsHTML is the package-level entry point used by both
// the scheduler and the CLI. at is the wall-clock time the report
// pipelineGaps is optional (DATA-RESILIENCE G6); nil/empty omits rows.
func renderOpsHTML(r opsReportResponse, at time.Time, pipelineGaps []opsHTMLPipelineGap) []byte {
	d := opsHTMLData{
		GeneratedAt:  at.UTC().Format(time.RFC3339),
		MTTR:         formatSecOrDash(r.MTTRSeconds),
		MeanResp:     formatSecOrDash(r.MeanResponseSeconds),
		MTBF:         formatSecOrDash(r.MTBFSeconds),
		ByState:      r.TicketCounts.ByState,
		BySeverity:   r.TicketCounts.BySeverity,
		AlarmTop:     r.AlarmTop,
		PipelineGaps: pipelineGaps,
	}
	// Defensive: even though computeOpsReport always populates the
	// maps, a hand-built response (e.g. test fixture) might pass
	// nil. Render nil maps as empty so the template's range is a
	// no-op rather than panicking.
	if d.ByState == nil {
		d.ByState = map[string]int{}
	}
	if d.BySeverity == nil {
		d.BySeverity = map[string]int{}
	}
	if d.AlarmTop == nil {
		d.AlarmTop = []opsReportAlarmTopItem{}
	}
	if d.PipelineGaps == nil {
		d.PipelineGaps = []opsHTMLPipelineGap{}
	}
	var buf []byte
	// Render into a small in-memory buffer; the scheduler writes
	// it to disk and the CLI writes to stdout. Buffer is local so
	// renderOpsHTML doesn't share state across calls.
	bw := byteWriter{p: make([]byte, 0, 4096)}
	if err := opsHTMLTmpl.Execute(&bw, d); err != nil {
		// Template execution only fails on a programmer error in
		// the template body; with a parsed template at init we
		// cannot recover here. Log and emit an empty document so
		// callers still get a valid HTML response.
		log.Printf("core: report render: %v", err)
		return []byte("<!doctype html><html><body><p>render error</p></body></html>\n")
	}
	buf = bw.p
	return buf
}

// byteWriter is a minimal io.Writer backed by a byte slice. We
// avoid importing bytes.Buffer to keep the dependency surface of
// this file at zero (matches PRMT-037's spirit). Append is the
// only Write we ever issue.
type byteWriter struct{ p []byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	w.p = append(w.p, p...)
	return len(p), nil
}

// formatSecOrDash prints a pointer-to-float seconds value as the
// HTML-side mirror of cli/report.go's formatSecondsOrDash — keep
// the two in sync so table and HTML render the same nil/non-nil
// shape.
func formatSecOrDash(p *float64) string {
	if p == nil {
		return "-"
	}
	// 3 decimal places matches the CLI's table format so HTML and
	// CLI pages are visually consistent.
	return fmt.Sprintf("%.3fs", *p)
}

func formatFloatOrDash(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

// pipelineGapsFromAlarms picks firing/acked alarms that indicate
// telemetry silence (cios-alarm rule pipeline-gap or summary text).
func pipelineGapsFromAlarms(alarms []Alarm) []opsHTMLPipelineGap {
	var out []opsHTMLPipelineGap
	for _, a := range alarms {
		if a.State != "firing" && a.State != "acked" {
			continue
		}
		sum := strings.ToLower(a.Summary)
		if !strings.Contains(sum, "pipeline gap") && !strings.Contains(sum, "pipeline-gap") {
			continue
		}
		out = append(out, opsHTMLPipelineGap{
			Path:    a.Path,
			Since:   a.Since.UTC().Format(time.RFC3339),
			Summary: a.Summary,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// reportFilePrefix is the basename prefix every scheduler-produced
// report file uses (ops-YYYY-MM-DD.html). Exported for tests so
// the whitelist check stays in one place; pruneReports and
// writeReportIndex both filter the directory listing against this
// prefix so they never touch unrelated files in the same dir.
const reportFilePrefix = "ops-"

// reportOne runs a single report generation tick: list tickets +
// alarms, compute the ops report, render, and write to
// dir/ops-YYYY-MM-DD.html. Pulled out of RunReportScheduler so
// the unit test can drive it without a ticker.
//
// After a successful write it (a) prunes the report directory
// down to the most recent keep files (PRMT-064 retention), and
// (b) rewrites <dir>/index.html with the live listing. Both
// steps are fail-soft — a prune/index error is logged and
// swallowed so a transient FS hiccup doesn't lose the report
// that was just written.
//
// Failure handling is fail-soft per PRMT-042 §5: ListTickets /
// ListAlarms / MkdirAll / WriteFile errors are logged and
// swallowed. The scheduler retries on the next tick.
func (s *Server) reportOne(ctx context.Context, dir string, keep int, now time.Time) error {
	if dir == "" {
		// Belt-and-braces: RunReportScheduler short-circuits on
		// empty dir before starting the ticker, so this branch
		// only fires if reportOne is called directly (tests).
		return nil
	}
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the report tick for
	// this tick window. The pg advisory lock is session-scoped
	// and released when the tick ends (release is deferred). On
	// error we log + return; on !acquired we silently skip —
	// another instance leads and will write the report for this
	// window. Placed AFTER the dir=="" short-circuit so a
	// disabled scheduler (empty dir) never touches the lock.
	ok, release, err := s.st.TryScannerLock(ctx, "report")
	if err != nil {
		log.Printf("core: report scheduler: try lock: %v", err)
		return err
	}
	if !ok {
		return nil
	}
	defer release()
	tickets, err := s.st.ListTickets(ctx)
	if err != nil {
		log.Printf("core: report scheduler: list tickets: %v", err)
		return err
	}
	alarms, err := s.st.ListAlarms(ctx)
	if err != nil {
		log.Printf("core: report scheduler: list alarms: %v", err)
		return err
	}
	resp := computeOpsReport(tickets)
	// Scheduler is site-wide (no principal → no scope filter) per
	// PRMT-042 §4 contract. The `**` glob is the same default the
	// HTTP handler uses for the unfiltered case.
	glob, _ := compileAllGlob()
	resp.AlarmTop = topAlarmsByPath(filterAlarmsByGlob(alarms, glob), 10)

	// DATA-RESILIENCE G6: pipeline gap rows from firing alarms.
	gaps := pipelineGapsFromAlarms(alarms)

	body := renderOpsHTML(resp, now, gaps)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("core: report scheduler: mkdir %s: %v", dir, err)
		return err
	}
	name := filepath.Join(dir, "ops-"+now.UTC().Format("2006-01-02")+".html")
	if err := os.WriteFile(name, body, 0o644); err != nil {
		log.Printf("core: report scheduler: write %s: %v", name, err)
		return err
	}
	// Fail-soft retention + index (PRMT-064). The report itself
	// is already on disk by this point; a prune or index error
	// must not erase that progress.
	if err := pruneReports(dir, keep); err != nil {
		log.Printf("core: report scheduler: prune: %v", err)
	}
	if err := writeReportIndex(dir); err != nil {
		log.Printf("core: report scheduler: index: %v", err)
	}
	return nil
}

// pruneReports deletes oldest scheduler-produced reports from dir
// until at most keep remain. keep == 0 (or negative) disables
// pruning and returns nil immediately. The function only touches
// files whose basename starts with reportFilePrefix — never
// RemoveAll, never an unfiltered listing. Sort key is the file's
// mtime (newest last → trim from the front), which is robust to
// multiple writes in the same day (same-date names are still
// pruned by mtime, not by lexical order). Errors are surfaced to
// the caller; the scheduler logs and continues (fail-soft).
func pruneReports(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type cand struct {
		name    string
		modTime time.Time
	}
	var reports []cand
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), reportFilePrefix) {
			// Whitelist: never delete anything that isn't a
			// scheduler-produced report (e.g. operator's
			// hand-placed notes, an existing index.html being
			// rewritten by writeReportIndex is the only other
			// thing in the dir by convention).
			continue
		}
		info, err := e.Info()
		if err != nil {
			// A file that disappears between ReadDir and Info
			// (raced removal by operator) is skipped, not
			// fatal — the rest of the prune still runs.
			continue
		}
		reports = append(reports, cand{name: e.Name(), modTime: info.ModTime()})
	}
	// Newest first so reports[len-keep:] is what we keep.
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].modTime.After(reports[j].modTime)
	})
	if len(reports) <= keep {
		return nil
	}
	for _, c := range reports[keep:] {
		if err := os.Remove(filepath.Join(dir, c.name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// reportIndexTmpl is the index page template. Kept as a package-
// level constant so a single template.Parse at init builds it
// (mirrors opsHTMLTmpl). Filenames flow through {{.}} which
// html/template auto-escapes — a filename with a stray < or &
// is harmless, no manual escaping required.
var reportIndexTmpl = template.Must(template.New("reportIndex").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>CIOS Ops Reports</title>
<style>
body{font-family:sans-serif;margin:2em;color:#222}
h1{border-bottom:2px solid #444}
table{border-collapse:collapse;margin:1em 0}
td,th{border:1px solid #aaa;padding:.4em .8em;text-align:left}
.empty{color:#888;font-style:italic}
</style>
</head>
<body>
<h1>CIOS Ops Reports</h1>
{{if .Entries}}
<table>
<tr><th>Generated at</th><th>Report</th></tr>
{{range .Entries}}<tr><td>{{.GeneratedAt}}</td><td><a href="{{.Name}}">{{.Name}}</a></td></tr>{{end}}
</table>
{{else}}
<p class="empty">No reports yet.</p>
{{end}}
</body>
</html>
`))

// reportIndexEntry is the per-row view-model fed to
// reportIndexTmpl. GeneratedAt is the formatted RFC3339 of the
// file's mtime (UTC) so the listing matches the "Generated at"
// timestamp inside each report.
type reportIndexEntry struct {
	Name        string
	GeneratedAt string
}

// writeReportIndex scans dir for scheduler-produced reports and
// writes <dir>/index.html with a reverse-chronological listing.
// It overwrites any existing index.html (the only allowed
// non-report write into the dir). The page is built with
// html/template so filenames are auto-escaped. An empty dir
// renders the "No reports yet." placeholder rather than an empty
// table. Errors are returned to the caller; the scheduler logs
// and continues (fail-soft per PRMT-064).
func writeReportIndex(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type withTime struct {
		name    string
		modTime time.Time
	}
	var rs []withTime
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), reportFilePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rs = append(rs, withTime{name: e.Name(), modTime: info.ModTime()})
	}
	// Newest first for the listing.
	sort.Slice(rs, func(i, j int) bool {
		return rs[i].modTime.After(rs[j].modTime)
	})
	view := struct {
		Entries []reportIndexEntry
	}{}
	for _, r := range rs {
		view.Entries = append(view.Entries, reportIndexEntry{
			Name:        r.name,
			GeneratedAt: r.modTime.UTC().Format(time.RFC3339),
		})
	}
	var buf []byte
	bw := byteWriter{p: make([]byte, 0, 1024)}
	if err := reportIndexTmpl.Execute(&bw, view); err != nil {
		return err
	}
	buf = bw.p
	return os.WriteFile(filepath.Join(dir, "index.html"), buf, 0o644)
}

// compileAllGlob returns the "**" glob used by the report path.
// Centralised so the report scheduler and the HTTP handler can't
// drift on the default-scope semantics. (The HTTP handler builds
// its own glob from cpath.CompileGlob — we re-import here via a
// thin wrapper to avoid coupling report_gen.go to cpath directly
// when the goal is just "match everything".)
func compileAllGlob() (globLike, error) {
	// Mirror the cpath.Match(**) semantics via a tiny shim:
	// "*" matches any non-empty path; "**" matches the same.
	// We use a struct with a Match method so report_gen stays
	// independent of cpath internals.
	return globLike{matchAll: true}, nil
}

// globLike is a stand-in for cpath.Glob scoped to this file.
// Only the report scheduler uses it, and it always wants
// "match everything" — keeping the type local avoids an import.
type globLike struct{ matchAll bool }

func (g globLike) Match(string) bool { return g.matchAll }

// filterAlarmsByGlob applies the given glob to alarms. Pulled out
// so report_gen.go does not import cpath directly — keeps the
// dependency footprint of this file at zero.
func filterAlarmsByGlob(alarms []Alarm, glob globLike) []Alarm {
	out := make([]Alarm, 0, len(alarms))
	for _, a := range alarms {
		if glob.Match(a.Path) {
			out = append(out, a)
		}
	}
	return out
}

// RunReportScheduler is the long-lived background goroutine.
// Empty dir ⇒ no-op (returns immediately); interval ≤ 0 falls
// back to 24h (matches -report-interval's default). keep ≤ 0
// disables retention (keep all files); keep > 0 keeps at most
// keep most-recent reports on disk after every tick. Runs once
// at startup, then ticks. Exits cleanly on ctx.Done.
//
// The scheduler is intentionally NOT scope-filtered: ops reports
// are a site-wide artifact. Per-tenant reports are M3.
func (s *Server) RunReportScheduler(ctx context.Context, interval time.Duration, dir string, keep int) {
	if dir == "" {
		// Default-off: matches -report-dir's "" default in
		// cmd/cios-core so existing deployments see no behaviour
		// change.
		log.Printf("core: report scheduler: disabled (empty dir)")
		return
	}
	if interval <= 0 {
		// Belt-and-braces for misconfigured flags.
		interval = 24 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run once at startup so a freshly-booted cios-core produces
	// today's report immediately rather than waiting for the
	// first tick. safeTick (PRMT-076) so a panic in reportOne
	// can't kill the long-lived goroutine; the panic-isolation
	// wrapper is the panic-only escape hatch, error handling
	// stays below.
	now := time.Now().UTC()
	safeTick("report", func() {
		if err := s.reportOne(ctx, dir, keep, now); err != nil {
			// PRMT-066: record startup-tick outcome for /v1/health/scanners.
			s.recordScanner("report", now, err)
		} else {
			s.recordScanner("report", now, nil)
		}
	})
	for {
		select {
		case <-ctx.Done():
			return
		case tickAt := <-t.C:
			tickAt = tickAt.UTC()
			safeTick("report", func() {
				if err := s.reportOne(ctx, dir, keep, tickAt); err != nil {
					s.recordScanner("report", tickAt, err)
				} else {
					s.recordScanner("report", tickAt, nil)
				}
			})
		}
	}
}

// RenderOpsHTMLForTest is a thin wrapper exported only to the
// test package (via the _test.go file in the same dir). It is
// not part of the stable surface — kept here so the test can
// invoke the renderer without rebuilding the template.
func RenderOpsHTMLForTest(r opsReportResponse, at time.Time) []byte {
	return renderOpsHTML(r, at, nil)
}
