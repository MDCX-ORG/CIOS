// Command cios-rules is the M1 derived-quantity calculation
// service. On each interval it asks VictoriaMetrics for the
// current value of every input quantity a derived formula needs,
// groups the returned series into per-(asset × location-prefix)
// buckets, runs Compute() on each bucket that has all its inputs,
// and POSTs the resulting promtext rows back to VM as
// derived-point samples.
//
// The service owns ONE loop: discover → bucket → compute → write.
// There is no NATS, no PG, no in-process state across ticks; the
// whole pipeline is a pure function of (current VM state, the
// compiled derived set). That property is what keeps the per-tick
// hot path lock-free and makes the missing-input policy
// (PRMT-021 §2-bis #3: "skip the bucket, do not emit a zero")
// trivially correct.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/promproj"
	"github.com/yurimeng/cios/pkg/rules"
)

func main() {
	vmURL := flag.String("vm-url", "", "VictoriaMetrics base URL (required)")
	protocolDir := flag.String("protocol-dir", "", "protocol/ directory containing quantities.yaml (required)")
	interval := flag.Duration("interval", 30*time.Second, "calculation period")
	site := flag.String("site", "", "site filter (empty = all sites)")
	flag.Parse()

	if *vmURL == "" || *protocolDir == "" {
		fmt.Fprintln(os.Stderr, "cios-rules: -vm-url and -protocol-dir are required")
		os.Exit(2)
	}

	derived, err := rules.LoadDerived(*protocolDir)
	if err != nil {
		log.Fatalf("cios-rules: load derived: %v", err)
	}
	if len(derived) == 0 {
		log.Printf("cios-rules: warning: no in-scope derived quantities; daemon will idle until quantities.yaml is extended")
	}

	dict, err := cpath.LoadDict(*protocolDir)
	if err != nil {
		log.Fatalf("cios-rules: load dict: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First tick fires immediately so a fresh start produces
	// derived points without waiting a full interval. Subsequent
	// ticks fire on the ticker schedule.
	if err := runOnce(ctx, derived, dict, *vmURL, *site, time.Now()); err != nil {
		log.Printf("cios-rules: initial tick: %v", err)
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("cios-rules: signal received, shutting down")
			return
		case tick := <-t.C:
			if err := runOnce(ctx, derived, dict, *vmURL, *site, tick); err != nil {
				log.Printf("cios-rules: tick %v: %v", tick.Format(time.RFC3339), err)
			}
		}
	}
}

// --- per-tick pipeline ------------------------------------------------------

// runOnce executes one full pipeline pass: for every in-scope
// Derived, discover its input series, bucket them, compute, and
// write back. Errors at the per-derived level are logged and
// skipped so one bad formula can't poison the whole tick
// (PRMT-021 §4.4 fail-soft).
func runOnce(ctx context.Context, derived []rules.Derived, dict *cpath.Dict, vmURL, site string, now time.Time) error {
	var allLines []string
	for _, d := range derived {
		lines, err := processOne(ctx, d, dict, vmURL, site, now)
		if err != nil {
			log.Printf("cios-rules: %s: %v", d.Name, err)
			continue
		}
		allLines = append(allLines, lines...)
	}
	if len(allLines) > 0 {
		if err := postImport(ctx, vmURL, allLines); err != nil {
			return fmt.Errorf("import: %w", err)
		}
		log.Printf("cios-rules: wrote %d derived sample(s)", len(allLines))
	}
	return nil
}

// processOne runs the PRMT-021 §4.4 (a)-(d) sub-pipeline for one
// derived quantity:
//
//	(a) discover: VM instant query for each input quantity
//	    listed in d.Formula.Refs();
//	(b) bucket: suffix-match each returned series against the
//	    ref list, group by (assetPath, locPrefix), so a single
//	    cdu's fws and tcs loops end up in distinct buckets;
//	(c) compute: for each bucket with every ref present, run
//	    rules.Compute;
//	(d) project: turn the resulting (pointPath, value) into a
//	    promtext line, identical in shape to what gateway/pipeline
//	    emits (label order, metric name format, ts_ms suffix).
//
// Steps (a) and (d) reuse pkg/promproj for naming/label
// conventions so the rules engine and the gateway emit the same
// wire format. (d) calls promproj.Render directly — since PRMT-024
// MetricName/Render see dict.Derived as well as dict.Quantities,
// so the once-private renderDerived helper is no longer needed
// (PRMT-021 §9 F1 closed).
func processOne(ctx context.Context, d rules.Derived, dict *cpath.Dict, vmURL, site string, now time.Time) ([]string, error) {
	// (a) Discover: one VM instant query per unique input quantity.
	// d.Formula.Refs() returns identifiers like "return.temp" —
	// the LAST segment of each is the quantity (per spec-002 §3
	// dotted-point convention). Empty ref list = constant formula
	// with no inputs to discover; nothing to compute per tick.
	refs := d.Formula.Refs()
	quantities := uniqueQuantities(refs)
	if len(quantities) == 0 {
		log.Printf("cios-rules: %s: constant formula, nothing to compute per tick", d.Name)
		return nil, nil
	}
	series, err := discoverInputs(ctx, dict, vmURL, d.Hosts, quantities, site)
	if err != nil {
		return nil, err
	}

	// (b) Bucket: for every returned series, suffix-match its
	// rel-point against refs and drop the matched suffix to
	// recover the locPrefix. Group by (assetPath, locPrefix).
	buckets := bucketByPrefix(series, refs, dict)

	// (c) + (d) Compute and project.
	var lines []string
	for key, inputs := range buckets {
		pointPath, value, err := rules.Compute(d, key.assetPath, key.locPrefix, inputs)
		if err != nil {
			// ErrMissingInput shouldn't occur here (the
			// bucket is built from "all refs present"), but
			// other eval errors (div-by-zero etc.) can.
			log.Printf("cios-rules: %s: bucket %s/%s: %v", d.Name, key.assetPath, key.locPrefix, err)
			continue
		}
		p, err := dict.ParsePoint(pointPath)
		if err != nil {
			log.Printf("cios-rules: %s: parse %s: %v", d.Name, pointPath, err)
			continue
		}
		line, err := promproj.Render(p, value, now, "good", dict)
		if err != nil {
			log.Printf("cios-rules: %s: render %s: %v", d.Name, pointPath, err)
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// --- (a) discovery ----------------------------------------------------------

// vmSeries is one row of a VM instant-query response: the original
// label set plus the numeric value at the queried instant. We
// don't carry the timestamp — by the time we render, we're
// stamping "now" so all derived samples in one tick share a
// timestamp (good for "what was true at this instant" UI
// presentations).
type vmSeries struct {
	Labels map[string]string
	Value  float64
}

// vmQueryResp is the subset of the Prometheus /api/v1/query JSON
// shape we read. Unused fields are dropped so the unmarshal stays
// tolerant of VM extensions.
type vmQueryResp struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"` // [<ts float>, "<v string>"]
		} `json:"result"`
	} `json:"data"`
}

// discoverInputs runs one VM instant query per input quantity and
// returns the raw series. The selector is
// `metric{asset_type=~"<hosts>"[, site="<site>"]}` — same shape
// core /v1/points uses, so an operator tracing "where did this
// number come from" can paste the same URL into VM UI.
func discoverInputs(ctx context.Context, dict *cpath.Dict, vmURL string, hosts, quantities []string, site string) ([]vmSeries, error) {
	hostFilter := strings.Join(hosts, "|")
	var out []vmSeries
	for _, q := range quantities {
		metric, err := promproj.MetricName(q, dict)
		if err != nil {
			// Unknown input quantity in the dict — likely a
			// formula reference typo. Skip this input;
			// downstream bucketing will treat its series as
			// missing and naturally drop affected buckets.
			log.Printf("cios-rules: input quantity %q: %v", q, err)
			continue
		}
		sel := buildSelector(metric, hostFilter, site)
		body, err := vmInstantQuery(ctx, vmURL, sel)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", sel, err)
		}
		var resp vmQueryResp
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parse %s: %w", sel, err)
		}
		for _, r := range resp.Data.Result {
			if len(r.Value) < 2 {
				continue
			}
			vStr, ok := r.Value[1].(string)
			if !ok {
				continue
			}
			v, err := strconv.ParseFloat(vStr, 64)
			if err != nil {
				continue
			}
			out = append(out, vmSeries{Labels: r.Metric, Value: v})
		}
	}
	return out, nil
}

// buildSelector assembles the PromQL selector for a one-quantity
// instant query. The label-order doesn't matter for an instant
// query; we keep {asset_type, site} in that order to match
// promproj's preference.
func buildSelector(metric, hostFilter, site string) string {
	var b strings.Builder
	b.WriteString(metric)
	b.WriteByte('{')
	b.WriteString(`asset_type=~"`)
	b.WriteString(hostFilter)
	b.WriteByte('"')
	if site != "" {
		b.WriteString(`,site="`)
		b.WriteString(site)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// vmInstantQuery is a tiny HTTP GET helper with a 10s timeout —
// VM's instant query is sub-second in practice, and a slow
// upstream is exactly the kind of thing we want to fail-soft on
// at the per-derived level (runOnce catches the error and skips
// the derived for this tick).
func vmInstantQuery(ctx context.Context, vmURL, query string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := vmURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// --- (b) bucketing ----------------------------------------------------------

// bucketKey is the dedup key (assetPath, locPrefix). assetPath is
// the value of the `path` label on the input series; locPrefix is
// the part of the rel-point BEFORE the matched ref (e.g. "fws"
// when rel-point is "fws.return.temp" and ref is "return.temp").
type bucketKey struct {
	assetPath string
	locPrefix string
}

// bucketByPrefix suffix-matches every series against the formula's
// refs and groups them into per-(assetPath, locPrefix) input maps.
//
// Suffix matching is "the rel-point equals the ref OR the rel-point
// ends with '.<ref>'". For `deltat = return.temp - supply.temp`,
// series rel-point "fws.return.temp" matches ref "return.temp"
// with locPrefix "fws"; rel-point "fws.return.temp.deg" would NOT
// match (ref would have to be "temp.deg" exactly). A rel-point
// that matches NO ref is dropped — it belongs to some other
// derived quantity.
//
// Refs containing dots (e.g. "return.temp") are matched as
// sub-strings; refs without dots (e.g. "status") match only when
// the rel-point is exactly the ref. The dedup is per-bucket so
// each ref is consumed at most once per series even if the
// matching is ambiguous — the first ref that wins in the loop
// below is the one we keep.
func bucketByPrefix(series []vmSeries, refs []string, dict *cpath.Dict) map[bucketKey]map[string]float64 {
	out := map[bucketKey]map[string]float64{}
	for _, s := range series {
		rel, err := promproj.RelPoint(s.Labels, dict)
		if err != nil {
			// Series whose metric isn't in the dict (or is missing
			// __name__) can't be bucketed; the old buildRelPoint
			// would produce a malformed rel that matchRef would
			// reject anyway. Skipping here is the byte-equivalent
			// outcome and surfaces nothing to the operator log
			// (matches the prior silent-drop behavior).
			continue
		}
		assetPath := s.Labels["path"]
		if assetPath == "" {
			continue
		}
		matchedRef, locPrefix := matchRef(rel, refs)
		if matchedRef == "" {
			// Series doesn't match any ref in this
			// formula. Likely belongs to a different
			// derived quantity.
			continue
		}
		key := bucketKey{assetPath: assetPath, locPrefix: locPrefix}
		inputs, ok := out[key]
		if !ok {
			inputs = map[string]float64{}
			out[key] = inputs
		}
		// If two series in the same bucket both claim the
		// same ref, the later one wins. This shouldn't
		// happen for a well-formed VM (one series per
		// rel-point per asset), but the last-write-wins
		// policy matches Prometheus' own behavior on
		// duplicate samples.
		inputs[matchedRef] = s.Value
	}
	return out
}

// buildRelPoint / quantityFromMetric used to live here as private
// copies of the inverse projection. PRMT-024 collapsed them back
// into pkg/promproj (RelPoint / QuantityFromMetric) so the rules
// daemon and the gateway share one authority. bucketByPrefix above
// calls promproj.RelPoint directly.

// matchRef returns the formula ref that this rel-point suffix-
// matches, along with the locPrefix (rel-point minus the matched
// ref suffix). An empty string return means "no match".
func matchRef(rel string, refs []string) (string, string) {
	for _, r := range refs {
		if rel == r {
			return r, ""
		}
		if strings.HasSuffix(rel, "."+r) {
			return r, strings.TrimSuffix(rel, "."+r)
		}
	}
	return "", ""
}

// uniqueQuantities is a one-shot helper: from a ref list like
// ["return.temp", "supply.temp", "return.temp"] derive the unique
// set of quantities (last dotted segment). The ref list itself
// stays the bucketing key; the quantity set is what we send to
// VM. Sorted for stable test output.
func uniqueQuantities(refs []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range refs {
		q := lastSegment(r)
		if q == "" {
			continue
		}
		if _, ok := seen[q]; ok {
			continue
		}
		seen[q] = struct{}{}
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// lastSegment returns the last dot-separated piece. "return.temp"
// → "temp", "status" → "status", "" → "".
func lastSegment(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// --- (d) projection ---------------------------------------------------------
//
// PRMT-024 collapsed the renderDerived path back into promproj.Render:
// MetricName/Render now consult both dict.Quantities and dict.Derived,
// so a derived point's promtext line goes through the same single
// source as every other CIOS sample. The once-private helpers
// (renderDerived, buildDerivedLabels, reverseDomain, escapeLabel,
// the local kv mirror) are gone; processOne calls promproj.Render
// directly.

// --- write-back -------------------------------------------------------------

// postImport POSTs the assembled promtext lines to VM's import
// endpoint, mirroring the format gateway/pipeline uses
// (text/plain, one line per sample). Fail-soft per §4.4: a
// non-2xx is logged and the daemon continues to the next tick.
func postImport(ctx context.Context, vmURL string, lines []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(vmURL, "/") + "/api/v1/import/prometheus"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(strings.Join(lines, "\n")+"\n")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
