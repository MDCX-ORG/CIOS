// Package promproj renders CIOS telemetry points as Prometheus
// exposition lines and instant-query selectors, following spec-002 §7.
//
// The package owns ONE thing: turning a (point, value, timestamp,
// quality) tuple into a single line of text that VictoriaMetrics'
// /api/v1/import/prometheus will accept, plus the selector form
// core /v1/points uses for instant queries. All label-order, label-
// omission, metric-name, and timestamp formatting decisions live
// here — gateway/pipeline.go is a caller, not a re-implementer.
//
// Spec-002 §7 calls out four rules the implementation must honor:
//
//  1. Metric name: cios_<quantity>_<unit>; enum-typed quantities
//     (unit == "enum") drop the _<unit> suffix and render as
//     cios_<quantity>.
//  2. Path-segment expansion: every Node in AssetPath.Nodes becomes
//     a <type>="<type><index>" label (e.g. pod="pod002").
//  3. Empty location segments (loop, side, phase with no value) are
//     OMITTED — the line never emits loop="" or phase="".
//  4. asset_type is the LEAF device type (the last Node's Type;
//     e.g. for site01.pod002.cdu000.fws.supply.temp → "cdu").
//     domain is reverse-looked-up from the TOPMOST Node's Type
//     against Dict.Domains. For the same example, topmost type is
//     "pod" and Dict.Domains maps "pod" → "computing", so the line
//     carries asset_type="cdu",domain="computing". This is the
//     spec-002 §7 example verbatim: two non-conflicting label keys
//     that come from different ends of the path.
package promproj

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// lookupQuantity returns the QuantityDef for `name`, looking in
// Dict.Quantities first and then Dict.Derived (spec-002 §9: derived
// quantities are on the same wire as core ones; the only difference
// is how they're computed). Returning ok=false means the name is
// in neither table — caller decides how to surface that.
//
// This helper exists so MetricName/Render/Selector can serve both
// core and derived points through a single projection path
// (PRMT-024: collapsing cios-rules.renderDerived back into
// promproj.Render).
func lookupQuantity(name string, d *cpath.Dict) (cpath.QuantityDef, bool) {
	if def, ok := d.Quantities[name]; ok {
		return def, true
	}
	if def, ok := d.Derived[name]; ok {
		return def, true
	}
	return cpath.QuantityDef{}, false
}

// MetricName returns the Prometheus metric name for one point's
// quantity. Enum-typed quantities (unit == "enum") drop the suffix;
// everything else keeps "cios_<quantity>_<unit>". An unknown
// quantity (not in either Dict.Quantities or Dict.Derived) is an
// error so a caller cannot silently produce a metric name with an
// empty unit segment.
func MetricName(quantity string, d *cpath.Dict) (string, error) {
	q, ok := lookupQuantity(quantity, d)
	if !ok {
		return "", fmt.Errorf("promproj: unknown quantity %q", quantity)
	}
	if q.Unit == "enum" {
		return "cios_" + quantity, nil
	}
	return "cios_" + quantity + "_" + q.Unit, nil
}

// Render produces one Prometheus exposition line (no trailing
// newline) for a sample. The line is suitable for the body of a
// POST to /api/v1/import/prometheus. Empty loop / side / phase
// segments are dropped from the label set; the domain label is
// dropped if the asset's topmost type is not in any domain.
//
// Label order is fixed: site, [one <type> per node in path order],
// path, [loop], [side], [phase], asset_type, [domain], quality.
// This order matches spec-002 §7's example and is what
// TestRender_ExactLabelOrder pins.
func Render(p cpath.Point, value float64, ts time.Time, quality string, d *cpath.Dict) (string, error) {
	if p.Quantity == "" {
		return "", fmt.Errorf("promproj: empty quantity")
	}
	name, err := MetricName(p.Quantity, d)
	if err != nil {
		return "", err
	}
	labels := buildLabels(p, quality, d)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l.v))
		b.WriteByte('"')
	}
	b.WriteString("} ")
	b.WriteString(formatFloat(value))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(ts.UnixMilli(), 10))
	return b.String(), nil
}

// Selector returns the instant-query form of a point: same metric
// name, same label set as Render minus the time-shifting tags
// (quality, since the selector is used to read the live data, not
// to filter by quality). It is what core /v1/points emits into
// "query" so a Grafana panel can drill in.
func Selector(p cpath.Point, d *cpath.Dict) (string, error) {
	if p.Quantity == "" {
		return "", fmt.Errorf("promproj: empty quantity")
	}
	name, err := MetricName(p.Quantity, d)
	if err != nil {
		return "", err
	}
	labels := buildLabels(p, "", d)
	// strip zero-value labels for the selector (loop/side/phase);
	// quality is never emitted by Selector.
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	first := true
	for _, l := range labels {
		if l.k == "quality" {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(l.k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l.v))
		b.WriteByte('"')
		first = false
	}
	b.WriteByte('}')
	return b.String(), nil
}

// --- internals --------------------------------------------------------------

type kv struct{ k, v string }

// buildLabels walks the point and emits the ordered label set. The
// caller is responsible for choosing what tags to render (Render
// passes the sample's quality; Selector passes "" because selectors
// don't carry it).
func buildLabels(p cpath.Point, quality string, d *cpath.Dict) []kv {
	out := make([]kv, 0, 8)
	out = append(out, kv{"site", p.Asset.Site})
	for _, n := range p.Asset.Nodes {
		out = append(out, kv{n.Type, n.Type + n.Index})
	}
	out = append(out, kv{"path", p.Asset.String()})
	if p.Loop != "" {
		v := p.Loop
		if p.LoopIndex >= 0 {
			v += strconv.Itoa(p.LoopIndex)
		}
		out = append(out, kv{"loop", v})
	}
	if p.Side != "" {
		out = append(out, kv{"side", p.Side})
	}
	if p.Phase != "" {
		out = append(out, kv{"phase", p.Phase})
	}
	if len(p.Asset.Nodes) > 0 {
		// asset_type is the LEAF device type (last Node). domain
		// is reverse-looked-up from the TOPMOST Node's type. The
		// two keys come from opposite ends of the path; spec-002
		// §7's "cdu path → asset_type=cdu, domain=computing"
		// example shows both at once, not a contradiction.
		leaf := p.Asset.Nodes[len(p.Asset.Nodes)-1].Type
		out = append(out, kv{"asset_type", leaf})
		top := p.Asset.Nodes[0].Type
		if dom := reverseDomain(top, d); dom != "" {
			out = append(out, kv{"domain", dom})
		}
	}
	if quality != "" {
		out = append(out, kv{"quality", quality})
	}
	return out
}

// reverseDomain returns the first domain key in Dict.Domains whose
// value-list contains topType. Map iteration order is unspecified,
// so we sort the domain keys to keep output deterministic for tests.
func reverseDomain(topType string, d *cpath.Dict) string {
	if d.Domains == nil {
		return ""
	}
	keys := make([]string, 0, len(d.Domains))
	for k := range d.Domains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, t := range d.Domains[k] {
			if t == topType {
				return k
			}
		}
	}
	return ""
}

// formatFloat emits a float64 in the most compact decimal form that
// preserves the value. Prometheus accepts both 23.5 and 23.5000000;
// the latter just bloats the import batch. strconv 'g' with -1
// precision already drops trailing zeros, so no extra trim is
// needed (and no .0 trailer is ever produced for whole numbers
// at this precision).
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabel is a minimal pass-through that escapes the characters
// Prometheus exposition format treats as syntactic: backslash,
// double-quote, and newline. Real-world CIOS point names and loop
// values don't contain these, but a defensive escape keeps a stray
// tab or quote from corrupting the whole import batch.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- inverse projection -----------------------------------------------------
//
// QuantityFromMetric / RelPoint / ParseLine invert what MetricName /
// Render produce. They exist so cmd/cios-rules and cmd/cios-alarm —
// both of which consume VM-shaped data — share the same projection
// authority as the producer side (spec-002 §7/§9, LOCKED L23).
// Without these, each consumer was forced to re-implement the
// inverse and drift was just a question of when (PRMT-021 §9 F1,
// PRMT-020 §8.1).

// QuantityFromMetric is the inverse of MetricName: given a Prometheus
// metric name like "cios_temp_celsius" or "cios_status", return the
// quantity name ("temp" / "status"). The dict disambiguates because
// quantity names themselves can contain underscores (spec-002 §3),
// so we cannot split on '_' — instead we compare against the exact
// "cios_"+q+"_"+def.Unit form for every quantity in Dict.Quantities
// and Dict.Derived (the latter so derived points like
// "cios_deltat_celsius" round-trip too).
//
// Enum-form metric names ("cios_status") have no unit suffix and so
// live only in Dict.Quantities (Dict.Derived has no enums today, but
// the scan would handle one if it appeared).
//
// An unmatched metric is an error; the caller (cios-rules bucketing
// or cios-alarm decode) decides whether to skip the series.
func QuantityFromMetric(metric string, d *cpath.Dict) (string, error) {
	const prefix = "cios_"
	if !strings.HasPrefix(metric, prefix) {
		return "", fmt.Errorf("promproj: metric %q lacks %q prefix", metric, prefix)
	}
	body := metric[len(prefix):]
	// Enum form: "cios_<q>" with no unit suffix. Scan Quantities
	// (and Derived, defensively — a future enum-typed derived
	// would not require a code change here).
	if def, ok := d.Quantities[body]; ok && def.Unit == "enum" {
		return body, nil
	}
	if def, ok := d.Derived[body]; ok && def.Unit == "enum" {
		return body, nil
	}
	// Non-enum form: compare full "cios_<q>_<unit>".
	scan := func(m map[string]cpath.QuantityDef) string {
		for q, def := range m {
			if def.Unit == "" || def.Unit == "enum" {
				continue
			}
			if prefix+q+"_"+def.Unit == metric {
				return q
			}
		}
		return ""
	}
	if q := scan(d.Quantities); q != "" {
		return q, nil
	}
	if q := scan(d.Derived); q != "" {
		return q, nil
	}
	return "", fmt.Errorf("promproj: metric %q matches no quantity in dict", metric)
}

// RelPoint reconstructs the relative-point identifier
// "loop[.side][.phase].quantity" from a Prometheus label set —
// the inverse of how Render's buildLabels lays them out. The
// label set must contain "__name__" (the metric, from which the
// quantity is decoded via QuantityFromMetric); the loop/side/
// phase labels are taken verbatim and any that are absent or
// empty are skipped (matching Render's "OMITTED, never emitted
// as side=\"\"" rule).
//
// A loop label whose value already carries an index suffix
// (e.g. "fws0") is preserved as-is — Render emits the index
// glued to the loop name, so the inverse pulls the whole token
// back into the rel-point.
func RelPoint(labels map[string]string, d *cpath.Dict) (string, error) {
	name, ok := labels["__name__"]
	if !ok || name == "" {
		return "", fmt.Errorf("promproj: labels missing __name__")
	}
	quantity, err := QuantityFromMetric(name, d)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 4)
	if v := labels["loop"]; v != "" {
		parts = append(parts, v)
	}
	if v := labels["side"]; v != "" {
		parts = append(parts, v)
	}
	if v := labels["phase"]; v != "" {
		parts = append(parts, v)
	}
	parts = append(parts, quantity)
	return strings.Join(parts, "."), nil
}

// ParseLine parses one Prometheus exposition line of the shape
// Render produces:
//
//	<metric>{<labels>} <value> <ts_ms>
//
// and returns the label map (with "__name__" filled in from the
// metric token) plus the float value. ts_ms is intentionally
// dropped — every current consumer (cios-alarm) re-stamps with its
// own clock or batch instant.
//
// The parser handles the escapes Render emits (\" \\ \n) and
// {/} pairing within label values; it is NOT a general-purpose
// Prometheus text-format parser (no HELP/TYPE lines, no exemplars,
// no Inf/NaN), only what promproj.Render itself emits — the same
// assumption PRMT-020 made when it first implemented parsePromLine.
func ParseLine(line string, d *cpath.Dict) (map[string]string, float64, error) {
	brace := strings.IndexByte(line, '{')
	if brace < 0 {
		return nil, 0, fmt.Errorf("promproj: no label set")
	}
	metric := line[:brace]
	closeBrace := findMatchingBrace(line, brace)
	if closeBrace < 0 {
		return nil, 0, fmt.Errorf("promproj: unterminated label set")
	}
	labelStr := line[brace+1 : closeBrace]
	tail := strings.TrimSpace(line[closeBrace+1:])
	if tail == "" {
		return nil, 0, fmt.Errorf("promproj: missing value")
	}
	valStr, _, _ := strings.Cut(tail, " ")
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("promproj: bad value %q: %w", valStr, err)
	}
	labels := parseLabels(labelStr)
	labels["__name__"] = metric
	return labels, v, nil
}

// findMatchingBrace locates the matching '}' for the '{' at open,
// honoring \" escapes inside label values. Returns -1 on no match.
func findMatchingBrace(s string, open int) int {
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip escaped char (\\ or \")
		case '"':
			// quoted string body — walk until close quote.
			for j := i + 1; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '"' {
					i = j
					break
				}
			}
		case '}':
			return i
		}
	}
	return -1
}

// parseLabels splits `k1="v1",k2="v2",…` into a map, undoing the
// Render-side escapes (\\, \", \n). Label values containing literal
// commas are not supported (nothing in spec-001/002 emits them).
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		kStart := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' {
			i++
		}
		k := s[kStart:i]
		for i < len(s) && (s[i] == '=' || s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] != '"' {
			break
		}
		i++ // opening quote
		var b strings.Builder
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case '\\':
					b.WriteByte('\\')
				case '"':
					b.WriteByte('"')
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			b.WriteByte(s[i])
			i++
		}
		if i < len(s) {
			i++ // closing quote
		}
		out[k] = b.String()
	}
	return out
}
