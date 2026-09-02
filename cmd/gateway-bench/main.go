// Command gateway-bench (PRMT-210 / P798 / D10) models pod-gateway
// resource demand across a driver-count × sample-rate matrix and
// emits an ADVISORY markdown report for D9 sizing.
//
// Posture (mirrors cmd/cardinality-bench / PRMT-183):
//   - local-only; never wired into `make ci`
//   - pure model (no ARM hardware required); assumptions documented in REPORT
//   - artifacts → artifacts/gateway-bench/ (gitignored)
//
// The tool does NOT lock sizing numbers — Yuri/architect lock after review.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Model constants (documented assumptions; not production measurements).
// Tuned so the L51 ~1e6 points/site design headroom appears in the matrix.
const (
	// baseRSSMB is process overhead before any driver.
	baseRSSMB = 48.0
	// perDriverRSSMB is session + buffers per pull driver.
	perDriverRSSMB = 2.5
	// perPointRSSKB is in-memory binding + last-sample cache per point.
	perPointRSSKB = 0.4
	// cpuNsPerSample is modelled CPU for one poll convert+project cycle.
	cpuNsPerSample = 25_000.0
	// bytesPerSample is WAL/exposition bytes per emitted sample (approx).
	bytesPerSample = 96.0
	// pointsPerDriverDefault when --points-per-driver is unset.
	pointsPerDriverDefault = 64
)

type cell struct {
	Drivers         int     `json:"drivers"`
	IntervalMS      int     `json:"interval_ms"`
	PointsPerDriver int     `json:"points_per_driver"`
	TotalPoints     int     `json:"total_points"`
	SamplesPerSec   float64 `json:"samples_per_sec"`
	CPUCores        float64 `json:"cpu_cores_est"`
	RSSMB           float64 `json:"rss_mb_est"`
	WALMBPerDay     float64 `json:"wal_mb_per_day_est"`
	StorageMBDay    float64 `json:"storage_mb_per_day_est"`
}

type report struct {
	GeneratedUTC string   `json:"generated_utc"`
	Assumptions  []string `json:"assumptions"`
	Cells        []cell   `json:"cells"`
	// Advisory note for D9 (not a lock).
	Advisory string `json:"advisory"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gateway-bench: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gateway-bench", flag.ContinueOnError)
	driversStr := fs.String("drivers", "10,50,100,500,1000", "comma-separated driver counts")
	intervalsStr := fs.String("intervals-ms", "1000,500,200,100", "comma-separated poll intervals (ms)")
	ppd := fs.Int("points-per-driver", pointsPerDriverDefault, "points polled per driver")
	artifacts := fs.String("artifacts", "artifacts/gateway-bench", "output directory (gitignored)")
	checkOnly := fs.Bool("check-only", false, "tiny matrix smoke; exit 0/1")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *checkOnly {
		*driversStr = "2,5"
		*intervalsStr = "1000,500"
		*ppd = 8
	}

	drivers, err := parseIntList(*driversStr)
	if err != nil {
		return fmt.Errorf("drivers: %w", err)
	}
	intervals, err := parseIntList(*intervalsStr)
	if err != nil {
		return fmt.Errorf("intervals-ms: %w", err)
	}
	if *ppd <= 0 {
		return fmt.Errorf("points-per-driver must be > 0")
	}

	var cells []cell
	for _, d := range drivers {
		for _, iv := range intervals {
			cells = append(cells, modelCell(d, iv, *ppd))
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].TotalPoints != cells[j].TotalPoints {
			return cells[i].TotalPoints < cells[j].TotalPoints
		}
		return cells[i].SamplesPerSec < cells[j].SamplesPerSec
	})

	adv := advisory(cells)
	rep := report{
		GeneratedUTC: time.Now().UTC().Format(time.RFC3339),
		Assumptions: []string{
			fmt.Sprintf("baseRSSMB=%.1f perDriverRSSMB=%.1f perPointRSSKB=%.2f", baseRSSMB, perDriverRSSMB, perPointRSSKB),
			fmt.Sprintf("cpuNsPerSample=%.0f bytesPerSample=%.0f", cpuNsPerSample, bytesPerSample),
			"Model is synthetic/advisory — not measured on target ARM silicon.",
			"L51 design headroom (~1e6 points/site) is a matrix target, not a pass/fail gate.",
			"Locked sizing numbers require Yuri/architect review after real D10 runs.",
		},
		Cells:    cells,
		Advisory: adv,
	}

	if err := os.MkdirAll(*artifacts, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*artifacts, "raw.json"), raw, 0o644); err != nil {
		return err
	}
	md := renderMarkdown(rep)
	if err := os.WriteFile(filepath.Join(*artifacts, "REPORT.md"), []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("gateway-bench: wrote %s (%d cells)\n", filepath.Join(*artifacts, "REPORT.md"), len(cells))
	fmt.Println("ADVISORY:", adv)
	return nil
}

func modelCell(drivers, intervalMS, ppd int) cell {
	if intervalMS <= 0 {
		intervalMS = 1000
	}
	hz := 1000.0 / float64(intervalMS)
	totalPts := drivers * ppd
	sps := float64(totalPts) * hz
	cpu := sps * cpuNsPerSample / 1e9
	rss := baseRSSMB + float64(drivers)*perDriverRSSMB + float64(totalPts)*perPointRSSKB/1024.0
	walDay := sps * bytesPerSample * 86400.0 / (1024.0 * 1024.0)
	return cell{
		Drivers:         drivers,
		IntervalMS:      intervalMS,
		PointsPerDriver: ppd,
		TotalPoints:     totalPts,
		SamplesPerSec:   sps,
		CPUCores:        cpu,
		RSSMB:           rss,
		WALMBPerDay:     walDay,
		StorageMBDay:    walDay * 1.15, // slight index overhead
	}
}

func advisory(cells []cell) string {
	// Recommend smallest matrix cell where CPU > 1.0 core OR RSS > 2 GiB
	// OR total points ≥ 1e6 (L51 headroom touch).
	type hit struct {
		c   cell
		why string
	}
	var hits []hit
	for _, c := range cells {
		if c.CPUCores >= 1.0 {
			hits = append(hits, hit{c, fmt.Sprintf("cpu_cores_est≥1.0 (%.2f)", c.CPUCores)})
		} else if c.RSSMB >= 2048 {
			hits = append(hits, hit{c, fmt.Sprintf("rss_mb_est≥2048 (%.0f)", c.RSSMB)})
		} else if c.TotalPoints >= 1_000_000 {
			hits = append(hits, hit{c, "total_points≥1e6 (L51 headroom)"})
		}
	}
	if len(hits) == 0 {
		return "ADVISORY: within this matrix, no cell crossed cpu≥1 / rss≥2GiB / 1e6-points; expand --drivers or tighten --intervals-ms before locking D9 sizing."
	}
	// Prefer lowest total_points hit
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].c.TotalPoints < hits[j].c.TotalPoints
	})
	h := hits[0]
	return fmt.Sprintf(
		"ADVISORY: first pressure at drivers=%d interval_ms=%d total_points=%d samples/s=%.0f — %s. Not locked; measure on target ARM before D9 BOM.",
		h.c.Drivers, h.c.IntervalMS, h.c.TotalPoints, h.c.SamplesPerSec, h.why,
	)
}

func renderMarkdown(rep report) string {
	var b strings.Builder
	b.WriteString("# Pod Gateway Resource Benchmark Report (PRMT-210 / D10)\n\n")
	b.WriteString(fmt.Sprintf("Generated: %s (UTC)\n\n", rep.GeneratedUTC))
	b.WriteString("## Assumptions (synthetic model)\n\n")
	for _, a := range rep.Assumptions {
		b.WriteString("- " + a + "\n")
	}
	b.WriteString("\n## Matrix\n\n")
	b.WriteString("| drivers | interval_ms | points/driver | total_points | samples/s | cpu_cores_est | rss_mb_est | wal_mb/day | storage_mb/day |\n")
	b.WriteString("|--------:|------------:|--------------:|-------------:|----------:|--------------:|-----------:|-----------:|---------------:|\n")
	for _, c := range rep.Cells {
		b.WriteString(fmt.Sprintf("| %d | %d | %d | %d | %.1f | %.3f | %.1f | %.1f | %.1f |\n",
			c.Drivers, c.IntervalMS, c.PointsPerDriver, c.TotalPoints, c.SamplesPerSec,
			c.CPUCores, c.RSSMB, c.WALMBPerDay, c.StorageMBDay))
	}
	b.WriteString("\n## Advisory (not locked)\n\n")
	b.WriteString(rep.Advisory + "\n")
	b.WriteString("\n> Numbers are model estimates for D9 sizing discussion. Yuri locks BOM after ARM evidence.\n")
	return b.String()
}

func parseIntList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("value %d must be > 0", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}
