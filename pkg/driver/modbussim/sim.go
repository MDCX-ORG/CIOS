// Package modbussim is a deterministic Modbus TCP server for testing
// the CIOS gateway driver without real hardware. It supports exactly
// four function codes (0x03 / 0x04 / 0x06 / 0x10) and a Mask() fault
// injection that turns a per-address read or write into an
// Illegal Data Address (0x02) exception. The package is intentionally
// stdlib-only and listens on 127.0.0.1 only; it is a testbed, not a
// device you would put on a plant network.
//
// Wire format: standard Modbus TCP (MBAP + PDU). The simulator echoes
// the transaction id and unit id verbatim; a non-zero protocol id is
// treated as a framing error and the connection is closed.
package modbussim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// Config is the static configuration handed to New. The zero value
// listens on a random 127.0.0.1 port with unit id 1; an empty
// Holding/Input map is valid and means "no registers are pre-defined".
type Config struct {
	// Listen is the bind address. Defaults to "127.0.0.1:0" (random
	// free port). The simulator refuses to bind to anything other
	// than a loopback address unless AllowPublicBind is true — see
	// Start.
	Listen string
	// UnitID is the modbus unit id the simulator responds to. The
	// simulator does NOT filter on the unit id of incoming requests;
	// it always echoes the request's unit id in the response. This
	// matches the spec-005 test-bedding intent (one device per port,
	// not a multi-drop bus).
	UnitID byte
	// Holding is the initial holding-register table (function codes
	// 0x03 / 0x06 / 0x10). The map is copied at New time.
	Holding map[uint16]uint16
	// Input is the initial input-register table (function code 0x04).
	// The map is copied at New time.
	Input map[uint16]uint16
	// AllowPublicBind lifts the loopback-only guard in Start so the
	// simulator can serve a co-resident process in a compose network
	// (e.g. cios-gateway reaching cios-modbussim across the bridge).
	// Default false preserves M0 / testbed semantics; the cmd-layer
	// -allow-public-bind flag in cmd/cios-modbussim plumbs this through.
	// Host-boundary safety (spec-006 §5.2) is the caller's
	// responsibility: do not publish the bind port to the host.
	AllowPublicBind bool
}

// Sim is the running simulator. Construct with New, bring up with
// Start, tear down with Stop. Methods are safe for concurrent use.
type Sim struct {
	cfg      Config
	listener net.Listener

	mu      sync.RWMutex      // protects holding, input, masked
	holding map[uint16]uint16 // function codes 0x03 / 0x06 / 0x10
	input   map[uint16]uint16 // function code 0x04
	masked  map[uint16]bool   // addresses that return exception 0x02

	conns sync.Map // net.Conn → struct{}; set of live client connections

	started atomic.Bool
	stopped atomic.Bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	requests atomic.Uint64
}

// --- MBAP + FC constants -----------------------------------------------------

const (
	mbapLen    = 7 // MBAP header size (txid 2 + protid 2 + len 2 + unitid 1 — wait, unitid is in PDU)
	mbapHeader = 6 // MBAP header without unit id: txid 2 + protid 2 + len 2
	pduMinLen  = 2 // minimum PDU: FC 1 + at least 1 byte of data
)

// Exception codes (modbus standard).
const (
	excIllegalFunction    byte = 0x01
	excIllegalDataAddress byte = 0x02
	excIllegalDataValue   byte = 0x03
)

// FC0x exception prefix is 0x80.
const fcExceptionPrefix byte = 0x80

// --- New + Start + Stop ------------------------------------------------------

// New returns a Sim initialised with cfg. The simulator is not yet
// listening; call Start to bind and accept.
func New(cfg Config) *Sim {
	s := &Sim{
		holding: make(map[uint16]uint16, len(cfg.Holding)),
		input:   make(map[uint16]uint16, len(cfg.Input)),
		masked:  make(map[uint16]bool),
		stopCh:  make(chan struct{}),
	}
	for k, v := range cfg.Holding {
		s.holding[k] = v
	}
	for k, v := range cfg.Input {
		s.input[k] = v
	}
	if cfg.Listen == "" {
		s.cfg.Listen = "127.0.0.1:0"
	} else {
		s.cfg.Listen = cfg.Listen
	}
	s.cfg.UnitID = cfg.UnitID
	s.cfg.AllowPublicBind = cfg.AllowPublicBind
	return s
}

// Start binds the listener and begins accepting connections. The
// returned address is the resolved "host:port" string. A second call
// on the same Sim returns an error.
func (s *Sim) Start() (string, error) {
	if !s.started.CompareAndSwap(false, true) {
		return "", errors.New("modbussim: already started")
	}
	host, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("modbussim: parse listen %q: %w", s.cfg.Listen, err)
	}
	// Refuse to bind to anything other than a loopback address unless
	// the caller has explicitly opted in to cross-container binds via
	// Config.AllowPublicBind. This keeps the M0 / testbed default
	// safe (loopback only) while letting compose workflows use a
	// non-loopback bind inside the bridge network.
	if !s.cfg.AllowPublicBind {
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return "", fmt.Errorf("modbussim: listen must be loopback, got host=%q", host)
		}
	}
	// Re-join so "127.0.0.1:0" still works through SplitHostPort.
	bind := net.JoinHostPort(host, port)
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return "", fmt.Errorf("modbussim: listen %q: %w", bind, err)
	}
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop()

	return ln.Addr().String(), nil
}

// Stop closes the listener and every live client connection. It is
// safe to call more than once; subsequent calls are no-ops.
func (s *Sim) Stop() {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.conns.Range(func(k, _ any) bool {
		_ = k.(net.Conn).Close()
		return true
	})
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Sim) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.conns.Store(conn, struct{}{})
		// Race-window repair (PRMT-014): Stop does CAS(stopped=true)
		// BEFORE Range(conns). If Accept returned a conn that Store
		// landed AFTER Stop's Range finished, that conn would never
		// be closed and its handle would block forever on an
		// unbounded read, hanging wg.Wait. Re-checking stopped here
		// covers both windows: Store-before-Range → Range closed it;
		// Store-after-Range → stopped is necessarily true here, so
		// we close it ourselves. handle is still spawned; on an
		// already-closed conn its read fails immediately and wg
		// balances as usual.
		if s.stopped.Load() {
			_ = conn.Close()
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

// --- runtime register / mask helpers ----------------------------------------

// SetHolding writes a value into the holding table.
func (s *Sim) SetHolding(addr, val uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding[addr] = val
}

// SetInput writes a value into the input table.
func (s *Sim) SetInput(addr, val uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.input[addr] = val
}

// GetHolding reads a value from the holding table. The boolean is
// false if the address is not defined.
func (s *Sim) GetHolding(addr uint16) (uint16, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.holding[addr]
	return v, ok
}

// Mask marks addr as fault-injected: any read or write touching this
// address returns exception 0x02.
func (s *Sim) Mask(addr uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.masked[addr] = true
}

// Unmask removes a previously-set Mask on addr.
func (s *Sim) Unmask(addr uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.masked, addr)
}

// Requests returns the total number of modbus requests the server has
// processed since Start. Safe to call concurrently.
func (s *Sim) Requests() uint64 {
	return s.requests.Load()
}

func (s *Sim) isMasked(addr uint16) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.masked[addr]
}

// --- connection handler -----------------------------------------------------

func (s *Sim) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer s.conns.Delete(conn)

	for {
		// Read the full MBAP length: first 6 bytes (txid 2 + protid 2 + len 2).
		var hdr [mbapHeader]byte
		if _, err := readFull(conn, hdr[:]); err != nil {
			return
		}
		txID := binary.BigEndian.Uint16(hdr[0:2])
		protoID := binary.BigEndian.Uint16(hdr[2:4])
		length := binary.BigEndian.Uint16(hdr[4:6])
		if protoID != 0 {
			// Modbus spec: protocol id must be 0. Anything else is
			// a framing error; close the connection.
			return
		}
		if length < 1 || int(length) > 253 {
			// PDU body length is bounded: unit id (1) + PDU (≤ 253).
			return
		}
		// length counts unit id + PDU; total bytes after the 6-byte
		// header is length bytes. We read `length-1` for the unit id
		// and PDU in one shot, but the dispatcher only needs the FC
		// and following bytes (unit id is echoed verbatim in responses).
		rest := make([]byte, length)
		if _, err := readFull(conn, rest); err != nil {
			return
		}
		unitID := rest[0]
		pdu := rest[1:]

		s.requests.Add(1)
		respPDU := s.dispatch(pdu, unitID)
		if err := writeResponse(conn, txID, unitID, respPDU); err != nil {
			return
		}
	}
}

// dispatch handles the FC byte in pdu and returns the response PDU
// (everything after the unit id, including the FC echo and any
// data/exc code). A return value with FC|0x80 means exception.
func (s *Sim) dispatch(pdu []byte, unitID byte) []byte {
	if len(pdu) < 1 {
		// No FC byte at all → illegal function with FC=0.
		return []byte{0x00 | fcExceptionPrefix, excIllegalFunction}
	}
	fc := pdu[0]
	switch fc {
	case 0x03:
		return s.fcRead(pdu, unitID, s.readHoldingRange)
	case 0x04:
		return s.fcRead(pdu, unitID, s.readInputRange)
	case 0x06:
		return s.fcWriteSingle(pdu)
	case 0x10:
		return s.fcWriteMultiple(pdu)
	default:
		return []byte{fc | fcExceptionPrefix, excIllegalFunction}
	}
}

// readOp is the table-specific half of a read function code.
type readOp func(start, qty uint16) (vals []uint16, missing []uint16, masked []uint16)

func (s *Sim) readHoldingRange(start, qty uint16) ([]uint16, []uint16, []uint16) {
	return s.readRange(s.holding, start, qty)
}

func (s *Sim) readInputRange(start, qty uint16) ([]uint16, []uint16, []uint16) {
	return s.readRange(s.input, start, qty)
}

func (s *Sim) readRange(tbl map[uint16]uint16, start, qty uint16) ([]uint16, []uint16, []uint16) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vals := make([]uint16, qty)
	var missing, masked []uint16
	for i := uint16(0); i < qty; i++ {
		addr := start + i
		if s.masked[addr] {
			masked = append(masked, addr)
			continue
		}
		v, ok := tbl[addr]
		if !ok {
			missing = append(missing, addr)
			continue
		}
		vals[i] = v
	}
	return vals, missing, masked
}

// fcRead implements FC 0x03 and FC 0x04. The wire layout is the same;
// the only difference is which register table is read.
func (s *Sim) fcRead(pdu []byte, unitID byte, op readOp) []byte {
	const minPdu = 5 // FC 1 + addrHi 1 + addrLo 1 + qtyHi 1 + qtyLo 1
	if len(pdu) < minPdu {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	start := binary.BigEndian.Uint16(pdu[1:3])
	qty := binary.BigEndian.Uint16(pdu[3:5])
	// FC 0x03 / 0x04 cap at 125 registers per the spec.
	if qty < 1 || qty > 125 {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	vals, missing, masked := op(start, qty)
	if len(missing) > 0 || len(masked) > 0 {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataAddress}
	}
	// Build response PDU: FC echo + byte count + registers (big-endian).
	out := make([]byte, 0, 2+qty*2)
	out = append(out, pdu[0], byte(qty*2))
	for _, v := range vals {
		out = append(out, byte(v>>8), byte(v))
	}
	return out
}

// fcWriteSingle implements FC 0x06. Address must be writable (not
// masked); if the address is not yet defined, this CREATES the entry
// (the simulator is a fresh-device testbed — no preset required).
func (s *Sim) fcWriteSingle(pdu []byte) []byte {
	const minPdu = 5 // FC 1 + addrHi 1 + addrLo 1 + valHi 1 + valLo 1
	if len(pdu) < minPdu {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	addr := binary.BigEndian.Uint16(pdu[1:3])
	val := binary.BigEndian.Uint16(pdu[3:5])
	if s.isMasked(addr) {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataAddress}
	}
	s.mu.Lock()
	s.holding[addr] = val
	s.mu.Unlock()
	// Response = request echo (FC + addr + value).
	resp := make([]byte, minPdu)
	copy(resp, pdu[:minPdu])
	return resp
}

// fcWriteMultiple implements FC 0x10. qty is 1..123, byte_count must
// equal 2*qty, and every address must be writable (not masked;
// undefined addresses are still CREATED, matching fcWriteSingle).
func (s *Sim) fcWriteMultiple(pdu []byte) []byte {
	const headerLen = 5 // FC 1 + addrHi 1 + addrLo 1 + qtyHi 1 + qtyLo 1
	if len(pdu) < headerLen+1 {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	start := binary.BigEndian.Uint16(pdu[1:3])
	qty := binary.BigEndian.Uint16(pdu[3:5])
	if qty < 1 || qty > 123 {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	byteCount := int(pdu[5])
	if byteCount != int(qty)*2 || len(pdu) < headerLen+1+int(qty)*2 {
		return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataValue}
	}
	// Check the masked set up front so we don't half-write.
	s.mu.RLock()
	for i := uint16(0); i < qty; i++ {
		if s.masked[start+i] {
			s.mu.RUnlock()
			return []byte{pdu[0] | fcExceptionPrefix, excIllegalDataAddress}
		}
	}
	s.mu.RUnlock()
	// Apply writes under the write lock.
	s.mu.Lock()
	for i := uint16(0); i < qty; i++ {
		off := headerLen + 1 + int(i)*2
		s.holding[start+i] = binary.BigEndian.Uint16(pdu[off : off+2])
	}
	s.mu.Unlock()
	// Response: FC + start + qty.
	resp := make([]byte, 5)
	resp[0] = pdu[0]
	binary.BigEndian.PutUint16(resp[1:3], start)
	binary.BigEndian.PutUint16(resp[3:5], qty)
	return resp
}

// --- wire I/O helpers --------------------------------------------------------

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// writeResponse writes a full MBAP frame: 6-byte header + unit id + pdu.
// The length field is computed from the actual pdu size.
func writeResponse(conn net.Conn, txID uint16, unitID byte, pdu []byte) error {
	length := uint16(1 + len(pdu)) // unit id (1) + pdu
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[0:2], txID)
	binary.BigEndian.PutUint16(hdr[2:4], 0) // protocol id
	binary.BigEndian.PutUint16(hdr[4:6], length)
	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{unitID}); err != nil {
		return err
	}
	if len(pdu) > 0 {
		if _, err := conn.Write(pdu); err != nil {
			return err
		}
	}
	return nil
}
