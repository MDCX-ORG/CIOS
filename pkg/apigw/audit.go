// Access-log middleware for the /api/* surface (PRMT-120).
//
// Sits OUTSIDE AuthMiddleware so 401/403 (unauthenticated /
// unauthorised) requests are still recorded — they are exactly the
// requests an auditor cares about most. Uses log/slog (stdlib) so
// there is no third-party dependency to add to go.mod.
//
// PRMT-120 §2 design constraint: this middleware is the OUTER
// wrapper, but AuthMiddleware injects claims into a child context
// (`r = r.WithContext(...)` visible only downstream). Therefore
// ClaimsFrom(r.Context()) from here returns nothing — we do NOT
// log subject/realm in this PRMT. Folding identity into the audit
// record is left to a follow-up PRMT (per spec-009 §7.1, owned by
// PRMT-104 territory).
//
// Sensitive-field discipline (PRMT-120 §5 MUST NOT):
//   - never log Authorization, Cookie, raw token, request body,
//     query string, subject, realm
//   - path is r.URL.Path (no RawQuery) so secrets in query strings
//     cannot leak via this log
package apigw

import (
	"log/slog"
	"net/http"
	"time"
)

// statusCapturingWriter wraps http.ResponseWriter so the access
// middleware can record the final status code that downstream
// handlers (and AuthMiddleware's 401/403/WriteProblem) emit. The
// default status is 200 per net/http semantics (if a handler
// writes a body without an explicit WriteHeader the response is
// implicitly 200), so we seed the field with 200 rather than 0.
type statusCapturingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader captures the status code the first time it is
// called. Subsequent calls fall through to the embedded writer so
// the wrapped handler's contract (one explicit WriteHeader) is
// preserved.
func (w *statusCapturingWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// accessLogMiddleware emits one structured slog record per
// request. The record contains only non-sensitive identifiers:
// method, path (no query), status, request_id (from the inbound
// X-Request-Id header if the caller set one — empty string
// otherwise), and duration_ms. 4xx/5xx are emitted at Warn so a
// log-level filter can surface them without parsing keys.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		durationMS := time.Since(start).Milliseconds()
		// r.URL.Path only — deliberately ignore r.URL.RawQuery
		// so query-string secrets (tokens, pre-signed URLs, etc.)
		// never appear in audit records.
		path := r.URL.Path
		status := sw.status
		reqID := r.Header.Get("X-Request-Id")

		level := slog.LevelInfo
		if status >= 400 {
			level = slog.LevelWarn
		}
		slog.LogAttrs(r.Context(), level, "apigw.access",
			slog.String("method", r.Method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.String("request_id", reqID),
			slog.Int64("duration_ms", durationMS),
		)
	})
}
