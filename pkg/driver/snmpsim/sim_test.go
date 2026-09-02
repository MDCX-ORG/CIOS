package snmpsim

import (
	"bytes"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- request / response helpers (v2c BER) -----------------------------------

// buildGetRequest encodes a single-OID GetRequest-PDU wrapped in the
// v2c SEQUENCE { version, community, GetRequest-PDU } envelope.
func buildGetRequest(community, oid string) []byte {
	return buildRequest(community, pduGetRequest, encodeSingleOIDVarbind(oid))
}

func buildSetRequest(community, oid string, value int64) []byte {
	// Varbind value side: INTEGER. OID TLV + Integer TLV in a SEQUENCE.
	oidTLV, _ := encodeObjectIdentifier(oid)
	valTLV := encodeInteger(uint32(value))
	inner := append([]byte{}, oidTLV...)
	inner = append(inner, valTLV...)
	vb := append([]byte{tagSequence}, encodeLength(len(inner))...)
	vb = append(vb, inner...)
	return buildRequest(community, pduSetRequest, vb)
}

func buildRequest(community string, pduType byte, varbinds []byte) []byte {
	// version INTEGER 1
	ver := []byte{tagInteger, 0x01, 0x01}
	// community OCTET STRING
	com := []byte{tagOctetString, byte(len(community))}
	com = append(com, []byte(community)...)
	// PDU body: request-id(0) + error-status(0) + error-index(0) + varbind-list
	header := encodeInteger(0)
	header = append(header, encodeInteger(0)...)
	header = append(header, encodeInteger(0)...)
	// wrap raw varbinds in SEQUENCE { ... } (SEQUENCE OF VarBind)
	vbl := append([]byte{tagSequence}, encodeLength(len(varbinds))...)
	vbl = append(vbl, varbinds...)
	pduBody := append(header, vbl...)
	pdu := append([]byte{pduType}, encodeLength(len(pduBody))...)
	pdu = append(pdu, pduBody...)
	// SEQUENCE { version, community, pdu }
	msgBody := append([]byte{}, ver...)
	msgBody = append(msgBody, com...)
	msgBody = append(msgBody, pdu...)
	msg := append([]byte{tagSequence}, encodeLength(len(msgBody))...)
	msg = append(msg, msgBody...)
	return msg
}

func encodeSingleOIDVarbind(oid string) []byte {
	oidTLV, _ := encodeObjectIdentifier(oid)
	null := []byte{tagNull, 0x00}
	inner := append([]byte{}, oidTLV...)
	inner = append(inner, null...)
	vb := append([]byte{tagSequence}, encodeLength(len(inner))...)
	vb = append(vb, inner...)
	return vb
}

// roundtrip sends one request and reads the full SEQUENCE response.
func roundtrip(t *testing.T, addr, community, oid string) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(buildGetRequest(community, oid)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

func setRoundtrip(t *testing.T, addr, community, oid string, value int64) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(buildSetRequest(community, oid, value)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

// decodeVarbindValues parses a response message and returns the
// decoded (oid, tag, intValue) for every varbind. We need a minimal
// decoder because the sim's own helpers are not exported.
type varbindResult struct {
	oid string
	tag byte
	val int64
}

func decodeResponse(t *testing.T, raw []byte) (requestID uint32, errStatus uint32, vbs []varbindResult) {
	t.Helper()
	if len(raw) < 2 || raw[0] != tagSequence {
		t.Fatalf("decodeResponse: not a sequence, got % X", raw)
	}
	seqLen, h, ok := parseLength(raw[1:])
	if !ok {
		t.Fatalf("decodeResponse: bad top length")
	}
	body := raw[1+h : 1+h+seqLen]
	// version
	cur := body
	if len(cur) < 3 || cur[0] != tagInteger {
		t.Fatalf("decodeResponse: no version INTEGER")
	}
	_, hl, ok := parseLength(cur[1:])
	if !ok {
		t.Fatalf("decodeResponse: bad version length")
	}
	cur = cur[2+hl:]
	// community
	if len(cur) < 2 || cur[0] != tagOctetString {
		t.Fatalf("decodeResponse: no community")
	}
	cl, hl, ok := parseLength(cur[1:])
	if !ok {
		t.Fatalf("decodeResponse: bad community length")
	}
	cur = cur[1+hl+cl:]
	// PDU
	if len(cur) < 2 {
		t.Fatalf("decodeResponse: no PDU")
	}
	pduLen, hl, ok := parseLength(cur[1:])
	if !ok {
		t.Fatalf("decodeResponse: bad PDU length")
	}
	pduBody := cur[1+hl : 1+hl+pduLen]
	// request-id / error-status / error-index
	rid, n, ok := decodeInteger(pduBody)
	if !ok {
		t.Fatalf("decodeResponse: bad request-id")
	}
	pduBody = pduBody[n:]
	es, n, ok := decodeInteger(pduBody)
	if !ok {
		t.Fatalf("decodeResponse: bad error-status")
	}
	pduBody = pduBody[n:]
	if _, n, ok = decodeInteger(pduBody); !ok {
		t.Fatalf("decodeResponse: bad error-index")
	}
	pduBody = pduBody[n:] // skip error-index
	requestID = rid
	errStatus = es
	// varbinds (SEQUENCE OF)
	if len(pduBody) < 2 || pduBody[0] != tagSequence {
		t.Fatalf("decodeResponse: no varbind list")
	}
	vl, hl, ok := parseLength(pduBody[1:])
	if !ok {
		t.Fatalf("decodeResponse: bad varbind list length")
	}
	vbBody := pduBody[1+hl : 1+hl+vl]
	for len(vbBody) > 0 {
		if vbBody[0] != tagSequence {
			t.Fatalf("decodeResponse: varbind not SEQUENCE")
		}
		innerLen, innerH, ok := parseLength(vbBody[1:])
		if !ok {
			t.Fatalf("decodeResponse: bad varbind length")
		}
		inner := vbBody[1+innerH : 1+innerH+innerLen]
		vbBody = vbBody[1+innerH+innerLen:]
		oid, n, ok := decodeOID(inner)
		if !ok {
			t.Fatalf("decodeResponse: bad OID in varbind")
		}
		rest := inner[n:]
		vr := varbindResult{oid: oid}
		if len(rest) >= 1 {
			vr.tag = rest[0]
			if vr.tag == tagInteger || vr.tag == tagCounter32 || vr.tag == tagGauge32 ||
				vr.tag == tagTimeTicks || vr.tag == tagUinteger32 || vr.tag == tagCounter64 {
				v, _, ok := decodeInteger(rest)
				if !ok {
					t.Fatalf("decodeResponse: bad value INTEGER in varbind")
				}
				vr.val = int64(v)
			}
		}
		vbs = append(vbs, vr)
	}
	return requestID, errStatus, vbs
}

// --- self-tests ------------------------------------------------------------

func TestGetRoundTrip(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.SetOID("1.3.6.1.2.1.1.3.0", 12345)

	resp := roundtrip(t, addr, "public", "1.3.6.1.2.1.1.3.0")
	_, es, vbs := decodeResponse(t, resp)
	if es != 0 {
		t.Errorf("error-status = %d, want 0", es)
	}
	if len(vbs) != 1 {
		t.Fatalf("vbs = %d, want 1", len(vbs))
	}
	if vbs[0].tag != tagInteger {
		t.Errorf("tag = 0x%02X, want INTEGER (0x02)", vbs[0].tag)
	}
	if vbs[0].val != 12345 {
		t.Errorf("val = %d, want 12345", vbs[0].val)
	}
	if vbs[0].oid != "1.3.6.1.2.1.1.3.0" {
		t.Errorf("oid = %q, want 1.3.6.1.2.1.1.3.0", vbs[0].oid)
	}
}

func TestGetUnsetOIDReturnsNoSuchInstance(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	resp := roundtrip(t, addr, "public", "1.3.6.1.2.1.2.2.1.10.1")
	_, _, vbs := decodeResponse(t, resp)
	if len(vbs) != 1 {
		t.Fatalf("vbs = %d, want 1", len(vbs))
	}
	if vbs[0].tag != tagNoSuchInstance {
		t.Errorf("tag = 0x%02X, want noSuchInstance (0x81)", vbs[0].tag)
	}
}

func TestMaskReturnsNoSuchInstance(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.SetOID("1.3.6.1.4.1.9999.1.1.0", 7)
	sim.Mask("1.3.6.1.4.1.9999.1.1.0")

	resp := roundtrip(t, addr, "public", "1.3.6.1.4.1.9999.1.1.0")
	_, _, vbs := decodeResponse(t, resp)
	if len(vbs) != 1 || vbs[0].tag != tagNoSuchInstance {
		t.Errorf("vbs[0] = %+v, want noSuchInstance", vbs[0])
	}
	// Unmask, the same OID should now read as INTEGER=7.
	sim.Unmask("1.3.6.1.4.1.9999.1.1.0")
	resp = roundtrip(t, addr, "public", "1.3.6.1.4.1.9999.1.1.0")
	_, _, vbs = decodeResponse(t, resp)
	if len(vbs) != 1 || vbs[0].tag != tagInteger || vbs[0].val != 7 {
		t.Errorf("after unmask vbs[0] = %+v, want INTEGER=7", vbs[0])
	}
}

func TestSetUpdatesValue(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	resp := setRoundtrip(t, addr, "public", "1.3.6.1.2.1.2.2.1.7.5", 1)
	_, es, vbs := decodeResponse(t, resp)
	if es != 0 {
		t.Errorf("SET error-status = %d, want 0", es)
	}
	if len(vbs) != 1 || vbs[0].val != 1 {
		t.Errorf("vbs = %+v, want INTEGER=1", vbs[0])
	}
	// Verify via Get
	resp = roundtrip(t, addr, "public", "1.3.6.1.2.1.2.2.1.7.5")
	_, _, vbs = decodeResponse(t, resp)
	if vbs[0].tag != tagInteger || vbs[0].val != 1 {
		t.Errorf("post-SET GET = %+v, want INTEGER=1", vbs[0])
	}
}

func TestSetOnMaskedOIDReturnsNoSuchInstance(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.Mask("1.3.6.1.2.1.2.2.1.7.6")
	resp := setRoundtrip(t, addr, "public", "1.3.6.1.2.1.2.2.1.7.6", 1)
	_, _, vbs := decodeResponse(t, resp)
	if len(vbs) != 1 || vbs[0].tag != tagNoSuchInstance {
		t.Errorf("vbs[0] = %+v, want noSuchInstance", vbs[0])
	}
}

func TestCommunityMismatch(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.SetOID("1.3.6.1.2.1.1.3.0", 42)

	resp := roundtrip(t, addr, "private", "1.3.6.1.2.1.1.3.0")
	_, es, vbs := decodeResponse(t, resp)
	if es != 16 { // authorizationError
		t.Errorf("error-status = %d, want 16 (authorizationError)", es)
	}
	if len(vbs) != 1 || vbs[0].oid != "1.3.6.1.2.1.1.3.0" {
		t.Errorf("vbs = %+v, want one varbind echoing the OID", vbs)
	}
}

func TestRequestsCountIncludesAuthFails(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.SetOID("1.3.6.1.2.1.1.3.0", 1)
	for i := 0; i < 3; i++ {
		_ = roundtrip(t, addr, "public", "1.3.6.1.2.1.1.3.0")
	}
	_ = roundtrip(t, addr, "private", "1.3.6.1.2.1.1.3.0")
	if got := sim.Requests(); got != 4 {
		t.Errorf("Requests() = %d, want 4", got)
	}
}

func TestStartTwiceFails(t *testing.T) {
	sim := New(Config{})
	if _, err := sim.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer sim.Stop()
	if _, err := sim.Start(); err == nil {
		t.Errorf("second Start: expected error, got nil")
	}
}

func TestStopIdempotent(t *testing.T) {
	sim := New(Config{})
	if _, err := sim.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sim.Stop()
	sim.Stop() // must not panic
}

func TestRefuseNonLoopback(t *testing.T) {
	sim := New(Config{Addr: "0.0.0.0:0"})
	_, err := sim.Start()
	if err == nil {
		t.Errorf("Start on 0.0.0.0: expected error, got nil")
		sim.Stop()
	}
}

// --- byte-vector reference (RFC 3416 example shape) ------------------------

// TestByteVectorGET_OK pins a single response to a known byte shape:
// confirms our encoder writes SEQUENCE, version=1, community, PDU type,
// integer tag/length/value in the order gosnmp expects.
func TestByteVectorGET_OK(t *testing.T) {
	sim := New(Config{Community: "public"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	sim.SetOID("1.3.6.1.2.1.1.3.0", 123)

	resp := roundtrip(t, addr, "public", "1.3.6.1.2.1.1.3.0")
	// top: 30 LL (sequence)
	if resp[0] != 0x30 {
		t.Fatalf("response[0] = 0x%02X, want 0x30 SEQUENCE", resp[0])
	}
	// walk past SEQUENCE+length to version INTEGER 02 01 01
	// (we don't pin the exact offset — we check the canonical tags
	// appear in the expected order so the gosnmp client can parse it)
	idx := 2 // SEQUENCE + 1-byte length
	// version: 02 01 01
	if !bytes.Equal(resp[idx:idx+3], []byte{0x02, 0x01, 0x01}) {
		t.Errorf("version bytes = % X, want 02 01 01", resp[idx:idx+3])
	}
	idx += 3
	// community: 04 06 "public"
	if resp[idx] != 0x04 || resp[idx+1] != 0x06 || string(resp[idx+2:idx+8]) != "public" {
		t.Errorf("community bytes = % X, want 04 06 70 75 62 6C 69 63", resp[idx:idx+8])
	}
	idx += 2 + int(resp[idx+1])
	// PDU type: 0xA2 (GetResponse)
	if resp[idx] != pduGetResponse {
		t.Errorf("pdu type = 0x%02X, want 0xA2", resp[idx])
	}
}

// --- concurrent / Stop race regression (PRMT-014 pattern) ------------------

// TestStopRaceNoHang hammers Start/Stop while external clients
// continuously send datagrams. The modbussim equivalent (PRMT-014)
// proved that Stop can hang if acceptLoop raced a Store past the
// listener-close. The UDP equivalent: after Stop closes the
// PacketConn, in-flight ReadFrom returns an error and the serveLoop
// returns. We verify Stop returns within a budget.
func TestStopRaceNoHang(t *testing.T) {
	const rounds = 200
	const stopBudget = 10 * time.Second

	for round := 0; round < rounds; round++ {
		sim := New(Config{Community: "public"})
		addr, err := sim.Start()
		if err != nil {
			t.Fatalf("round %d: Start: %v", round, err)
		}
		sim.SetOID("1.3.6.1.2.1.1.3.0", 1)

		// Sender pumps datagrams continuously.
		stopSender := make(chan struct{})
		var senderWG sync.WaitGroup
		for s := 0; s < 4; s++ {
			senderWG.Add(1)
			go func() {
				defer senderWG.Done()
				conn, derr := net.Dial("udp", addr)
				if derr != nil {
					return
				}
				defer conn.Close()
				req := buildGetRequest("public", "1.3.6.1.2.1.1.3.0")
				for {
					select {
					case <-stopSender:
						return
					default:
					}
					conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
					_, werr := conn.Write(req)
					if werr != nil {
						return
					}
					_, rerr := conn.Read(make([]byte, 2048))
					if rerr != nil {
						// timeouts are fine; the conn is still live
					}
				}
			}()
		}

		// Watchdog-bounded Stop.
		timer := time.AfterFunc(stopBudget, func() {
			t.Fatalf("round %d: Stop() hung > %s", round, stopBudget)
		})
		sim.Stop()
		timer.Stop()

		close(stopSender)
		senderWG.Wait()
	}
}

// --- canonicalOID trim coverage --------------------------------------------

func TestCanonicalOIDTrim(t *testing.T) {
	if got := canonicalOID(".1.3.6.1.2.1"); got != "1.3.6.1.2.1" {
		t.Errorf("canonicalOID(.1.3.6.1.2.1) = %q, want 1.3.6.1.2.1", got)
	}
	if got := canonicalOID("1.3.6.1.2.1"); got != "1.3.6.1.2.1" {
		t.Errorf("canonicalOID(1.3.6.1.2.1) = %q, want 1.3.6.1.2.1", got)
	}
}

// --- internal BER round-trip (defence in depth) ----------------------------

// TestBERRoundTrip_Int wraps a known integer through encode/decode to
// pin the shortest-form rule. Catches regressions in encodeInteger
// without depending on a live agent.
func TestBERRoundTrip_Int(t *testing.T) {
	cases := []uint32{0, 1, 127, 128, 255, 256, 65535, 0x80000000 - 1, 0x80000000}
	for _, c := range cases {
		enc := encodeInteger(c)
		got, _, ok := decodeInteger(enc)
		if !ok {
			t.Errorf("decodeInteger(%d) failed: % X", c, enc)
			continue
		}
		if got != c {
			t.Errorf("round-trip %d → %d", c, got)
		}
	}
}

func TestBERRoundTrip_Length(t *testing.T) {
	for _, n := range []int{0, 1, 2, 127, 128, 255, 1000} {
		enc := encodeLength(n)
		got, _, ok := parseLength(enc)
		if !ok {
			t.Errorf("parseLength(%d) failed: % X", n, enc)
			continue
		}
		if got != n {
			t.Errorf("length round-trip %d → %d", n, got)
		}
	}
}

func TestBERRoundTrip_OID(t *testing.T) {
	cases := []string{"1.3.6.1.2.1.1.3.0", "1.3.6.1.4.1.9999.1.1.0", "1.3.6.1.2.1.2.2.1.7.5"}
	for _, oid := range cases {
		enc, err := encodeObjectIdentifier(oid)
		if err != nil {
			t.Errorf("encodeObjectIdentifier(%q): %v", oid, err)
			continue
		}
		got, _, ok := decodeOID(enc)
		if !ok {
			t.Errorf("decodeOID(%q) failed: % X", oid, enc)
			continue
		}
		if got != oid {
			t.Errorf("OID round-trip %q → %q", oid, got)
		}
	}
}

// silence unused import warnings on stdlib slices we may not use
var _ = binary.BigEndian
var _ = strings.Join
var _ = strconv.Itoa
