// Package plugindriver wires the in-process driver.Driver contract
// (spec-005 §2) onto a hashicorp/go-plugin gRPC channel so the gateway
// can host drivers in their own OS process (spec-005 §1, LOCKED L60).
// The package is split four ways: plugin.go owns the go-plugin
// HandshakeConfig and the GRPCPlugin shim; client.go is the gateway
// side that launches the subprocess and forwards driver.Driver calls
// over gRPC; server.go is the plugin side that wraps a real
// driver.Driver as the gRPC service; types.go is this file — the
// proto ↔ Go-struct conversion layer.
//
// The conversion helpers are deliberately one-shot pure functions
// with fixed signatures (PRMT-017 §4.5). The DriverHealth ↔
// DriverHealthProto mapping is the only non-obvious one: the wire
// names (healthy/message) differ from the Go field names
// (Connected/Detail), and the wire uses uint64 for the error counter
// while Go uses int — the mapping is centralised here so callers
// don't have to remember it.
package plugindriver

import (
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yurimeng/cios/pkg/driver"
	driverproto "github.com/yurimeng/cios/proto"
)

// SampleToProto converts an in-process Sample to its wire form. A
// zero Sample.Ts is encoded as a nil Timestamp so the receiver gets
// back the zero time.Time, matching the round-trip identity the
// gateway expects.
func SampleToProto(s driver.Sample) *driverproto.Sample {
	out := &driverproto.Sample{
		Point:   s.Point,
		Value:   s.Value,
		Quality: string(s.Quality),
	}
	if !s.Ts.IsZero() {
		out.Ts = timestamppb.New(s.Ts)
	}
	return out
}

// SampleFromProto is the inverse of SampleToProto. A nil proto or a
// nil/zero Ts decodes to a zero time.Time so suspect-style fixtures
// survive the round-trip.
func SampleFromProto(p *driverproto.Sample) driver.Sample {
	if p == nil {
		return driver.Sample{}
	}
	s := driver.Sample{
		Point:   p.GetPoint(),
		Value:   p.GetValue(),
		Quality: driver.Quality(p.GetQuality()),
	}
	if ts := p.GetTs(); ts != nil {
		s.Ts = ts.AsTime()
	}
	return s
}

// DriverConfigToProto copies the per-instance bring-up config onto
// the wire. A nil Options map encodes as a nil proto map; the
// receiver must treat both nil and empty as "no options" — that
// matches the modbus driver's Init contract.
func DriverConfigToProto(c driver.DriverConfig) *driverproto.DriverConfigProto {
	out := &driverproto.DriverConfigProto{Endpoint: c.Endpoint}
	if len(c.Options) > 0 {
		out.Options = make(map[string]string, len(c.Options))
		for k, v := range c.Options {
			out.Options[k] = v
		}
	}
	return out
}

// DriverConfigFromProto is the inverse of DriverConfigToProto.
func DriverConfigFromProto(p *driverproto.DriverConfigProto) driver.DriverConfig {
	if p == nil {
		return driver.DriverConfig{}
	}
	c := driver.DriverConfig{Endpoint: p.GetEndpoint()}
	if opts := p.GetOptions(); len(opts) > 0 {
		c.Options = make(map[string]string, len(opts))
		for k, v := range opts {
			c.Options[k] = v
		}
	}
	return c
}

// ControlCommandToProto wires a Write command onto the channel. TTL
// is a Duration, not a Timestamp — non-positive values survive the
// round-trip so the receiving driver can still reject "expired on
// arrival" commands (modbus.ErrExpired).
func ControlCommandToProto(c driver.ControlCommand) *driverproto.ControlCommandProto {
	return &driverproto.ControlCommandProto{
		Point:     c.Point,
		Value:     c.Value,
		RequestId: c.RequestID,
		Ttl:       durationpb.New(c.TTL),
	}
}

// ControlCommandFromProto is the inverse of ControlCommandToProto.
func ControlCommandFromProto(p *driverproto.ControlCommandProto) driver.ControlCommand {
	if p == nil {
		return driver.ControlCommand{}
	}
	c := driver.ControlCommand{
		Point:     p.GetPoint(),
		Value:     p.GetValue(),
		RequestID: p.GetRequestId(),
	}
	if ttl := p.GetTtl(); ttl != nil {
		c.TTL = ttl.AsDuration()
	}
	return c
}

// ControlResultToProto wires a Write reply onto the channel.
func ControlResultToProto(r driver.ControlResult) *driverproto.ControlResultProto {
	out := &driverproto.ControlResultProto{
		Accepted: r.Accepted,
		Readback: r.Readback,
	}
	if !r.ReadbackTs.IsZero() {
		out.ReadbackTs = timestamppb.New(r.ReadbackTs)
	}
	return out
}

// ControlResultFromProto is the inverse of ControlResultToProto.
func ControlResultFromProto(p *driverproto.ControlResultProto) driver.ControlResult {
	if p == nil {
		return driver.ControlResult{}
	}
	r := driver.ControlResult{
		Accepted: p.GetAccepted(),
		Readback: p.GetReadback(),
	}
	if ts := p.GetReadbackTs(); ts != nil {
		r.ReadbackTs = ts.AsTime()
	}
	return r
}

// DriverHealthToProto bridges the field-name + integer-width gap
// between the Go struct (Connected/Detail, int) and the wire
// (healthy/message, uint64). A negative ErrorCount is clamped to 0
// because the wire type is unsigned; that should never happen — the
// modbus driver's counter is monotonic — but the clamp keeps the
// conversion total.
func DriverHealthToProto(h driver.DriverHealth) *driverproto.DriverHealthProto {
	out := &driverproto.DriverHealthProto{
		Healthy: h.Connected,
		Message: h.Detail,
	}
	if h.ErrorCount > 0 {
		out.ErrorCount = uint64(h.ErrorCount)
	}
	if !h.LastSuccess.IsZero() {
		out.LastSuccess = timestamppb.New(h.LastSuccess)
	}
	return out
}

// DriverHealthFromProto is the inverse of DriverHealthToProto.
func DriverHealthFromProto(p *driverproto.DriverHealthProto) driver.DriverHealth {
	if p == nil {
		return driver.DriverHealth{}
	}
	h := driver.DriverHealth{
		Connected:  p.GetHealthy(),
		Detail:     p.GetMessage(),
		ErrorCount: int(p.GetErrorCount()),
	}
	if ts := p.GetLastSuccess(); ts != nil {
		h.LastSuccess = ts.AsTime()
	}
	return h
}
