// Command cardinality-bench (PRMT-183) sweeps per-tenant active-series
// cardinality against a throwaway VictoriaMetrics instance and reports
// query-latency p50/p95/p99 plus an ADVISORY label→row threshold.
//
// Posture (mirrors scripts/m3-seed-dev.sh, PRMT-098):
//   - local-only; never wired into `make ci`
//   - ephemeral VM (no named volume), trap-cleaned on every exit path
//   - artifacts → artifacts/cardinality-bench/ (gitignored)
//
// Vocabulary (protocol/types.yaml):
//
//	metric stems = type keys from protocol/cardinality-budget.yaml;
//	`tenant` label = real L53 dimension. See REPORT for the mapping.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PRMT-183 pinned image (mirrors deploy/edge/docker-compose.yml).
const (
	vmImage             = "victoriametrics/victoria-metrics:v1.145.0"
	vmPort              = 8428
	healthPath          = "/health"
	importPath          = "/api/v1/import/prometheus"
	queryPath           = "/api/v1/query"
	rangePath           = "/api/v1/query_range"
	containerNamePrefix = "cardinality-bench-"
)

// Default query set: exercises label-filtered selection, range+rate,
// and top-k aggregation. Pinned (PRMT-183 §4).
var defaultQueries = []string{
	`count({__name__=~"cios_.*",tenant="t0"})`,
	`count({__name__=~"cios_.*",tenant="t1"})`,
	`sum by (type) (rate({__name__=~"cios_.*",tenant="t0"}[5m]))`,
	`topk(3, {__name__=~"cios_.*",tenant="t1"})`,
}

// cardinalType is a min(view of cardinality-budget.yaml type keys
// pruned to ones present in this harness. The list is intentionally
// restricted to keep the script short and the math testable; the
// cardinality-budget.yaml file (the speccheck guardrail) is the
// authoritative source of full counts.
var synthTypes = []string{"gpu", "node", "cell", "rack", "pdu", "valve", "meter", "fan"}

// config is the parsed CLI input. All fields are tested/pinned by
// PRMT-183 §4 (no abstraction beyond what the prompt asked for).
type config struct {
	levels        []int
	reps          int
	degradeFactor float64
	tenants       []string
	checkOnly     bool
	artifactsDir  string
	protocolDir   string
}

func parseConfig() (config, error) {
	levelsStr := flag.String("levels", "10000,30000,100000", "comma-separated per-tenant active-series counts to sweep")
	reps := flag.Int("reps", 20, "repetitions per (level, query) for latency stats")
	degrade := flag.Float64("degrade-factor", 3.0, "advisory p95 threshold = factor × facility-tier baseline p95")
	tenants := flag.String("tenants", "t0,t1", "comma-separated tenant labels (>=2; L53)")
	check := flag.Bool("check-only", false, "tiny levels + reps=2; mechanism smoke; exit 0/1")
	artifacts := flag.String("artifacts", "artifacts/cardinality-bench", "output directory (gitignored)")
	protoDir := flag.String("protocol", "./protocol", "protocol/ directory (read-only)")
	flag.Parse()

	cf := config{
		reps:          *reps,
		degradeFactor: *degrade,
		checkOnly:     *check,
		artifactsDir:  *artifacts,
		protocolDir:   *protoDir,
		tenants:       strings.Split(*tenants, ","),
	}
	for _, t := range cf.tenants {
		if t == "" {
			return cf, errors.New("empty tenant label")
		}
	}
	if len(cf.tenants) < 2 {
		return cf, errors.New("--tenants must contain >=2 labels (L53 isolation sweep)")
	}
	if cf.degradeFactor <= 1.0 {
		return cf, errors.New("--degrade-factor must be > 1.0")
	}
	for _, s := range strings.Split(*levelsStr, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return cf, fmt.Errorf("invalid --levels entry %q", s)
		}
		cf.levels = append(cf.levels, n)
	}
	sort.Ints(cf.levels)
	if cf.checkOnly {
		cf.levels = []int{100, 300}
		cf.reps = 2
	}
	return cf, nil
}

// loadTypeKeys reads protocol/cardinality-budget.yaml and returns the
// per_type_count type keys (protocol/types.yaml vocabulary lock). Falls back
// to synthTypes if the file is missing (e.g. --check-only on a slim
// worktree) — the harness must still be self-contained for the
// mechanism smoke.
func loadTypeKeys(dir string) ([]string, error) {
	path := filepath.Join(dir, "cardinality-budget.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return synthTypes, nil
	}
	lines := strings.Split(string(b), "\n")
	var keys []string
	inBlock := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if strings.HasPrefix(trim, "per_type_count:") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		// Heuristic: top-level indented entries `  key: <int>`.
		// cardinality-budget.yaml is small + flat; this is enough.
		if strings.HasPrefix(ln, "  ") && !strings.HasPrefix(ln, "    ") {
			parts := strings.SplitN(trim, ":", 2)
			if len(parts) == 2 {
				k := strings.TrimSpace(parts[0])
				if k != "" && !strings.HasPrefix(k, "#") {
					keys = append(keys, k)
				}
			}
		}
	}
	if len(keys) == 0 {
		return synthTypes, nil
	}
	return keys, nil
}

// buildSeries renders Prometheus text-exposition lines that hit
// EXACTLY `targetSeries` total series across all tenants, drawn from
// `types`. Pure function (testable in *_test.go): no I/O, no globals.
//
// Each series carries tenant + site + crn labels (L53). crn is a
// synthetic `<site>.<type>.<seq>` stem; this is a LOAD harness, not
// a fidelity claim (PRMT-183 §4).
//
// Allocation: targetSeries split evenly across (tenant, type) tuples;
// the LAST tuple absorbs all rounding remainder so the total is
// exact (matters for the test's activeSeriesFor round-trip and for
// the report's "active series" column being truthful).
func buildSeries(targetSeries int, tenants, types []string) []byte {
	plan := allocateSeries(targetSeries, tenants, types)
	if len(plan) == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.Grow(targetSeries * 96)
	idx := 0
	// Render in the order tenants × types for stability.
	lastTenant, lastType := "", ""
	for ti, t := range tenants {
		for tyi, ty := range types {
			n := plan[ti][tyi]
			if ti == len(tenants)-1 && tyi == len(types)-1 {
				lastTenant, lastType = t, ty
			}
			for k := 0; k < n; k++ {
				idx++
				fmt.Fprintf(&buf,
					"cios_%s_point{site=\"sgp01\",tenant=%q,crn=\"sgp01.%s.%06d\"} %d\n",
					ty, t, ty, idx, idx)
			}
		}
	}
	_ = lastTenant
	_ = lastType
	return buf.Bytes()
}

// allocateSeries returns a [tenant][type] count matrix whose sum
// equals targetSeries exactly (last cell absorbs remainder). Pure.
func allocateSeries(targetSeries int, tenants, types []string) [][]int {
	if targetSeries <= 0 || len(tenants) == 0 || len(types) == 0 {
		return nil
	}
	out := make([][]int, len(tenants))
	for i := range out {
		out[i] = make([]int, len(types))
	}
	cells := len(tenants) * len(types)
	base := targetSeries / cells
	rem := targetSeries - base*cells
	for ti := range tenants {
		for tyi := range types {
			out[ti][tyi] = base
		}
	}
	// Rounding remainder: tile row-major from the last cell backwards.
	// Easier to read forward and subtract — but the round-trip
	// invariant needs the LAST cell to absorb rem so activeSeriesFor
	// (which reads the same matrix) stays exact. We pick the last
	// cell; tests will validate the sum.
	out[len(tenants)-1][len(types)-1] += rem
	return out
}

// activeSeriesFor returns the actual series count rendered by
// buildSeries for the given inputs. Test hook for the round-trip
// property (PRMT-183 §4: "scale to hit each target").
func activeSeriesFor(targetSeries int, tenants, types []string) int {
	plan := allocateSeries(targetSeries, tenants, types)
	total := 0
	for _, row := range plan {
		for _, n := range row {
			total += n
		}
	}
	return total
}

// percentile returns the p-th percentile (0..100) of samples. Pure
// (copy + sort), tolerant of empty input. Linear interpolation between
// adjacent ranks.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	pos := (p / 100) * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + time.Duration(float64(sorted[hi]-sorted[lo])*frac)
}

// recommendThreshold returns the smallest level whose p95 exceeds
// factor × facility-tier p95. Returns 0 if no level crosses (the
// caller reports "no crossing within swept range"). Pure.
func recommendThreshold(perLevel map[int]time.Duration, baselineP95 time.Duration, factor float64) int {
	limit := time.Duration(float64(baselineP95) * factor)
	var levels []int
	for lv := range perLevel {
		levels = append(levels, lv)
	}
	sort.Ints(levels)
	for _, lv := range levels {
		if perLevel[lv] > limit {
			return lv
		}
	}
	return 0
}

// vmHarness owns the throwaway container's lifecycle. All operations
// are serial; the only concurrency primitive is the install-once trap
// (kill + rm by captured id on every exit/INT/TERM).
type vmHarness struct {
	id   string
	host string // http://127.0.0.1:<port>
	cli  string // docker CLI path
	trap func()
}

func startVM(ctx context.Context) (*vmHarness, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker CLI not on PATH: %w", err)
	}
	name := fmt.Sprintf("%s%d", containerNamePrefix, os.Getpid())
	// `-d` so we get the id; `--rm` is a belt; the trap is the suspenders
	// (PRMT-166 orphan lesson).
	runCmd := exec.CommandContext(ctx, docker, "run", "--rm", "-d",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", vmPort, vmPort),
		"--name", name, vmImage)
	out, err := runCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return nil, errors.New("docker run returned empty container id")
	}

	h := &vmHarness{
		id:   id,
		host: fmt.Sprintf("http://127.0.0.1:%d", vmPort),
		cli:  docker,
	}

	// Trap: kill + rm by captured id. Re-entry safe (|| true).
	// Mirror PRMT-166: kill by id is the only reliable handle; the
	// container name is just a convenience for `docker ps` filters.
	h.trap = func() {
		_ = exec.Command(h.cli, "kill", h.id).Run()
		_ = exec.Command(h.cli, "rm", "-f", h.id).Run()
	}

	// Install real trap. Must reference the same closure semantics.
	trapFn := func() {
		_ = exec.Command(h.cli, "kill", h.id).Run()
		_ = exec.Command(h.cli, "rm", "-f", h.id).Run()
	}
	h.trap = trapFn
	installTrap(trapFn)

	// Poll /health (max ~30s).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.host + healthPath)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return h, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	trapFn()
	return nil, errors.New("VM /health did not return 200 within 30s")
}

func (h *vmHarness) close() {
	if h != nil && h.trap != nil {
		h.trap()
	}
}

// --- process-wide trap (one harness per process; --check-only and
// the full run both install exactly once). Installs a SIGINT/SIGTERM
// handler that kills + removes the VM by captured id (PRMT-166
// orphan lesson: never rely on wrappers; kill the container id we
// captured). The defer-based h.close() is the happy path; the
// signal-based trap is the user-interrupt safety net because
// os.Exit() does NOT run defers.
var (
	globalTrapInstalled bool
	installTrap         = installTrapFn
)

func installTrapFn(fn func()) {
	if globalTrapInstalled {
		return
	}
	globalTrapInstalled = true
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fn()
		os.Exit(130)
	}()
}

// importSeries POSTs the text-exposition payload to /api/v1/import/prometheus.
// Uses stream semantics (Content-Length: exact; no chunking) so VM
// ingests the batch in one shot.
func (h *vmHarness) importSeries(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", h.host+importPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("import status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// runQueryTimes runs a single query `reps` times and returns sorted
// per-rep wall-clock latencies. /api/v1/query used for instant;
// /api/v1/query_range for range-bound queries (detected by presence
// of `[...]` in the query — pinned query set; PRMT-183 §4).
func (h *vmHarness) runQueryTimes(ctx context.Context, q string, reps int) ([]time.Duration, error) {
	out := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		path := queryPath
		body := []byte("query=" + q)
		if strings.Contains(q, "[") {
			// Range query: append a fixed window. VM accepts
			// step + start/end as form params.
			now := time.Now().Unix()
			path = rangePath
			body = []byte(fmt.Sprintf("query=%s&start=%d&end=%d&step=30s",
				q, now-300, now))
		}
		req, err := http.NewRequestWithContext(ctx, "POST", h.host+path, bytes.NewReader(body))
		if err != nil {
			return out, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		t0 := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return out, err
		}
		// Drain body so connection reuse + timing are honest.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out = append(out, time.Since(t0))
		if resp.StatusCode/100 != 2 {
			return out, fmt.Errorf("query status %d", resp.StatusCode)
		}
	}
	return out, nil
}

// levelResult is the per-(level, query) latency summary written to
// REPORT.md and the raw JSON artifact.
type levelResult struct {
	Level        int             `json:"level"`
	ActiveSeries int             `json:"active_series"`
	PerQuery     map[string]lat3 `json:"per_query"`
}

type lat3 struct {
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	P99 time.Duration `json:"p99"`
}

func run(ctx context.Context, cf config) (report string, err error) {
	keys, _ := loadTypeKeys(cf.protocolDir)
	if len(keys) > 8 {
		keys = keys[:8]
	}
	if err := os.MkdirAll(cf.artifactsDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir artifacts: %w", err)
	}

	vm, err := startVM(ctx)
	if err != nil {
		return "", err
	}
	defer vm.close()

	results := make([]levelResult, 0, len(cf.levels))
	baselineP95 := time.Duration(0)

	for _, lv := range cf.levels {
		payload := buildSeries(lv, cf.tenants, keys)
		if err := vm.importSeries(ctx, payload); err != nil {
			return "", fmt.Errorf("import level %d: %w", lv, err)
		}
		actual := activeSeriesFor(lv, cf.tenants, keys)
		lr := levelResult{
			Level:        lv,
			ActiveSeries: actual,
			PerQuery:     make(map[string]lat3, len(defaultQueries)),
		}
		// Aggregate per-query p95 across the query set as the level's
		// headline latency (worst per-query p95 is the most honest
		// "this level broke" signal for an isolation-tier sweep).
		worstP95 := time.Duration(0)
		for _, q := range defaultQueries {
			samples, err := vm.runQueryTimes(ctx, q, cf.reps)
			if err != nil {
				return "", fmt.Errorf("query %q @ level %d: %w", q, lv, err)
			}
			lr.PerQuery[q] = lat3{
				P50: percentile(samples, 50),
				P95: percentile(samples, 95),
				P99: percentile(samples, 99),
			}
			if lr.PerQuery[q].P95 > worstP95 {
				worstP95 = lr.PerQuery[q].P95
			}
		}
		if lv == cf.levels[0] {
			baselineP95 = worstP95
		}
		results = append(results, lr)
		rawPath := filepath.Join(cf.artifactsDir, fmt.Sprintf("level-%d.json", lv))
		writeJSON(rawPath, lr)
	}

	perLevelP95 := make(map[int]time.Duration, len(results))
	for _, r := range results {
		var worst time.Duration
		for _, q := range r.PerQuery {
			if q.P95 > worst {
				worst = q.P95
			}
		}
		perLevelP95[r.Level] = worst
	}
	recommended := recommendThreshold(perLevelP95, baselineP95, cf.degradeFactor)

	rendered := renderReport(cf, results, baselineP95, recommended)
	reportPath := filepath.Join(cf.artifactsDir, "REPORT.md")
	if err := os.WriteFile(reportPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write REPORT.md: %w", err)
	}
	return reportPath, nil
}

func renderReport(cf config, results []levelResult, baselineP95 time.Duration, recommended int) string {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	fmt.Fprintf(w, "# Cardinality Benchmark Report (PRMT-183)\n\n")
	fmt.Fprintf(w, "- generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "- vm image: %s\n", vmImage)
	fmt.Fprintf(w, "- tenants: %s (L53 isolation dimension)\n", strings.Join(cf.tenants, ","))
	fmt.Fprintf(w, "- reps per (level, query): %d\n", cf.reps)
	fmt.Fprintf(w, "- degrade-factor: %.2f × facility-tier p95\n", cf.degradeFactor)
	fmt.Fprintf(w, "- facility-tier p95 (baseline, smallest level): %s\n\n", baselineP95)

	fmt.Fprintln(w, "## Per-level latency (worst per-query p95 across the pinned query set)")
	fmt.Fprintln(w, "| level (target) | active series | worst p95 |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, r := range results {
		var worst time.Duration
		for _, q := range r.PerQuery {
			if q.P95 > worst {
				worst = q.P95
			}
		}
		fmt.Fprintf(w, "| %d | %d | %s |\n", r.Level, r.ActiveSeries, worst)
	}

	fmt.Fprintln(w, "\n## Per-query latency per level")
	for _, r := range results {
		fmt.Fprintf(w, "\n### level=%d active_series=%d\n", r.Level, r.ActiveSeries)
		fmt.Fprintln(w, "| query | p50 | p95 | p99 |")
		fmt.Fprintln(w, "|---|---|---|---|")
		for _, q := range defaultQueries {
			l := r.PerQuery[q]
			fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", q, l.P50, l.P95, l.P99)
		}
	}

	fmt.Fprintln(w, "\n## Advisory label→row threshold")
	if recommended == 0 {
		fmt.Fprintf(w, "- **recommended label→row threshold ≈ NOT CROSSED within swept range** (worst p95 stayed within %.2f× baseline)\n", cf.degradeFactor)
	} else {
		fmt.Fprintf(w, "- **recommended label→row threshold ≈ %d active series** (smallest level whose worst p95 exceeds %.2f × facility-tier p95)\n", recommended, cf.degradeFactor)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "> ADVISORY ONLY. The locked label→row threshold is Yuri/architect's call")
	fmt.Fprintln(w, "> (D33(a)). This harness produces evidence; the threshold number itself is")
	fmt.Fprintln(w, "> the architect's lock. The full multi-level run (e.g. up to ≥100万/site) is")
	fmt.Fprintln(w, "> expected to be executed on a capable host by Yuri; numbers above are from")
	fmt.Fprintln(w, "> this run only and reflect a synthetic load — metric stems use")
	fmt.Fprintln(w, "> `protocol/cardinality-budget.yaml` type keys, not live CIOS metrics.")

	fmt.Fprintln(w, "\n## Vocabulary mapping")
	fmt.Fprintln(w, "- Metric stems: `cios_<type>_point` for `<type>` ∈ cardinality-budget per_type_count keys.")
	fmt.Fprintln(w, "- Labels: `site` (synthetic, single sgp01), `tenant` (real L53 dim, swept axis), `crn` (synthetic path-like stem).")
	fmt.Fprintln(w, "- This is a LOAD harness — not a fidelity claim against live CIOS telemetry.")

	_ = w.Flush()
	return b.String()
}

func writeJSON(path string, v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(path, b, 0o644)
}

func main() {
	cf, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cardinality-bench: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	reportPath, err := run(ctx, cf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cardinality-bench: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("REPORT:", reportPath)
}
