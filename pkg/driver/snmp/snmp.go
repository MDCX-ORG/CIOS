// Package snmp implements a pull-style SNMP v2c driver that satisfies
// driver.Driver. It is the v2c companion to the modbus TCP driver
// (pkg/driver/modbus) and covers the same conformance slice
// (spec-005 §4 C1–C4, C6). Wire protocol details are owned by
// github.com/gosnmp/gosnmp so the only BER we hand-roll is in the
// companion simulator (pkg/driver/snmpsim).
//
// What this driver does NOT do (by design of PRMT-018):
//   - SNMPv3 / USM (M1+ follow-up)
//   - trap / Subscribe (covered by C5; net-new driver surface)
//   - GETBULK / batched GET (one OID per request matches modbus's
//     "one register per request" posture; gateway owns scheduling)
//   - go-plugin wrapping (M1 E1.3, reuses pkg/plugindriver)
//
// Quality / connection state are reported via Health; LastSuccess is
// only stamped when a full Collect round finishes with zero suspect
// samples (matches the "fully good round" semantics L61 / spec-006
// §4 needs for staleness detection). Write success does not refresh
// LastSuccess.
package snmp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/yurimeng/cios/pkg/driver"
)

// --- Sentinels --------------------------------------------------------------

// ErrNotWritable is returned by Write when the target point either
// is not in the binding table or is in the table but flagged
// read-only (Writable=false or Kind not set).
var ErrNotWritable = errors.New("snmp: point not writable")

// ErrExpired is returned by Write when ControlCommand.TTL is
// non-positive at the moment the driver receives the command.
var ErrExpired = errors.New("snmp: command ttl expired")

// ErrAuth is returned by Init when the agent rejected the
// community string (RFC 3416 error-status = authorizationError).
// Init treats this as a configuration error (fatal) and does not
// auto-retry; network errors are returned separately so the
// gateway can fall back to a network-class retry.
var ErrAuth = errors.New("snmp: authentication failed")

// errConnLost is the in-driver sentinel for "connection-level
// failure": the underlying UDP socket is no longer usable, the
// read/write deadline elapsed, or the agent returned a framing
// error. Collect triggers a single in-round reconnect on this.
var errConnLost = errors.New("snmp: connection lost")

// errInvalidBinding is returned by Init when the static binding-
// table check fails. No UDP socket is opened in this case.
var errInvalidBinding = errors.New("snmp: invalid binding")

// --- Protocol constants -----------------------------------------------------

const (
	defaultCommunity = "public"
	defaultPort      = 161

	connectTimeout = 2 * time.Second
	probeTimeout   = 2 * time.Second

	// validKinds lists the integer encodings the driver accepts
	// for SET. We only decode the integer variants the conformance
	// test exercises; a wider set (OctetString, IPAddress, etc.) is
	// out of scope for v2c control writes per the prompt.
	kindInteger    = "integer"
	kindGauge      = "gauge"
	kindCounter    = "counter"
	kindTimeticks  = "timeticks"
	kindCounter64  = "counter64"
	kindUinteger32 = "uinteger32"
)

// validOID matches a dotted-decimal OID with at least two arcs; the
// driver accepts a leading '.' for callers that hand in
// ".1.3.6.1.4.1.9" form (a common SNMP convention).
var validOID = regexp.MustCompile(`^\.?\d+(\.\d+)+$`)

// --- Public types -----------------------------------------------------------

// Binding ties one telemetry point address to one SNMP object (OID).
// The driver does NOT validate Point against the CIOS point-path
// vocabulary — that is the gateway's job (pkg/cpath, pkg/pointmap).
// The driver treats Point as an opaque label it copies into every
// emitted Sample and matches verbatim in Write.
type Binding struct {
	Point    string
	OID      string
	Writable bool
	// Kind selects the integer encoding for SET: "integer" (signed
	// 32-bit), "gauge" (Gauge32), "counter" (Counter32),
	// "timeticks" (TimeTicks), "counter64" (Counter64),
	// "uinteger32" (Uinteger32). Writable=true requires a non-empty
	// Kind; Writable=false requires empty Kind.
	Kind string
}

// Driver is an SNMP v2c client implementing driver.Driver. It owns
// one *gosnmp.GoSNMP session and serialises all I/O behind mu;
// Collect, Write, and Health may be called concurrently from the
// gateway.
type Driver struct {
	bindings []Binding

	mu        sync.Mutex
	endpoint  string
	community string
	sess      *gosnmp.GoSNMP

	connected   bool
	lastSuccess time.Time
	errorCount  int
	detail      string
}

// New returns a Driver bound to the given point/OID table. The
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

// --- Driver methods ---------------------------------------------------------

// Init validates the binding table, parses community/endpoint, and
// dials the agent. On dial failure the binding state is retained so
// a later Collect can reconnect transparently; on binding-
// validation failure the driver does not even try to connect.
func (d *Driver) Init(ctx context.Context, cfg driver.DriverConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	// --- Static binding-table validation ---
	if len(d.bindings) == 0 {
		return fmt.Errorf("%w: empty binding table", errInvalidBinding)
	}
	seen := make(map[string]struct{}, len(d.bindings))
	for i, b := range d.bindings {
		if b.Point == "" {
			return fmt.Errorf("%w: binding[%d] empty Point", errInvalidBinding, i)
		}
		if b.OID == "" || !validOID.MatchString(b.OID) {
			return fmt.Errorf("%w: binding[%d] %q invalid OID %q", errInvalidBinding, i, b.Point, b.OID)
		}
		if _, dup := seen[b.Point]; dup {
			return fmt.Errorf("%w: binding[%d] %q duplicate Point", errInvalidBinding, i, b.Point)
		}
		seen[b.Point] = struct{}{}
		if b.Writable {
			if !validKind(b.Kind) {
				return fmt.Errorf("%w: binding[%d] %q invalid Kind %q (want integer|gauge|counter|timeticks|counter64|uinteger32)", errInvalidBinding, i, b.Point, b.Kind)
			}
		} else {
			if b.Kind != "" {
				return fmt.Errorf("%w: binding[%d] %q Kind %q set on read-only point", errInvalidBinding, i, b.Point, b.Kind)
			}
		}
	}

	// --- Options + endpoint parsing ---
	community := cfg.Options["community"]
	if community == "" {
		community = defaultCommunity
	}
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("snmp: parse endpoint %q: %w", cfg.Endpoint, err)
	}

	// --- Reset state, drop any prior session ---
	if d.sess != nil {
		_ = d.sess.Close()
		d.sess = nil
	}
	d.endpoint = endpoint
	d.community = community
	d.connected = false
	d.lastSuccess = time.Time{}
	d.errorCount = 0
	d.detail = ""

	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("snmp: split endpoint: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("snmp: parse port %q: %w", portStr, err)
	}

	sess := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port),
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   connectTimeout,
		Retries:   1,
		MaxOids:   gosnmp.MaxOids,
	}
	if err := sess.Connect(); err != nil {
		d.detail = err.Error()
		return fmt.Errorf("snmp: connect %s: %w", endpoint, err)
	}
	d.sess = sess

	// --- Probe with one GET on bindings[0].OID ---
	// gosnmp sessions are not concurrent-safe: Close must not race
	// with an in-flight Get (CODE-SCAN-2026-07-16 §2.1). Always join
	// the probe goroutine before Close. sess.Timeout bounds Get so
	// the join cannot hang past connectTimeout*(Retries+1).
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probeOIDs := []string{normalizeOID(d.bindings[0].OID)}
	type result struct {
		pkt *gosnmp.SnmpPacket
		err error
	}
	done := make(chan result, 1)
	go func() {
		pkt, e := sess.Get(probeOIDs)
		done <- result{pkt, e}
	}()
	var pkt *gosnmp.SnmpPacket
	select {
	case <-probeCtx.Done():
		// Join probe before Close — never Close while Get is in-flight.
		<-done
		_ = sess.Close()
		d.sess = nil
		return probeCtx.Err()
	case r := <-done:
		pkt, err = r.pkt, r.err
	}

	if err != nil {
		// Network-class failure. Per the prompt, this is recoverable
		// — the driver keeps the session-less state and returns the
		// error so the gateway can back off and retry Collect.
		d.detail = err.Error()
		_ = sess.Close()
		d.sess = nil
		return fmt.Errorf("snmp: init probe: %w", err)
	}
	if pkt != nil && pkt.Error == gosnmp.AuthorizationError {
		// Community mismatch is a configuration error. Fatal:
		// close the session and return ErrAuth so callers can
		// distinguish it from network failure (C1).
		d.detail = "authorizationError"
		_ = sess.Close()
		d.sess = nil
		return ErrAuth
	}
	// All other PDU-level errors (noSuchInstance, etc.) are fine
	// for a probe: the agent is reachable and the community is
	// accepted.
	d.connected = true
	return nil
}

// Discover has no concept in SNMP GET-only mode; the driver returns
// ErrNotSupported. A future PR can implement an OID-walk discovery.
func (d *Driver) Discover(ctx context.Context) ([]driver.AssetCandidate, error) {
	return nil, driver.ErrNotSupported
}

// Subscribe is unsupported: this driver is pull-based and the
// gateway polls via Collect. (SNMP trap / inform, when added later,
// will live in a separate streaming path.)
func (d *Driver) Subscribe(ctx context.Context, ch chan<- driver.Sample) error {
	return driver.ErrNotSupported
}

// Collect reads every binding once and returns one Sample per
// binding. Single-point failures (noSuchInstance, non-numeric
// type, per-request timeout) become Quality=suspect samples; the
// round continues. A connection-level error triggers a single
// in-round reconnect; if that reconnect fails, the remaining
// bindings are emitted as suspect and Collect returns (nil error).
// Collect never returns a non-nil error for device-level trouble —
// error reporting is via Health.ErrorCount + Health.Detail. L61.
func (d *Driver) Collect(ctx context.Context) ([]driver.Sample, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	samples := make([]driver.Sample, len(d.bindings))

	if d.sess == nil {
		if err := d.reconnect(); err != nil {
			d.errorCount++
			now := time.Now().UTC()
			for i, b := range d.bindings {
				samples[i] = driver.Sample{Point: b.Point, Ts: now, Quality: driver.QualitySuspect}
			}
			return samples, nil
		}
	}

	allGood := true
	for i := 0; i < len(d.bindings); i++ {
		b := d.bindings[i]
		oid := normalizeOID(b.OID)
		pkt, err := d.sess.Get([]string{oid})
		if err != nil {
			// Per-request transport error. Treat as connection
			// class: this point is suspect, the connection is
			// presumed broken. Reconnect once: success continues
			// the round, failure fills the rest with suspect and
			// short-circuits.
			d.recordErr(err)
			// Mark the driver offline immediately so any Health
			// snapshot taken in the middle of the round reflects
			// the bad state. A subsequent successful Get on the
			// reconnected session re-arms Connected.
			d.connected = false
			samples[i] = suspectSample(b.Point)
			allGood = false
			if rerr := d.reconnect(); rerr != nil {
				// Hard connection loss: this round cannot recover
				// on the same endpoint. Fill the rest with
				// suspect and short-circuit.
				d.fillSuspect(samples, i+1)
				return samples, nil
			}
			continue
		}
		// Get succeeded — the session is healthy. This also
		// re-arms Connected after a mid-round reconnect.
		d.connected = true
		if pkt == nil || len(pkt.Variables) == 0 {
			d.recordErr(fmt.Errorf("snmp: empty response for %s", oid))
			samples[i] = suspectSample(b.Point)
			allGood = false
			continue
		}
		v := pkt.Variables[0]
		if v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance || v.Type == gosnmp.EndOfMibView {
			d.recordErr(fmt.Errorf("snmp: %s not present at agent (type=%s)", oid, v.Type))
			samples[i] = suspectSample(b.Point)
			allGood = false
			continue
		}
		val, ok := numericToFloat(v.Value)
		if !ok {
			// Non-numeric value (OctetString, IPAddress, etc.) —
			// the driver spec says it is a suspect sample, not a
			// driver-level error. This is the SNMP equivalent of
			// the modbus "non-numeric register" branch.
			d.recordErr(fmt.Errorf("snmp: %s non-numeric type %s", oid, v.Type))
			samples[i] = suspectSample(b.Point)
			allGood = false
			continue
		}
		samples[i] = driver.Sample{
			Point:   b.Point,
			Value:   val,
			Ts:      time.Now().UTC(),
			Quality: driver.QualityGood,
		}
	}
	if allGood {
		d.lastSuccess = time.Now().UTC()
	}
	// Connected state follows the last Get we observed: a failure
	// sets it to false, a success re-arms it. The last binding's
	// outcome is the canonical Health signal. (The hard-loss case
	// returns early with connected=false set above.)
	return samples, nil
}

// to confirm. Result.Accepted = true requires the SET response to
// report error-status=NoError. Lookup / writability / TTL failures
// short-circuit before any I/O. A successful Write does NOT refresh
// LastSuccess (L61 contract; see TestWriteDoesNotRefreshLastSuccess).
func (d *Driver) Write(ctx context.Context, cmd driver.ControlCommand) (driver.ControlResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return driver.ControlResult{}, err
	}
	if cmd.TTL <= 0 {
		return driver.ControlResult{}, fmt.Errorf("%w: %s", ErrExpired, cmd.Point)
	}
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
	oid := normalizeOID(found.OID)

	if d.sess == nil {
		if err := d.reconnect(); err != nil {
			d.errorCount++
			return driver.ControlResult{Accepted: false}, fmt.Errorf("snmp: connect %s: %w", cmd.Point, err)
		}
	}

	// Build the SET pdu. gosnmp encodes integer-like values into the
	// matching BER tag from the SnmpPDU.Type field; we round-trip
	// the float64 through the same numeric kind the binding
	// declared. Narrowing is intentional: Counter32 / Gauge32 /
	// TimeTicks are 32-bit unsigned, and the conformance tests
	// only round-trip values in the [0, 0xFFFF] range. Anything
	// out of range is clipped, which is the documented behaviour
	// of v2c control writes in this driver.
	iv, pdutype, err := encodeForKind(cmd.Value, found.Kind)
	if err != nil {
		return driver.ControlResult{Accepted: false}, err
	}
	setPDU := gosnmp.SnmpPDU{Name: oid, Type: pdutype, Value: iv}
	setResp, err := d.sess.Set([]gosnmp.SnmpPDU{setPDU})
	if err != nil {
		d.recordErr(err)
		return driver.ControlResult{Accepted: false}, fmt.Errorf("snmp: write %s: %w", cmd.Point, err)
	}
	if setResp == nil || setResp.Error != gosnmp.NoError {
		es := gosnmp.SNMPError(0)
		if setResp != nil {
			es = setResp.Error
		}
		d.recordErr(fmt.Errorf("snmp: write %s error-status %d", cmd.Point, es))
		return driver.ControlResult{Accepted: false}, fmt.Errorf("snmp: write %s error-status %d", cmd.Point, es)
	}

	// Readback.
	rbPkt, err := d.sess.Get([]string{oid})
	if err != nil {
		d.recordErr(err)
		return driver.ControlResult{Accepted: true}, fmt.Errorf("snmp: readback %s: %w", cmd.Point, err)
	}
	if rbPkt == nil || len(rbPkt.Variables) == 0 {
		return driver.ControlResult{Accepted: true}, fmt.Errorf("snmp: readback %s empty", cmd.Point)
	}
	val, _ := numericToFloat(rbPkt.Variables[0].Value)
	// LastSuccess intentionally NOT touched here. See package doc.
	return driver.ControlResult{Accepted: true, Readback: val, ReadbackTs: time.Now().UTC()}, nil
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

// reconnect tears down the current session (if any) and dials the
// stored endpoint. Caller holds d.mu. A fresh *gosnmp.GoSNMP is
// built each time so a stale socket state can never leak across
// reconnect attempts.
func (d *Driver) reconnect() error {
	if d.sess != nil {
		_ = d.sess.Close()
		d.sess = nil
	}
	if d.endpoint == "" {
		d.connected = false
		return errConnLost
	}
	host, portStr, err := net.SplitHostPort(d.endpoint)
	if err != nil {
		d.connected = false
		d.detail = err.Error()
		return errConnLost
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		d.connected = false
		d.detail = err.Error()
		return errConnLost
	}
	sess := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port),
		Community: d.community,
		Version:   gosnmp.Version2c,
		Timeout:   connectTimeout,
		Retries:   1,
		MaxOids:   gosnmp.MaxOids,
	}
	if err := sess.Connect(); err != nil {
		d.connected = false
		d.detail = err.Error()
		return err
	}
	d.sess = sess
	// Do NOT flip d.connected here: reconnect only proves the
	// socket is open, not that the agent is actually responding.
	// The next Get on the new session is the oracle: success
	// re-arms Connected, failure leaves it false.
	return nil
}

// recordErr increments the cumulative error counter and updates the
// human-readable detail line. Called from every error path.
func (d *Driver) recordErr(err error) {
	d.errorCount++
	d.detail = err.Error()
}

// fillSuspect writes suspect samples into samples[from:]. Used when
// a connection-level error in the middle of a round cannot recover.
func (d *Driver) fillSuspect(samples []driver.Sample, from int) {
	now := time.Now().UTC()
	for j := from; j < len(samples); j++ {
		samples[j] = driver.Sample{Point: d.bindings[j].Point, Ts: now, Quality: driver.QualitySuspect}
	}
}

// suspectSample is the canonical zero-value suspect sample for a
// point; Value is intentionally 0.
func suspectSample(point string) driver.Sample {
	return driver.Sample{Point: point, Ts: time.Now().UTC(), Quality: driver.QualitySuspect}
}

// normalizeOID strips a leading '.' so gosnmp sees the bare dotted
// form it expects. The driver canonicalises on the way in so the
// probe and Collect look-ups agree.
func normalizeOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

// normalizeEndpoint appends ":161" when the endpoint is missing a
// port segment. This is the "default SNMP UDP port" convention.
// Uses net.SplitHostPort so the "missing port" detection is reliable
// for both IPv4 ("10.0.0.5") and bracketed IPv6 ("[::1]:161").
func normalizeEndpoint(ep string) (string, error) {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return "", fmt.Errorf("empty endpoint")
	}
	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		// Missing port. Two cases net.SplitHostPort rejects:
		//   - "10.0.0.5" (no colon at all) — IPv4 shorthand.
		//   - "::1" (unbracketed IPv6) — caller used IPv6 shorthand.
		// net.JoinHostPort adds the IPv6 brackets automatically, so
		// both reduce to the same path: append the default port.
		return net.JoinHostPort(ep, strconv.Itoa(defaultPort)), nil
	}
	if portStr == "" {
		// Defensive: SplitHostPort returned no error but no port —
		// treat as missing port.
		return net.JoinHostPort(host, strconv.Itoa(defaultPort)), nil
	}
	if _, perr := strconv.Atoi(portStr); perr != nil {
		return "", fmt.Errorf("endpoint %q: invalid port %q", ep, portStr)
	}
	return ep, nil
}

// validKind checks against the integer-type set the driver supports
// for SET.
func validKind(kind string) bool {
	switch kind {
	case kindInteger, kindGauge, kindCounter, kindTimeticks, kindCounter64, kindUinteger32:
		return true
	}
	return false
}

// encodeForKind narrows a float64 into the wire-side integer (and
// matching Asn1BER) for a Binding.Kind. Signed kinds clamp into the
// int32 range; unsigned kinds clamp into uint32 / uint64. We return
// int (not int32) for kind=integer because gosnmp's marshal switch
// only accepts byte or int — see marshal.go: marshalVarbind's
// Integer branch explicitly rejects other concrete types.
func encodeForKind(v float64, kind string) (any, gosnmp.Asn1BER, error) {
	switch kind {
	case kindInteger:
		if v < -2147483648 || v > 2147483647 {
			return nil, 0, fmt.Errorf("snmp: integer out of int32 range: %v", v)
		}
		return int(v), gosnmp.Integer, nil
	case kindGauge:
		if v < 0 || v > 4294967295 {
			return nil, 0, fmt.Errorf("snmp: gauge out of uint32 range: %v", v)
		}
		return uint32(v), gosnmp.Gauge32, nil
	case kindCounter:
		if v < 0 || v > 4294967295 {
			return nil, 0, fmt.Errorf("snmp: counter out of uint32 range: %v", v)
		}
		return uint32(v), gosnmp.Counter32, nil
	case kindTimeticks:
		if v < 0 || v > 4294967295 {
			return nil, 0, fmt.Errorf("snmp: timeticks out of uint32 range: %v", v)
		}
		return uint32(v), gosnmp.TimeTicks, nil
	case kindUinteger32:
		if v < 0 || v > 4294967295 {
			return nil, 0, fmt.Errorf("snmp: uinteger32 out of uint32 range: %v", v)
		}
		return uint32(v), gosnmp.Uinteger32, nil
	case kindCounter64:
		if v < 0 || v > 1.8446744073709552e19 {
			return nil, 0, fmt.Errorf("snmp: counter64 out of uint64 range: %v", v)
		}
		return uint64(v), gosnmp.Counter64, nil
	}
	return nil, 0, fmt.Errorf("snmp: unsupported Kind %q", kind)
}

// numericToFloat turns the any-typed Value that gosnmp returns into
// a float64. Integer kinds in gosnmp come back as int / int32 /
// uint32 / int64 / uint64; the only kinds the driver promises to
// surface are the ones the prompt §4.5 enumerates.
func numericToFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
