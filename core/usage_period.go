// Package core — usage_period.go: calendar period helpers for Usage
// scanner (PRMT-198 / L102). Daily = previous UTC day; monthly =
// previous complete calendar month in site timezone (IANA Spec
// "timezone", fallback UTC).
package core

import (
	"strings"
	"time"
)

// previousCalendarMonth returns [start, end) of the last complete
// calendar month relative to now, in loc (use time.UTC when unknown).
// end = first instant of the current month in loc; start = first
// instant of the previous month in loc. Both are absolute instants
// (location embedded).
func previousCalendarMonth(now time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	// First day of this month 00:00 in loc.
	end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	start = end.AddDate(0, -1, 0)
	return start, end
}

// locationFromSiteSpec reads Spec["timezone"] as IANA name.
// Empty / invalid → time.UTC (fail-soft; never panics).
func locationFromSiteSpec(spec map[string]any) *time.Location {
	if spec == nil {
		return time.UTC
	}
	raw, _ := spec["timezone"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return time.UTC
	}
	return loc
}

// siteLocationMap builds siteID → *time.Location from assets.
// Prefers type=site (or path with no dots) Spec.timezone; later
// assets for the same site do not override an already-set non-UTC
// location only if first-wins for any set timezone string.
func siteLocationMap(assets []Asset) map[string]*time.Location {
	out := map[string]*time.Location{}
	for _, a := range assets {
		site := siteIDFromPath(a.Path)
		if site == "" {
			continue
		}
		// Prefer the site-root asset path (single segment).
		if a.Path != site && a.Path != site+"." {
			// Still allow timezone from nested assets only if site unknown.
			if _, ok := out[site]; ok {
				continue
			}
		}
		if a.Spec == nil {
			continue
		}
		if _, ok := a.Spec["timezone"].(string); !ok {
			// If this is not a site root and no timezone, skip.
			if a.Path != site {
				continue
			}
		}
		out[site] = locationFromSiteSpec(a.Spec)
	}
	return out
}

// locationForSite returns loc for siteID from map, else UTC.
func locationForSite(m map[string]*time.Location, siteID string) *time.Location {
	if m != nil {
		if loc, ok := m[siteID]; ok && loc != nil {
			return loc
		}
	}
	return time.UTC
}
