package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/promproj"
)

// minimalDict mirrors PRMT-020 main_test's: enough types/quantities
// for cpath.ParsePoint to accept the test paths we throw at it.
func minimalDict(t *testing.T) *cpath.Dict {
	t.Helper()
	return &cpath.Dict{
		Types: map[string]cpath.TypeDef{
			"cdu":  {Parents: []string{"pod"}, Level: cpath.LevelDevice},
			"site": {},
			"pod":  {Parents: []string{"site"}, Level: cpath.LevelDevice},
		},
		Quantities: map[string]cpath.QuantityDef{
			"temp": {Unit: "celsius"},
			"flow": {Unit: "lpm"},
			// PRMT-024: TestBuildRelPoint feeds cios_status to
			// promproj.RelPoint, which now errors on unknown
			// quantities (was best-effort fallback before). The
			// enum status entry restores the round-trip the test
			// asserts on (rel-point "fws.status" / "status") —
			// the assertions themselves are unchanged.
			"status": {Unit: "enum"},
		},
		// deltat lives in Derived (it's a derived quantity); it
		// still must parse through cpath.ParsePoint, which
		// enforces the host list (L48).
		Derived: map[string]cpath.QuantityDef{
			"deltat": {Unit: "celsius", Host: []string{"cdu", "chiller"}},
		},
		Loops:   map[string]bool{"fws": true, "tcs": true},
		Sides:   map[string]bool{"supply": true, "return": true},
		Domains: map[string][]string{"computing": {"pod", "cdu"}},
	}
}

// ---- (b) bucketing ---------------------------------------------------------

func TestBucketByPrefix_SeparateLoopsEmitSeparateBuckets(t *testing.T) {
	// spec-002 §9 / PRMT-021 §5 "分桶: 同一 CDU 的 fws/tcs 两回路
	// 各产出独立 deltat 点". One cdu has both fws and tcs loops,
	// each with supply.temp + return.temp. They must end up in
	// distinct buckets so the two deltats are written separately.
	series := []vmSeries{
		// cdu000 / fws / supply.temp = 30
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "site": "site01", "pod": "pod002", "cdu": "cdu000", "path": "site01.pod002.cdu000", "loop": "fws", "side": "supply", "asset_type": "cdu", "domain": "computing", "quality": "good"}, Value: 30},
		// cdu000 / fws / return.temp = 34
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "site": "site01", "pod": "pod002", "cdu": "cdu000", "path": "site01.pod002.cdu000", "loop": "fws", "side": "return", "asset_type": "cdu", "domain": "computing", "quality": "good"}, Value: 34},
		// cdu000 / tcs / supply.temp = 25
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "site": "site01", "pod": "pod002", "cdu": "cdu000", "path": "site01.pod002.cdu000", "loop": "tcs", "side": "supply", "asset_type": "cdu", "domain": "computing", "quality": "good"}, Value: 25},
		// cdu000 / tcs / return.temp = 31
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "site": "site01", "pod": "pod002", "cdu": "cdu000", "path": "site01.pod002.cdu000", "loop": "tcs", "side": "return", "asset_type": "cdu", "domain": "computing", "quality": "good"}, Value: 31},
	}
	refs := []string{"return.temp", "supply.temp"}
	buckets := bucketByPrefix(series, refs, minimalDict(t))
	if len(buckets) != 2 {
		t.Fatalf("want 2 buckets (fws + tcs), got %d: %+v", len(buckets), buckets)
	}
	// Verify each bucket has both refs.
	for key, inputs := range buckets {
		if len(inputs) != 2 {
			t.Fatalf("bucket %+v: want 2 inputs, got %d (%+v)", key, len(inputs), inputs)
		}
		if _, ok := inputs["return.temp"]; !ok {
			t.Fatalf("bucket %+v: missing return.temp", key)
		}
		if _, ok := inputs["supply.temp"]; !ok {
			t.Fatalf("bucket %+v: missing supply.temp", key)
		}
	}
	// And the locPrefix values must be distinct — this is the
	// specific failure mode the test is named for.
	prefixes := map[string]bool{}
	for key := range buckets {
		prefixes[key.locPrefix] = true
	}
	if !prefixes["fws"] || !prefixes["tcs"] {
		t.Fatalf("locPrefixes %v must contain both fws and tcs", prefixes)
	}
}

func TestBucketByPrefix_CrossLoopRefNotMatched(t *testing.T) {
	// "跨 loop ref（带 loop 段）不被命中而跳过". A formula like
	// "fws.x - tcs.y" carries loop-segment refs; series
	// "fws.fws.x" would NOT match the ref "fws.x" because
	// HasSuffix(".fws.x", ".fws.x") is true, but the locPrefix
	// it computes is "fws" (which is fine), while series
	// "tcs.tcs.y" matches "tcs.y" with locPrefix "tcs". The
	// real assertion is: a series whose rel-point DOES NOT
	// suffix-match any ref is silently dropped.
	series := []vmSeries{
		// fws.return.temp — matches "return.temp" with locPrefix "fws"
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "path": "site01.pod002.cdu000", "loop": "fws", "side": "return", "asset_type": "cdu"}, Value: 34},
		// fws.x — rel-point "fws.x" — does NOT suffix-match either ref
		{Labels: map[string]string{"__name__": "cios_temp_celsius", "path": "site01.pod002.cdu000", "loop": "fws", "side": "x", "asset_type": "cdu"}, Value: 99},
		// (also supply.temp missing → the bucket won't be complete)
	}
	refs := []string{"return.temp", "supply.temp"}
	buckets := bucketByPrefix(series, refs, minimalDict(t))
	// Only the "return.temp" series should have landed in
	// some bucket; "fws.x" is dropped. The bucket exists but
	// is incomplete (caller's processOne will skip it).
	if len(buckets) != 1 {
		t.Fatalf("want 1 bucket, got %d: %+v", len(buckets), buckets)
	}
	for key, inputs := range buckets {
		if v, ok := inputs["fws.x"]; ok {
			t.Fatalf("non-matching ref %q leaked into bucket %+v with value %v", "fws.x", key, v)
		}
	}
}

func TestMatchRef(t *testing.T) {
	cases := []struct {
		rel      string
		refs     []string
		wantRef  string
		wantPref string
	}{
		// Exact match: rel-point IS the ref, no prefix.
		{"status", []string{"status", "leak"}, "status", ""},
		// Suffix match: rel-point = "<loc>.<ref>".
		{"fws.return.temp", []string{"return.temp", "supply.temp"}, "return.temp", "fws"},
		{"tcs.supply.temp", []string{"return.temp", "supply.temp"}, "supply.temp", "tcs"},
		// Multi-segment prefix: loop + side both live before the ref.
		{"fws.supply.return.temp", []string{"return.temp"}, "return.temp", "fws.supply"},
		// No match.
		{"fws.flow", []string{"return.temp", "supply.temp"}, "", ""},
		// Ref has multiple dots; rel-point ends with same.
		{"fws.return.temp", []string{"return.temp.deg"}, "", ""},
	}
	for _, tc := range cases {
		gotRef, gotPref := matchRef(tc.rel, tc.refs)
		if gotRef != tc.wantRef || gotPref != tc.wantPref {
			t.Fatalf("matchRef(%q, %v) = (%q, %q), want (%q, %q)",
				tc.rel, tc.refs, gotRef, gotPref, tc.wantRef, tc.wantPref)
		}
	}
}

func TestUniqueQuantities(t *testing.T) {
	// uniqueQuantities returns the unique QUANTITY last-segment
	// set, not the unique refs. "return.temp" and "supply.temp"
	// both end in "temp" — only one VM instant query is needed
	// for that quantity. "flow" is its own quantity.
	got := uniqueQuantities([]string{"return.temp", "supply.temp", "return.temp", "flow"})
	want := []string{"flow", "temp"} // sorted, deduped by quantity
	if len(got) != len(want) {
		t.Fatalf("uniqueQuantities = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("uniqueQuantities[%d] = %q, want %q (full %v vs %v)", i, got[i], want[i], got, want)
		}
	}
}

func TestLastSegment(t *testing.T) {
	cases := map[string]string{
		"return.temp":     "temp",
		"fws.return.temp": "temp",
		"status":          "status",
		"":                "",
		"a.b.c.d":         "d",
	}
	for in, want := range cases {
		if got := lastSegment(in); got != want {
			t.Fatalf("lastSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRelPoint(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"__name__": "cios_temp_celsius", "loop": "fws", "side": "supply"}, "fws.supply.temp"},
		{map[string]string{"__name__": "cios_status", "loop": "fws"}, "fws.status"},
		{map[string]string{"__name__": "cios_temp_celsius", "loop": "fws", "side": "supply", "phase": "p1"}, "fws.supply.p1.temp"},
		// enum quantity: cios_status
		{map[string]string{"__name__": "cios_status"}, "status"},
	}
	for _, tc := range cases {
		got, err := promproj.RelPoint(tc.labels, minimalDict(t))
		if err != nil {
			t.Fatalf("RelPoint(%v): %v", tc.labels, err)
		}
		if got != tc.want {
			t.Fatalf("buildRelPoint(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

// ---- (a) selector ----------------------------------------------------------

func TestBuildSelector(t *testing.T) {
	cases := []struct {
		metric, hostFilter, site, want string
	}{
		// No site filter: single asset_type clause.
		{"cios_temp_celsius", "cdu|chiller", "", `cios_temp_celsius{asset_type=~"cdu|chiller"}`},
		// With site filter: comma-separated, second clause.
		{"cios_temp_celsius", "cdu|chiller", "site01", `cios_temp_celsius{asset_type=~"cdu|chiller",site="site01"}`},
		// Single host: just one.
		{"cios_flow_lpm", "cdu", "", `cios_flow_lpm{asset_type=~"cdu"}`},
	}
	for _, tc := range cases {
		if got := buildSelector(tc.metric, tc.hostFilter, tc.site); got != tc.want {
			t.Fatalf("buildSelector(%q,%q,%q) = %q, want %q",
				tc.metric, tc.hostFilter, tc.site, got, tc.want)
		}
	}
}

// ---- (d) projection --------------------------------------------------------

func TestRenderDerived_DeltatFwsOnCDU(t *testing.T) {
	// The end-to-end pin: a deltat derived point on a cdu's
	// fws loop must serialize to a promtext line with:
	//   - metric cios_deltat_celsius (spec-002 §7: cios_<name>_<unit>)
	//   - label order site, pod, cdu, path, loop, asset_type, domain, quality
	//     (per promproj.buildLabels; no side because the derived
	//      point has no `side` segment — only the inputs do)
	//   - value as a Go-'g' formatted float
	//   - ts in ms since epoch
	pointPath := "site01.pod002.cdu000.fws.deltat"
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dict := minimalDict(t)
	p, err := dict.ParsePoint(pointPath)
	if err != nil {
		t.Fatalf("ParsePoint: %v", err)
	}
	got, err := promproj.Render(p, 4.0, ts, "good", dict)
	if err != nil {
		t.Fatalf("renderDerived: %v", err)
	}
	want := `cios_deltat_celsius{site="site01",pod="pod002",cdu="cdu000",path="site01.pod002.cdu000",loop="fws",asset_type="cdu",domain="computing",quality="good"} 4 1767268800000`
	if got != want {
		t.Fatalf("renderDerived mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderDerived_NoLoop(t *testing.T) {
	// locPrefix="" collapse: pointPath has no loop segment;
	// the loop label must be omitted entirely (spec-002 §7
	// "empty location segments are OMITTED", not emitted as loop="").
	pointPath := "site01.pod002.cdu000.deltat"
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dict := minimalDict(t)
	p, err := dict.ParsePoint(pointPath)
	if err != nil {
		t.Fatalf("ParsePoint: %v", err)
	}
	got, err := promproj.Render(p, 5.5, ts, "good", dict)
	if err != nil {
		t.Fatalf("renderDerived: %v", err)
	}
	if strings.Contains(got, `loop=""`) {
		t.Fatalf("loop=\"\" leaked into line: %s", got)
	}
	if !strings.Contains(got, " 5.5 ") {
		t.Fatalf("value not 5.5: %s", got)
	}
	// Quality is always "good" on a derived point (we have no
	// upstream NaN to propagate; the instant query drops those
	// at the discovery step).
	if !strings.Contains(got, `quality="good"`) {
		t.Fatalf("missing quality=good: %s", got)
	}
}

// ---- flag validation -------------------------------------------------------

func TestMainFlags_MissingArgs(t *testing.T) {
	// We can't drive main() directly (it calls os.Exit), but
	// the flag-parse + os.Exit(2) path is exercised in
	// PRMT-020's main_test.go pattern. Here we just confirm
	// the contract: the binary refuses to start without
	// -vm-url and -protocol-dir. (Skipped if the test driver
	// is the actual binary, which would be an integration test
	// for a later PRMT.)
	t.Skip("main() reads flags + os.Exit(2); covered by the deploy/smoke harness, not by the unit suite")
}
