// Pipeline turns one device's point map into: (a) the list of
// modbus.Binding values the driver should poll, and (b) a Convert
// function that maps a driver.Sample into a Prometheus exposition
// line. The pipeline is constructed once per device at startup and
// then is read-only; no driver state lives here.
//
// Conversion order is fixed and matches the L1 entry-check pipeline
// (spec-006 §4): raw -> ×Scale -> enum_map -> unit conversion ->
// (limits check) -> Prometheus projection. Quality is carried
// verbatim from the driver except for the two cases where L1 forces
// it to "suspect": enum_map value not in the standard enum table,
// and Limits violated. In both cases the line is still emitted —
// the gateway never silently drops a sample.
package gateway

import (
	"fmt"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/modbus"
	"github.com/yurimeng/cios/pkg/driver/snmp"
	"github.com/yurimeng/cios/pkg/modbusbind"
	"github.com/yurimeng/cios/pkg/pointmap"
	"github.com/yurimeng/cios/pkg/promproj"
)

// Pipeline holds one device's precomputed translation tables. It
// is constructed by NewPipeline and consumed by the run loop in
// run.go.
type Pipeline struct {
	dict  *cpath.Dict
	point cpath.Point
	qDef  cpath.QuantityDef
	conv  pointmap.Conv
	scale float64
	// emap maps vendor-codes to standard enum codes. nil means the
	// point either is not enum-typed or speaks the standard
	// vocabulary natively (no translation needed). The lookup uses
	// int->int.
	emap       map[int]int
	limits     *pointmap.Limits
	metricName string
}

// NewPipeline validates and precomputes everything one device's
// point needs. The absolute point address is composed from the
// asset string the caller passes in (Device.Asset) and the
// relative point in the pointmap (PointDef.Point); the dict parses
// and validates the result. The modbus binding's Writable flag is
// always false (control wiring is M1, PRMT-010 §2).
//
// Signature and behaviour are unchanged in PRMT-023: this function
// stays the modbus-only entry point. Protocol-agnostic construction
// is factored into newPipelineCore so the snmp path in run.go can
// reuse the same V1/scale/enum/limits/metricName logic without
// pulling in the modbus binding type.
func NewPipeline(
	asset string,
	pm *pointmap.PointMap,
	pd pointmap.PointDef,
	d *cpath.Dict,
	u *pointmap.Units,
) (*Pipeline, modbus.Binding, error) {
	pl, err := newPipelineCore(asset, pm, pd, d, u)
	if err != nil {
		return nil, modbus.Binding{}, err
	}
	// Modbus binding: register + table come from the per-point
	// Protocol map. Other Protocol keys (e.g. type/readback) are
	// ignored by the gateway; they are checked by pointmap.Load.
	// PRMT-030 §A: modbusbind.BuildFromPointDef is the single source
	// of truth (previously a private helper duplicated in this file
	// and cmd/cios-modbus-driver).
	binding, err := modbusbind.BuildFromPointDef(pd)
	if err != nil {
		return nil, modbus.Binding{}, fmt.Errorf("gateway: %q: %w", pd.Point, err)
	}
	return pl, binding, nil
}

// newPipelineCore builds the protocol-agnostic Pipeline (V1 path
// parse + unit conversion + enum + limits + metric name). No
// binding is produced — callers that need a modbus or snmp binding
// must pair this with modbusbind.BuildFromPointDef or
// snmpBindingFromProtocol. The error messages match the pre-PRMT-023
// NewPipeline behaviour verbatim so existing tests stay green.
func newPipelineCore(
	asset string,
	pm *pointmap.PointMap,
	pd pointmap.PointDef,
	d *cpath.Dict,
	u *pointmap.Units,
) (*Pipeline, error) {
	if pm == nil {
		return nil, fmt.Errorf("gateway: nil point map")
	}
	if asset == "" {
		return nil, fmt.Errorf("gateway: %q: empty asset", pd.Point)
	}
	// V1: compose the absolute path through cpath. ParsePoint is
	// the single source of truth for asset-path / location /
	// quantity validation — we never re-implement parent-chain
	// synthesis in the gateway.
	abs := asset + "." + pd.Point
	p, err := d.ParsePoint(abs)
	if err != nil {
		return nil, fmt.Errorf("gateway: V1 %q: %w", pd.Point, err)
	}
	q, ok := d.Quantities[p.Quantity]
	if !ok {
		return nil, fmt.Errorf("gateway: %q: quantity %q not registered",
			pd.Point, p.Quantity)
	}

	// unit_in -> standard unit conversion. unit_in == "" means the
	// vendor speaks the standard unit natively; the identity Conv
	// is correct.
	var conv pointmap.Conv
	if pd.UnitIn != "" {
		c, ok := u.CanConvert(q.Unit, pd.UnitIn)
		if !ok {
			return nil, fmt.Errorf("gateway: %q: cannot convert %q -> %q",
				pd.Point, pd.UnitIn, q.Unit)
		}
		conv = c
	} else {
		conv = pointmap.Conv{Factor: 1, Offset: 0}
	}

	// Cache the Prometheus metric name. Enum-typed quantities drop
	// the _enum suffix; we let promproj.MetricName make that
	// decision so the package owns the rule.
	mn, err := promproj.MetricName(p.Quantity, d)
	if err != nil {
		return nil, fmt.Errorf("gateway: %q: %w", pd.Point, err)
	}

	pl := &Pipeline{
		dict:       d,
		point:      p,
		qDef:       q,
		conv:       conv,
		scale:      pd.Scale,
		emap:       pd.EnumMap,
		limits:     pd.Limits,
		metricName: mn,
	}
	return pl, nil
}

// Convert takes a driver sample and returns one Prometheus
// exposition line. Errors are returned for samples that cannot be
// rendered (NaN/Inf, missing point identity). The run loop logs
// and skips them so one bad point does not poison a batch.
//
// L1 quality adjustments (enum outside std, limits violated)
// downgrade quality to suspect but the line is still emitted.
func (p *Pipeline) Convert(s driver.Sample) (string, error) {
	// 1. raw -> ×Scale
	v := s.Value * p.scale
	// 2. enum_map (only for enum-typed quantities). The semantics
	// from PRMT-010 §4.2 are: translate the vendor code to a
	// standard code first (if a point-level enum_map is present),
	// THEN check whether the resulting code is in the quantity's
	// standard enum vocabulary. If the quantity is enum-typed but
	// the value (translated, or the scaled raw if no enum_map) is
	// not a known standard code, the sample is suspect. A point
	// with no enum_map that receives a value within the standard
	// vocabulary passes through as good — the test fixture in
	// PRMT-009's stdSimConfig relies on this.
	quality := string(s.Quality)
	if p.qDef.Enum != nil {
		if p.emap != nil {
			if mapped, ok := p.emap[int(v)]; ok {
				v = float64(mapped)
			}
		}
		if _, ok := p.qDef.Enum[int(v)]; !ok {
			quality = string(driver.QualitySuspect)
		}
	}
	// 3. unit conversion (raw-after-scale -> standard)
	v = v*p.conv.Factor + p.conv.Offset
	// 4. limits check (only meaningful for class-a writable points
	// — but spec-006 §4 L1 says it applies to all points with
	// declared limits).
	if p.limits != nil && (v < p.limits.Min || v > p.limits.Max) {
		quality = string(driver.QualitySuspect)
	}
	// 5. promproj.Render. The metric name is taken from the dict
	// inside promproj; we cache the lookup in pl.metricName for
	// diagnostics but Render re-derives it. One map hit per tick
	// per point is fine for M0.
	return promproj.Render(p.point, v, s.Ts, quality, p.dict)
}

// --- modbus binding construction ------------------------------------------

// The modbus binding construction logic moved to pkg/modbusbind in
// PRMT-030 §A. The gateway now calls modbusbind.BuildFromPointDef;
// the prior package-private bindingFromProtocol is gone.

// --- snmp binding construction --------------------------------------------

// snmpBindingFromProtocol reads the per-point Protocol map and
// produces a snmp.Binding. It mirrors the modbus mapper's posture:
// Protocol key "oid" is required; optional "kind" for SET encoding.
// P722: Writable follows pointmap access=rw.
func snmpBindingFromProtocol(pd pointmap.PointDef) (snmp.Binding, error) {
	raw, ok := pd.Protocol["oid"]
	if !ok || raw == nil {
		return snmp.Binding{}, fmt.Errorf("missing oid")
	}
	oid, ok := raw.(string)
	if !ok {
		return snmp.Binding{}, fmt.Errorf("oid has unexpected type %T", raw)
	}
	kind := ""
	if k, ok := pd.Protocol["kind"].(string); ok {
		kind = k
	}
	return snmp.Binding{
		Point:    pd.Point,
		OID:      oid,
		Writable: pd.Access == "rw",
		Kind:     kind,
	}, nil
}
