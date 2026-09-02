// Package driver defines the CIOS gateway-driver interface and the data
// types that flow across it (spec-005 §2). The interface is the in-process
// shape; the out-of-process plugin wrapper (hashicorp/go-plugin or
// equivalent) is M1 work and is intentionally out of scope here.
//
// This package is **type-only**: no logic, no I/O, no goroutines. Anything
// that ships compiled code or speaks a wire protocol lives in a
// sub-package (e.g. pkg/driver/modbussim) or in a downstream driver.
package driver

import (
	"context"
	"errors"
	"time"
)

// Quality is the per-sample data-quality marker a driver attaches to
// every read. The four values are the subset spec-006 §4 recognises for
// downstream pipeline decisions (substitute / alert / drop).
type Quality string

const (
	QualityGood        Quality = "good"        // device read succeeded within budget
	QualityStale       Quality = "stale"       // last known good, beyond the freshness window
	QualitySuspect     Quality = "suspect"     // read succeeded but a self-check failed (CRC, range, etc.)
	QualitySubstituted Quality = "substituted" // value is a manual entry / computed stand-in, not a device read
)

// Sample is one telemetry reading flowing driver → gateway. Ts is the
// device-side timestamp; the gateway attaches its own receive-side
// timestamp when it consumes the sample. A driver may downgrade Quality
// to QualitySuspect on receipt of the sample if it cannot confirm
// freshness (spec-006 §4.2).
type Sample struct {
	Point   string // full point address (spec-002 §1)
	Value   float64
	Ts      time.Time // device time stamp
	Quality Quality
}

// ControlCommand is one write flowing gateway → driver. RequestID is the
// idempotency key (spec-004 §5); a driver must dedupe retries with the
// same key. TTL bounds how long the command may sit in a driver queue
// before being discarded (spec-006 §5.4): a driver that cannot execute
// within TTL must return an error rather than applying late.
type ControlCommand struct {
	Point     string
	Value     float64
	RequestID string
	TTL       time.Duration
}

// ControlResult is what a driver returns from a Write. Accepted is the
// primary signal: false means the driver refused and the gateway must
// surface that to the operator. Readback / ReadbackTs are the
// post-write read of the same point (modbus read-after-write or
// equivalent), if the driver can provide them; zero values mean "not
// available" and the gateway must not treat them as an error.
type ControlResult struct {
	Accepted   bool
	Readback   float64
	ReadbackTs time.Time
}

// DriverHealth is what a driver reports when asked. Connected is the
// primary health bit; LastSuccess is the time of the most recent
// successful Collect / Subscribe-read. ErrorCount is cumulative since
// Init. Detail is a human-readable diagnostic line (may be empty).
type DriverHealth struct {
	Connected   bool
	LastSuccess time.Time
	ErrorCount  int
	Detail      string
}

// DriverConfig is the per-instance configuration a driver needs to
// bring up its connection. Endpoint is transport-level ("127.0.0.1:1502"
// for modbus TCP, "/dev/ttyUSB0" for serial, etc.); Options is the
// protocol-specific bag (unit_id, baud, parity, slave timeout, ...).
// Drivers own the schema of Options; the gateway treats it as opaque.
type DriverConfig struct {
	Endpoint string
	Options  map[string]string
}

// AssetCandidate is one device Discover found. Serial is the vendor's
// unique key (modbus serial, MAC, OID sysName, ...); Hints are
// free-form metadata the driver collected (firmware, model, ...) that
// the gateway can use to pre-fill an asset form.
type AssetCandidate struct {
	Type   string
	Serial string
	Hints  map[string]string
}

// ErrNotSupported is the sentinel drivers return from optional
// methods (Discover, Subscribe) when the underlying protocol has no
// equivalent concept. Callers use errors.Is(err, ErrNotSupported) to
// branch; a driver that genuinely fails to Discover because the device
// is offline returns a different, more specific error.
var ErrNotSupported = errors.New("driver: operation not supported")

// Driver is the contract every CIOS driver must satisfy. The lifecycle
// is Init → (Discover? → Collect* / Subscribe*) → Write* → Health, all
// driven by the gateway; drivers do not start their own goroutines
// beyond what Subscribe and Collect need internally.
type Driver interface {
	// Init brings up the connection with cfg. Must be called before any
	// other method. Re-Init with a new cfg is allowed and is the
	// documented way to reconnect after a configuration change.
	Init(ctx context.Context, cfg DriverConfig) error

	// Discover enumerates devices visible on the bus. A driver for a
	// protocol that has no broadcast / enumeration concept returns
	// ErrNotSupported. Per-device errors must be reported per-element,
	// not as a single failure.
	Discover(ctx context.Context) ([]AssetCandidate, error)

	// Collect pulls a single batch of samples, poll-style. A driver for
	// a push-only protocol may return ErrNotSupported if Subscribe
	// covers all the same data; drivers that can do both should
	// implement Collect as a one-shot Subscribe read.
	Collect(ctx context.Context) ([]Sample, error)

	// Subscribe opens a streaming channel of samples. Drivers that are
	// strictly pull-based return ErrNotSupported. The channel is owned
	// by the driver; the gateway reads until the channel is closed or
	// the context is cancelled.
	Subscribe(ctx context.Context, ch chan<- Sample) error

	// Write executes one control command. The result reports whether
	// the device accepted the write and, when available, the
	// post-write readback (so the gateway can present a closed-loop
	// confirmation to the operator).
	Write(ctx context.Context, cmd ControlCommand) (ControlResult, error)

	// Health returns a snapshot of the driver's connectivity state.
	// Must be safe to call concurrently with Collect / Subscribe.
	Health(ctx context.Context) DriverHealth
}
