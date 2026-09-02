package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelCell_Monotonic(t *testing.T) {
	a := modelCell(10, 1000, 64)
	b := modelCell(100, 1000, 64)
	if b.CPUCores <= a.CPUCores {
		t.Fatalf("cpu should rise with drivers: a=%.4f b=%.4f", a.CPUCores, b.CPUCores)
	}
	if b.RSSMB <= a.RSSMB {
		t.Fatalf("rss should rise with drivers: a=%.1f b=%.1f", a.RSSMB, b.RSSMB)
	}
	if b.TotalPoints != 100*64 {
		t.Fatalf("total points: got %d", b.TotalPoints)
	}
}

func TestParseIntList(t *testing.T) {
	got, err := parseIntList("10, 20,30")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 10 || got[2] != 30 {
		t.Fatalf("got %v", got)
	}
	if _, err := parseIntList(""); err == nil {
		t.Fatal("empty should error")
	}
}

func TestRun_CheckOnly(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"--check-only", "--artifacts", dir}); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(dir, "REPORT.md")
	raw := filepath.Join(dir, "raw.json")
	if _, err := os.Stat(md); err != nil {
		t.Fatalf("missing REPORT.md: %v", err)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Fatalf("missing raw.json: %v", err)
	}
	b, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ADVISORY") {
		t.Fatalf("report missing ADVISORY: %s", b)
	}
}

func TestAdvisory_FindsPressure(t *testing.T) {
	// Force a cell past 1e6 points.
	cells := []cell{modelCell(20000, 1000, 64)}
	s := advisory(cells)
	if !strings.Contains(s, "ADVISORY") {
		t.Fatalf("got %q", s)
	}
}
