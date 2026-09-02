// Package core — usage.go: Measurement / UsageRecord domain + pure
// compute for Commercial Platform MVP (E3.2 / L102 / PRMT-192 /
// protocol/spec-010-metering-usage.md v0.1).
//
// Scope: types, ID generation, pure rack_hour / energy compute, and
// the ErrUsagePGNotImplemented sentinel for the pgStore stub. Store
// interface methods + fileStore persistence live in store.go; HTTP
// arrives in PRMT-193. No money / invoice / Pricing / Cost fields.
package core

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"time"
)

// UsageKind is the usage-fact kind (spec-010 §2.1). MVP: energy +
// rack_hour only (L102; gpu_hour is out of scope).
type UsageKind string

const (
	UsageKindEnergy   UsageKind = "energy"
	UsageKindRackHour UsageKind = "rack_hour"
)

// UsageGranularity is the rollup grain (spec-010 §2.1).
type UsageGranularity string

const (
	UsageDaily   UsageGranularity = "daily"
	UsageMonthly UsageGranularity = "monthly"
)

// Measurement is a meter observation (spec-010). Not priced.
type Measurement struct {
	AssetPath string
	Time      time.Time
	Quantity  float64 // for energy: kWh delta in the sample interval
	Unit      string  // must be "kWh" for the energy path in MVP
}

// UsageRecord is a normalized usage fact (spec-010 §2.1).
type UsageRecord struct {
	ID          string           `json:"id"`
	Kind        UsageKind        `json:"kind"`
	TenantID    string           `json:"tenant_id"`
	OrgID       string           `json:"org_id,omitempty"`
	SiteID      string           `json:"site_id"`
	AssetPath   string           `json:"asset_path"`
	PeriodStart time.Time        `json:"period_start"`
	PeriodEnd   time.Time        `json:"period_end"`
	Granularity UsageGranularity `json:"granularity"`
	Quantity    float64          `json:"quantity"`
	Unit        string           `json:"unit"`
}

// UsageListFilter for ListUsage.
type UsageListFilter struct {
	TenantID    string
	SiteID      string
	Kind        UsageKind        // empty = all
	Granularity UsageGranularity // empty = all
	// Period overlap: records with PeriodStart < PeriodEndFilter &&
	// PeriodEnd > PeriodStartFilter. Zero times are open bounds.
	PeriodStart time.Time // zero = open
	PeriodEnd   time.Time // zero = open
}

// ErrUsagePGNotImplemented is retained for tests that still probe the
// sentinel; pgStore (PRMT-195) implements real Upsert/List against
// migrations/018_usage.sql and no longer returns this error.
var ErrUsagePGNotImplemented = errors.New("core: usage: postgres backend not implemented")

// UsageEventSink is notified after a successful Usage upsert (compute
// path). Nil-safe via NoopUsageEventSink. Commercial/Forecast may
// subscribe later (L102 H2).
type UsageEventSink interface {
	OnUsageUpserted(ctx context.Context, rec UsageRecord)
}

// NoopUsageEventSink discards events.
type NoopUsageEventSink struct{}

// OnUsageUpserted implements UsageEventSink.
func (NoopUsageEventSink) OnUsageUpserted(context.Context, UsageRecord) {}

// newUsageID produces "us_" + 16 base32 chars (no padding). Mirror of
// newOrgID / newTicketID entropy scheme (10 random bytes → 16 chars).
func newUsageID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "us_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// usageNaturalKey identifies a usage fact for idempotent recompute.
func usageNaturalKey(r UsageRecord) string {
	return string(r.Kind) + "|" + r.AssetPath + "|" +
		r.PeriodStart.UTC().Format(time.RFC3339Nano) + "|" +
		r.PeriodEnd.UTC().Format(time.RFC3339Nano) + "|" +
		string(r.Granularity)
}

// EnrichUsageIdentity fills OrgID/TenantID from SiteOrg+Org when empty.
// SiteID is taken from rec.SiteID or the first segment of AssetPath.
func EnrichUsageIdentity(ctx context.Context, st Store, rec UsageRecord) (UsageRecord, error) {
	if rec.SiteID == "" {
		rec.SiteID = siteIDFromPath(rec.AssetPath)
	}
	if rec.SiteID == "" {
		return rec, nil
	}
	so, ok, err := st.GetSiteOrg(ctx, rec.SiteID)
	if err != nil {
		return rec, err
	}
	if !ok || so.OrgID == "" {
		return rec, nil
	}
	if rec.OrgID == "" {
		rec.OrgID = so.OrgID
	}
	if rec.TenantID == "" {
		org, ook, oerr := st.GetOrg(ctx, so.OrgID)
		if oerr != nil {
			return rec, oerr
		}
		if ook {
			rec.TenantID = org.TenantID
		}
	}
	return rec, nil
}

// siteIDFromPath returns the first path segment (before '.') or the
// whole path when there is no separator. Spec-010: site_id is the
// first path segment or explicit.
func siteIDFromPath(path string) string {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return path
}

// ComputeRackHourUsage: for each Asset where Spec["type"]=="rack"
// and Spec["lifecycle"]=="active", emit one UsageRecord for the full
// period with Quantity = end.Sub(start).Hours(), Unit "h". SiteID =
// first path segment; TenantID/OrgID left "". ID empty (caller Upsert
// fills). Granularity as passed.
func ComputeRackHourUsage(assets []Asset, start, end time.Time, g UsageGranularity) []UsageRecord {
	hours := end.Sub(start).Hours()
	out := make([]UsageRecord, 0)
	for _, a := range assets {
		if a.Spec == nil {
			continue
		}
		typ, _ := a.Spec["type"].(string)
		lc, _ := a.Spec["lifecycle"].(string)
		if typ != "rack" || lc != "active" {
			continue
		}
		out = append(out, UsageRecord{
			Kind:        UsageKindRackHour,
			SiteID:      siteIDFromPath(a.Path),
			AssetPath:   a.Path,
			PeriodStart: start,
			PeriodEnd:   end,
			Granularity: g,
			Quantity:    hours,
			Unit:        "h",
		})
	}
	return out
}

// ComputeEnergyUsage: group measurements by AssetPath where Unit=="kWh".
// Quantity = sum of Measurement.Quantity in [start,end) (each sample
// is a kWh delta). Skip non-kWh. Same site/tenant defaults as rack.
func ComputeEnergyUsage(ms []Measurement, start, end time.Time, g UsageGranularity) []UsageRecord {
	sums := map[string]float64{}
	// Preserve first-seen order for deterministic emission.
	order := make([]string, 0)
	for _, m := range ms {
		if m.Unit != "kWh" {
			continue
		}
		// Half-open window [start, end).
		if m.Time.Before(start) || !m.Time.Before(end) {
			continue
		}
		if _, ok := sums[m.AssetPath]; !ok {
			order = append(order, m.AssetPath)
		}
		sums[m.AssetPath] += m.Quantity
	}
	out := make([]UsageRecord, 0, len(order))
	for _, path := range order {
		out = append(out, UsageRecord{
			Kind:        UsageKindEnergy,
			SiteID:      siteIDFromPath(path),
			AssetPath:   path,
			PeriodStart: start,
			PeriodEnd:   end,
			Granularity: g,
			Quantity:    sums[path],
			Unit:        "kWh",
		})
	}
	return out
}
