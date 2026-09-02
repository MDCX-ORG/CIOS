// Package modbus implements a pull-style Modbus TCP driver that
// satisfies driver.Driver. It is the reference driver for the CIOS
// gateway conformance suite (spec-005 §4 C1–C4, C6) and is deliberately
// minimal: one register per request (qty=1), single-connection mutex,
// fail-soft Collect that turns every device-level failure into
// Sample.Quality=suspect and never returns a non-nil error.
//
// What this driver does NOT do (by design of PRMT-009):
//   - batched / contiguous-range reads (the gateway owns scheduling)
//   - float / multi-register decoding (the gateway scales raw uint16s)
//   - Discover / Subscribe (pull-only protocol → driver.ErrNotSupported)
//   - go-plugin wrapping (M1 work)
//
// Quality / connection state are reported via Health; LastSuccess is
// only stamped when a full Collect round finishes with zero suspect
// samples (matches the "fully good round" semantics spec-006 §4 needs
// for staleness detection).
package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
)

// --- Sentinels --------------------------------------------------------------

// ErrNotWritable is returned by Write when the target point either is
// not in the binding table or is in the table but flagged read-only.
// The point name is wrapped with %w so callers can recover it via
// errors.Unwrap / Error() while still matching errors.Is.
var ErrNotWritable = errors.New("modbus: point not writable")

// ErrExpired is returned by Write when ControlCommand.TTL is
// non-positive at the moment the driver receives the command. The
// driver does not attempt to track elapsed queue time (it has no
// queue) — a non-positive TTL is the gateway's signal that the command
// must not be applied.
var ErrExpired = errors.New("modbus: command ttl expired")

// errConnLost is the in-driver sentinel for "connection-level
// failure": socket I/O error, framing error (bad protocol id, bad
// length), transaction-id mismatch, or read deadline expiry. These
// all corrupt the wire stream, so the driver closes the connection
// and tries to reconnect once within the same Collect round.
var errConnLost = errors.New("modbus: connection lost")

// errInvalidBinding is returned by Init when the static binding-table
// check fails. No TCP connection is attempted in this case.
var errInvalidBinding = errors.New("modbus: invalid binding")

// --- Protocol constants -----------------------------------------------------

const (
	fcReadHolding byte = 0x03
	fcReadInput   byte = 0x04
	fcWriteSingle byte = 0x06

	// exceptionMask is OR'd into the FC byte in an exception response.
	exceptionMask byte = 0x80

	// MBAP header is 6 bytes (txid 2 + protid 2 + len 2); the unit id
	// lives in the PDU section per the wire spec.
	mbapHeader = 6

	connectTimeout = 3 * time.Second
	requestTimeout = 1 * time.Second
)

// --- Public types -----------------------------------------------------------

// Binding ties one telemetry point address to one modbus register.
// The driver does NOT validate Point against the CIOS point-path
// vocabulary — that is the gateway's job (pkg/cpath, pkg/pointmap).
// The driver treats Point as an opaque label it copies into every
// emitted Sample and matches verbatim in Write.
type Binding struct {
	Point    string // full point address; opaque to the driver
	Table    string // "holding" → FC3 / FC6, "input" → FC4
	Register uint16 // register address (0..65535)
	Writable bool   // only meaningful for holding; input+Writable=true is rejected by Init
}

// Driver is a Modbus TCP client implementing driver.Driver. It owns
// one TCP connection and serialises all I/O behind mu; Collect,
// Write, and Health may all be called concurrently from the gateway.
type Driver struct {
	bindings []Binding

	mu       sync.Mutex
	endpoint string
	unitID   byte
	conn     net.Conn
	txID     uint16

	connected   bool
	lastSuccess time.Time
	errorCount  int
	detail      string
}

// New returns a Driver bound to the given point/register table. The
// returned value still needs Init before any other method becomes
// useful. The slice is copied so the caller may reuse the source.
func New(bindings []Binding) *Driver {
	return &Driver{
		bindings: append([]Binding(nil), bindings...),
	}
}

// Compile-time assertion: *Driver must satisfy driver.Driver. A
// signature drift in pkg/driver fails this line and surfaces the
// contract change at the call site.
var _ driver.Driver = (*Driver)(nil)

// modbusException is the typed error roundTrip returns when the peer
// answered with a well-formed exception (FC | 0x80). The connection
// is still usable; the caller treats this as a per-point suspect.
type modbusException struct{ code byte }

func (e modbusException) Error() string {
	return fmt.Sprintf("modbus: exception 0x%02X", e.code)
}

// isModbusException reports whether err is an application-level
// modbus exception (as opposed to a connection-level loss).
func isModbusException(err error) bool {
	var e modbusException
	return errors.As(err, &e)
}

// --- Driver methods ---------------------------------------------------------

// Init validates the binding table, parses options, and dials the
// endpoint. On dial failure the binding state is retained so a later
// Collect can reconnect transparently; on binding-validation failure
// the driver does not even try to connect.
func (d *Driver) Init(ctx context.Context, cfg driver.DriverConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	// Static binding-table validation. Anything that depends on the
	// table being well-formed (Write lookup, Collect dispatch) assumes
	// these checks have passed.
	if len(d.bindings) == 0 {
		return fmt.Errorf("%w: empty binding table", errInvalidBinding)
	}
	seen := make(map[string]struct{}, len(d.bindings))
	for i, b := range d.bindings {
		if b.Point == "" {
			return fmt.Errorf("%w: binding[%d] empty Point", errInvalidBinding, i)
		}
		if b.Table != "holding" && b.Table != "input" {
			return fmt.Errorf("%w: binding[%d] %q invalid Table %q", errInvalidBinding, i, b.Point, b.Table)
		}
		if b.Table == "input" && b.Writable {
			return fmt.Errorf("%w: binding[%d] %q input register cannot be writable", errInvalidBinding, i, b.Point)
		}
		if _, dup := seen[b.Point]; dup {
			return fmt.Errorf("%w: binding[%d] %q duplicate Point", errInvalidBinding, i, b.Point)
		}
		seen[b.Point] = struct{}{}
	}

	// unit_id parsing (default 1). An empty string in the map is
	// treated as "not provided".
	uidStr := cfg.Options["unit_id"]
	if uidStr == "" {
		uidStr = "1"
	}
	uid, err := strconv.ParseUint(uidStr, 10, 8)
	if err != nil {
		return fmt.Errorf("modbus: parse unit_id %q: %w", uidStr, err)
	}

	// Reset connection-level + health state. The driver is allowed to
	// be re-Init'd with a new endpoint (spec-005 §2), so we drop any
	// existing socket first.
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	d.endpoint = cfg.Endpoint
	d.unitID = byte(uid)
	d.txID = 0
	d.connected = false
	d.lastSuccess = time.Time{}
	d.errorCount = 0
	d.detail = ""

	// Dial. Failure here is recoverable — the driver retains the
	// binding/endpoint state so the gateway's next Collect will
	// attempt reconnect on its own.
	c, dialErr := net.DialTimeout("tcp", cfg.Endpoint, connectTimeout)
	if dialErr != nil {
		d.detail = dialErr.Error()
		return fmt.Errorf("modbus: dial %s: %w", cfg.Endpoint, dialErr)
	}
	d.conn = c
	d.connected = true
	return nil
}

// Discover has no concept in modbus TCP (no broadcast / enumeration)
// and is therefore unsupported.
func (d *Driver) Discover(ctx context.Context) ([]driver.AssetCandidate, error) {
	return nil, driver.ErrNotSupported
}

// Subscribe is unsupported: modbus is strictly pull-based and the
// gateway polls via Collect.
func (d *Driver) Subscribe(ctx context.Context, ch chan<- driver.Sample) error {
	return driver.ErrNotSupported
}

// Collect reads every binding once and returns one Sample per binding.
// Single-point failures (modbus exception or per-request socket
// trouble) become Quality=suspect samples; the round continues. A
// connection-level error triggers a single in-round reconnect; if
// that reconnect fails, the remaining bindings are emitted as
// suspect and Collect returns (nil error). Collect never returns a
// non-nil error for device-level trouble — error reporting is via
// Health.ErrorCount + Health.Detail.
func (d *Driver) Collect(ctx context.Context) ([]driver.Sample, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	samples := make([]driver.Sample, len(d.bindings))

	// Ensure a connection at the start of the round. A failure here
	// short-circuits the whole round as suspect.
	if d.conn == nil {
		if err := d.reconnect(); err != nil {
			d.errorCount++
			now := time.Now()
			for i, b := range d.bindings {
				samples[i] = driver.Sample{Point: b.Point, Ts: now, Quality: driver.QualitySuspect}
			}
			return samples, nil
		}
	}

	allGood := true
	for i := 0; i < len(d.bindings); i++ {
		b := d.bindings[i]
		var fc byte
		if b.Table == "holding" {
			fc = fcReadHolding
		} else {
			fc = fcReadInput
		}
		pdu := []byte{fc, byte(b.Register >> 8), byte(b.Register), 0x00, 0x01}
		resp, err := d.roundTrip(pdu)
		switch {
		case err == nil:
			// resp layout: FC(1) + byteCount(1) + 2 bytes for qty=1.
			if len(resp) < 4 {
				// Malformed but well-framed response — treat as
				// connection-level to be safe.
				d.recordErr(fmt.Errorf("modbus: short read response"))
				samples[i] = suspectSample(b.Point)
				allGood = false
				d.closeConn()
				if rerr := d.reconnect(); rerr != nil {
					d.fillSuspect(samples, i+1)
					return samples, nil
				}
				continue
			}
			val := binary.BigEndian.Uint16(resp[2:4])
			samples[i] = driver.Sample{
				Point:   b.Point,
				Value:   float64(val),
				Ts:      time.Now(),
				Quality: driver.QualityGood,
			}
		case isModbusException(err):
			// Application-level exception (e.g. masked register) —
			// just this point suspect, connection survives.
			d.recordErr(err)
			samples[i] = suspectSample(b.Point)
			allGood = false
		default:
			// Connection-level loss: this point suspect, then try to
			// reconnect once. Reconnect failure → rest of the round
			// is suspect.
			d.recordErr(err)
			samples[i] = suspectSample(b.Point)
			allGood = false
			d.closeConn()
			if rerr := d.reconnect(); rerr != nil {
				d.fillSuspect(samples, i+1)
				return samples, nil
			}
		}
	}
	if allGood {
		d.lastSuccess = time.Now()
	}
	return samples, nil
}

// Write executes one FC6 write to the binding's register and then
// FC3-reads it back to confirm. Result.Accepted = true requires both
// to succeed; any device-level trouble surfaces as (Accepted=false,
// non-nil error). Lookup / writability / TTL failures short-circuit
// before any I/O.
func (d *Driver) Write(ctx context.Context, cmd driver.ControlCommand) (driver.ControlResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return driver.ControlResult{}, err
	}

	// Locate the binding. A missing binding and a non-writable
	// binding produce the same sentinel — both mean "the gateway must
	// not present this point as settable", and the wrapped string
	// disambiguates for humans.
	var found *Binding
	for i := range d.bindings {
		if d.bindings[i].Point == cmd.Point {
			found = &d.bindings[i]
			break
		}
	}
	if found == nil {
		return driver.ControlResult{}, fmt.Errorf("%w: %s (not in binding table)", ErrNotWritable, cmd.Point)
	}
	if !found.Writable {
		return driver.ControlResult{}, fmt.Errorf("%w: %s", ErrNotWritable, cmd.Point)
	}
	if cmd.TTL <= 0 {
		return driver.ControlResult{}, fmt.Errorf("%w: %s", ErrExpired, cmd.Point)
	}

	if d.conn == nil {
		if err := d.reconnect(); err != nil {
			d.errorCount++
			return driver.ControlResult{Accepted: false}, fmt.Errorf("modbus: connect %s: %w", cmd.Point, err)
		}
	}

	val := uint16(cmd.Value)
	wpdu := []byte{fcWriteSingle, byte(found.Register >> 8), byte(found.Register), byte(val >> 8), byte(val)}
	if _, err := d.roundTrip(wpdu); err != nil {
		d.recordErr(err)
		if !isModbusException(err) {
			d.closeConn()
		}
		return driver.ControlResult{Accepted: false}, fmt.Errorf("modbus: write %s: %w", cmd.Point, err)
	}

	rpdu := []byte{fcReadHolding, byte(found.Register >> 8), byte(found.Register), 0x00, 0x01}
	resp, err := d.roundTrip(rpdu)
	if err != nil {
		d.recordErr(err)
		if !isModbusException(err) {
			d.closeConn()
		}
		return driver.ControlResult{Accepted: false}, fmt.Errorf("modbus: readback %s: %w", cmd.Point, err)
	}
	if len(resp) < 4 {
		d.recordErr(fmt.Errorf("modbus: short readback response"))
		d.closeConn()
		return driver.ControlResult{Accepted: false}, fmt.Errorf("modbus: readback %s: malformed", cmd.Point)
	}
	rb := binary.BigEndian.Uint16(resp[2:4])
	// Note: we deliberately do NOT touch d.lastSuccess here.
	// spec-004 §4 reserves LastSuccess for "fully-good Collect round"
	// so staleness detection survives a "control loop still works,
	// telemetry is dead" scenario. Refreshing it from a successful
	// Write would mask an ongoing telemetry outage.
	return driver.ControlResult{Accepted: true, Readback: float64(rb), ReadbackTs: time.Now()}, nil
}

// Health returns a snapshot of internal counters. Safe to call
// concurrently with Collect / Write; the mutex serialises access.
func (d *Driver) Health(ctx context.Context) driver.DriverHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	return driver.DriverHealth{
		Connected:   d.connected,
		LastSuccess: d.lastSuccess,
		ErrorCount:  d.errorCount,
		Detail:      d.detail,
	}
}

// --- internals --------------------------------------------------------------

// nextTxID returns a monotonically increasing transaction id. uint16
// wraps at 65535 → 0 by construction, satisfying the long-running
// requirement (≥70k requests without misorder).
func (d *Driver) nextTxID() uint16 {
	d.txID++
	return d.txID
}

// closeConn drops the current socket and flips connected=false.
// Safe to call when d.conn is already nil.
func (d *Driver) closeConn() {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	d.connected = false
}

// reconnect closes any existing socket and dials the stored endpoint
// with the same 3s budget Init uses. The caller holds d.mu.
func (d *Driver) reconnect() error {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	if d.endpoint == "" {
		d.connected = false
		return errors.New("modbus: no endpoint configured")
	}
	c, err := net.DialTimeout("tcp", d.endpoint, connectTimeout)
	if err != nil {
		d.connected = false
		d.detail = err.Error()
		return err
	}
	d.conn = c
	d.connected = true
	return nil
}

// recordErr increments the cumulative error counter and updates the
// human-readable detail line. Called from every error path.
func (d *Driver) recordErr(err error) {
	d.errorCount++
	d.detail = err.Error()
}

// fillSuspect writes a suspect sample into samples[from:]. Used when
// a connection-level error in the middle of a round cannot recover.
func (d *Driver) fillSuspect(samples []driver.Sample, from int) {
	now := time.Now()
	for j := from; j < len(samples); j++ {
		samples[j] = driver.Sample{Point: d.bindings[j].Point, Ts: now, Quality: driver.QualitySuspect}
	}
}

// suspectSample is the canonical zero-value suspect sample for a
// point; Value is intentionally 0 (spec-006 §4: suspect samples are
// not consumed as telemetry, so the value carries no meaning).
func suspectSample(point string) driver.Sample {
	return driver.Sample{Point: point, Ts: time.Now(), Quality: driver.QualitySuspect}
}

// roundTrip sends pdu and returns the response PDU (bytes after the
// unit-id field). Three error classes:
//
//   - nil:                    response was a normal echo of fc
//   - modbusException{code}:  peer answered with FC|0x80 + code; the
//     connection is fine, the caller marks the
//     point suspect
//   - errConnLost / wrapped:  socket I/O failed, framing was wrong,
//     txid did not echo, or the read deadline
//     expired; the caller must close the socket
//     and try to reconnect
//
// roundTrip must be called with d.mu held and d.conn non-nil.
func (d *Driver) roundTrip(pdu []byte) ([]byte, error) {
	if d.conn == nil {
		return nil, errConnLost
	}
	txID := d.nextTxID()

	frame := make([]byte, mbapHeader+1+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], txID)
	// proto id = 0 (already zero from make)
	binary.BigEndian.PutUint16(frame[4:6], uint16(1+len(pdu))) // unit id + PDU
	frame[6] = d.unitID
	copy(frame[7:], pdu)

	deadline := time.Now().Add(requestTimeout)
	if err := d.conn.SetWriteDeadline(deadline); err != nil {
		return nil, fmt.Errorf("%w: set write deadline: %v", errConnLost, err)
	}
	if _, err := d.conn.Write(frame); err != nil {
		return nil, fmt.Errorf("%w: write: %v", errConnLost, err)
	}
	if err := d.conn.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("%w: set read deadline: %v", errConnLost, err)
	}

	var hdr [mbapHeader]byte
	if _, err := io.ReadFull(d.conn, hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: read header: %v", errConnLost, err)
	}
	rxID := binary.BigEndian.Uint16(hdr[0:2])
	protoID := binary.BigEndian.Uint16(hdr[2:4])
	length := binary.BigEndian.Uint16(hdr[4:6])
	if protoID != 0 {
		return nil, fmt.Errorf("%w: non-zero proto id 0x%04X", errConnLost, protoID)
	}
	if length < 2 || length > 253 {
		return nil, fmt.Errorf("%w: bad length %d", errConnLost, length)
	}
	if rxID != txID {
		return nil, fmt.Errorf("%w: txid mismatch want=%d got=%d", errConnLost, txID, rxID)
	}

	rest := make([]byte, length)
	if _, err := io.ReadFull(d.conn, rest); err != nil {
		return nil, fmt.Errorf("%w: read body: %v", errConnLost, err)
	}
	// rest = unitID(1) + PDU
	respPDU := rest[1:]
	if len(respPDU) < 1 {
		return nil, fmt.Errorf("%w: empty PDU", errConnLost)
	}
	fc := respPDU[0]
	if fc&exceptionMask != 0 {
		if len(respPDU) < 2 {
			return nil, fmt.Errorf("%w: truncated exception PDU", errConnLost)
		}
		return nil, modbusException{code: respPDU[1]}
	}
	return respPDU, nil
}
