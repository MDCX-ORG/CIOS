// Package modbusbind — single source of truth for translating a
// pointmap.PointDef into a modbus.Binding. Merged in PRMT-030 §A
// from gateway.bindingFromProtocol and cmd/cios-modbus-driver.
// pointDefToBinding, both of which were byte-for-byte equivalent
// (modulo the `missing register` / `register %T` / `table %v` error
// strings, preserved verbatim here). Edge plugins and the gateway
// both depend on this package; pkg/driver/modbus does not, which
// keeps the dependency graph one-way (gateway → modbusbind →
// driver/modbus), not circular.
package modbusbind

import (
	"fmt"
	"strconv"

	"github.com/yurimeng/cios/pkg/driver/modbus"
	"github.com/yurimeng/cios/pkg/pointmap"
)

// BuildFromPointDef reads the per-point Protocol map and produces a
// modbus.Binding. It is the only place that touches
// Protocol[register] / Protocol[table]; the field is opaque to
// pointmap (pointmap only validates its well-known keys).
//
// Behaviour (bit-for-bit with the prior duplicates, PRMT-030 §A.4):
//   - register missing or nil → "missing register"
//   - register supports int / int64 / float64 / string(ParseUint
//     base 10, 16-bit). Other types → "register has unexpected
//     type %T". On a string parse error, returns
//     "register %q: %v" (preserved so existing tests stay green).
//   - table defaults to "holding"; only "holding" and "input" are
//     accepted. Any other type or value →
//     "table must be holding|input, got %v".
//   - Writable stays false until M1 control wiring is in place
//     (PRMT-010 §2).
func BuildFromPointDef(pd pointmap.PointDef) (modbus.Binding, error) {
	raw, ok := pd.Protocol["register"]
	if !ok || raw == nil {
		return modbus.Binding{}, fmt.Errorf("missing register")
	}
	var reg uint16
	switch n := raw.(type) {
	case int:
		reg = uint16(n)
	case int64:
		reg = uint16(n)
	case float64:
		reg = uint16(n)
	case string:
		// yaml-v3 keeps block-scalar ints as int, but a quoted
		// "30021" comes through as string. Be lenient.
		v, err := strconv.ParseUint(n, 10, 16)
		if err != nil {
			return modbus.Binding{}, fmt.Errorf("register %q: %v", n, err)
		}
		reg = uint16(v)
	default:
		return modbus.Binding{}, fmt.Errorf("register has unexpected type %T", raw)
	}

	table := "holding"
	if t, ok := pd.Protocol["table"]; ok {
		s, ok := t.(string)
		if !ok || (s != "holding" && s != "input") {
			return modbus.Binding{}, fmt.Errorf("table must be holding|input, got %v", t)
		}
		table = s
	}

	// P722: Writable follows pointmap access=rw (was hard-false pre-Set).
	writable := pd.Access == "rw"
	return modbus.Binding{
		Point:    pd.Point,
		Table:    table,
		Register: reg,
		Writable: writable,
	}, nil
}
