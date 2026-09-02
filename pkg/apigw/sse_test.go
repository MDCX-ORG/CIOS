// Tests for the SSE telemetry broker (PRMT-106). The contract
// here pins four properties:
//
//  1. Headers: text/event-stream; charset=utf-8, no-cache, etc.
//  2. Wire shape: per-event id/event/data lines + trailing blank
//     line, with multi-line payloads split across data: lines.
//  3. Lifecycle: ctx cancellation closes the server goroutine
//     cleanly (no leak) and the SSE channel is closed by the
//     source.
//  4. Auth: no token → 401 via AuthMiddleware; handler also
//     fails closed (401) when claims are absent from ctx, as
//     a defensive branch (PRMT-105 §5).
//
// We use httptest.NewServer (real TCP) rather than the bare
// ResponseRecorder because SSE REQUIRES http.Flusher, and the
// recorder does not implement it. A newServerWithSource helper
// builds the same Server that production would, with a stub
// TelemetrySource.
package apigw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/sts"
)

// streamTestClaims is the verified identity every SSE test
// injects. The default source forwards claims.Subject as the
// Authorization header, mirroring handleSites' PRMT-105 behaviour.
var streamTestClaims = sts.TokenClaims{
	Subject: "streamer@example.com",
	Realm:   "ops",
}

// stubSource is a deterministic TelemetrySource used by tests
// that need to control what events the SSE writer sees. By
// default it emits the supplied events and then closes the
// channel. Tests that need to hold the subscription open
// (e.g. the ctx-cancel test) set block; closing block lets
// the source goroutine exit so the SSE handler's select
// unblocks with a closed channel.
type stubSource struct {
	mu         sync.Mutex
	events     []Event
	calls      int32
	lastSite   string
	lastClaims sts.TokenClaims
	// block, when non-nil, is selected alongside the event
	// sends. If block is nil, the source emits its events and
	// closes the channel as soon as the events list is
	// drained. If block is non-nil, the source keeps the
	// subscription open until either ctx.Done() or block
	// closes — useful for tests that want to assert ctx-cancel
	// teardown without leaking goroutines.
	block chan struct{}
}

func (s *stubSource) Subscribe(ctx context.Context, site string, claims sts.TokenClaims) (<-chan Event, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	s.lastSite = site
	s.lastClaims = claims
	s.mu.Unlock()
	out := make(chan Event)
	go func() {
		defer close(out)
		for _, ev := range s.events {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			case <-s.block:
				return
			}
		}
		// If block is set, hold the subscription open until
		// ctx cancels or block closes. Otherwise exit
		// immediately — the handler will see a closed
		// channel and return.
		if s.block != nil {
			select {
			case <-ctx.Done():
			case <-s.block:
			}
		}
	}()
	return out, nil
}

// newServerWithSource constructs an *httptest.Server wrapping a
// Server whose TelemetrySource is the supplied src. The wrapping
// uses the same Handler() entry point production uses, so the
// test exercises the full dispatch (including AuthMiddleware's
// pass-through when no STS is wired).
func newServerWithSource(t *testing.T, src TelemetrySource) *httptest.Server {
	t.Helper()
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// bearerCtx is a convenience for tests: it returns the
// Authorization header value the test's request must carry, with
// the verified claims sitting in the request context.
func bearerCtx() (string, func(*http.Request) *http.Request) {
	return "Bearer " + streamTestClaims.Subject, func(r *http.Request) *http.Request {
		return r.WithContext(WithClaims(r.Context(), streamTestClaims))
	}
}

// readSSEFrame reads one SSE frame from r. SSE frames end with a
// blank line; this helper returns when it has consumed a complete
// frame (id/event/data lines + terminator) or when timeout
// elapses. The returned slice contains the raw lines without the
// trailing blank line.
//
// We use a goroutine + select-with-timer to bound the wait —
// *bufio.Reader doesn't expose a read deadline, so per-Read
// timeouts are not possible. The test reader goroutine owns
// channel closure; the caller never closes the channels.
func readSSEFrame(t *testing.T, r *bufio.Reader, timeout time.Duration) []string {
	t.Helper()
	lines := make(chan string, 8)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var out []string
	for {
		select {
		case line := <-lines:
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if len(out) == 0 {
					// Empty frame: skip; the next
					// non-empty line still belongs
					// to a frame in progress.
					continue
				}
				return out
			}
			out = append(out, line)
		case err := <-errCh:
			if err == io.EOF {
				return out
			}
			return out
		case <-timer.C:
			t.Fatalf("timed out waiting for SSE frame after %s (got %v)", timeout, out)
			return nil
		}
	}
}

// TestSSE_HeadersAndFirstFrame: GET /api/sites/site01/stream
// with verified claims returns 200 + text/event-stream and
// emits the first event frame. The stub source emits one
// event; the test reads the frame and asserts the headers +
// data line.
//
// The test bypasses the network and calls handleSiteStream
// directly with a recorder so the verified claims injected
// into r.Context() are visible to the handler.
func TestSSE_HeadersAndFirstFrame(t *testing.T) {
	src := &stubSource{events: []Event{
		{ID: "1", Type: "telemetry", Data: []byte(`{"x":1}`)},
	}}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	w := httptest.NewRecorder()
	srv.handleSiteStream(w, r)

	if w.Code != http.StatusOK {
		body := w.Body.String()
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if conn := w.Header().Get("Connection"); conn != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", conn)
	}
	if xb := w.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}

	// Parse the recorded body. The recorder's body buffer
	// contains the full SSE wire shape written by the handler.
	// We read it directly rather than through bufio+goroutine
	// so the assertions are deterministic.
	body := w.Body.String()
	want := []string{"id: 1", "event: telemetry", `data: {"x":1}`}
	for _, line := range want {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("body missing %q (got %q)", line, body)
		}
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("body does not end with blank-line terminator (got %q)", body)
	}
	if got := atomic.LoadInt32(&src.calls); got != 1 {
		t.Errorf("Subscribe calls = %d, want 1", got)
	}
	src.mu.Lock()
	lastSite := src.lastSite
	lastClaims := src.lastClaims
	src.mu.Unlock()
	if lastSite != "site01" {
		t.Errorf("Subscribe site = %q, want site01", lastSite)
	}
	if lastClaims.Subject != streamTestClaims.Subject {
		t.Errorf("Subscribe claims.Subject = %q, want %q", lastClaims.Subject, streamTestClaims.Subject)
	}
}

// TestSSE_NoToken_Returns401: missing Authorization header must
// be rejected by AuthMiddleware with 401 + RFC 7807, NEVER
// reaching the SSE handler. PRMT-106 §5 pins this contract.
func TestSSE_NoToken_Returns401(t *testing.T) {
	src := &stubSource{}
	ts := newServerWithSource(t, src)

	resp, err := http.Get(ts.URL + "/api/sites/site01/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Errorf("body does not contain 'unauthorized': %s", body)
	}
	if got := atomic.LoadInt32(&src.calls); got != 0 {
		t.Errorf("Subscribe was called %d times; AuthMiddleware MUST short-circuit before handler", got)
	}
}

// TestSSE_MethodNotAllowed: POST /api/sites/{site}/stream must
// 405 + Allow: GET. PRMT-106 §5: the route is GET-only.
func TestSSE_MethodNotAllowed(t *testing.T) {
	src := &stubSource{}
	ts := newServerWithSource(t, src)
	tok, inject := bearerCtx()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/sites/site01/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", tok)
	req = inject(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
	if got := atomic.LoadInt32(&src.calls); got != 0 {
		t.Errorf("Subscribe was called %d times on POST; handler MUST short-circuit", got)
	}
}

// TestSSE_MalformedPath: /api/sites//stream (empty site) and
// /api/sites/site01/foo (wrong suffix) must 404. The path parser
// pins {site} != "" and suffix == "stream".
func TestSSE_MalformedPath(t *testing.T) {
	src := &stubSource{}
	ts := newServerWithSource(t, src)
	tok, inject := bearerCtx()

	cases := []string{
		"/api/sites//stream",
		"/api/sites/site01/foo",
		"/api/sites/stream",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+p, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Authorization", tok)
			req = inject(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("path %s: status = %d, want 404", p, resp.StatusCode)
			}
		})
	}
	if got := atomic.LoadInt32(&src.calls); got != 0 {
		t.Errorf("Subscribe was called %d times for malformed paths", got)
	}
}

// TestSSE_NoFlusher_Returns500: a ResponseWriter that does NOT
// implement http.Flusher must surface a 500 internal error. We
// construct a noFlusherWriter that embeds the recorder's body
// storage but does NOT expose Flush, so the http.Flusher type
// assertion in handleSiteStream fails. (httptest.NewRecorder
// itself DOES implement Flusher; this test pins the defensive
// branch by deliberately stripping it.)
func TestSSE_NoFlusher_Returns500(t *testing.T) {
	src := &stubSource{}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	w := newNoFlusherWriter()
	srv.handleSiteStream(w, r)

	if w.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%q)", w.code, w.body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "internal") {
		t.Errorf("type = %v, want tail internal", body["type"])
	}
}

// noFlusherWriter is a minimal http.ResponseWriter that does
// NOT implement http.Flusher. It mirrors the parts of
// httptest.ResponseRecorder that handleSiteStream needs
// (Header, Write, WriteHeader) but deliberately omits Flush
// so the type assertion in handleSiteStream fails — pinning
// the defensive 500 branch.
type noFlusherWriter struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func newNoFlusherWriter() *noFlusherWriter {
	return &noFlusherWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}
}

func (w *noFlusherWriter) Header() http.Header         { return w.header }
func (w *noFlusherWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *noFlusherWriter) WriteHeader(statusCode int)  { w.code = statusCode }

// TestSSE_NoClaims_Returns401: the handler's defensive branch
// fires when claims are absent from ctx. AuthMiddleware gates
// this in production, but the contract (PRMT-105 §5) requires
// the handler to fail closed if invoked directly without
// claims.
func TestSSE_NoClaims_Returns401(t *testing.T) {
	src := &stubSource{}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	w := httptest.NewRecorder()
	srv.handleSiteStream(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := atomic.LoadInt32(&src.calls); got != 0 {
		t.Errorf("Subscribe was called %d times without claims; handler MUST 401 first", got)
	}
}

// TestSSE_NoSource_Returns500: when Server.Source is nil
// (e.g. a future caller forgot to wire one), the handler must
// 500 rather than panicking.
func TestSSE_NoSource_Returns500(t *testing.T) {
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.Source = nil // explicit override of the default

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	// httptest.NewRecorder is fine for the 500 path; the
	// handler returns BEFORE attempting to write a stream
	// frame, so Flusher isn't required.
	w := httptest.NewRecorder()
	srv.handleSiteStream(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

// TestSSE_SubscribeError_Returns502: a source that fails to
// subscribe must surface a 502 upstream-unavailable to the
// client. This pins the wire contract for the day a real NATS
// source is wired and the broker fails to attach.
func TestSSE_SubscribeError_Returns502(t *testing.T) {
	errSrc := errorSource{err: fmt.Errorf("simulated broker attach failure")}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(errSrc)

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	w := httptest.NewRecorder()
	srv.handleSiteStream(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasSuffix(body["type"].(string), "upstream-unavailable") {
		t.Errorf("type = %v, want tail upstream-unavailable", body["type"])
	}
}

// errorSource is a TelemetrySource that always returns err.
type errorSource struct{ err error }

func (e errorSource) Subscribe(_ context.Context, _ string, _ sts.TokenClaims) (<-chan Event, error) {
	return nil, e.err
}

// TestSSE_MultiLineData: a payload that contains '\n' must be
// split into multiple `data:` lines so SSE clients see the
// payload intact. The stub emits a multi-line payload; the
// assertion checks the wire shape.
func TestSSE_MultiLineData(t *testing.T) {
	src := &stubSource{events: []Event{
		{Type: "telemetry", Data: []byte("line1\nline2\nline3")},
	}}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	w := httptest.NewRecorder()
	srv.handleSiteStream(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	want := []string{"event: telemetry\n", "data: line1\n", "data: line2\n", "data: line3\n"}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q (got %q)", w, body)
		}
	}
}

// TestSSE_CtxCancel_ClosesServerGoroutine: when the request
// context is cancelled, the server-side goroutine must observe
// ctx.Done() and return WITHOUT further writes. We verify this
// by holding the stub source's subscription open (stubSource
// .block stays open until the test closes it), starting the
// handler in a goroutine, cancelling the request context, and
// asserting the handler returns within a short window.
//
// The handler's exit leaves the stub's goroutine free to
// return as well (ctx.Done unblocks it), so the test doubles
// as a leak assertion when run under `go test -race`.
func TestSSE_CtxCancel_ClosesServerGoroutine(t *testing.T) {
	src := &stubSource{
		events: []Event{{Type: "telemetry", Data: []byte("hi")}},
		block:  make(chan struct{}),
	}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil).WithContext(ctx)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleSiteStream(w, r)
		close(done)
	}()

	// Give the handler a moment to subscribe and start its
	// select loop. Without this small sleep, the cancel()
	// could fire BEFORE Subscribe returns, racing the source
	// goroutine. 50ms is more than enough on any reasonable
	// CI runner.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Unblock the stub so its goroutine can exit too.
	close(src.block)

	select {
	case <-done:
		// good — server goroutine returned within the window
	case <-time.After(2 * time.Second):
		t.Fatalf("handleSiteStream did not return within 2s of ctx cancellation")
	}
}

// TestSSE_KeepAlive: a source that holds the subscription open
// without sending events must still see `:keep-alive` comments
// emitted on the keep-alive ticker. This pins the heartbeat
// discipline so middleboxes don't sever the connection.
func TestSSE_KeepAlive(t *testing.T) {
	// We need a fast keep-alive to keep the test runtime sane.
	// The default 15s is too long; we override the package
	// variable and restore it on test exit.
	prev := sseKeepAlive
	sseKeepAlive = 100 * time.Millisecond
	t.Cleanup(func() { sseKeepAlive = prev })

	src := &stubSource{
		block: make(chan struct{}),
	}
	cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
	srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
	srv.SetSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/sites/site01/stream", nil).WithContext(ctx)
	r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
	// safeWriter is mutex-guarded so the test goroutine can
	// poll the body for ":keep-alive" while the handler
	// goroutine writes to it concurrently.
	w := newSafeWriter()

	done := make(chan struct{})
	go func() {
		srv.handleSiteStream(w, r)
		close(done)
	}()

	// Poll the body for ":keep-alive". Each read is mutex-
	// guarded; the write side is mutex-guarded too, so no
	// race is possible.
	deadline := time.Now().Add(2 * time.Second)
	sawKeepAlive := false
	for time.Now().Before(deadline) && !sawKeepAlive {
		time.Sleep(50 * time.Millisecond)
		if strings.Contains(w.String(), ":keep-alive") {
			sawKeepAlive = true
		}
	}

	// Tear down: cancel ctx and unblock the stub.
	cancel()
	close(src.block)
	<-done

	if !sawKeepAlive {
		t.Errorf("did not see a `:keep-alive` heartbeat within 2s")
	}
}

// safeWriter is a minimal mutex-guarded http.ResponseWriter
// used by the keep-alive test, where the test goroutine polls
// the body concurrently with the handler's writes. It
// implements http.Flusher so the handler's type assertion
// succeeds. Reads and writes are serialised through the
// embedded sync.Mutex.
type safeWriter struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	code   int
}

func newSafeWriter() *safeWriter {
	return &safeWriter{header: make(http.Header)}
}

func (w *safeWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header
}

func (w *safeWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(b)
}

func (w *safeWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.code = statusCode
}

func (w *safeWriter) Flush() {
	// Flush is required by the SSE handler; it is a no-op
	// for the in-memory recorder.
}

// String returns a snapshot of the body under the mutex so
// the test can poll for the keep-alive comment without
// racing the handler.
func (w *safeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

// TestParseSiteFromStreamPath: the path parser must accept
// /api/sites/{site}/stream with a non-empty site and reject
// every other shape. PRMT-106 §2 pins the contract.
func TestParseSiteFromStreamPath(t *testing.T) {
	cases := []struct {
		path     string
		wantSite string
		wantOk   bool
	}{
		{"/api/sites/site01/stream", "site01", true},
		{"/api/sites/abc.def.ghi/stream", "abc.def.ghi", true},
		{"/api/sites//stream", "", false},
		{"/api/sites/stream", "", false},
		{"/api/sites/site01/foo", "", false},
		{"/api/sites", "", false},
		{"/healthz", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			site, ok := parseSiteFromStreamPath(tc.path)
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
			if site != tc.wantSite {
				t.Errorf("site = %q, want %q", site, tc.wantSite)
			}
		})
	}
}

// TestSplitDataLines: the multi-line payload splitter must
// preserve each '\n'-separated line as a separate slice entry
// and normalise CRLF to LF.
func TestSplitDataLines(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{"empty", nil, nil},
		{"single line", []byte("hello"), [][]byte{[]byte("hello")}},
		{"two lines", []byte("a\nb"), [][]byte{[]byte("a"), []byte("b")}},
		{"CRLF normalised", []byte("a\r\nb"), [][]byte{[]byte("a"), []byte("b")}},
		{"trailing newline", []byte("a\n"), [][]byte{[]byte("a"), []byte("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitDataLines(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d: %q", len(got), len(tc.want), got)
			}
			for i := range got {
				if string(got[i]) != string(tc.want[i]) {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDefaultTelemetrySource_ForwardsRawBearer: PRMT-114 §2 —
// the default telemetry source MUST forward the raw JWS bearer
// (from ctx) to core /v1/sites/{site}/telemetry, not the bare
// claims.Subject. The upstream is a real httptest server that
// asserts the Authorization header value.
func TestDefaultTelemetrySource_ForwardsRawBearer(t *testing.T) {
	const rawJWS = "raw-jws-default-telemetry"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+rawJWS {
			t.Errorf("Authorization = %q, want %q (raw JWS MUST reach upstream, PRMT-114 §2)", got, "Bearer "+rawJWS)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	src := newDefaultTelemetrySource(NewUpstream(upstream.URL, upstream.Client()))
	ctx := WithRawToken(t.Context(), rawJWS)
	ch, err := src.Subscribe(ctx, "site01", streamTestClaims)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Drain the channel to ensure the upstream request completed
	// (the source closes the channel on 2xx non-streaming body).
	for range ch {
	}
}

// TestDefaultTelemetrySource_NoRawTokenOmitsHeader: PRMT-114
// §2-bis — when no rawToken is in ctx, the Authorization header
// MUST be omitted entirely (not "Bearer " with empty value).
// The upstream asserts the header is absent.
func TestDefaultTelemetrySource_NoRawTokenOmitsHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty (no rawToken in ctx MUST omit the header, PRMT-114 §2-bis)", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	src := newDefaultTelemetrySource(NewUpstream(upstream.URL, upstream.Client()))
	ch, err := src.Subscribe(t.Context(), "site01", streamTestClaims)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for range ch {
	}
}

// TestSSE_InvalidSite_Returns400: PRMT-122 §2 / §5 — HTTP entry
// MUST reject malformed site values that parseSiteFromStreamPath
// lets through (it only validates path SHAPE, not site VALUE).
// Each subcase asserts the 400 + application/problem+json +
// type "bad-request" contract AND that src.Subscribe was never
// reached (so neither the default source nor NATS can be touched
// with an injection site).
func TestSSE_InvalidSite_Returns400(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "wildcard", path: "/api/sites/*/stream"},
		{name: "uppercase", path: "/api/sites/SITE01/stream"},
		{name: "alpha_only", path: "/api/sites/abc/stream"},
		{name: "gt_injection", path: "/api/sites/%3E/stream"},
		{name: "overlong", path: "/api/sites/abcdefghij01/stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &stubSource{}
			cfg := Config{ListenAddr: ":0", UpstreamURL: "http://unused"}
			srv := NewServer(cfg, NewUpstream(cfg.UpstreamURL, nil))
			srv.SetSource(src)

			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = r.WithContext(WithClaims(r.Context(), streamTestClaims))
			w := httptest.NewRecorder()
			srv.handleSiteStream(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			// The RFC 7807 body MUST carry type=bad-request so
			// downstream clients can distinguish a value 400
			// from the shape 404 above. WriteProblem encodes the
			// slug as the canonical URL form
			// https://cios.dev/errors/<slug>; match the tail.
			var problem map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatalf("unmarshal problem body: %v (body=%q)", err, w.Body.String())
			}
			if got, _ := problem["type"].(string); !strings.HasSuffix(got, "/bad-request") {
				t.Errorf("problem.type = %q, want suffix /bad-request (full body=%v)", got, problem)
			}
			if got := atomic.LoadInt32(&src.calls); got != 0 {
				t.Errorf("Subscribe calls = %d, want 0 (invalid site MUST NOT reach the source)", got)
			}
		})
	}
}

// TestDefaultTelemetrySource_InvalidSite_ReturnsError: PRMT-122
// §2-bis / §5 — the defense-in-depth guard inside
// defaultTelemetrySource.Subscribe MUST reject injection site
// shapes even when handleSiteStream's HTTP entry check is
// bypassed. Each subcase wires a real httptest upstream that
// asserts it is NEVER contacted (no /v1/sites/*/telemetry).
func TestDefaultTelemetrySource_InvalidSite_ReturnsError(t *testing.T) {
	cases := []string{"*", "SITE01", "abc", "", "abcdefghij01", "../"}
	for _, bad := range cases {
		t.Run("site="+bad, func(t *testing.T) {
			upstreamCalled := atomic.Int32{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled.Add(1)
				t.Errorf("upstream reached with path=%q (invalid site MUST NOT trigger upstream request, PRMT-122 §2-bis)", r.URL.Path)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)

			src := newDefaultTelemetrySource(NewUpstream(upstream.URL, upstream.Client()))
			ch, err := src.Subscribe(t.Context(), bad, streamTestClaims)
			if err == nil {
				// If a channel was returned, drain it to allow
				// any spurious upstream call to surface before
				// the assertion below.
				for range ch {
				}
				t.Fatalf("Subscribe(site=%q) err = nil, want error (PRMT-122 §2-bis)", bad)
			}
			if ch != nil {
				t.Errorf("Subscribe(site=%q) returned non-nil channel with err, want nil channel on error", bad)
			}
			if got := upstreamCalled.Load(); got != 0 {
				t.Errorf("upstream calls = %d, want 0 (defense-in-depth MUST short-circuit before URL build)", got)
			}
		})
	}
}
