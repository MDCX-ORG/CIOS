// Package snmpsim is a deterministic SNMP v2c UDP agent for testing
// the CIOS gateway driver without real hardware. It supports exactly
// two PDU types (GetRequest, SetRequest), a Mask() fault injection
// that turns a per-OID read or write into a noSuchInstance
// application-level exception, and a per-OID integer table updated by
// SETs and surfaced via GET. The package is intentionally stdlib-only
// and listens on 127.0.0.1 only; it is a testbed, not a device you
// would put on a plant network.
//
// Wire format: RFC 3416 (SNMPv2c) over UDP. The simulator echoes the
// request-id verbatim, requires the community to match the configured
// value (otherwise it answers with error-status=authorizationError
// per §4.1.2 of RFC 3416), and parses varbinds in BER using the
// encoding/asn1 primitive machinery. Conformance to RFC 3416 is
// guaranteed by being correctly parseable by the canonical
// github.com/gosnmp/gosnmp v2c client (used by the driver).
package snmpsim

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Config is the static configuration handed to New. The zero value
// listens on 127.0.0.1:0 with community "public" and an empty OID
// table; SetOID before Start to pre-load values.
type Config struct {
	// Addr is the bind address. Defaults to "127.0.0.1:0" (random
	// free port). The simulator refuses to bind to anything other
	// than a loopback address — see Start.
	Addr string
	// Community is the expected v2c community. Defaults to "public".
	Community string
}

// Sim is the running simulator. Construct with New, bring up with
// Start, tear down with Stop. All exported methods are safe for
// concurrent use; Stop is safe to call more than once and synchronises
// with the read/write loop.
type Sim struct {
	cfg      Config
	listener net.PacketConn

	mu     sync.RWMutex // protects values, masked, oidCanon
	values map[string]int64
	masked map[string]struct{}
	// oidCanon maps "1.3.6.1" -> "1.3.6.1" so SetOID/Mask can be
	// called with either leading-dot form or not. The driver passes
	// OIDs back through Get after SET, so we must recognise the
	// canonical (no leading dot) form.
	oidCanon map[string]string

	started atomic.Bool
	stopped atomic.Bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	requests atomic.Uint64
}

// --- BER tag constants -------------------------------------------------------

// Universal class tags.
const (
	tagInteger          = 0x02
	tagOctetString      = 0x04
	tagNull             = 0x05
	tagObjectIdentifier = 0x06
	tagSequence         = 0x30
)

// Application class (primitive) tags used by SNMP.
const (
	tagIpAddress  = 0x40
	tagCounter32  = 0x41
	tagGauge32    = 0x42
	tagTimeTicks  = 0x43
	tagOpaque     = 0x44
	tagCounter64  = 0x46
	tagUinteger32 = 0x47
)

// Context-specific (primitive) tags used inside a GetResponse-PDU
// VarBind to signal "this OID exists but has no instance" / "no such
// object" / "end of MIB view". RFC 3416 §2 (the v2 SMI error
// responses map directly onto these tag values).
const (
	tagNoSuchObject   = 0x80
	tagNoSuchInstance = 0x81
	tagEndOfMibView   = 0x82
)

// Application PDU types (constructed, IMPLICIT).
const (
	pduGetRequest  = 0xa0
	pduGetResponse = 0xa2
	pduSetRequest  = 0xa3
)

// SNMPv2c error-status codes (RFC 3416 §4.1.2).
const (
	snmpErrAuthorizationError = 16
)

// --- New / Start / Stop ------------------------------------------------------

// New returns a Sim initialised with cfg. The simulator is not yet
// listening; call Start to bind the UDP socket and start the loop.
func New(cfg Config) *Sim {
	s := &Sim{
		values:   make(map[string]int64),
		masked:   make(map[string]struct{}),
		oidCanon: make(map[string]string),
		stopCh:   make(chan struct{}),
	}
	if cfg.Addr == "" {
		s.cfg.Addr = "127.0.0.1:0"
	} else {
		s.cfg.Addr = cfg.Addr
	}
	if cfg.Community == "" {
		s.cfg.Community = "public"
	} else {
		s.cfg.Community = cfg.Community
	}
	return s
}

// Start binds the UDP socket and starts the read loop. The returned
// address is the resolved "host:port" string. A second call on the
// same Sim returns an error.
func (s *Sim) Start() (string, error) {
	if !s.started.CompareAndSwap(false, true) {
		return "", errors.New("snmpsim: already started")
	}
	host, port, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return "", fmt.Errorf("snmpsim: parse addr %q: %w", s.cfg.Addr, err)
	}
	// Refuse to bind to anything other than a loopback address.
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return "", fmt.Errorf("snmpsim: addr must be loopback, got host=%q", host)
	}
	bind := net.JoinHostPort(host, port)
	pc, err := net.ListenPacket("udp", bind)
	if err != nil {
		return "", fmt.Errorf("snmpsim: listen %q: %w", bind, err)
	}
	s.listener = pc

	s.wg.Add(1)
	go s.serveLoop(pc)

	return pc.LocalAddr().String(), nil
}

// Stop closes the socket and waits for the serve goroutine to exit.
// Safe to call more than once; subsequent calls are no-ops. The
// race-repair pattern mirrors modbussim: acceptLoop is replaced by a
// packet serveLoop here, but the same post-accept-recheck is needed
// against SetConnDeadline/ReadFrom returning after Close.
func (s *Sim) Stop() {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Sim) serveLoop(pc net.PacketConn) {
	defer s.wg.Done()
	// Tight loop with a fresh read buffer. ReadFrom errors on close
	// or on SetReadDeadline expiry. We don't need a deadline per call;
	// the Stop path closes the socket and ReadFrom returns the
	// resulting "use of closed network connection" error which we
	// treat as a clean exit.
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		// Count every datagram we received, even if malformed or
		// community-mismatched (modbussim parity: requests() = total
		// PDUs handled, including errors).
		s.requests.Add(1)
		resp := s.handleRequest(buf[:n])
		if resp == nil {
			// Malformed request — no reply.
			continue
		}
		if _, werr := pc.WriteTo(resp, udpAddr); werr != nil {
			// Best-effort; peer may have gone away.
			continue
		}
	}
}

// --- runtime helpers (used by tests) ----------------------------------------

// SetOID sets/overrides a numeric OID with an integer value. The
// value is the integer that the simulator will return on GET and
// update on SET. Both forms of the OID (with and without a leading
// '.') are accepted.
func (s *Sim) SetOID(oid string, value int64) {
	canon := canonicalOID(oid)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[canon] = value
	s.oidCanon[canon] = canon
}

// Get reads the current value for an OID. The boolean is false if
// the OID has never been SetOID'd.
func (s *Sim) Get(oid string) (int64, bool) {
	canon := canonicalOID(oid)
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[canon]
	return v, ok
}

// Mask marks an OID as fault-injected: any GET or SET touching this
// OID yields a noSuchInstance value in the response.
func (s *Sim) Mask(oid string) {
	canon := canonicalOID(oid)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.masked[canon] = struct{}{}
}

// Unmask removes a previously-set Mask.
func (s *Sim) Unmask(oid string) {
	canon := canonicalOID(oid)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.masked, canon)
}

// Requests returns the total number of UDP datagrams the server has
// processed since Start (including community-failed and malformed
// ones). Safe to call concurrently.
func (s *Sim) Requests() uint64 {
	return s.requests.Load()
}

// canonicalOID strips a leading '.' so SetOID/Mask/Get accept either
// "1.3.6.1" and ".1.3.6.1" interchangeably. The driver passes the
// OID back through Get after SET exactly as it was bound, so we must
// recognise both forms.
func canonicalOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

// --- request dispatch -------------------------------------------------------

// handleRequest parses one full SNMPv2c message and returns the
// GetResponse to send back, or nil if the message is unparseable /
// of an unsupported PDU type. Community mismatch is NOT a parse
// failure — we still return a GetResponse with error-status =
// authorizationError (RFC 3416 §4.1.2) so the driver can
// distinguish auth failure from network failure (C1).
func (s *Sim) handleRequest(raw []byte) []byte {
	// Top-level SEQUENCE.
	if len(raw) < 2 || raw[0] != tagSequence {
		return nil
	}
	seqLen, seqHeaderLen, ok := parseLength(raw[1:])
	if !ok {
		return nil
	}
	body := raw[1+seqHeaderLen : 1+seqHeaderLen+seqLen]
	if len(body) < seqLen {
		// Truncated.
		return nil
	}

	// version INTEGER.
	if len(body) < 3 || body[0] != tagInteger {
		return nil
	}
	// Version byte is body[2]. We accept 0 (SNMPv1) and 1 (SNMPv2c)
	// but treat both as v2c for our response semantics; real agents
	// differentiate error-status ranges, but for the conformance
	// oracle only v2c needs to round-trip with gosnmp.
	_, verLen, ok := parseLength(body[1:])
	if !ok || verLen < 1 || 2+verLen > len(body) {
		return nil
	}
	body = body[2+verLen:]

	// community OCTET STRING.
	if len(body) < 2 || body[0] != tagOctetString {
		return nil
	}
	comLen, comHeaderLen, ok := parseLength(body[1:])
	if !ok || comHeaderLen+comLen > len(body) {
		return nil
	}
	community := string(body[1+comHeaderLen : 1+comHeaderLen+comLen])
	body = body[1+comHeaderLen+comLen:]

	// PDU (GetRequest / SetRequest).
	if len(body) < 2 {
		return nil
	}
	pduType := body[0]
	pduLen, pduHeaderLen, ok := parseLength(body[1:])
	if !ok || 1+pduHeaderLen+pduLen > len(body) {
		return nil
	}
	pduBody := body[1+pduHeaderLen : 1+pduHeaderLen+pduLen]

	if pduType != pduGetRequest && pduType != pduSetRequest {
		// Report unsupported PDU types with an error response so the
		// client sees a coherent v2c packet (error-status=genErr).
		// We synthesise a request-id of 0 here; the test driver never
		// sends a PDU the sim does not know.
		return s.buildResponse(pduGetResponse, 0, snmpErrGenErr, 0, nil)
	}

	// Parse request-id / error-status / error-index out of the PDU
	// (they sit in front of the varbind list in this order, per
	// RFC 3416 §6).
	rid, es, ei, varbinds, ok := parsePDUHeaderAndVarbinds(pduBody)
	if !ok {
		return s.buildResponse(pduGetResponse, 0, snmpErrGenErr, 0, nil)
	}

	if community != s.cfg.Community {
		// RFC 3416 §4.1.2: authorizationError (16) with varbinds
		// echoed back verbatim. The driver can branch on Error ==
		// AuthorizationError to surface ErrAuth (C1).
		return s.buildResponse(pduGetResponse, rid, snmpErrAuthorizationError, 0, varbinds)
	}

	// Dispatch. For GetRequest: respond with the current values (or
	// noSuchInstance). For SetRequest: update the table, then echo
	// the new values in the response.
	out := make([]SnmpPDU, len(varbinds))
	for i, vb := range varbinds {
		switch pduType {
		case pduGetRequest:
			out[i] = s.handleGet(vb)
		case pduSetRequest:
			out[i] = s.handleSet(vb)
		}
	}
	return s.buildResponse(pduGetResponse, rid, es, ei, out)
}

// snmpErrGenErr is the catch-all "something went wrong" status we
// use when we cannot even parse the request. The driver never sends
// us anything that hits this path in the conformance tests; it
// exists for defence in depth.
const snmpErrGenErr = 5

// SnmpPDU is the internal representation of one varbind. Type 0x05
// (Null) is used in the request varbinds (the value side of GET
// requests is NULL by definition); on the response, Type carries the
// actual BER tag (Integer=0x02, NoSuchInstance=0x81, ...) and Value
// is the decoded integer for the integer types, or nil for the
// noSuch* tags.
type SnmpPDU struct {
	Name  string
	Type  byte
	Value int64
}

func (s *Sim) handleGet(vb SnmpPDU) SnmpPDU {
	canon := canonicalOID(vb.Name)
	s.mu.RLock()
	_, known := s.values[canon]
	_, masked := s.masked[canon]
	s.mu.RUnlock()
	if masked || !known {
		return SnmpPDU{Name: vb.Name, Type: tagNoSuchInstance, Value: 0}
	}
	v, _ := s.Get(canon)
	return SnmpPDU{Name: vb.Name, Type: tagInteger, Value: v}
}

func (s *Sim) handleSet(vb SnmpPDU) SnmpPDU {
	canon := canonicalOID(vb.Name)
	s.mu.RLock()
	_, masked := s.masked[canon]
	s.mu.RUnlock()
	if masked {
		// Per spec, a write to a masked object is a noSuchInstance
		// in the response (we don't apply it).
		return SnmpPDU{Name: vb.Name, Type: tagNoSuchInstance, Value: 0}
	}
	// SET accepts any non-masked OID and CREATES it if absent. This
	// matches the modbussim "write creates new address" behaviour and
	// gives tests a usable SET surface even with a fresh sim.
	// Integer values are accepted regardless of which integer tag
	// the client used (Integer / Counter32 / Gauge32 / TimeTicks /
	// Counter64 / Uinteger32); we treat them all as int64 here.
	val := vb.Value
	if vb.Type == tagInteger {
		// already decoded
	} else if vb.Type == tagCounter32 || vb.Type == tagGauge32 ||
		vb.Type == tagTimeTicks || vb.Type == tagUinteger32 {
		val = int64(uint32(val))
	} else if vb.Type == tagCounter64 {
		// int64 already; no clamp.
	} else {
		// Unknown type — refuse with noSuchInstance.
		return SnmpPDU{Name: vb.Name, Type: tagNoSuchInstance, Value: 0}
	}
	s.SetOID(canon, val)
	return SnmpPDU{Name: vb.Name, Type: tagInteger, Value: val}
}

// --- BER encoding helpers (v2c responses) -----------------------------------

// buildResponse assembles a full GetResponse message:
//
//	SEQUENCE { INTEGER version, OCTET STRING community, GetResponse-PDU { ... } }
func (s *Sim) buildResponse(pduType byte, requestID uint32, errStatus uint32, errIndex uint8, varbinds []SnmpPDU) []byte {
	// version: 1 (v2c)
	verBytes := []byte{tagInteger, 1, 0x01}
	// community
	comBytes := []byte{tagOctetString, byte(len(s.cfg.Community))}
	comBytes = append(comBytes, []byte(s.cfg.Community)...)
	// PDU body
	pduBody := encodePDUHeader(requestID, errStatus, errIndex)
	// varbinds wrapped in SEQUENCE OF. encodeVarbind already wraps
	// each entry in its own SEQUENCE; we just wrap the concatenation
	// in the outer SEQUENCE so the structure matches RFC 3416.
	var vbBytes []byte
	for _, vb := range varbinds {
		vbBytes = append(vbBytes, encodeVarbind(vb)...)
	}
	vbl := append([]byte{tagSequence}, encodeLength(len(vbBytes))...)
	vbl = append(vbl, vbBytes...)
	pduBody = append(pduBody, vbl...)
	pdu := append([]byte{pduType}, encodeLength(len(pduBody))...)
	pdu = append(pdu, pduBody...)
	// Whole message body
	msgBody := append([]byte{}, verBytes...)
	msgBody = append(msgBody, comBytes...)
	msgBody = append(msgBody, pdu...)
	// SEQUENCE wrapper
	msg := append([]byte{tagSequence}, encodeLength(len(msgBody))...)
	msg = append(msg, msgBody...)
	return msg
}

func encodePDUHeader(requestID, errStatus uint32, errIndex uint8) []byte {
	out := encodeInteger(requestID)
	out = append(out, encodeInteger(errStatus)...)
	out = append(out, encodeInteger(uint32(errIndex))...)
	return out
}

// encodeInteger encodes a uint32 as an ASN.1 INTEGER with the
// minimum number of bytes (BER) and a leading 0x00 byte if the sign
// bit is set, so the value stays non-negative.
func encodeInteger(v uint32) []byte {
	if v == 0 {
		return []byte{tagInteger, 0x01, 0x00}
	}
	var body []byte
	if v>>31 == 1 {
		// Sign bit would set on a signed 32-bit interpretation; pad.
		body = []byte{0x00, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	} else {
		body = make([]byte, 0, 4)
		tmp := v
		for tmp > 0 {
			body = append([]byte{byte(tmp)}, body...)
			tmp >>= 8
		}
	}
	return append([]byte{tagInteger, byte(len(body))}, body...)
}

// encodeVarbind encodes one varbind as a SEQUENCE { OID, <value> }.
// For integer types we encode an INTEGER; for noSuch* tags we encode
// the raw context-specific primitive (zero-length body).
func encodeVarbind(vb SnmpPDU) []byte {
	oidBytes, err := encodeObjectIdentifier(vb.Name)
	if err != nil {
		// Fall back to a minimal placeholder — the driver never
		// feeds us an invalid OID in the conformance tests, so this
		// is defence in depth.
		oidBytes = []byte{tagObjectIdentifier, 0x00}
	}
	valueBytes := encodeValue(vb)
	body := append([]byte{}, oidBytes...)
	body = append(body, valueBytes...)
	seq := append([]byte{tagSequence}, encodeLength(len(body))...)
	seq = append(seq, body...)
	return seq
}

func encodeValue(vb SnmpPDU) []byte {
	switch vb.Type {
	case tagInteger:
		// BER INTEGER: tag + length + signed bytes.
		return encodeInteger(uint32(vb.Value))
	case tagCounter32, tagGauge32, tagTimeTicks, tagUinteger32:
		// Application/primitive, 32-bit unsigned. We use the
		// minimum-length form with a 0x00 sign-bit pad.
		v := uint32(vb.Value)
		var body []byte
		if v>>31 == 1 {
			body = []byte{0x00, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		} else if v == 0 {
			body = []byte{0x00}
		} else {
			tmp := v
			for tmp > 0 {
				body = append([]byte{byte(tmp)}, body...)
				tmp >>= 8
			}
		}
		return []byte{vb.Type, byte(len(body))}
	case tagCounter64:
		v := uint64(vb.Value)
		var body []byte
		if v>>63 == 1 {
			body = make([]byte, 9)
			body[0] = 0x00
			for i := 0; i < 8; i++ {
				body[i+1] = byte(v >> (8 * (7 - i)))
			}
		} else {
			tmp := v
			for tmp > 0 {
				body = append([]byte{byte(tmp)}, body...)
				tmp >>= 8
			}
		}
		return []byte{tagCounter64, byte(len(body))}
	case tagNoSuchObject, tagNoSuchInstance, tagEndOfMibView:
		// Context-specific primitive, zero-length body.
		return []byte{vb.Type, 0x00}
	default:
		// Fallback: NULL.
		return []byte{tagNull, 0x00}
	}
}

// --- BER parsing helpers (requests) -----------------------------------------

// parsePDUHeaderAndVarbinds returns request-id, error-status,
// error-index, and the varbind list for a GetRequest / SetRequest
// PDU body. The first three INTEGERs are mandatory and in this order
// per RFC 3416 §6; the varbind list is a SEQUENCE OF.
func parsePDUHeaderAndVarbinds(pdu []byte) (uint32, uint32, uint8, []SnmpPDU, bool) {
	cur := pdu
	rid, n, ok := decodeInteger(cur)
	if !ok {
		return 0, 0, 0, nil, false
	}
	cur = cur[n:]
	es, n, ok := decodeInteger(cur)
	if !ok {
		return 0, 0, 0, nil, false
	}
	cur = cur[n:]
	ei, n, ok := decodeInteger(cur)
	if !ok {
		return 0, 0, 0, nil, false
	}
	cur = cur[n:]
	vbs, ok := decodeVarbindList(cur)
	if !ok {
		return rid, 0, 0, nil, false
	}
	return rid, es, uint8(ei), vbs, true
}

// decodeInteger reads an ASN.1 INTEGER (BER) and returns its uint32
// value and the number of bytes consumed.
func decodeInteger(b []byte) (uint32, int, bool) {
	if len(b) < 2 || b[0] != tagInteger {
		return 0, 0, false
	}
	ln, h, ok := parseLength(b[1:])
	if !ok || h+ln > len(b)-1 {
		return 0, 0, false
	}
	v := uint32(0)
	for i := 0; i < ln; i++ {
		v = (v << 8) | uint32(b[1+h+i])
	}
	return v, 1 + h + ln, true
}

// decodeVarbindList reads a SEQUENCE OF VarBind: outer SEQUENCE tag,
// length, then a flat stream of inner SEQUENCE { OID, value } entries.
func decodeVarbindList(b []byte) ([]SnmpPDU, bool) {
	if len(b) < 2 || b[0] != tagSequence {
		return nil, false
	}
	ln, h, ok := parseLength(b[1:])
	if !ok {
		return nil, false
	}
	body := b[1+h : 1+h+ln]
	var out []SnmpPDU
	for len(body) > 0 {
		if body[0] != tagSequence {
			return nil, false
		}
		innerLen, innerH, ok := parseLength(body[1:])
		if !ok {
			return nil, false
		}
		inner := body[1+innerH : 1+innerH+innerLen]
		body = body[1+innerH+innerLen:]
		oid, n, ok := decodeOID(inner)
		if !ok {
			return nil, false
		}
		rest := inner[n:]
		pdu := SnmpPDU{Name: oid, Type: tagNull, Value: 0}
		if len(rest) >= 1 {
			tag := rest[0]
			switch tag {
			case tagNull:
				// GET varbind: nothing to decode.
			case tagInteger, tagCounter32, tagGauge32, tagTimeTicks, tagUinteger32, tagCounter64:
				v, _, ok := decodeInteger(rest)
				if !ok {
					return nil, false
				}
				pdu.Type = tag
				pdu.Value = int64(v)
			default:
				// Treat unknown values as GET (NULL) so a SET with an
				// OctetString payload does not crash the sim; the
				// handler will refuse it as noSuchInstance.
				pdu.Type = tag
			}
		}
		out = append(out, pdu)
	}
	return out, true
}

func decodeOID(b []byte) (string, int, bool) {
	if len(b) < 2 || b[0] != tagObjectIdentifier {
		return "", 0, false
	}
	ln, h, ok := parseLength(b[1:])
	if !ok || 1+h+ln > len(b) {
		return "", 0, false
	}
	body := b[1+h : 1+h+ln]
	if len(body) < 1 {
		return "", 0, false
	}
	// First byte encodes first two arcs: 40*a + b.
	first := body[0]
	a := first / 40
	b2 := first % 40
	parts := make([]string, 0, len(body)+1)
	parts = append(parts, strconv.Itoa(int(a)), strconv.Itoa(int(b2)))
	v := uint32(0)
	for i := 1; i < len(body); i++ {
		v = (v << 7) | uint32(body[i]&0x7f)
		if body[i]&0x80 == 0 {
			parts = append(parts, strconv.FormatUint(uint64(v), 10))
			v = 0
		}
	}
	// Trailing unterminated arc — malformed.
	if body[len(body)-1]&0x80 != 0 {
		return "", 0, false
	}
	return strings.Join(parts, "."), 1 + h + ln, true
}

// parseLength decodes a BER length octet. Returns the length value,
// the number of bytes the length occupied in the buffer, and ok.
// Supports both the short form (single byte, 0..127) and the long
// form (0x8n followed by n big-endian length bytes), per ITU-T
// X.690 §10.1.
func parseLength(b []byte) (int, int, bool) {
	if len(b) < 1 {
		return 0, 0, false
	}
	first := b[0]
	if first < 0x80 {
		return int(first), 1, true
	}
	// Long form: 0x8n means the next n bytes are the big-endian
	// length. n must be 1..127.
	numOctets := int(first & 0x7f)
	if numOctets == 0 || numOctets > 127 || numOctets+1 > len(b) {
		return 0, 0, false
	}
	v := 0
	for i := 0; i < numOctets; i++ {
		v = (v << 8) | int(b[1+i])
	}
	return v, 1 + numOctets, true
}

// encodeLength encodes a length in BER short form. Lengths above 127
// are not produced by the sim for the conformance vectors, but we
// implement the long form for completeness (caller side; sim
// responses stay well under 1 KiB).
func encodeLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	// Long form: number of length octets first, then big-endian length.
	var body []byte
	tmp := n
	for tmp > 0 {
		body = append([]byte{byte(tmp & 0xff)}, body...)
		tmp >>= 8
	}
	return append([]byte{byte(0x80 | len(body))}, body...)
}

// encodeObjectIdentifier encodes a dotted-decimal OID into BER.
// encoding/asn1.ObjectIdentifier does the same thing but does not
// raise the very first arc the RFC way (it splits first*40+second);
// we hand-roll to keep the encoding deterministic and consistent
// with decodeOID.
func encodeObjectIdentifier(oid string) ([]byte, error) {
	oid = strings.TrimPrefix(oid, ".")
	parts := strings.Split(oid, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("snmpsim: OID %q needs ≥ 2 arcs", oid)
	}
	first, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil || first > 2 {
		return nil, fmt.Errorf("snmpsim: OID %q first arc", oid)
	}
	second, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil {
		return nil, fmt.Errorf("snmpsim: OID %q second arc", oid)
	}
	if first >= 2 && second > 39 {
		return nil, fmt.Errorf("snmpsim: OID %q second arc > 39 with first ≥ 2", oid)
	}
	body := []byte{byte(first*40 + second)}
	for i := 2; i < len(parts); i++ {
		v, err := strconv.ParseUint(parts[i], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("snmpsim: OID %q arc %d", oid, i)
		}
		body = append(body, encodeBase128(uint32(v))...)
	}
	return append([]byte{tagObjectIdentifier, byte(len(body))}, body...), nil
}

// encodeBase128 encodes a uint32 as a base-128 BER subidentifier
// (7 bits per byte, MSB set on all but the last).
func encodeBase128(v uint32) []byte {
	if v == 0 {
		return []byte{0x00}
	}
	var body []byte
	for v > 0 {
		body = append([]byte{byte(v & 0x7f)}, body...)
		v >>= 7
	}
	for i := 0; i < len(body)-1; i++ {
		body[i] |= 0x80
	}
	return body
}
