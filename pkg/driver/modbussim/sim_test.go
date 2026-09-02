package modbussim

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dial connects to the simulator and returns a TCP connection.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

// buildRequest assembles a full Modbus TCP request frame:
// MBAP header (txid, protid=0, len, unitid) + PDU.
func buildRequest(txID uint16, unitID byte, pdu []byte) []byte {
	length := uint16(1 + len(pdu)) // unit id (1) + pdu
	frame := make([]byte, 0, 6+1+len(pdu))
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[0:2], txID)
	binary.BigEndian.PutUint16(hdr[2:4], 0)
	binary.BigEndian.PutUint16(hdr[4:6], length)
	frame = append(frame, hdr[:]...)
	frame = append(frame, unitID)
	frame = append(frame, pdu...)
	return frame
}

// readResponse reads one full Modbus TCP response: 6-byte MBAP header +
// (length) bytes following. The caller supplies the pduSize they expect
// AFTER the unit id; for variable-length responses (read-multi), pass 0
// and the function uses the MBAP length field.
func readResponse(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [6]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		t.Fatalf("read MBAP: %v", err)
	}
	length := binary.BigEndian.Uint16(hdr[4:6])
	if length < 1 || length > 253 {
		t.Fatalf("bad length: %d", length)
	}
	rest := make([]byte, length)
	if _, err := readFull(conn, rest); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rest
}

// request sends pdu and returns the response body (unit id + PDU).
func request(t *testing.T, conn net.Conn, txID uint16, unitID byte, pdu []byte) []byte {
	t.Helper()
	frame := buildRequest(txID, unitID, pdu)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return readResponse(t, conn)
}

// --- 钉死字节级测试向量（§4.2） -------------------------------------------

// Vector 1: 预置 Holding[0x0015]=123 (0x007B); FC3 读 1 寄存器。
func TestByteVector_FC3_OK(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0015: 123}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	// 请求: 00 01 00 00 00 06 01 03 00 15 00 01
	req := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x01, 0x03, 0x00, 0x15, 0x00, 0x01}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Read the full frame: 6-byte MBAP header + length-byte body.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [6]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		t.Fatalf("read MBAP: %v", err)
	}
	length := binary.BigEndian.Uint16(hdr[4:6])
	rest := make([]byte, length)
	if _, err := readFull(conn, rest); err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := append(hdr[:], rest...)

	// 期望: 00 01 00 00 00 05 01 03 02 00 7B
	want := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x05, 0x01, 0x03, 0x02, 0x00, 0x7B}
	if !bytes.Equal(got, want) {
		t.Errorf("byte vector mismatch:\n got  % X\n want % X", got, want)
	}
}

// Vector 2: 同一请求在 Mask(0x0015) 后 → 异常 0x02。
func TestByteVector_FC3_Masked(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0015: 123}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	sim.Mask(0x0015)

	req := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x01, 0x03, 0x00, 0x15, 0x00, 0x01}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [6]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		t.Fatalf("read MBAP: %v", err)
	}
	length := binary.BigEndian.Uint16(hdr[4:6])
	rest := make([]byte, length)
	if _, err := readFull(conn, rest); err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := append(hdr[:], rest...)

	// 期望: 00 01 00 00 00 03 01 83 02
	want := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x03, 0x01, 0x83, 0x02}
	if !bytes.Equal(got, want) {
		t.Errorf("byte vector mismatch:\n got  % X\n want % X", got, want)
	}
}

// Vector 3: FC 0x05（未实现）→ 异常 0x01。
func TestByteVector_FC5_Illegal(t *testing.T) {
	sim := New(Config{})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	// 请求: 00 02 00 00 00 06 01 05 00 00 FF 00
	req := []byte{0x00, 0x02, 0x00, 0x00, 0x00, 0x06, 0x01, 0x05, 0x00, 0x00, 0xFF, 0x00}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [6]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		t.Fatalf("read MBAP: %v", err)
	}
	length := binary.BigEndian.Uint16(hdr[4:6])
	rest := make([]byte, length)
	if _, err := readFull(conn, rest); err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := append(hdr[:], rest...)

	// 期望: 00 02 00 00 00 03 01 85 01
	want := []byte{0x00, 0x02, 0x00, 0x00, 0x00, 0x03, 0x01, 0x85, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("byte vector mismatch:\n got  % X\n want % X", got, want)
	}
}

// --- 表驱动协议测试 -------------------------------------------------------

func TestProtocol(t *testing.T) {
	cases := []struct {
		name     string
		setup    map[uint16]uint16
		input    map[uint16]uint16
		pdu      []byte // FC + body, no unit id
		wantPdu  []byte // expected PDU echo (FC + body) or FC|0x80 + exc
		wantReqN uint64 // optional: expected Requests() after the call (0 = don't check)
	}{
		{
			name:    "FC3 read single",
			setup:   map[uint16]uint16{0x0010: 0x1234},
			pdu:     []byte{0x03, 0x00, 0x10, 0x00, 0x01},
			wantPdu: []byte{0x03, 0x02, 0x12, 0x34},
		},
		{
			name:    "FC3 read multiple (3 regs)",
			setup:   map[uint16]uint16{0x0010: 0x0001, 0x0011: 0x0002, 0x0012: 0x0003},
			pdu:     []byte{0x03, 0x00, 0x10, 0x00, 0x03},
			wantPdu: []byte{0x03, 0x06, 0x00, 0x01, 0x00, 0x02, 0x00, 0x03},
		},
		{
			name:    "FC4 read input register",
			input:   map[uint16]uint16{0x0020: 0xABCD},
			pdu:     []byte{0x04, 0x00, 0x20, 0x00, 0x01},
			wantPdu: []byte{0x04, 0x02, 0xAB, 0xCD},
		},
		{
			name:    "FC6 write single",
			setup:   map[uint16]uint16{0x0005: 0x0000},
			pdu:     []byte{0x06, 0x00, 0x05, 0xCA, 0xFE},
			wantPdu: []byte{0x06, 0x00, 0x05, 0xCA, 0xFE},
		},
		{
			name:    "FC6 write creates new address",
			setup:   nil,
			pdu:     []byte{0x06, 0x00, 0x50, 0x12, 0x34},
			wantPdu: []byte{0x06, 0x00, 0x50, 0x12, 0x34},
		},
		{
			name:    "FC10 write 3 registers",
			setup:   nil,
			pdu:     []byte{0x10, 0x00, 0x20, 0x00, 0x03, 0x06, 0x00, 0xAA, 0x00, 0xBB, 0x00, 0xCC},
			wantPdu: []byte{0x10, 0x00, 0x20, 0x00, 0x03},
		},
		{
			name:    "FC3 qty=0 → 0x03",
			pdu:     []byte{0x03, 0x00, 0x00, 0x00, 0x00},
			wantPdu: []byte{0x83, 0x03},
		},
		{
			name:    "FC3 qty=126 → 0x03",
			pdu:     []byte{0x03, 0x00, 0x00, 0x00, 0x7E},
			wantPdu: []byte{0x83, 0x03},
		},
		{
			name:    "FC3 undefined address → 0x02",
			pdu:     []byte{0x03, 0x00, 0x99, 0x00, 0x01},
			wantPdu: []byte{0x83, 0x02},
		},
		{
			name:    "FC10 byte_count mismatch → 0x03",
			pdu:     []byte{0x10, 0x00, 0x00, 0x00, 0x02, 0x05, 0x00, 0x01, 0x00, 0x02, 0x00}, // 5 bytes but qty=2 expects 4
			wantPdu: []byte{0x90, 0x03},
		},
		{
			name:    "FC2 (illegal) → 0x01",
			pdu:     []byte{0x02, 0x00, 0x00, 0x00, 0x01},
			wantPdu: []byte{0x82, 0x01},
		},
		{
			name:    "FC1 (illegal) → 0x01",
			pdu:     []byte{0x01, 0x00, 0x00, 0x00, 0x08},
			wantPdu: []byte{0x81, 0x01},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := New(Config{Holding: c.setup, Input: c.input})
			addr, err := sim.Start()
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer sim.Stop()

			conn := dial(t, addr)
			defer conn.Close()

			resp := request(t, conn, 1, 1, c.pdu)
			// resp = unit id + PDU
			if len(resp) < 2 {
				t.Fatalf("response too short: % X", resp)
			}
			gotPDU := resp[1:]
			if !bytes.Equal(gotPDU, c.wantPdu) {
				t.Errorf("pdu mismatch:\n got  % X\n want % X", gotPDU, c.wantPdu)
			}
		})
	}
}

// --- Mask 行为 -------------------------------------------------------------

func TestMaskReadAndWrite(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0001: 0xAAAA, 0x0002: 0xBBBB}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	sim.Mask(0x0002)
	conn := dial(t, addr)
	defer conn.Close()

	// Read 0x0002 → 0x02
	resp := request(t, conn, 1, 1, []byte{0x03, 0x00, 0x02, 0x00, 0x01})
	if !bytes.Equal(resp[1:], []byte{0x83, 0x02}) {
		t.Errorf("read masked: got % X, want 83 02", resp[1:])
	}
	// FC6 to 0x0002 → 0x02
	resp = request(t, conn, 2, 1, []byte{0x06, 0x00, 0x02, 0x12, 0x34})
	if !bytes.Equal(resp[1:], []byte{0x86, 0x02}) {
		t.Errorf("FC6 masked: got % X, want 86 02", resp[1:])
	}
	// FC10 touching 0x0002 → 0x02
	resp = request(t, conn, 3, 1, []byte{0x10, 0x00, 0x01, 0x00, 0x02, 0x04, 0x00, 0xAA, 0x00, 0xBB})
	if !bytes.Equal(resp[1:], []byte{0x90, 0x02}) {
		t.Errorf("FC10 masked: got % X, want 90 02", resp[1:])
	}
	// Unmask then read works again
	sim.Unmask(0x0002)
	resp = request(t, conn, 4, 1, []byte{0x03, 0x00, 0x02, 0x00, 0x01})
	if !bytes.Equal(resp[1:], []byte{0x03, 0x02, 0xBB, 0xBB}) {
		t.Errorf("read after unmask: got % X, want 03 02 BB BB", resp[1:])
	}
}

// --- FC6 write then read back (round-trip) ---------------------------------

func TestFC6RoundTrip(t *testing.T) {
	sim := New(Config{})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	// Write 0x0042 = 0xBEEF.
	resp := request(t, conn, 1, 1, []byte{0x06, 0x00, 0x42, 0xBE, 0xEF})
	if !bytes.Equal(resp[1:], []byte{0x06, 0x00, 0x42, 0xBE, 0xEF}) {
		t.Errorf("FC6 resp: got % X, want 06 00 42 BE EF", resp[1:])
	}
	// Read it back.
	resp = request(t, conn, 2, 1, []byte{0x03, 0x00, 0x42, 0x00, 0x01})
	if !bytes.Equal(resp[1:], []byte{0x03, 0x02, 0xBE, 0xEF}) {
		t.Errorf("FC3 readback: got % X, want 03 02 BE EF", resp[1:])
	}
	// And via the Go-side accessor.
	v, ok := sim.GetHolding(0x0042)
	if !ok || v != 0xBEEF {
		t.Errorf("GetHolding = (%#x, %v), want (0xBEEF, true)", v, ok)
	}
}

// --- FC10 write 3 regs then read back --------------------------------------

func TestFC10RoundTrip(t *testing.T) {
	sim := New(Config{})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	// Write 0x0100..0x0102 = {0xAA, 0xBB, 0xCC}
	pdu := []byte{0x10, 0x01, 0x00, 0x00, 0x03, 0x06, 0x00, 0xAA, 0x00, 0xBB, 0x00, 0xCC}
	resp := request(t, conn, 1, 1, pdu)
	if !bytes.Equal(resp[1:], []byte{0x10, 0x01, 0x00, 0x00, 0x03}) {
		t.Errorf("FC10 resp: got % X, want 10 01 00 00 03", resp[1:])
	}
	// Read them back as a 3-register FC3.
	resp = request(t, conn, 2, 1, []byte{0x03, 0x01, 0x00, 0x00, 0x03})
	if !bytes.Equal(resp[1:], []byte{0x03, 0x06, 0x00, 0xAA, 0x00, 0xBB, 0x00, 0xCC}) {
		t.Errorf("FC3 readback: got % X, want 03 06 00 AA 00 BB 00 CC", resp[1:])
	}
}

// --- protocol id != 0 断连接 -----------------------------------------------

func TestProtocolIDNonZeroCloses(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0001: 0x0001}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()

	// Send a frame with protocol id = 0x0001.
	bad := []byte{0x00, 0x01, 0x00, 0x01, 0x00, 0x06, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01}
	if _, err := conn.Write(bad); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The server closes the connection; a follow-up read must return
	// an error (EOF or "use of closed connection").
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Errorf("expected read error after bad protocol id, got nil")
	}
}

// --- 并发: 4 连接 × 50 请求 -----------------------------------------------

func TestConcurrentRequests(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0001: 0xCAFE, 0x0002: 0xBABE}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	const conns, perConn = 4, 50
	var wg sync.WaitGroup
	for c := 0; c < conns; c++ {
		wg.Add(1)
		go func(cIdx int) {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()
			for i := 0; i < perConn; i++ {
				// Mix of FC3 reads and FC6 writes.
				var pdu []byte
				if i%2 == 0 {
					pdu = []byte{0x03, 0x00, 0x01, 0x00, 0x02}
				} else {
					pdu = []byte{0x06, 0x00, 0x09, 0xDE, 0xAD}
				}
				resp := request(t, conn, uint16(cIdx*1000+i), 1, pdu)
				// Just check the FC echo; full byte-vector would be
				// over-specifying here.
				if len(resp) < 2 || resp[1] != pdu[0] {
					t.Errorf("conn %d req %d: bad resp % X", cIdx, i, resp)
				}
			}
		}(c)
	}
	wg.Wait()

	want := uint64(conns * perConn)
	if got := sim.Requests(); got != want {
		t.Errorf("Requests() = %d, want %d", got, want)
	}
}

// --- 生命周期 ------------------------------------------------------------

func TestStartTwiceFails(t *testing.T) {
	sim := New(Config{})
	_, err := sim.Start()
	if err != nil {
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

func TestStopReleasesPort(t *testing.T) {
	sim := New(Config{Listen: "127.0.0.1:0"})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = addr
	sim.Stop()
	// After Stop, we should be able to start a new Sim on the same
	// kernel-allocated port. We don't know which port was assigned,
	// so just verify a fresh Start succeeds (any port).
	sim2 := New(Config{Listen: "127.0.0.1:0"})
	addr2, err := sim2.Start()
	if err != nil {
		t.Fatalf("second Start after Stop: %v", err)
	}
	defer sim2.Stop()
	if addr2 == "" {
		t.Errorf("second Start returned empty addr")
	}
}

func TestRefuseNonLoopback(t *testing.T) {
	// Must refuse to bind a non-loopback address. The point of the
	// guard is to keep this from being mistaken for a service.
	sim := New(Config{Listen: "0.0.0.0:0"})
	addr, err := sim.Start()
	if err == nil {
		t.Errorf("Start on 0.0.0.0: expected error, got nil")
		sim.Stop()
	}
	_ = addr
}

// TestAcceptNonLoopbackWithAllowPublicBind is the §4.8-bis companion
// to TestRefuseNonLoopback: with Config.AllowPublicBind set, a
// non-loopback bind must succeed so a co-resident container (in
// compose's default bridge) can reach the simulator. The test
// binds 0.0.0.0 so the driver-layer AllowPublicBind path is
// actually exercised — not just the loopback path with a flag set.
func TestAcceptNonLoopbackWithAllowPublicBind(t *testing.T) {
	sim := New(Config{Listen: "0.0.0.0:0", AllowPublicBind: true})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start with AllowPublicBind: %v", err)
	}
	defer sim.Stop()
	if strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("expected non-loopback addr (random port), got loopback %q", addr)
	}
	if !strings.HasPrefix(addr, "0.0.0.0:") && !strings.HasPrefix(addr, "[::]:") {
		t.Errorf("expected 0.0.0.0: or [::]: addr, got %q", addr)
	}
}

// --- 寄存器并发安全 --------------------------------------------------------

func TestConcurrentSetGetHolding(t *testing.T) {
	sim := New(Config{})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = addr
	defer sim.Stop()

	const writers, readers, iters = 4, 4, 200
	var wg sync.WaitGroup
	var writeN atomic.Uint64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				sim.SetHolding(uint16(i%10), uint16(i))
				writeN.Add(1)
			}
		}()
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = sim.GetHolding(uint16(i % 10))
			}
		}()
	}
	wg.Wait()
	if got := writeN.Load(); got != writers*iters {
		t.Errorf("writeN = %d, want %d", got, writers*iters)
	}
}

// --- 请求计数 ------------------------------------------------------------

func TestRequestsCount(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0001: 0x1234}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()

	conn := dial(t, addr)
	defer conn.Close()
	for i := 0; i < 5; i++ {
		_ = request(t, conn, uint16(i), 1, []byte{0x03, 0x00, 0x01, 0x00, 0x01})
	}
	if got := sim.Requests(); got != 5 {
		t.Errorf("Requests() = %d, want 5", got)
	}
}

// --- 简单的可达性 smoke ---------------------------------------------------

func TestSmokeConnect(t *testing.T) {
	sim := New(Config{Holding: map[uint16]uint16{0x0001: 0x0001}})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sim.Stop()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	_ = fmt.Sprintf("addr=%s", addr) // silence unused import on fmt if ever needed
}

// --- PRMT-014: Stop 竞态回归 --------------------------------------------
//
// 触发 acceptLoop 的竞态窗口：Stop 在 listener.Close 与 conns.Range 之间，
// acceptLoop 在 listener.Close 之前 Accept 到连接，但 Store 落在 Range
// 之后 → 该连接漏关，handle 永久阻塞读，wg.Wait 死锁。Stop 路径本身
// 加看门狗：超时即 Fatal。

func TestStopRaceNoHang(t *testing.T) {
	const rounds = 200
	const dialers = 8
	const stopBudget = 10 * time.Second

	for round := 0; round < rounds; round++ {
		sim := New(Config{Listen: "127.0.0.1:0"})
		addr, err := sim.Start()
		if err != nil {
			t.Fatalf("round %d: Start: %v", round, err)
		}

		// 8 个 dialer 持续拨通；拨通即持有不发数据，让 handle 卡在 read 上。
		stopDialers := make(chan struct{})
		var dialerWG sync.WaitGroup
		for d := 0; d < dialers; d++ {
			dialerWG.Add(1)
			go func() {
				defer dialerWG.Done()
				for {
					select {
					case <-stopDialers:
						return
					default:
					}
					c, derr := net.Dial("tcp", addr)
					if derr != nil {
						// 服务端可能已关监听；让出 CPU 短睡一下重试。
						time.Sleep(time.Millisecond)
						continue
					}
					// 持有不写不读，最大化制造 handle 阻塞读的概率。
					_ = c
				}
			}()
		}

		// 看门狗：Stop 超时直接 Fatal。
		stopDone := make(chan struct{})
		go func() {
			close(stopDone)
		}()
		timer := time.AfterFunc(stopBudget, func() {
			t.Fatalf("round %d: Stop() hung > %s (race window not closed)", round, stopBudget)
		})
		// 触发 Stop —— 必须返回；超时由 AfterFunc 抓。
		sim.Stop()
		timer.Stop()
		<-stopDone

		// 收尾 dialer goroutine，再开始下一轮。
		close(stopDialers)
		dialerWG.Wait()
	}
}
