package natspub

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/wal"
)

// mockJS is a hand-rolled stand-in for nats.JetStreamContext. It
// records every Publish call and returns whatever failure the test
// has set. It is intentionally tiny: the Publisher contract is just
// "Publish(subject, data)", and a hand-rolled mock is far cheaper
// than pulling in nats-server for these unit tests.
type mockJS struct {
	mu       sync.Mutex
	calls    []mockCall
	failWith error // if non-nil, Publish returns this error
	failN    int   // if >0, return failWith for the first failN calls then succeed
}

type mockCall struct {
	subject string
	data    []byte
}

func (m *mockJS) Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failN > 0 {
		m.failN--
		return nil, m.failWith
	}
	if m.failWith != nil {
		return nil, m.failWith
	}
	// Copy data: the Publisher may reuse the buffer.
	cp := make([]byte, len(data))
	copy(cp, data)
	m.calls = append(m.calls, mockCall{subject: subject, data: cp})
	return &nats.PubAck{Stream: "CIOS_TLM", Sequence: uint64(len(m.calls))}, nil
}

func (m *mockJS) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func newBatch() TelemetryBatch {
	return TelemetryBatch{
		Site:      "sgp01",
		TopAsset:  "sgp01.pod002",
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		Encoding:  "promtext",
		Lines:     []string{`metric{foo="bar"} 1 1700000000000`},
	}
}

func TestSubject(t *testing.T) {
	b := TelemetryBatch{Site: "sgp01", TopAsset: "sgp01.pod002"}
	if got := b.Subject(); got != "cios.tlm.sgp01.sgp01.pod002" {
		t.Errorf("Subject = %q", got)
	}
}

func TestPublishHappyPath(t *testing.T) {
	js := &mockJS{}
	pub := New(js, nil)
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := js.callCount(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	// Sanity-check the payload round-trips back to the batch.
	got := js.calls[0]
	if got.subject != "cios.tlm.sgp01.sgp01.pod002" {
		t.Errorf("subject = %q", got.subject)
	}
	// Encoding field is present in the wire bytes (the consumer
	// branches on it).
	for _, frag := range []string{`"encoding":"promtext"`, `"site":"sgp01"`} {
		if !contains(got.data, frag) {
			t.Errorf("payload missing fragment %q in %s", frag, got.data)
		}
	}
}

func TestPublishFailureFallsBackToWAL(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer w.Close()

	js := &mockJS{failWith: errors.New("nats down")}
	pub := New(js, w)
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish should have succeeded via WAL, got %v", err)
	}
	if got := js.callCount(); got != 0 {
		t.Errorf("calls = %d, want 0 (NATS was down)", got)
	}
	n, err := w.Len()
	if err != nil {
		t.Fatalf("wal.Len: %v", err)
	}
	if n != 1 {
		t.Errorf("wal.Len = %d, want 1", n)
	}
}

func TestPublishFailureWithoutWALReturnsError(t *testing.T) {
	js := &mockJS{failWith: errors.New("nats down")}
	pub := New(js, nil)
	err := pub.Publish(context.Background(), newBatch())
	if err == nil {
		t.Fatalf("Publish should have returned the NATS error")
	}
	if !contains([]byte(err.Error()), "nats down") {
		t.Errorf("err = %v, want it to wrap the NATS failure", err)
	}
}

func TestWALReplayBeforePublish(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer w.Close()

	// Pre-load the WAL with one stale frame (as if NATS had been
	// down for the previous tick). The next Publish must replay it
	// first, then publish the live batch.
	js := &mockJS{}
	pub := New(js, w)
	// Force the WAL to fail its first Publish so the live batch
	// ends up in the WAL too.
	js.failWith = errors.New("nats down")
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	// NATS comes back. Now Publish must (a) replay the previously
	// buffered frame and (b) publish the live one.
	js.failWith = nil
	js.failN = 0
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}
	// 2 calls expected: 1 from replay + 1 from live.
	if got := js.callCount(); got != 2 {
		t.Errorf("calls = %d, want 2 (replay + live)", got)
	}
	// WAL must be drained.
	n, err := w.Len()
	if err != nil {
		t.Fatalf("wal.Len: %v", err)
	}
	if n != 0 {
		t.Errorf("wal.Len = %d, want 0", n)
	}
}

func TestWALReplayFailureDoesNotBlockLivePublish(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer w.Close()

	// Step 1: stuff a frame into the WAL.
	js := &mockJS{failWith: errors.New("nats down")}
	pub := New(js, w)
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	// Step 2: NATS still down; the replay call will fail AND the
	// live publish will fail. The live batch should land in the
	// WAL again.
	if err := pub.Publish(context.Background(), newBatch()); err != nil {
		t.Fatalf("Publish 2 should not have returned an error (live batch went to WAL): %v", err)
	}
	// The live batch is now the second frame in the WAL.
	n, err := w.Len()
	if err != nil {
		t.Fatalf("wal.Len: %v", err)
	}
	if n != 2 {
		t.Errorf("wal.Len = %d, want 2 (previous + new live)", n)
	}
}

func TestPublishRespectsContext(t *testing.T) {
	js := &mockJS{}
	pub := New(js, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pub.Publish(ctx, newBatch())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := js.callCount(); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}

func TestPublishNilJS(t *testing.T) {
	pub := New(nil, nil)
	err := pub.Publish(context.Background(), newBatch())
	if err == nil {
		t.Fatal("Publish with nil js should return an error")
	}
}

// contains is a tiny local helper; pulling in strings.Contains
// would be fine but the call sites read more naturally with a
// package-private helper when we already import nothing else.
func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

// F1 regression (PRMT-015 R1): when WAL frames originate from
// multiple top_assets (NATS was down across a top_asset
// boundary), each frame must be replayed to ITS OWN subject —
// not the current tick's subject. This was the bug F1 caught:
// the original replay callback used b.Subject() for every
// buffered frame, silently violating spec-006 §2.2 routing.
func TestWALReplayPreservesPerFrameSubject(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer w.Close()

	// Step 1: NATS is down. Two different top_assets each get
	// their batch buffered into the same WAL.
	js := &mockJS{failWith: errors.New("nats down")}
	pub := New(js, w)
	batchA := TelemetryBatch{
		Site: "sgp01", TopAsset: "sgp01.pod002",
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		Encoding:  "promtext",
		Lines:     []string{`a{foo="1"} 1 1700000000000`},
	}
	batchB := TelemetryBatch{
		Site: "sgp01", TopAsset: "sgp01.pod003",
		Timestamp: time.Unix(1_700_000_001, 0).UTC(),
		Encoding:  "promtext",
		Lines:     []string{`b{foo="2"} 2 1700000001000`},
	}
	if err := pub.Publish(context.Background(), batchA); err != nil {
		t.Fatalf("Publish A (NATS down): %v", err)
	}
	if err := pub.Publish(context.Background(), batchB); err != nil {
		t.Fatalf("Publish B (NATS down): %v", err)
	}
	if got := js.callCount(); got != 0 {
		t.Fatalf("calls after step 1 = %d, want 0 (NATS was down)", got)
	}
	if n, _ := w.Len(); n != 2 {
		t.Fatalf("wal.Len = %d, want 2", n)
	}

	// Step 2: NATS recovers. Publish a third batch whose subject
	// is yet another top_asset. The Publisher must first replay
	// the two buffered frames to THEIR subjects, then publish the
	// live batch to its own subject.
	js.failWith = nil
	batchC := TelemetryBatch{
		Site: "sgp01", TopAsset: "sgp01.pod004",
		Timestamp: time.Unix(1_700_000_002, 0).UTC(),
		Encoding:  "promtext",
		Lines:     []string{`c{foo="3"} 3 1700000002000`},
	}
	if err := pub.Publish(context.Background(), batchC); err != nil {
		t.Fatalf("Publish C (NATS up): %v", err)
	}

	js.mu.Lock()
	defer js.mu.Unlock()
	if got := len(js.calls); got != 3 {
		t.Fatalf("total calls = %d, want 3 (2 replay + 1 live)", got)
	}
	// Order: WAL is drained in append order; the live batch
	// publishes after the replay completes.
	wantSubjects := []string{
		"cios.tlm.sgp01.sgp01.pod002", // buffered A
		"cios.tlm.sgp01.sgp01.pod003", // buffered B
		"cios.tlm.sgp01.sgp01.pod004", // live C
	}
	for i, want := range wantSubjects {
		if got := js.calls[i].subject; got != want {
			t.Errorf("call[%d] subject = %q, want %q", i, got, want)
		}
	}
	// And the per-frame payload must be the exact bytes of the
	// originating batch — not the current tick's payload.
	if !contains(js.calls[0].data, `"top_asset":"sgp01.pod002"`) {
		t.Errorf("replayed A lost its identity: %s", js.calls[0].data)
	}
	if !contains(js.calls[1].data, `"top_asset":"sgp01.pod003"`) {
		t.Errorf("replayed B lost its identity: %s", js.calls[1].data)
	}

	// WAL must be drained.
	if n, _ := w.Len(); n != 0 {
		t.Errorf("wal.Len after replay = %d, want 0", n)
	}
}

// F1 regression: a single undecodable WAL frame must not wedge
// the replay. Skip the bad frame, keep replaying the rest, and
// leave the WAL truncation to the successful final frame.
func TestWALReplaySkipsUndecodableFrame(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer w.Close()

	// Hand-craft: one good frame + one garbage frame.
	if err := w.Write([]byte(`{"site":"sgp01","top_asset":"sgp01.pod002","encoding":"promtext","lines":[]}`)); err != nil {
		t.Fatalf("Write good: %v", err)
	}
	if err := w.Write([]byte("not-json-at-all")); err != nil {
		t.Fatalf("Write bad: %v", err)
	}
	if err := w.Write([]byte(`{"site":"sgp01","top_asset":"sgp01.pod003","encoding":"promtext","lines":[]}`)); err != nil {
		t.Fatalf("Write good: %v", err)
	}

	js := &mockJS{}
	pub := New(js, w)
	if err := pub.Publish(context.Background(), TelemetryBatch{
		Site: "sgp01", TopAsset: "sgp01.pod099",
		Encoding: "promtext",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Two good frames should have been replayed (the bad one
	// skipped). The live batch publishes after the replay.
	js.mu.Lock()
	defer js.mu.Unlock()
	if got := len(js.calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (2 good replay + 1 live)", got)
	}
	if got := js.calls[0].subject; got != "cios.tlm.sgp01.sgp01.pod002" {
		t.Errorf("call[0] subject = %q", got)
	}
	if got := js.calls[1].subject; got != "cios.tlm.sgp01.sgp01.pod003" {
		t.Errorf("call[1] subject = %q", got)
	}
	if got := js.calls[2].subject; got != "cios.tlm.sgp01.sgp01.pod099" {
		t.Errorf("call[2] subject = %q", got)
	}
	// WAL drained because the replay finished without error.
	if n, _ := w.Len(); n != 0 {
		t.Errorf("wal.Len = %d, want 0", n)
	}
}
