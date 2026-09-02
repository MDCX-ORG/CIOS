// Package core — usage_scan.go: scheduled Usage recompute (PRMT-197/198).
// Opt-in via RunUsageScanner(interval>0). Each tick materializes:
//   - previous complete UTC day (UsageDaily)
//   - previous complete calendar month per site timezone (UsageMonthly)
//
// rack_hour from CMDB + energy from VM (cios_energy_kwh increase,
// fallback power→kWh). Fail-soft per asset.
package core

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strconv"
	"time"
)

// usageJSPublisher is the minimal publish surface for NATSUsageEventSink
// (avoids importing nats.go into every core test).
type usageJSPublisher interface {
	Publish(subject string, data []byte) error
}

// NATSUsageEventSink publishes each upserted UsageRecord as JSON to
// Subject (default "cios.usage.upserted").
type NATSUsageEventSink struct {
	Pub     usageJSPublisher
	Subject string
}

// OnUsageUpserted implements UsageEventSink.
func (n NATSUsageEventSink) OnUsageUpserted(_ context.Context, rec UsageRecord) {
	if n.Pub == nil {
		return
	}
	subj := n.Subject
	if subj == "" {
		subj = "cios.usage.upserted"
	}
	b, err := json.Marshal(rec)
	if err != nil {
		log.Printf("core: usage nats sink: marshal: %v", err)
		return
	}
	if err := n.Pub.Publish(subj, b); err != nil {
		log.Printf("core: usage nats sink: publish: %v", err)
	}
}

// RunUsageScanner ticks on interval. interval<=0 disables (returns
// immediately) — same opt-in pattern as reconcile.
func (s *Server) RunUsageScanner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	safeTick("usage", func() { s.scanUsageTick(ctx, time.Now().UTC()) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("usage", func() { s.scanUsageTick(ctx, now.UTC()) })
		}
	}
}

// previousUTCDay returns [start, end) for the last complete UTC day
// relative to now (end = today 00:00 UTC, start = yesterday 00:00).
func previousUTCDay(now time.Time) (start, end time.Time) {
	now = now.UTC()
	end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start = end.Add(-24 * time.Hour)
	return start, end
}

// scanUsageTick computes daily (previous UTC day) + monthly
// (previous calendar month, per-site timezone) usage facts.
func (s *Server) scanUsageTick(ctx context.Context, now time.Time) {
	var tickErr error
	defer func() {
		s.recordScanner("usage", now, tickErr)
	}()
	ok, release, err := s.st.TryScannerLock(ctx, "usage")
	if err != nil {
		log.Printf("core: usage scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()

	assets, err := s.st.ListAssets(ctx)
	if err != nil {
		log.Printf("core: usage scanner: list assets: %v", err)
		tickErr = err
		return
	}

	var computed []UsageRecord

	// Daily: previous complete UTC day (all assets, one window).
	dStart, dEnd := previousUTCDay(now)
	computed = append(computed, s.computeUsageWindow(ctx, assets, dStart, dEnd, UsageDaily)...)

	// Monthly: previous complete calendar month. Group assets by
	// site so each site's Spec.timezone selects the month bounds.
	bySite := map[string][]Asset{}
	for _, a := range assets {
		sid := siteIDFromPath(a.Path)
		if sid == "" {
			sid = "_"
		}
		bySite[sid] = append(bySite[sid], a)
	}
	locs := siteLocationMap(assets)
	// Deduplicate month windows that resolve to the same [start,end)
	// so multi-site UTC deployments compute energy once per window
	// per asset set is fine (per-site asset slices already disjoint).
	for sid, siteAssets := range bySite {
		loc := locationForSite(locs, sid)
		mStart, mEnd := previousCalendarMonth(now, loc)
		computed = append(computed, s.computeUsageWindow(ctx, siteAssets, mStart, mEnd, UsageMonthly)...)
	}

	sink := s.usageSink
	if sink == nil {
		sink = NoopUsageEventSink{}
	}
	for _, rec := range computed {
		enriched, eerr := EnrichUsageIdentity(ctx, s.st, rec)
		if eerr != nil {
			log.Printf("core: usage scanner: enrich: %v", eerr)
			tickErr = eerr
			continue
		}
		saved, uerr := s.st.UpsertUsage(ctx, enriched)
		if uerr != nil {
			log.Printf("core: usage scanner: upsert: %v", uerr)
			tickErr = uerr
			continue
		}
		sink.OnUsageUpserted(ctx, saved)
	}
}

// computeUsageWindow runs rack_hour + energy compute for one period.
func (s *Server) computeUsageWindow(ctx context.Context, assets []Asset, start, end time.Time, g UsageGranularity) []UsageRecord {
	var out []UsageRecord
	out = append(out, ComputeRackHourUsage(assets, start, end, g)...)
	ms := s.fetchEnergyMeasurements(ctx, assets, start, end)
	if len(ms) > 0 {
		out = append(out, ComputeEnergyUsage(ms, start, end, g)...)
	}
	return out
}

// fetchEnergyMeasurements builds kWh deltas per asset for [start,end).
func (s *Server) fetchEnergyMeasurements(ctx context.Context, assets []Asset, start, end time.Time) []Measurement {
	if s.vmURL == "" {
		return nil
	}
	hours := end.Sub(start).Hours()
	if hours <= 0 {
		return nil
	}
	window := strconv.Itoa(int(hours+0.5)) + "h"
	if window == "0h" {
		window = "24h"
	}
	mid := start.Add(end.Sub(start) / 2)
	out := make([]Measurement, 0)
	seen := map[string]struct{}{}
	for _, a := range assets {
		path := a.Path
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		kwh, ok := s.fetchEnergyKWhIncrease(ctx, path, window)
		if !ok {
			kwh, ok = s.fetchPowerAvgAsKWh(ctx, path, window, hours)
		}
		if !ok || kwh <= 0 {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, Measurement{
			AssetPath: path,
			Time:      mid,
			Quantity:  kwh,
			Unit:      "kWh",
		})
	}
	return out
}

func (s *Server) fetchEnergyKWhIncrease(ctx context.Context, assetPath, window string) (float64, bool) {
	query := `increase(cios_energy_kwh{asset_path="` + escapeLabelValue(assetPath) + `"}[` + window + `])`
	return s.fetchVMScalar(ctx, query)
}

func (s *Server) fetchPowerAvgAsKWh(ctx context.Context, assetPath, window string, hours float64) (float64, bool) {
	query := `avg_over_time(cios_power_watt{asset_path="` + escapeLabelValue(assetPath) + `"}[` + window + `])`
	watts, ok := s.fetchVMScalar(ctx, query)
	if !ok {
		return 0, false
	}
	return watts * hours / 1000.0, true
}

func (s *Server) fetchVMScalar(ctx context.Context, query string) (float64, bool) {
	q := url.Values{}
	q.Set("query", query)
	body, err := s.fetchVM(ctx, s.vmURL+"/api/v1/query", q)
	if err != nil {
		return 0, false
	}
	var vresp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		return 0, false
	}
	if vresp.Status != "success" || len(vresp.Data.Result) == 0 {
		return 0, false
	}
	val := vresp.Data.Result[0].Value
	if len(val) < 2 {
		return 0, false
	}
	str, ok := val[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
