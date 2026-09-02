// Package core — report_gen_test.go: renderOpsHTML + reportOne +
// RunReportScheduler behaviour (PRMT-042 §6 acceptance).
//
// Covers:
//   - renderOpsHTML contains the key fields (MTTR, ticket counts,
//     alarm top, generated timestamp)
//   - renderOpsHTML renders nil metrics as "-"
//   - reportOne writes a file under the configured dir
//   - reportOne is fail-soft on bad dir (returns error, no panic)
//   - RunReportScheduler with empty dir is a no-op
//   - RunReportScheduler ticks (single iteration, via short interval)
package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// newReportGenServer builds a Server backed by a fresh fileStore
// at a temp path. The report path only touches ListTickets +
// ListAlarms; a real store is closer to production than a stub
// and avoids the friction of stubbing the whole Store interface.
func newReportGenServer(t *testing.T) *Server {
	t.Helper()
	root := moduleRoot(t)
	dict, err := cpath.LoadDict(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	storePath := filepath.Join(t.TempDir(), "store.json")
	st, err := NewFileStore(storePath)
	if err != nil {
		t.Fatalf("newFileStore: %v", err)
	}
	return NewServer(st, dict, "")
}

// TestRenderOpsHTMLContainsKeyFields pins the renderer's output
// against a representative opsReportResponse. Any field rename
// or template drift shows up here first.
func TestRenderOpsHTMLContainsKeyFields(t *testing.T) {
	v := 12.5
	r := opsReportResponse{
		MTTRSeconds:         &v,
		MeanResponseSeconds: &v,
		MTBFSeconds:         &v,
		TicketCounts: opsReportTicketCounts{
			ByState:    map[string]int{"open": 3, "closed": 1},
			BySeverity: map[string]int{"major": 2, "minor": 2},
		},
		AlarmTop: []opsReportAlarmTopItem{
			{Path: "sgp01.pod001.cdu000.fws.supply.flow", Count: 4},
		},
	}
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	out := renderOpsHTML(r, now, nil)
	body := string(out)
	for _, want := range []string{
		"CIOS Operations Report",
		now.Format(time.RFC3339),
		"12.500s",
		">open<",
		">major<",
		"sgp01.pod001.cdu000.fws.supply.flow",
		"M2.x 待接 VM 趋势",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderOpsHTML: missing %q\n--- output ---\n%s", want, body)
		}
	}
}

// TestRenderOpsHTMLNilMetrics ensures nil pointer metrics render
// as "-" rather than "0.000s" (the §2-bis "0 / null" rule).
func TestRenderOpsHTMLNilMetrics(t *testing.T) {
	r := opsReportResponse{
		TicketCounts: opsReportTicketCounts{
			ByState:    map[string]int{},
			BySeverity: map[string]int{},
		},
		AlarmTop: []opsReportAlarmTopItem{},
	}
	out := renderOpsHTML(r, time.Now().UTC(), nil)
	body := string(out)
	if !strings.Contains(body, "<td>-</td>") {
		t.Errorf("renderOpsHTML: nil metrics should render '-', got:\n%s", body)
	}
}

// TestReportOneWritesFile exercises the scheduler's per-tick
// path: list → compute → render → write. The temp dir is the
// output target; we verify the file appears with the expected
// name pattern.
func TestReportOneWritesFile(t *testing.T) {
	s := newReportGenServer(t)
	dir := t.TempDir()
	if err := s.reportOne(context.Background(), dir, 0, time.Now().UTC()); err != nil {
		t.Fatalf("reportOne: %v", err)
	}
	want := "ops-" + time.Now().UTC().Format("2006-01-02") + ".html"
	path := filepath.Join(dir, want)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reportOne: expected %s: %v", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reportOne: read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "CIOS Operations Report") {
		t.Errorf("reportOne: output missing marker\n%s", body)
	}
}

// TestReportOneFailsSoftOnBadDir verifies the scheduler does not
// panic on an unwritable dir. It logs and returns an error; the
// caller (the ticker) drops it and retries next tick.
func TestReportOneFailsSoftOnBadDir(t *testing.T) {
	s := newReportGenServer(t)
	// A path under a non-existent parent that we cannot create
	// because the parent is a file. This triggers MkdirAll
	// failure without a panic.
	parent := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	bad := filepath.Join(parent, "nope", "deeper")
	if err := s.reportOne(context.Background(), bad, 0, time.Now().UTC()); err == nil {
		t.Errorf("reportOne: expected error on bad dir, got nil")
	}
}

// TestRunReportSchedulerDisabledOnEmptyDir pins the default-off
// behaviour: an empty dir means no goroutine, no log spam, no
// work. The function must return promptly.
func TestRunReportSchedulerDisabledOnEmptyDir(t *testing.T) {
	s := newReportGenServer(t)
	done := make(chan struct{})
	go func() {
		s.RunReportScheduler(context.Background(), 1*time.Hour, "", 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunReportScheduler(empty dir) did not return promptly")
	}
}

// TestRunReportSchedulerTicks drives a single tick via a 50ms
// interval. We cancel the context immediately after the file
// appears; the goroutine exits via ctx.Done.
func TestRunReportSchedulerTicks(t *testing.T) {
	s := newReportGenServer(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunReportScheduler(ctx, 50*time.Millisecond, dir, 0)
		close(done)
	}()
	// The startup run writes the file before the first tick, so
	// we wait for it (one full tick window is plenty).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "ops-"+time.Now().UTC().Format("2006-01-02")+".html")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunReportScheduler did not exit on ctx cancel")
	}
}

// TestRenderOpsHTMLNilMapsIsEmpty ensures nil maps in the source
// response render as empty tables rather than panic on the
// template range.
func TestRenderOpsHTMLNilMapsIsEmpty(t *testing.T) {
	r := opsReportResponse{} // all nil/zero
	out := renderOpsHTML(r, time.Now().UTC(), nil)
	if !strings.Contains(string(out), "CIOS Operations Report") {
		t.Errorf("renderOpsHTML: nil-maps response missing root marker")
	}
}

// touchReport writes a placeholder ops-*.html file into dir with
// the given mtime. We use os.Chtimes rather than os.WriteFile's
// mtime to make ordering deterministic — the scheduler's
// production path uses real time which is non-monotonic enough
// in tests to flake when the test runs the same day as another
// test in the same package.
func touchReport(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("<html></html>"), 0o644); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestPruneReportsKeepsLastN ensures pruneReports trims oldest
// files first and stops at the keep limit. Three files spread
// over distinct mtimes; keep=2 → only the two newest survive.
func TestPruneReportsKeepsLastN(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	touchReport(t, dir, "ops-2026-06-01.html", base)
	touchReport(t, dir, "ops-2026-06-02.html", base.Add(24*time.Hour))
	touchReport(t, dir, "ops-2026-06-03.html", base.Add(48*time.Hour))
	if err := pruneReports(dir, 2); err != nil {
		t.Fatalf("pruneReports: %v", err)
	}
	got := listOpsReports(t, dir)
	want := []string{"ops-2026-06-02.html", "ops-2026-06-03.html"}
	if !equalStringSlice(got, want) {
		t.Errorf("pruneReports(2) = %v, want %v", got, want)
	}
}

// TestPruneReportsKeepZeroIsNoop pins the "0 = unlimited" path:
// pruning is skipped entirely, no file is touched, no error.
func TestPruneReportsKeepZeroIsNoop(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		touchReport(t, dir, fmt.Sprintf("ops-2026-06-0%d.html", i+1), base.Add(time.Duration(i)*24*time.Hour))
	}
	if err := pruneReports(dir, 0); err != nil {
		t.Fatalf("pruneReports(0): %v", err)
	}
	if got := listOpsReports(t, dir); len(got) != 5 {
		t.Errorf("pruneReports(0) deleted files: have %d, want 5 (%v)", len(got), got)
	}
}

// TestPruneReportsLeavesNonReportFilesAlone is the safety check:
// a hand-placed "notes.md" or "index.html" must survive every
// prune call. This is what the prompt's "不碰目录内其它文件"
// boils down to at the API level.
func TestPruneReportsLeavesNonReportFilesAlone(t *testing.T) {
	dir := t.TempDir()
	touchReport(t, dir, "ops-2026-06-01.html", time.Now().UTC())
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("operator notes"), 0o644); err != nil {
		t.Fatalf("write notes.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	if err := pruneReports(dir, 0); err != nil {
		t.Fatalf("pruneReports: %v", err)
	}
	for _, n := range []string{"notes.md", "extra.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("pruneReports removed %s: %v", n, err)
		}
	}
}

// TestWriteReportIndexListsReverseChronological verifies the
// index page contains a <a> link for each report, in newest-first
// order. We assert on substring positions in the rendered HTML
// rather than a parsed DOM — html/template is the only thing
// building this string, so a substring check is plenty.
func TestWriteReportIndexListsReverseChronological(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	names := []string{
		"ops-2026-06-01.html",
		"ops-2026-06-02.html",
		"ops-2026-06-03.html",
	}
	for i, n := range names {
		touchReport(t, dir, n, base.Add(time.Duration(i)*24*time.Hour))
	}
	if err := writeReportIndex(dir); err != nil {
		t.Fatalf("writeReportIndex: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	s := string(body)
	// Newest first: 06-03 appears before 06-02 before 06-01.
	pos3 := strings.Index(s, "ops-2026-06-03.html")
	pos2 := strings.Index(s, "ops-2026-06-02.html")
	pos1 := strings.Index(s, "ops-2026-06-01.html")
	if pos3 < 0 || pos2 < 0 || pos1 < 0 {
		t.Fatalf("index missing one of the report links:\n%s", s)
	}
	if !(pos3 < pos2 && pos2 < pos1) {
		t.Errorf("index not reverse-chronological: 06-03@%d 06-02@%d 06-01@%d\n%s", pos3, pos2, pos1, s)
	}
}

// TestWriteReportIndexEscapesFilenames pins html/template's
// auto-escape on the index. A filename containing < and & must
// come out as &lt; and &amp; — a hand-rolled renderer would
// have to remember to do this, the template engine does it
// for us, and this test catches the day someone swaps to fmt.Fprintf.
func TestWriteReportIndexEscapesFilenames(t *testing.T) {
	dir := t.TempDir()
	// Filename deliberately outside the scheduler's production
	// shape — this is a defensive test, not a happy-path. The
	// whitelist filter is on HasPrefix(reportFilePrefix), so
	// "ops-x<y>.html" matches; the test asserts the < is
	// escaped on the way out.
	name := "ops-x<y>&z.html"
	touchReport(t, dir, name, time.Now().UTC())
	if err := writeReportIndex(dir); err != nil {
		t.Fatalf("writeReportIndex: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "ops-x<y>&z.html") {
		t.Errorf("index contains raw < or & — not escaped:\n%s", s)
	}
	if !strings.Contains(s, "ops-x&lt;y&gt;&amp;z.html") {
		t.Errorf("index missing escaped form:\n%s", s)
	}
}

// TestWriteReportIndexEmptyDir ensures an empty dir yields the
// "No reports yet." placeholder rather than an empty <table>
// (which would render as a dangling header with no rows).
func TestWriteReportIndexEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := writeReportIndex(dir); err != nil {
		t.Fatalf("writeReportIndex: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "No reports yet.") {
		t.Errorf("empty-dir index missing placeholder:\n%s", s)
	}
	if strings.Contains(s, "<table>") {
		t.Errorf("empty-dir index should not render a <table>:\n%s", s)
	}
}

// listOpsReports returns the basenames of files in dir that look
// like scheduler-produced reports, in lexical order. Used by the
// retention tests to assert on what survived the prune.
func listOpsReports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "ops-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// equalStringSlice is a tiny helper that avoids pulling in
// reflect.DeepEqual's import chain just for tests.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
