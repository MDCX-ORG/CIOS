// SSE telemetry broker: PRMT-106 implements the Gateway-side SSE
// endpoint `/api/sites/{site}/stream` that fans out telemetry
// deltas to the Portal (spec-009 §7.1, §6, D39). The Portal never
// connects NATS / VictoriaMetrics directly — every incremental
// update crosses the experience-layer boundary through THIS
// handler. The data source is abstracted as a TelemetrySource
// interface so the underlying transport (currently a polling
// adapter to core /v1, eventually a NATS subscription) can be
// swapped without changing the wire shape or the auth contract.
package apigw

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/sts"
)

// Event is the unit of telemetry delta the source hands to the
// SSE writer. Field semantics (PRMT-106 §4):
//
//   - ID   : SSE `id:` field. Optional; zero value means omit the
//     `id:` line entirely. A non-zero value lets clients
//     resume from the Last-Event-ID header on reconnect.
//   - Type : SSE `event:` field. Optional; "" means omit the
//     `event:` line. Clients dispatch on this value (e.g.
//     "metric", "alarm", "twin").
//   - Data : raw payload that follows the `data:` line as written
//     (already JSON / already serialised by the source).
//     Newlines inside the payload are split into multiple
//     `data:` lines per the SSE spec so multi-line payloads
//     don't get clipped by the parser on the other end.
//
// The struct intentionally has no marshalling logic of its own —
// the source is the only authority on what each event looks like
// on the wire.
type Event struct {
	ID   string
	Type string
	Data []byte
}

// TelemetrySource produces telemetry deltas for a single site.
// Subscribe MUST return promptly and respect ctx cancellation —
// the SSE writer closes the channel by cancelling ctx when the
// HTTP client disconnects, and the source is responsible for
// tearing down its subscription (closing its own NATS connection,
// terminating a polling loop, etc.) at that point.
//
// claims is the verified STS claims (PRMT-105). The source uses
// claims to authorise the subscription (callers see ONLY the
// resource scope they were granted) — the Gateway itself does not
// interpret claims for filtering (L34/L50 stay authoritative via
// core /v1; the Gateway only carries identity, never judges
// visibility, per spec-009 §7.1 red line).
//
// A non-nil error short-circuits the SSE session: the writer
// translates it into RFC 7807 and closes the connection without
// opening a stream.
type TelemetrySource interface {
	Subscribe(ctx context.Context, site string, claims sts.TokenClaims) (<-chan Event, error)
}

// sseKeepAlive is the period of the `:keep-alive` SSE comment
// line emitted on otherwise-idle connections. The 15-second value
// mirrors the standard guidance for keeping intermediaries (load
// balancers, reverse proxies) from severing a long-lived stream
// that has produced no data in a while. It is the only knob the
// SSE writer exposes — longer intervals risk idle timeouts at
// proxy layers, shorter intervals waste bandwidth.
//
// Declared as a var (not const) so the keep-alive test can
// shorten the interval without spinning for 15 seconds; tests
// that mutate it MUST restore the original value on cleanup.
var sseKeepAlive = 15 * time.Second

// defaultTelemetrySource is the OPEN default TelemetrySource
// (PRMT-106 §4 / §8). It does NOT connect to NATS — that violates
// the Gateway→infra red line (spec-009 §7.1) and the prompt's
// MUST NOT list. Instead, it subscribes to core /v1 through the
// existing Upstream client and forwards whatever the upstream
// returns as SSE events. A future PRMT (NATS wiring per §6/D39)
// can replace this with a NATS-backed implementation by setting
// Server.Source before Routes() is called; the interface and the
// SSE writer stay the same.
//
// For this PRMT the adapter is intentionally minimal: it issues a
// single GET /v1/sites/{site}/telemetry to upstream and emits
// each returned delta verbatim. This is enough to prove the SSE
// wire shape, the auth hook, and the ctx-cancel teardown; the
// data plumbing lands in the NATS PRMT.
type defaultTelemetrySource struct {
	up *Upstream
}

// newDefaultTelemetrySource returns a TelemetrySource backed by
// the given Upstream. The Upstream must be non-nil; main.go is
// responsible for constructing one.
func newDefaultTelemetrySource(up *Upstream) *defaultTelemetrySource {
	return &defaultTelemetrySource{up: up}
}

// Subscribe implements TelemetrySource. The current default is a
// single-shot poll: it issues GET /v1/sites/{site}/telemetry and
// emits each delta on the returned channel until the channel
// closes (one response = one-shot). The handler treats a closed
// channel as "end of stream" and a future PRMT can layer a real
// long-lived subscription on top without changing the wire
// contract.
//
// ctx cancellation aborts the upstream request (via GetV1As) and
// closes the channel so the SSE writer's select unblocks.
func (s *defaultTelemetrySource) Subscribe(ctx context.Context, site string, claims sts.TokenClaims) (<-chan Event, error) {
	if s == nil || s.up == nil {
		return nil, fmt.Errorf("apigw: default telemetry source has no upstream")
	}
	// PRMT-122 S1 defense-in-depth: reject injection site shapes here too
	// (symmetry with natsTelemetrySource), so the source is safe even if a
	// future caller bypasses handleSiteStream's entry check.
	if !reSite.MatchString(site) {
		return nil, fmt.Errorf("apigw: default telemetry source: invalid site %q", site)
	}
	// /v1/sites/{site}/telemetry is the eventual read endpoint
	// (PRMT-106 §4 OPEN); if the path isn't wired upstream we
	// surface that as an error so the SSE writer can return
	// 502 RFC7807 rather than silently emitting an empty
	// stream.
	rawToken, _ := RawTokenFrom(ctx)
	status, body, _, err := s.up.GetV1As(ctx, claims, rawToken, "/v1/sites/"+site+"/telemetry")
	if err != nil {
		return nil, fmt.Errorf("apigw: telemetry upstream: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("apigw: telemetry upstream status %d: %s", status, string(body))
	}
	out := make(chan Event, 1)
	go func() {
		defer close(out)
		// For this PRMT the response is a single delta — emit
		// the raw body as one event. A future PRMT (NATS) will
		// parse /v1's streaming format here.
		select {
		case out <- Event{Type: "telemetry", Data: body}:
		case <-ctx.Done():
			return
		}
	}()
	return out, nil
}

// handleSiteStream serves GET /api/sites/{site}/stream. It is
// mounted by Routes() inside the AuthMiddleware-wrapped /api/
// mux, so by the time the handler runs the bearer has been
// verified and claims have been injected into r.Context() by
// WithClaims (PRMT-105).
//
// Wire shape (PRMT-106 §5):
//
//   - Headers: Content-Type: text/event-stream; charset=utf-8,
//     Cache-Control: no-cache, Connection: keep-alive, X-Accel-
//     Buffering: no (the last prevents nginx from accumulating
//     output, which would defeat the per-event flush).
//   - Per event: zero or one `id:` line, zero or one `event:`
//     line, one or more `data:` lines (multi-line payloads are
//     split per the SSE spec), terminated by a blank line. Each
//     event is followed by Flusher.Flush() so the bytes leave the
//     server immediately.
//   - Heartbeat: a `:keep-alive` SSE comment is emitted every
//     sseKeepAlive on otherwise-idle connections so middleboxes
//     don't sever the stream.
//   - Teardown: r.Context().Done() triggers a clean shutdown —
//     the source subscription is closed, the goroutine returns,
//     and the response writer is no longer touched.
//
// Errors:
//   - Method != GET → 405 (handled by the dispatch switch in
//     Routes; this handler still re-checks defensively).
//   - No source wired (Server.Source is nil) → 500 internal.
//   - No claims in context (theoretical; AuthMiddleware gates
//     this) → 401.
//   - Flusher not supported on the ResponseWriter → 500 internal.
//   - Subscribe returned an error → 502 upstream-unavailable.
func (s *Server) handleSiteStream(w http.ResponseWriter, r *http.Request) {
	// Defensive method check. Routes() dispatches by exact path
	// and the surrounding middleware also enforces GET via
	// actionForMethod; this is belt-and-braces so a future
	// caller can invoke handleSiteStream directly without
	// getting a 200 on POST.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteProblem(w, http.StatusMethodNotAllowed,
			"bad-request", "method not allowed",
			"/api/sites/{site}/stream only supports GET", r.URL.Path)
		return
	}

	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		// Defensive — AuthMiddleware already 401s unauthenticated
		// requests, so this branch is unreachable in production.
		// It pins the contract for direct handler invocation in
		// tests (PRMT-105 §5: identity missing → 401).
		WriteProblem(w, http.StatusUnauthorized,
			"unauthorized", "no verified identity",
			"the request did not carry a verified token", r.URL.Path)
		return
	}

	site, ok := parseSiteFromStreamPath(r.URL.Path)
	if !ok {
		WriteProblem(w, http.StatusNotFound,
			"path-not-found", "stream path malformed",
			"expected /api/sites/{site}/stream", r.URL.Path)
		return
	}

	// PRMT-122 S1: validate the site VALUE (parseSiteFromStreamPath only
	// checks path shape). Reuse the package-level reSite (sse_nats.go) —
	// no second regex. A malformed-but-shaped path with an injection
	// site (e.g. "*") is a 400, distinct from the 404 above.
	if !reSite.MatchString(site) {
		WriteProblem(w, http.StatusBadRequest,
			"bad-request", "invalid site",
			"site must match ^[a-z]{2,8}[0-9]{2}$", r.URL.Path)
		return
	}

	// http.Flusher is required for the per-event flush. If the
	// server's ResponseWriter doesn't support it (rare, but
	// possible in tests using bare http.ResponseRecorder), we
	// cannot honour the SSE contract — fail loudly.
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteProblem(w, http.StatusInternalServerError,
			"internal", "streaming unsupported",
			"the ResponseWriter does not support http.Flusher", r.URL.Path)
		return
	}

	// Resolve the source. PRMT-106 §4 / §8: the default
	// implementation does not connect to NATS; main.go can
	// override Server.Source before serving traffic to swap
	// in a NATS-backed source once the wiring lands.
	src := s.Source
	if src == nil {
		WriteProblem(w, http.StatusInternalServerError,
			"internal", "telemetry source not configured",
			"no TelemetrySource is wired into the server", r.URL.Path)
		return
	}

	// Subscribe before writing headers so Subscribe errors map
	// to RFC 7807. A successful Subscribe returns a channel that
	// is closed (by the source) when its work is done; the
	// request's context is cancelled on client disconnect.
	events, err := src.Subscribe(r.Context(), site, claims)
	if err != nil {
		WriteProblem(w, http.StatusBadGateway,
			"upstream-unavailable", "upstream unavailable",
			"telemetry source could not be opened", r.URL.Path)
		return
	}

	// Headers MUST be set before the first Flush / Write. SSE
	// requires text/event-stream; the no-cache + connection
	// headers are belt-and-braces for intermediaries that
	// peek at them.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Flush the response header line immediately so clients
	// observing the stream see the 200 + Content-Type before
	// the first event arrives — the usual SSE handshake.
	flusher.Flush()

	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected (or request was cancelled).
			// Returning here tears down the goroutine cleanly
			// — no further writes, no further reads from the
			// source channel. The source is expected to
			// observe ctx cancellation and close its channel;
			// we don't read from `events` again.
			return
		case ev, open := <-events:
			if !open {
				// Source exhausted the stream (e.g. NATS
				// subject closed, or the default
				// single-shot poll completed). End the
				// session cleanly.
				return
			}
			writeSSEEvent(w, ev)
			flusher.Flush()
		case <-keepAlive.C:
			// SSE comment: a line that starts with ':' is
			// ignored by EventSource clients but still
			// occupies a TCP segment, which keeps
			// middleboxes from severing an idle
			// connection. Two flushes (comment + a no-op
			// newline) ensure the bytes leave the server.
			if _, err := w.Write([]byte(":keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent emits a single Event in the wire format the SSE
// spec mandates. Each frame is:
//
//	id: <id>\n
//	event: <type>\n
//	data: <line 1>\n
//	data: <line 2>\n
//	\n
//
// Empty ID / Type are omitted. A payload containing '\n' is split
// into multiple `data:` lines so multi-line JSON (pretty-printed)
// survives the parser. The trailing blank line marks the end of
// the event per the spec.
//
// w is NOT flushed here — the caller controls when to flush
// (e.g. once per event in the loop, or after batching several).
func writeSSEEvent(w http.ResponseWriter, ev Event) {
	if ev.ID != "" {
		_, _ = w.Write([]byte("id: "))
		_, _ = w.Write([]byte(ev.ID))
		_, _ = w.Write([]byte("\n"))
	}
	if ev.Type != "" {
		_, _ = w.Write([]byte("event: "))
		_, _ = w.Write([]byte(ev.Type))
		_, _ = w.Write([]byte("\n"))
	}
	// Split on '\n' so the SSE parser receives one `data:` line
	// per logical line. A trailing newline is preserved as an
	// empty final `data:` line, matching the spec's
	// "data:<line>" shape.
	if len(ev.Data) == 0 {
		_, _ = w.Write([]byte("data: \n"))
	} else {
		for _, line := range splitDataLines(ev.Data) {
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(line)
			_, _ = w.Write([]byte("\n"))
		}
	}
	_, _ = w.Write([]byte("\n"))
}

// splitDataLines breaks payload on '\n' boundaries. We avoid
// strings.Split on the hot path to keep the SSE writer
// allocation-light (per spec-006 §5 hotpath discipline). A nil or
// empty payload yields no lines, so the caller still emits the
// minimal "data: \n" framing.
func splitDataLines(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	// Allocate a fresh buffer so we don't mutate the caller's
	// bytes (ev.Data is supplied by the source and may be
	// shared). Normalise CRLF to LF in the same pass: when we
	// see '\r' followed by '\n', we emit only the '\n' so the
	// line count matches the logical (post-normalisation) lines.
	buf := make([]byte, len(payload))
	j := 0
	for i := 0; i < len(payload); i++ {
		b := payload[i]
		if b == '\r' {
			// If the next byte is '\n' skip the '\n' too so
			// CRLF collapses to a single newline.
			if i+1 < len(payload) && payload[i+1] == '\n' {
				buf[j] = '\n'
				j++
				i++ // skip the '\n'
				continue
			}
			// Standalone '\r' is rare in SSE payloads; treat
			// it as a newline anyway so the line break is
			// visible to the SSE parser.
			buf[j] = '\n'
			j++
			continue
		}
		buf[j] = b
		j++
	}
	out := buf[:j]
	// Count lines so we can size the slice exactly. The split
	// is then O(n) with no reslicing churn.
	count := 1
	for _, b := range out {
		if b == '\n' {
			count++
		}
	}
	lines := make([][]byte, 0, count)
	start := 0
	for i, b := range out {
		if b == '\n' {
			lines = append(lines, out[start:i])
			start = i + 1
		}
	}
	lines = append(lines, out[start:])
	return lines
}

// parseSiteFromStreamPath returns the {site} segment of
// /api/sites/{site}/stream. The shape is fixed by PRMT-106 §2:
// the third segment is the site code, the fourth (and last) is
// the literal "stream". An empty site or wrong suffix yields
// ok=false so the caller can 404 the path.
//
// We avoid net/http's prefix matching for the same reason
// tokenAction does: registering /api/sites/{site}/stream on a
// stdlib ServeMux would shadow /api/sites (the existing exact
// match in Routes()) and break dispatch. Path parsing here keeps
// the routing tree intact.
func parseSiteFromStreamPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/sites/")
	if rest == p {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] != "stream" {
		return "", false
	}
	return parts[0], true
}
