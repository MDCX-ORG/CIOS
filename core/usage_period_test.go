package core

import (
	"testing"
	"time"
)

func TestPreviousCalendarMonth_UTC(t *testing.T) {
	// 2026-07-13 → previous month June 2026 UTC
	now := time.Date(2026, 7, 13, 15, 0, 0, 0, time.UTC)
	start, end := previousCalendarMonth(now, time.UTC)
	if !start.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v", end)
	}
}

func TestPreviousCalendarMonth_AsiaSingapore(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-01 00:30 SGT is still Feb in UTC for some hours, but
	// In(loc) month is March → previous = February SGT.
	now := time.Date(2026, 3, 1, 0, 30, 0, 0, loc)
	start, end := previousCalendarMonth(now, loc)
	wantStart := time.Date(2026, 2, 1, 0, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	if !start.Equal(wantStart) {
		t.Fatalf("start = %v want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Fatalf("end = %v want %v", end, wantEnd)
	}
}

func TestLocationFromSiteSpec(t *testing.T) {
	if locationFromSiteSpec(nil) != time.UTC {
		t.Fatal("nil → UTC")
	}
	if locationFromSiteSpec(map[string]any{}) != time.UTC {
		t.Fatal("empty → UTC")
	}
	if locationFromSiteSpec(map[string]any{"timezone": "Not/AZone"}) != time.UTC {
		t.Fatal("invalid → UTC")
	}
	loc := locationFromSiteSpec(map[string]any{"timezone": "Asia/Singapore"})
	if loc.String() != "Asia/Singapore" {
		t.Fatalf("got %s", loc)
	}
}

func TestSiteLocationMap(t *testing.T) {
	assets := []Asset{
		{Path: "sgp01", Spec: map[string]any{"type": "site", "timezone": "Asia/Singapore"}},
		{Path: "sgp01.pod000.rack001", Spec: map[string]any{"type": "rack"}},
		{Path: "fra01", Spec: map[string]any{"type": "site"}}, // no tz → UTC
	}
	m := siteLocationMap(assets)
	if m["sgp01"].String() != "Asia/Singapore" {
		t.Fatalf("sgp01 = %v", m["sgp01"])
	}
	if locationForSite(m, "fra01") != time.UTC {
		t.Fatalf("fra01 want UTC got %v", m["fra01"])
	}
	if locationForSite(m, "missing") != time.UTC {
		t.Fatal("missing → UTC")
	}
}
