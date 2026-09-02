package promproj

import (
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// loadTestDict returns a Dict hand-built from the canonical tables
// we test against. It avoids touching protocol/*.yaml from inside
// pkg/promproj's own tests so the rendering logic can be unit-tested
// without a filesystem.
func loadTestDict(t *testing.T) *cpath.Dict {
	t.Helper()
	d := &cpath.Dict{
		Types: map[string]cpath.TypeDef{
			"site":  {Parents: nil, Level: cpath.LevelDevice},
			"pod":   {Parents: []string{"site"}, Level: cpath.LevelDevice},
			"cdu":   {Parents: []string{"pod"}, Level: cpath.LevelDevice},
			"gpu":   {Parents: []string{"node"}, Level: cpath.LevelChip},
			"node":  {Parents: []string{"tank", "rack"}, Level: cpath.LevelDevice},
			"tank":  {Parents: []string{"pod"}, Level: cpath.LevelDevice},
			"rack":  {Parents: []string{"pod"}, Level: cpath.LevelDevice},
			"meter": {Parents: []string{"site"}, Level: cpath.LevelDevice},
		},
		Quantities: map[string]cpath.QuantityDef{
			"temp":    {Unit: "celsius", Kind: "gauge"},
			"flow":    {Unit: "lpm", Kind: "gauge"},
			"opening": {Unit: "percent", Kind: "gauge"},
			"power":   {Unit: "watt", Kind: "gauge"},
			"status":  {Unit: "enum", Kind: "gauge"},
			"leak":    {Unit: "enum", Kind: "gauge"},
			"door":    {Unit: "enum", Kind: "gauge"},
		},
		Derived: map[string]cpath.QuantityDef{},
		Loops:   map[string]bool{"fws": true, "tcs": true},
		Sides:   map[string]bool{"supply": true, "return": true},
		Phases:  map[string]bool{"l1": true, "l2": true, "l3": true, "n": true},
		Domains: map[string][]string{
			"computing": {"pod"},
			"cooling":   {"cdu"},
			"feed":      {"meter"},
		},
	}
	return d
}

// parseP convenience wrapper used by every render test.
func parseP(t *testing.T, d *cpath.Dict, s string) cpath.Point {
	t.Helper()
	p, err := d.ParsePoint(s)
	if err != nil {
		t.Fatalf("ParsePoint(%q): %v", s, err)
	}
	return p
}

func TestMetricName_Quantity(t *testing.T) {
	d := loadTestDict(t)
	cases := map[string]string{
		"temp":    "cios_temp_celsius",
		"flow":    "cios_flow_lpm",
		"opening": "cios_opening_percent",
		"status":  "cios_status",
		"leak":    "cios_leak",
		"door":    "cios_door",
	}
	for q, want := range cases {
		got, err := MetricName(q, d)
		if err != nil {
			t.Errorf("MetricName(%q) err: %v", q, err)
			continue
		}
		if got != want {
			t.Errorf("MetricName(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestMetricName_Unknown(t *testing.T) {
	d := loadTestDict(t)
	if _, err := MetricName("nope", d); err == nil {
		t.Errorf("MetricName(unknown) returned no error")
	}
}

func TestRender_ExactLabelOrder(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.flow")
	ts := time.UnixMilli(1_700_000_000_000)
	got, err := Render(p, 12.5, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `cios_flow_lpm{site="site01",pod="pod002",cdu="cdu000",path="site01.pod002.cdu000",loop="fws",side="supply",asset_type="cdu",domain="computing",quality="good"} 12.5 1700000000000`
	if got != want {
		t.Errorf("Render mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestRender_EnumOmitsUnitSuffix(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.status")
	ts := time.UnixMilli(1)
	got, err := Render(p, 3, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// status has no loop/side/phase and its unit is "enum", so the
	// metric is cios_status (no _enum).
	if !strings.HasPrefix(got, "cios_status{") {
		t.Errorf("Render metric prefix wrong: %s", got)
	}
	if strings.Contains(got, "_enum") {
		t.Errorf("Render emitted _enum suffix for enum-typed quantity: %s", got)
	}
}

func TestRender_EmptyLocationSegmentsOmitted(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.status")
	ts := time.UnixMilli(1)
	got, err := Render(p, 0, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, banned := range []string{`loop=""`, `side=""`, `phase=""`} {
		if strings.Contains(got, banned) {
			t.Errorf("Render emitted empty %q: %s", banned, got)
		}
	}
}

func TestRender_DomainReverseLookup(t *testing.T) {
	d := loadTestDict(t)
	// cdu lives in domain "cooling".
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.temp")
	ts := time.UnixMilli(1)
	got, err := Render(p, 23.5, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The path's topmost type is "pod" (parent of cdu), so the
	// domain label resolves to "computing", not "cooling". This
	// matches the spec-002 §7 example exactly.
	if !strings.Contains(got, `domain="computing"`) {
		t.Errorf("Render missing domain=computing: %s", got)
	}
}

func TestRender_LoopIndex(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws0.supply.flow")
	ts := time.UnixMilli(1)
	got, err := Render(p, 1, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `loop="fws0"`) {
		t.Errorf("Render missing loop=fws0: %s", got)
	}
}

func TestRender_QualitySuspect(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.temp")
	ts := time.UnixMilli(1)
	got, err := Render(p, 0, ts, "suspect", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `quality="suspect"`) {
		t.Errorf("Render missing quality=suspect: %s", got)
	}
}

func TestRender_TimestampMillis(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.temp")
	ts := time.Unix(0, 0).Add(1*time.Second + 234*time.Millisecond)
	got, err := Render(p, 0, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasSuffix(got, " 1234") {
		t.Errorf("Render ts = %s, want trailing ' 1234'", got)
	}
}

func TestRender_DomainFromTopLevelType(t *testing.T) {
	// asset_type = LEAF node (gpu), domain = TOPMOST node's
	// reverse-lookup (pod → computing). spec-002 §7's example shows
	// both keys at once: a cdu-path's asset_type=cdu and domain=
	// computing without any "asset_type = topmost" overloading.
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.tank000.node000.gpu0.temp")
	ts := time.UnixMilli(1)
	got, err := Render(p, 50, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `asset_type="gpu"`) {
		t.Errorf("Render: expected asset_type=gpu (leaf), got: %s", got)
	}
	if !strings.Contains(got, `domain="computing"`) {
		t.Errorf("Render: expected domain=computing (top type=pod), got: %s", got)
	}
}

func TestRender_NoDomainWhenTopTypeUnknown(t *testing.T) {
	// meter is the only types entry that lives in domain "feed"
	// in our test dict. To exercise the "top type not in any
	// domain" branch we manufacture a Dict where 'pod' is NOT in
	// any domain (its parents are kept so ParsePoint still works).
	d := loadTestDict(t)
	d.Domains = map[string][]string{
		"feed":    {"meter"},
		"cooling": {"cdu"},
	}
	// site-level asset (no nodes) so the topmost type is "" and
	// buildLabels skips the asset_type/domain emission entirely.
	// site-level addresses must use a derived quantity; use the
	// 'deltat' (no, not in our test dict) — instead test that a
	// node whose top type is a 'bare' entry skips domain.
	// Simpler: pick a top type that exists in Types but not in
	// any domain. We add 'gizmo' as a top type with no domain.
	d.Types["gizmo"] = cpath.TypeDef{Parents: []string{"site"}, Level: cpath.LevelDevice}
	p := parseP(t, d, "site01.gizmo000.temp")
	ts := time.UnixMilli(1)
	got, err := Render(p, 1, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "domain=") {
		t.Errorf("Render emitted domain= for type with no domain: %s", got)
	}
}

func TestSelector_MatchesRenderLabelsExceptQuality(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.flow")
	ts := time.UnixMilli(1)
	rendered, err := Render(p, 1, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	sel, err := Selector(p, d)
	if err != nil {
		t.Fatalf("Selector: %v", err)
	}
	// Selector is the rendered line's "{"..."}" with quality= and the
	// trailing " <value> <ts>" stripped. We assert on the set of label
	// pairs and the metric name, not byte-for-byte (since Render
	// emits quality="good" and Selector must not).
	ri := strings.Index(rendered, "{")
	rj := strings.Index(rendered, "}")
	if ri < 0 || rj < 0 {
		t.Fatalf("Render missing label braces: %s", rendered)
	}
	renderedLabels := rendered[ri+1 : rj]
	if !strings.Contains(sel, "{") {
		t.Fatalf("Selector missing braces: %s", sel)
	}
	si := strings.Index(sel, "{")
	sj := strings.LastIndex(sel, "}")
	selLabels := sel[si+1 : sj]
	// Drop quality from renderedLabels for the comparison.
	parts := strings.Split(renderedLabels, ",")
	var kept []string
	for _, p := range parts {
		if !strings.HasPrefix(p, "quality=") {
			kept = append(kept, p)
		}
	}
	if got := strings.Join(kept, ","); got != selLabels {
		t.Errorf("Selector labels != Render labels minus quality\n selector: %s\n  rendered: %s", selLabels, got)
	}
}

func TestSelector_OmitsEmptyLocations(t *testing.T) {
	d := loadTestDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.status")
	sel, err := Selector(p, d)
	if err != nil {
		t.Fatalf("Selector: %v", err)
	}
	for _, banned := range []string{`loop=""`, `side=""`, `phase=""`, "quality="} {
		if strings.Contains(sel, banned) {
			t.Errorf("Selector emitted %q: %s", banned, sel)
		}
	}
}

func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		"plain":        "plain",
		`with "quote"`: `with \"quote\"`,
		"back\\slash":  `back\\slash`,
		"line\nbreak":  `line\nbreak`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- PRMT-024: derived-aware projection + inverse helpers ------------------

// derivedDict extends loadTestDict with one derived quantity
// (deltat) so we can pin §4.1's "MetricName/Render see Dict.Derived".
// Hand-built rather than loaded from protocol/ to keep the unit suite
// filesystem-free.
func derivedDict(t *testing.T) *cpath.Dict {
	t.Helper()
	d := loadTestDict(t)
	d.Derived = map[string]cpath.QuantityDef{
		// deltat is the M1 derived host on cdu/chiller — Host must
		// be set so ParsePoint accepts "cdu000.fws.deltat"
		// (cpath.go enforces L48).
		"deltat": {Unit: "celsius", Host: []string{"cdu", "chiller"}},
	}
	return d
}

func TestMetricName_DerivedQuantity(t *testing.T) {
	// §4.1: MetricName must see Dict.Derived so derived quantities
	// project on the same wire as core ones (spec-002 §9).
	d := derivedDict(t)
	got, err := MetricName("deltat", d)
	if err != nil {
		t.Fatalf("MetricName(deltat): %v", err)
	}
	if got != "cios_deltat_celsius" {
		t.Errorf("MetricName(deltat) = %q, want %q", got, "cios_deltat_celsius")
	}
}

func TestRender_DerivedQuantity(t *testing.T) {
	// §2-bis #1: Render must succeed on a derived point. Before
	// PRMT-024, MetricName scanned only Dict.Quantities and this
	// returned "unknown quantity". The line shape is identical to
	// a core-quantity render (label order, escapes, ts_ms).
	d := derivedDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.deltat")
	ts := time.UnixMilli(1)
	got, err := Render(p, 4.5, ts, "good", d)
	if err != nil {
		t.Fatalf("Render(deltat): %v", err)
	}
	if !strings.HasPrefix(got, "cios_deltat_celsius{") {
		t.Errorf("Render(deltat) metric prefix wrong: %s", got)
	}
	if !strings.Contains(got, `loop="fws"`) {
		t.Errorf("Render(deltat) missing loop=fws: %s", got)
	}
	if !strings.HasSuffix(got, " 4.5 1") {
		t.Errorf("Render(deltat) value/ts suffix wrong: %s", got)
	}
}

func TestQuantityFromMetric(t *testing.T) {
	// §4.2: enum, non-enum, derived, and unknown. The unknown
	// case must return error (cios-rules' old best-effort fallback
	// is intentionally NOT preserved; see PRMT-024 §4.2 rationale).
	d := derivedDict(t)
	cases := []struct {
		metric  string
		want    string
		wantErr bool
	}{
		{"cios_temp_celsius", "temp", false},     // non-enum
		{"cios_flow_lpm", "flow", false},         // non-enum, different unit
		{"cios_status", "status", false},         // enum (no _unit suffix)
		{"cios_leak", "leak", false},             // enum
		{"cios_deltat_celsius", "deltat", false}, // derived non-enum
		{"cios_nope_celsius", "", true},          // unknown — must error
		{"node_load1", "", true},                 // lacks cios_ prefix
		{"cios_temp_kelvin", "", true},           // wrong unit for known quantity
	}
	for _, tc := range cases {
		got, err := QuantityFromMetric(tc.metric, d)
		if tc.wantErr {
			if err == nil {
				t.Errorf("QuantityFromMetric(%q): want error, got %q", tc.metric, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("QuantityFromMetric(%q): %v", tc.metric, err)
			continue
		}
		if got != tc.want {
			t.Errorf("QuantityFromMetric(%q) = %q, want %q", tc.metric, got, tc.want)
		}
	}
}

func TestRelPoint(t *testing.T) {
	// §4.3: loop[.side[.phase]].quantity. Empty/absent loop/side/
	// phase labels are dropped (mirrors Render's "OMITTED" rule).
	d := derivedDict(t)
	cases := []struct {
		labels map[string]string
		want   string
	}{
		// Full triple: loop + side + quantity.
		{map[string]string{"__name__": "cios_temp_celsius", "loop": "fws", "side": "supply"}, "fws.supply.temp"},
		// loop + side + phase (rare but legal).
		{map[string]string{"__name__": "cios_temp_celsius", "loop": "fws", "side": "supply", "phase": "p1"}, "fws.supply.p1.temp"},
		// Enum quantity with loop only.
		{map[string]string{"__name__": "cios_status", "loop": "fws"}, "fws.status"},
		// Bare enum, no loop.
		{map[string]string{"__name__": "cios_status"}, "status"},
		// Derived (deltat) with loop only — locPrefix collapse.
		{map[string]string{"__name__": "cios_deltat_celsius", "loop": "fws"}, "fws.deltat"},
		// Loop with index preserved as-is (Render emits loop="fws0").
		{map[string]string{"__name__": "cios_flow_lpm", "loop": "fws0", "side": "supply"}, "fws0.supply.flow"},
	}
	for _, tc := range cases {
		got, err := RelPoint(tc.labels, d)
		if err != nil {
			t.Errorf("RelPoint(%v): %v", tc.labels, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RelPoint(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
	// Missing __name__ must error.
	if _, _, errOK := func() (string, bool, error) {
		_, err := RelPoint(map[string]string{"loop": "fws"}, d)
		return "", false, err
	}(); errOK == nil {
		t.Errorf("RelPoint(no __name__): want error, got nil")
	}
}

func TestParseLine_RoundTripsRender(t *testing.T) {
	// §4.4: ParseLine must invert Render exactly. We feed the
	// canonical TestRender_ExactLabelOrder line back through
	// ParseLine and verify __name__, value, and every original
	// label survives the round-trip.
	d := derivedDict(t)
	p := parseP(t, d, "site01.pod002.cdu000.fws.supply.flow")
	ts := time.UnixMilli(1_700_000_000_000)
	line, err := Render(p, 12.5, ts, "good", d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	labels, value, err := ParseLine(line, d)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if value != 12.5 {
		t.Errorf("ParseLine value = %v, want 12.5", value)
	}
	if labels["__name__"] != "cios_flow_lpm" {
		t.Errorf("ParseLine __name__ = %q, want cios_flow_lpm", labels["__name__"])
	}
	for k, want := range map[string]string{
		"site": "site01", "pod": "pod002", "cdu": "cdu000",
		"path": "site01.pod002.cdu000", "loop": "fws", "side": "supply",
		"asset_type": "cdu", "domain": "computing", "quality": "good",
	} {
		if labels[k] != want {
			t.Errorf("ParseLine label %q = %q, want %q", k, labels[k], want)
		}
	}
	// Round-trip into RelPoint must reproduce the relative-point form.
	rel, err := RelPoint(labels, d)
	if err != nil {
		t.Fatalf("RelPoint after ParseLine: %v", err)
	}
	if rel != "fws.supply.flow" {
		t.Errorf("RelPoint after ParseLine = %q, want %q", rel, "fws.supply.flow")
	}
}

func TestParseLine_Escapes(t *testing.T) {
	// Hand-built line with the three escapes Render emits.
	// "\\\"" → " (one literal quote in the unescaped value)
	// "\\\\" → \  (one literal backslash)
	// "\\n"  → \n (one literal newline)
	d := loadTestDict(t)
	line := `cios_temp_celsius{site="s\"x",pod="p\\y",path="p\nz",asset_type="cdu"} 1 1`
	labels, v, err := ParseLine(line, d)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if v != 1 {
		t.Fatalf("value = %v, want 1", v)
	}
	if labels["site"] != `s"x` {
		t.Errorf("escape \\\" not undone: site=%q", labels["site"])
	}
	if labels["pod"] != `p\y` {
		t.Errorf("escape \\\\ not undone: pod=%q", labels["pod"])
	}
	if labels["path"] != "p\nz" {
		t.Errorf("escape \\n not undone: path=%q", labels["path"])
	}
}

func TestParseLine_Errors(t *testing.T) {
	d := loadTestDict(t)
	cases := []string{
		"",                                  // empty
		"no_braces 1 1",                     // no '{'
		`cios_temp_celsius{site="x" 1 1`,    // unterminated label set
		`cios_temp_celsius{site="x"}`,       // missing value
		`cios_temp_celsius{site="x"} NaN 1`, // ParseFloat accepts NaN — not an error here
	}
	for i, line := range cases {
		_, _, err := ParseLine(line, d)
		// case 4 (NaN) is intentionally a non-error: strconv.ParseFloat
		// accepts "NaN". The other four must all error.
		if i < 4 {
			if err == nil {
				t.Errorf("ParseLine(case %d %q): want error, got nil", i, line)
			}
		}
	}
}
