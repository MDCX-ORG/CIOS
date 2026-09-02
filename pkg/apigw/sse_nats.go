// SSE telemetry source backed by NATS JetStream (PRMT-119,
// spec-009 §6.1 / D39). The Gateway is the ONLY process that
// connects to NATS for the SSE fan-out path (L81 red line — the
// Portal must never see NATS); this source replaces the polling
// defaultTelemetrySource when main.go wires a NATS connection
// into Server.Source.
//
// Wire contract (spec-009 §6.1, locked 2026-06-26):
//
//   - Subject (per site): cios.tlm.<site>.>  (NATSPub publishes to
//     cios.tlm.<site>.<top_asset> per pkg/natspub/types.go).
//   - Consumer: ephemeral filtered push consumer with
//     nats.DeliverNew() — no durable, no replay; the SSE writer
//     only cares about live ticks. ctx cancellation calls
//     sub.Unsubscribe() to tear it down.
//   - Payload: the raw TelemetryBatch JSON body as published by
//     the gateway is forwarded verbatim as Event.Data
//     (Yuri 2026-06-26 — §7.1 red line: do not reformat the
//     wire). Event.Type = "telemetry" so SSE clients can dispatch.
package apigw

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/sts"
)

// reSite mirrors pkg/cpath/cpath.go:49 reSite. PRMT-119 §3 whitelist
// forbids importing pkg/cpath (it lives outside the apigw package's
// own files), so the pattern is duplicated verbatim here. Keep in
// sync with cpath.reSite — any change to the canonical site shape
// must be propagated to this copy. See PRMT-119b follow-up for the
// cross-cutting HTTP-entry validation.
var reSite = regexp.MustCompile("^[a-z]{2,8}[0-9]{2}$")

// jsSubscriber is the SUBSCRIBE-side subset of nats.JetStreamContext
// the SSE source needs. Mirrors the JetStreamContext pattern in
// pkg/natspub/publisher.go (Publish subset) so the source can be
// unit-tested with a hand-rolled mock instead of a real nats-server
// (PRMT-119 §4 / spec-006 §5 hotpath discipline). Connection
// construction lives in main.go; this interface only admits what
// the source already calls.
type jsSubscriber interface {
	Subscribe(subj string, cb nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error)
}

// natsTelemetrySource subscribes to cios.tlm.<site>.> and forwards
// each NATS message body verbatim to the SSE channel as one
// Event. It is the NATS counterpart of defaultTelemetrySource; the
// polling default is kept as a fallback when no NATS connection is
// wired (PRMT-111+ will own the main.go assembly).
//
// The struct holds ONLY the injected JetStream subscriber. All
// per-connection state (channel, close mutex/flag, subscription
// handle) lives inside Subscribe as closure-local variables, so
// each call to Subscribe is fully isolated from every other — the
// Server holds one source instance (Server.Source) and calls
// Subscribe once per SSE connection (handleSiteStream).
type natsTelemetrySource struct {
	js jsSubscriber
}

// NewNATSTelemetrySource returns a TelemetrySource backed by the
// given JetStream subscriber. js must be non-nil; wiring it nil is
// a programmer error surfaced at first Subscribe, not at
// construction time, so main.go can swap sources freely without
// having to rewire earlier init code.
func NewNATSTelemetrySource(js jsSubscriber) TelemetrySource {
	return &natsTelemetrySource{js: js}
}

// Subscribe opens an ephemeral filtered push consumer on
// cios.tlm.<site>.> and returns a channel that emits one Event per
// matching message. Returns an error if the subscription cannot
// be established (the SSE handler maps that to 502 RFC7807).
//
// Lifecycle:
//   - A single goroutine reads from the channel context and
//     fans NATS messages into it. ctx cancellation selects
//     sub.Unsubscribe() then closes the channel so the SSE
//     writer's <-events branch unblocks.
//   - The channel is buffered (size 1) so a slow SSE consumer
//     doesn't block the NATS dispatcher goroutine; back-pressure
//     comes from the client disconnecting (which cancels ctx),
//     not from NATS.
//   - On Subscribe error, no goroutine is started and no channel
//     is returned (handler sees err != nil).
func (s *natsTelemetrySource) Subscribe(ctx context.Context, site string, claims sts.TokenClaims) (<-chan Event, error) {
	if s == nil || s.js == nil {
		return nil, fmt.Errorf("apigw: nats telemetry source has no JetStream context")
	}
	if site == "" {
		return nil, fmt.Errorf("apigw: nats telemetry source: empty site")
	}
	// PRMT-119 §5-bis S1: validate site shape before building the
	// NATS subject so injection chars (`, >, .., slashes) cannot
	// reach the broker as wildcards or path separators. Pattern
	// mirrors pkg/cpath/cpath.go:49 reSite.
	if !reSite.MatchString(site) {
		return nil, fmt.Errorf("apigw: nats telemetry source: invalid site %q (must match ^[a-z]{2,8}[0-9]{2}$)", site)
	}

	subj := "cios.tlm." + site + ".>"
	// evtCh must exist before the Subscribe call so the message
	// handler closure can refer to it. Buffered (size 1) so a
	// slow SSE consumer doesn't block the NATS dispatcher; the
	// non-blocking send below means back-pressure is via client
	// disconnect (ctx cancel), not channel full.
	evtCh := make(chan Event, 1)

	// PRMT-119 R4 (F6 fix): mu + closed are per-connection locals
	// captured by the callback and the watcher closures below.
	// They MUST NOT live on the shared natsTelemetrySource struct:
	// Server.Source is a single instance reused across every SSE
	// connection, so a struct-level `closed=true` set by the first
	// disconnect would poison every other live and future
	// connection (F6 — cross-connection poisoning). The mutex
	// makes the close and the send mutually exclusive so a send
	// can never observe a closed channel (F5, the original bug
	// this pattern replaced a faulty WaitGroup for).
	var (
		mu     sync.Mutex
		closed bool
	)

	sub, err := s.js.Subscribe(subj, func(msg *nats.Msg) {
		mu.Lock()
		if closed {
			mu.Unlock()
			return
		}
		// Verbatim forward: msg.Data is the TelemetryBatch JSON
		// the gateway published (spec-009 §6.1). We do NOT parse
		// or reshape — the SSE client (and any downstream parser)
		// is the single authority on the wire shape (§7.1 red
		// line).
		ev := Event{
			Type: "telemetry",
			Data: msg.Data,
		}
		// §4 empty-ID branch: ephemeral DeliverNew consumers do
		// not expose a stable per-message sequence on *nats.Msg
		// reachable from the callback — the broker sequence lives
		// on JetStream.MsgSequence / Subscription.ConsumerInfo,
		// which §3/§6 forbids adding. So Event.ID is left empty
		// (zero value omits the id: line per sse.go Event doc;
		// same posture as the polling defaultTelemetrySource).
		// Non-blocking send into the buffered channel. If the
		// channel is full AND the SSE consumer is gone, we drop
		// rather than wedge the NATS dispatcher goroutine: the
		// client disconnect (ctx cancel) is the proper shutdown
		// signal, not a slow consumer.
		select {
		case <-ctx.Done():
			mu.Unlock()
			return
		case evtCh <- ev:
		default:
		}
		mu.Unlock()
	}, nats.DeliverNew())
	if err != nil {
		return nil, fmt.Errorf("apigw: nats telemetry subscribe %q: %w", subj, err)
	}

	// Watcher goroutine: when ctx is cancelled, unsubscribe and
	// close the channel. The SSE writer's <-events case will
	// then return open==false and end the session cleanly. The
	// message-handler goroutine spawned inside nats.Subscribe
	// exits because its select sees ctx.Done() on the next
	// message (or because sub.Unsubscribe stops the dispatch).
	//
	// R4: mu/closed are captured from the per-connection locals
	// above. Tearing down one connection's watcher only touches
	// this connection's mu/closed/evtCh — siblings on the same
	// source are untouched.
	go func() {
		<-ctx.Done()
		// Unsubscribe first so no new messages are dispatched
		// after the channel close; THEN take the mutex, mark
		// closed, and close evtCh while holding it. Any
		// in-flight callback that loses the race for the mutex
		// either completes its send before we mark closed
		// (channel still open) or sees closed=true and returns
		// without sending (channel now closed). Drain the
		// unsubscribe error — the source contract is "tear down
		// on ctx", not "report teardown errors to the caller".
		_ = sub.Unsubscribe()
		mu.Lock()
		closed = true
		close(evtCh)
		mu.Unlock()
	}()

	return evtCh, nil
}
