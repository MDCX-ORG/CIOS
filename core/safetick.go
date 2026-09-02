package core

import (
	"log"
	"runtime/debug"
)

// safeTick runs fn, recovering from any panic so a single bad
// tick (e.g. nil-deref in a downstream library, an out-of-range
// index) cannot kill the long-lived scanner goroutine. It is
// the panic-isolation seam called once per tick by the
// Run*Scanner loops and the report scheduler (PRMT-076 /
// eval H1).
//
// Contract:
//   - panic in fn is captured, logged with name + stack, and
//     swallowed — the goroutine continues to the next tick.
//   - fn is `func()` and does not return an error. safeTick
//     must NOT be used to mask normal error returns; scanner
//     loops already log+continue on error and that's a
//     different code path. safeTick is the panic-only escape
//     hatch.
//   - name is the short scanner name logged alongside the
//     stack (e.g. "sla", "pm", "report"). It must be stable
//     and human-readable so an operator can correlate the
//     stack with the right scanner when grepping logs.
//
// Failure to recover would propagate the panic up the
// goroutine, into `go srv.RunXxxScanner(ctx, ...)`, and from
// there crash the whole cios-core process. The whole point of
// this helper is to keep cios-core alive through a single
// buggy tick.
func safeTick(name string, fn func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// %v on a recovered value handles both error types and
		// arbitrary panic payloads (string, struct, etc.). The
		// stack is the operator's primary diagnostic — without
		// it, the panic value alone is often useless.
		log.Printf("core: scanner %s panic: %v\n%s", name, r, debug.Stack())
	}()
	fn()
}
