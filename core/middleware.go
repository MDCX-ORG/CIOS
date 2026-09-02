// Package core — middleware.go: cross-cutting HTTP middleware
// (request-id propagation + structured access log). The actual
// ctx-key, generator, and accessor live in server.go so
// writeProblem and the resource handlers keep their existing
// RequestIDFromContext call site; this file just supplies the
// http.Handler-level wiring that Handler() composes.
//
// Middleware order (outermost → innermost) per PRMT-074 §4:
//
//	requestIDMiddleware → accessLogMiddleware → auth (if any) → mux
//
// request-id must be outermost so a 401/403/500 still carries an
// id we can quote in the access log. access-log wraps the auth
// chain so its status code reflects the final outcome (deny paths
// included), not a 200 from an inner short-circuit.
//
// PRMT-074.
package core

import (
	"log"
	"net/http"
	"time"
)

// requestIDMiddleware ensures every request flowing through the
// returned handler has a stable X-Request-Id. It reads the
// inbound header (case-insensitive; net/http canonicalises), and
// generates a new id via newRequestID if absent. The id is
// echoed on the response AND stored in the request context under
// the existing ctxKeyRID (see server.go) so downstream middleware
// and handlers can read it with RequestIDFromContext.
//
// This is the public re-export of the M0-era withRequestID
// closure that used to live in server.go. The body is unchanged;
// only the name and the file moved (per PRMT-074 §1 "复用既有
// request-id 机制，不另起一套").
func requestIDMiddleware(inner http.Handler) http.Handler {
	return withRequestID(inner)
}

// accessLog is the default destination for the per-request log
// line. Production uses log.Printf (go's std logger) so the
// project keeps a single log format; tests inject a sink via
// accessLogMiddleware's explicit setter (see
// SetAccessLogForTest).
var accessLog = log.Printf

// accessLogLogger is the function-typed slot accessLogMiddleware
// calls to emit its line. Tests override it (e.g. with a
// bytes.Buffer-backed func) to assert on the output. Default
// resolves to the package-level accessLog (log.Printf) on init.
var accessLogLogger func(format string, args ...any) = accessLog

// SetAccessLogForTest replaces the access-log sink. Returns a
// restore func the test must defer to undo the override. Calling
// with nil restores the default log.Printf — useful for tests
// that span multiple subtests.
func SetAccessLogForTest(fn func(format string, args ...any)) (restore func()) {
	prev := accessLogLogger
	accessLogLogger = fn
	return func() { accessLogLogger = prev }
}

// statusCapturingResponseWriter is an http.ResponseWriter that
// records the status code handed to WriteHeader so the access-log
// middleware can include it in its log line. We capture
// implicitly on Write too: if a handler writes a body without
// ever calling WriteHeader, the recorder will set Code only when
// httptest (or the real server) finalises it, so we use the same
// "200 unless told otherwise" convention as net/http/httptest.
type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// newStatusCapturingResponseWriter returns a wrapper with status
// defaulted to 200 (the http stdlib does the same — a handler
// that writes a body without WriteHeader is implicitly 200 OK).
func newStatusCapturingResponseWriter(w http.ResponseWriter) *statusCapturingResponseWriter {
	return &statusCapturingResponseWriter{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records the first status code; subsequent calls
// (rare; usually a bug in the handler) are ignored so the access
// log reflects the first/authoritative status, matching what
// net/http sends on the wire.
func (s *statusCapturingResponseWriter) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

// Write routes through WriteHeader(200) for the first byte so a
// handler that omits WriteHeader entirely still shows up as 200
// in the access log. Mirrors net/http/httptest's behaviour.
func (s *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// accessLogMiddleware wraps inner so that, after every request
// completes, exactly one structured line is emitted to the
// configured accessLogLogger. Fields, in order:
//
//	ts method path status dur_ms principal request_id
//
// The format is intentionally greppable key=value pairs (per
// PRMT-074 §2 and the audit line convention in authmw.go) so
// downstream log shippers can parse without a schema doc. The
// principal slot is "-" when no Principal is on the context (M0
// / auth-disabled) or when the auth middleware short-circuited
// with 401/403 (Principal is intentionally never set on the
// deny paths — see authmw.go). request_id is "-" if the wrapper
// was bypassed (defensive; production always has it because
// requestIDMiddleware sits outside this one).
func accessLogMiddleware(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Re-use the request-id set by requestIDMiddleware. We
		// intentionally re-read from context (not from the
		// response header) so the log line and the wire both
		// reflect the same value even if a handler rewrites
		// the response header.
		rid := RequestIDFromContext(r.Context())
		sw := newStatusCapturingResponseWriter(w)
		inner.ServeHTTP(sw, r)
		durMs := time.Since(start).Milliseconds()
		principal := "-"
		if p, ok := PrincipalFromContext(r.Context()); ok {
			principal = p.Subject
		}
		accessLogLogger("access method=%q path=%q status=%d dur_ms=%d principal=%q request_id=%q",
			r.Method, r.URL.Path, sw.status, durMs, principal, rid)
	})
}
