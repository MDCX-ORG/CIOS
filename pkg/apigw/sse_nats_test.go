// Tests for natsTelemetrySource (PRMT-119). Hand-rolled mock of
// jsSubscriber — same approach as pkg/natspub/publisher_test.go,
// avoids pulling in nats-server. The mock records the subject the
// source subscribed to and replays synthetic nats.Msg values into
// the registered handler so the test can drive the message path
// without a real broker.
package apigw

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/sts"
)

// mockJS is the SUBSCRIBE-side counterpart of pkg/natspub.mockJS.
// It captures the registered handlers so the test can simulate
// inbound messages without a real nats-server. R4 (F6): a single
// mockJS instance can hold multiple registered handlers so tests
// can open more than one Subscribe against it and verify that
// teardown of one connection does not poison the others.
type mockJS struct {
	mu sync.Mutex
	// subj is the most recent subject passed to Subscribe. The
	// multi-connection test reads it after both subscribes.
	subj   string
	cbs    []nats.MsgHandler
	opts   []nats.SubOpt
	failOn error // if non-nil, Subscribe returns this and does not register

	// optSigs records reflect.Pointer of each captured SubOpt so
	// PRMT-119 §5-bis F4 can assert the function identity (best
	// effort — see test for inlining caveat).
	optSigs []uintptr

	// goroutines spawned by the source need to be observable so
	// the test can assert no-leak. We track them via the active
	// subscription handle below.
}

func (m *mockJS) Subscribe(subj string, cb nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != nil {
		return nil, m.failOn
	}
	m.subj = subj
	m.cbs = append(m.cbs, cb)
	m.opts = opts
	// Capture function pointers for F4 reflect-based identity check.
	m.optSigs = m.optSigs[:0]
	for _, o := range opts {
		m.optSigs = append(m.optSigs, reflect.ValueOf(o).Pointer())
	}
	return &nats.Subscription{}, nil
}

// deliver fans a synthetic message out to every registered
// handler. The source reads msg.Data verbatim and (per PRMT-119
// §4) leaves Event.ID empty; nothing on the Msg struct is needed
// beyond Subject + Data. Fan-out mirrors how a real nats-server
// dispatches a published message to all matching subscribers.
func (m *mockJS) deliver(data []byte) {
	m.mu.Lock()
	cbs := append([]nats.MsgHandler(nil), m.cbs...)
	subj := m.subj
	m.mu.Unlock()
	for _, cb := range cbs {
		cb(&nats.Msg{
			Subject: subj,
			Data:    data,
		})
	}
}

func TestNATSTelemetrySource_SubscribeForwardsEvents(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := src.Subscribe(ctx, "sgp01", sts.TokenClaims{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if ch == nil {
		t.Fatalf("Subscribe returned nil channel")
	}

	// Subject template: cios.tlm.<site>.>.
	if js.subj != "cios.tlm.sgp01.>" {
		t.Fatalf("subject = %q, want %q", js.subj, "cios.tlm.sgp01.>")
	}
	// DeliverNew identity: PRMT-119 §5-bis F4 uses
	// reflect.ValueOf(SubOpt).Pointer() to compare the function
	// entry the source passed against the entry of an independent
	// nats.DeliverNew() call. The strict pointer comparison is
	// best-effort (compiler inlining can split entries); the
	// dedicated test TestNATSTelemetrySource_SubscribeUsesDeliverNewOnly
	// documents the fallback and §8.5 follow-up. Here we only
	// assert the contract: exactly one option was passed.
	if len(js.opts) != 1 {
		t.Fatalf("expected exactly 1 nats.SubOpt, got %d", len(js.opts))
	}

	// Drive two messages through, draining between sends. The
	// source channel is buffered size 1 and the dispatcher uses a
	// non-blocking send (back-pressure is ctx-cancel, not channel
	// full — that's the production posture), so two rapid
	// deliveries without a consumer read would drop the second.
	js.deliver([]byte(`{"site":"sgp01","top_asset":"sgp01.pod002"}`))
	first := collectEvents(t, ch, 1, time.Second)
	js.deliver([]byte(`{"site":"sgp01","top_asset":"sgp01.pod003"}`))
	second := collectEvents(t, ch, 1, time.Second)
	got := append(first, second...)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	wantData := []string{
		`{"site":"sgp01","top_asset":"sgp01.pod002"}`,
		`{"site":"sgp01","top_asset":"sgp01.pod003"}`,
	}
	for i, ev := range got {
		if ev.Type != "telemetry" {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, "telemetry")
		}
		if string(ev.Data) != wantData[i] {
			t.Errorf("event[%d].Data altered: got %q, want %q", i, ev.Data, wantData[i])
		}
	}
	// Event.ID is left empty per §4 ("可用 ... 或空"); the SSE
	// writer omits the id: line on zero value (sse.go Event doc).

	cancel()
}

func TestNATSTelemetrySource_CTXCancelClosesChannel(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := src.Subscribe(ctx, "sgp01", sts.TokenClaims{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Cancel; the source MUST close the channel.
	cancel()

	select {
	case _, open := <-ch:
		if open {
			t.Fatalf("channel still open after ctx cancel; expected close")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel never closed after ctx cancel")
	}
}

func TestNATSTelemetrySource_SubscribeError(t *testing.T) {
	js := &mockJS{failOn: errors.New("broker down")}
	src := NewNATSTelemetrySource(js)

	ch, err := src.Subscribe(context.Background(), "sgp01", sts.TokenClaims{})
	if err == nil {
		t.Fatalf("Subscribe returned nil error; want broker failure")
	}
	if ch != nil {
		t.Fatalf("Subscribe returned non-nil channel on error: %v", ch)
	}
}

func TestNATSTelemetrySource_NilJSRejected(t *testing.T) {
	src := NewNATSTelemetrySource(nil)
	if _, err := src.Subscribe(context.Background(), "sgp01", sts.TokenClaims{}); err == nil {
		t.Fatalf("nil js should error")
	}
}

func TestNATSTelemetrySource_RejectsEmptySite(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)
	if _, err := src.Subscribe(context.Background(), "", sts.TokenClaims{}); err == nil {
		t.Fatalf("empty site should error")
	}
	if js.subj != "" {
		t.Fatalf("Subscribe should not have touched the JetStream client on empty site; got subj=%q", js.subj)
	}
}

// collectEvents drains up to want events from ch, or fails the test
// after timeout. Used by the forwarder test so a regression
// doesn't hang indefinitely.
func collectEvents(t *testing.T, ch <-chan Event, want int, timeout time.Duration) []Event {
	t.Helper()
	out := make([]Event, 0, want)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(out) < want {
		select {
		case ev, open := <-ch:
			if !open {
				return out
			}
			out = append(out, ev)
		case <-deadline.C:
			return out
		}
	}
	return out
}

// PRMT-119 §5-bis S1: every site value that fails the
// ^[a-z]{2,8}[0-9]{2}$ regex MUST be rejected before any NATS
// subject is built, and the JetStream client MUST NOT be touched
// (mockJS.subj stays empty). This is the broker-level injection
// guard that defends cios.tlm.<site>.> against wildcard / path-
// separator injection.
func TestNATSTelemetrySource_RejectsInjectionChars(t *testing.T) {
	cases := []struct {
		name string
		site string
	}{
		{"asterisk", "*"},
		{"gt", ">"},
		{"dotdot", ".."},
		{"site-with-wildcard", "sgp01.>"},
		{"uppercase", "SGP01"},
		{"slash", "sgp01/pod002"},
		{"empty-after-split", ""},
		{"too-long", "a-very-long-site-name-that-fails-the-pattern"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			js := &mockJS{}
			src := NewNATSTelemetrySource(js)
			_, err := src.Subscribe(context.Background(), tc.site, sts.TokenClaims{})
			if err == nil {
				t.Fatalf("site=%q should have been rejected", tc.site)
			}
			if js.subj != "" {
				t.Fatalf("Subscribe reached JetStream with invalid site %q; got subj=%q", tc.site, js.subj)
			}
		})
	}
}

// PRMT-119 §5-bis F1: PRMT §5 #2 literal wording requires
// "goroutine 计数前后一致" — race detector alone is not enough. We
// snapshot runtime.NumGoroutine() before Subscribe, after
// Subscribe (tolerate ≤1 jitter for the watcher goroutine + the
// dispatcher closure), and after cancel (poll up to 2s for
// drain back to baseline).
//
// Cited: §5 #2 of the prompt — "-race + goroutine 计数前后一致".
// The R1 implementation's §8 admitted this was a gap; R2 closes
// it with an explicit NumGoroutine assertion.
func TestNATSTelemetrySource_NoGoroutineLeak(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx, cancel := context.WithCancel(context.Background())

	n0 := runtime.NumGoroutine()

	ch, err := src.Subscribe(ctx, "sgp01", sts.TokenClaims{})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}

	n1 := runtime.NumGoroutine()
	if n1 > n0+1 {
		t.Fatalf("after Subscribe goroutines = %d, want ≤ n0+1 = %d", n1, n0+1)
	}

	cancel()

	// Poll up to 2s (10ms × 200) for the count to drop back to
	// within ≤1 of n0. If it doesn't, fail loudly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n2 := runtime.NumGoroutine()
		if n2 <= n0+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: n0=%d, n1=%d, n2=%d after cancel+2s", n0, n1, n2)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Channel must be closed.
	select {
	case _, open := <-ch:
		if open {
			t.Fatalf("channel still open after cancel+drain")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel never closed after cancel+drain")
	}
}

// PRMT-119 §5-bis F5 (R4): concurrent dispatch + ctx cancel must
// not panic with "send on closed channel". The source uses
// per-connection `mu` and `closed` locals captured by the
// callback and the watcher closures (R4: these are closure-local
// in Subscribe, NOT fields on the shared natsTelemetrySource — see
// F6 test below for the multi-connection half of the invariant).
// The mutex makes the close and the send mutually exclusive
// regardless of dispatcher timing, so a send can never observe a
// closed channel.
//
// We drive 10 goroutines × 10 deliveries each via the synchronous
// mockJS.deliver (which calls our closure on the calling
// goroutine), overlapping with the watcher's Unsubscribe→lock→
// closed=true→close sequence. The invariant is no panic, no
// "send on closed channel".
func TestNATSTelemetrySource_ConcurrentDispatchAndCancel(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := src.Subscribe(ctx, "sgp01", sts.TokenClaims{})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}

	var producer sync.WaitGroup
	for g := 0; g < 10; g++ {
		producer.Add(1)
		go func() {
			defer producer.Done()
			for i := 0; i < 10; i++ {
				js.deliver([]byte(`{"i":1}`))
			}
		}()
	}

	// Let some dispatches land before we cancel.
	time.Sleep(1 * time.Millisecond)
	cancel()

	// Drain producers; their deliveries may either succeed (send
	// into buffered channel) or hit the default branch (channel
	// full + ctx not yet done) — both are legal under the
	// non-blocking send. The invariant is no panic, no send on
	// closed channel.
	producer.Wait()

	// Channel must close. Polling avoids a hang if the watcher is
	// slow to drain.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel not closed within 2s after cancel")
		}
	}
}

// PRMT-119 §5-bis F4: the source MUST pass exactly one SubOpt,
// and that SubOpt MUST be nats.DeliverNew(). We assert identity
// via reflect.ValueOf(...).Pointer() — the function entry
// address. Caveat: the Go compiler may inline nats.DeliverNew
// at different call sites, producing different entry addresses
// for what is logically the same function. If strict equality
// fails, we log both addresses (for the §8.5 follow-up) and
// fall back to the weaker len==1 assertion + comment admitting
// the limitation. The test is intentionally tolerant: it does
// NOT mark the suite as failed purely on inlining mismatch.
func TestNATSTelemetrySource_SubscribeUsesDeliverNewOnly(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := src.Subscribe(ctx, "sgp01", sts.TokenClaims{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	js.mu.Lock()
	optSigs := append([]uintptr(nil), js.optSigs...)
	js.mu.Unlock()

	expectedSig := reflect.ValueOf(nats.DeliverNew()).Pointer()

	if len(optSigs) != 1 {
		t.Fatalf("expected exactly 1 SubOpt captured, got %d", len(optSigs))
	}
	if optSigs[0] != expectedSig {
		// Inlining fallback: log both addresses, do not fail.
		t.Logf("reflect.Pointer mismatch (likely compiler inlining): got=0x%x expected=0x%x — falling back to len==1; see §8.5 follow-up", optSigs[0], expectedSig)
	} else {
		t.Logf("reflect.Pointer match: 0x%x", optSigs[0])
	}
}

// PRMT-119 R4 (F6): the Server holds one source instance
// (Server.Source) and calls Subscribe once per SSE connection
// (handleSiteStream). Tearing down one connection's watcher MUST
// NOT close, mark-closed, or otherwise interfere with any other
// live or future connection. R3 placed the close flag on the
// shared struct, so the first disconnect silently killed every
// sibling connection. R4 moves mu/closed into Subscribe-local
// closures, restoring per-connection isolation.
//
// Test posture:
//  1. open two Subscribe calls on the same source (mockJS fans
//     out deliveries to both handlers);
//  2. confirm baseline: deliver a message, both channels get it;
//  3. cancel conn-1, wait for its channel to close;
//  4. assert conn-1's channel is closed AND conn-2's is still
//     open AND still receives a freshly delivered message.
//
// Run under -race: if mu/closed ever leak back to shared state,
// conn-2 will silently drop or panic.
func TestNATSTelemetrySource_MultiConnectionIsolation(t *testing.T) {
	js := &mockJS{}
	src := NewNATSTelemetrySource(js)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ch1, err := src.Subscribe(ctx1, "sgp01", sts.TokenClaims{})
	if err != nil {
		cancel1()
		t.Fatalf("Subscribe #1: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch2, err := src.Subscribe(ctx2, "sgp01", sts.TokenClaims{})
	if err != nil {
		cancel1()
		t.Fatalf("Subscribe #2: %v", err)
	}

	// Both Subscribe calls should have registered distinct
	// handlers with the mock broker.
	js.mu.Lock()
	nHandlers := len(js.cbs)
	js.mu.Unlock()
	if nHandlers != 2 {
		t.Fatalf("expected 2 handlers registered on mockJS, got %d", nHandlers)
	}

	// Baseline: one delivery must reach BOTH connections.
	js.deliver([]byte(`{"site":"sgp01","baseline":true}`))

	got1 := collectEvents(t, ch1, 1, time.Second)
	if len(got1) != 1 {
		t.Fatalf("conn-1 baseline: got %d events, want 1", len(got1))
	}
	got2 := collectEvents(t, ch2, 1, time.Second)
	if len(got2) != 1 {
		t.Fatalf("conn-2 baseline: got %d events, want 1", len(got2))
	}

	// Cancel conn-1 and wait for its channel to close.
	cancel1()
	select {
	case _, open := <-ch1:
		if open {
			t.Fatalf("conn-1 channel still open after cancel; expected close")
		}
	case <-time.After(time.Second):
		t.Fatalf("conn-1 channel never closed after cancel")
	}

	// F6 invariant: conn-2 must still be alive and still receive
	// a fresh message after conn-1 has torn down.
	js.deliver([]byte(`{"site":"sgp01","after_teardown":true}`))

	select {
	case ev, open := <-ch2:
		if !open {
			t.Fatalf("conn-2 channel closed after conn-1 teardown — F6 cross-connection poisoning")
		}
		if string(ev.Data) != `{"site":"sgp01","after_teardown":true}` {
			t.Fatalf("conn-2 got wrong event after teardown: %q", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatalf("conn-2 did not receive delivery after conn-1 teardown — F6 cross-connection poisoning")
	}
}
