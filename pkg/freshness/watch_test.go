package freshness

import (
	"testing"
	"time"
)

func TestWatch_GapsAndFresh(t *testing.T) {
	w := New(10 * time.Minute)
	t0 := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	w.Touch("sgp01.pod000.cdu000", t0)
	w.Touch("sgp01.pod001.cdu000", t0)

	// Still fresh at +5m
	if gaps := w.Gaps(t0.Add(5 * time.Minute)); len(gaps) != 0 {
		t.Fatalf("gaps=%+v", gaps)
	}
	if n := len(w.Fresh(t0.Add(5 * time.Minute))); n != 2 {
		t.Fatalf("fresh=%d", n)
	}

	// Stale at +11m
	gaps := w.Gaps(t0.Add(11 * time.Minute))
	if len(gaps) != 2 {
		t.Fatalf("gaps=%+v", gaps)
	}
	if gaps[0].AssetPath != "sgp01.pod000.cdu000" {
		t.Fatalf("order %+v", gaps)
	}

	// One asset recovers
	w.Touch("sgp01.pod000.cdu000", t0.Add(12*time.Minute))
	gaps = w.Gaps(t0.Add(12 * time.Minute))
	if len(gaps) != 1 || gaps[0].AssetPath != "sgp01.pod001.cdu000" {
		t.Fatalf("gaps after recover=%+v", gaps)
	}
}

func TestWatch_DefaultStale(t *testing.T) {
	w := New(0)
	if w.StaleAfter() != DefaultStaleAfter {
		t.Fatal(w.StaleAfter())
	}
}
