package core

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSafeTickRecoverPanic exercises the panic-isolation
// contract: a fn that panics must not propagate the panic out
// of safeTick, and the panic value must be logged with the
// supplied name plus a runtime stack.
func TestSafeTickRecoverPanic(t *testing.T) {
	// Capture the standard logger to a buffer so we can assert
	// both the panic value and the scanner name appear in the
	// log line. Restored on exit so other tests aren't noisy.
	buf := &bytes.Buffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	// mustNotPanic: if the panic escapes safeTick, this defer
	// will record a t.Fatal. safeTick's contract is that no
	// panic propagates.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("safeTick propagated panic: %v", r)
		}
	}()

	safeTick("sla", func() {
		panic("boom-from-test")
	})

	got := buf.String()
	if !strings.Contains(got, "sla") {
		t.Fatalf("log output missing scanner name; got: %q", got)
	}
	if !strings.Contains(got, "boom-from-test") {
		t.Fatalf("log output missing panic value; got: %q", got)
	}
	// The stack should also be present (debug.Stack always
	// emits "goroutine" or "panic" in the first frame).
	if !strings.Contains(got, "goroutine") {
		t.Fatalf("log output missing stack trace; got: %q", got)
	}
}

// TestSafeTickNextCallRuns exercises the "next tick still
// fires" contract: after a panicking fn, a subsequent call into
// safeTick must still invoke its fn. This is the property that
// prevents a single bad tick from killing the long-lived
// scanner goroutine — the loop body must keep ticking.
func TestSafeTickNextCallRuns(t *testing.T) {
	// Quiet the logger so a successful test stays silent;
	// panic-recovery still logs, so redirect to a discard
	// buffer.
	orig := log.Writer()
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(orig) })

	var calls int
	safeTick("pm", func() { calls++ })
	if calls != 1 {
		t.Fatalf("first call: want calls=1, got %d", calls)
	}
	// Panic in the second call.
	safeTick("pm", func() {
		calls++
		panic("oops")
	})
	if calls != 2 {
		t.Fatalf("after panic: want calls=2 (the panic'd fn still ran its first line), got %d", calls)
	}
	// Third call must still run — this is the headline property.
	safeTick("pm", func() { calls++ })
	if calls != 3 {
		t.Fatalf("third call: want calls=3, got %d", calls)
	}
}

// TestSafeTickNonPanicNoLog verifies the negative case: when
// fn does not panic, safeTick is silent. This is the property
// the docstring promises ("recover() and the if-r==nil return")
// and the test guards against a noisy logger on the happy path.
func TestSafeTickNonPanicNoLog(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	safeTick("report", func() { /* no-op */ })

	if got := buf.String(); got != "" {
		t.Fatalf("safeTick on a non-panicking fn must not log; got: %q", got)
	}
}

// TestSafeTickNonStringPanic guards the %v formatting in the
// log line against a panic value that's not a string (Go allows
// any value as the panic payload). The contract is "log
// whatever was recovered" — we don't stringify, we %v.
func TestSafeTickNonStringPanic(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("safeTick propagated non-string panic: %v", r)
		}
	}()

	safeTick("inspection", func() {
		panic(struct{ Code int }{Code: 42})
	})

	got := buf.String()
	if !strings.Contains(got, "inspection") {
		t.Fatalf("log output missing scanner name; got: %q", got)
	}
	if !strings.Contains(got, "42") {
		t.Fatalf("log output missing panic payload value; got: %q", got)
	}
}

// TestScannerLoopSurvivesPanic is the scanner-level guard
// against the F1 regression caught in R1 of PRMT-076. It
// replicates the loop shape used by every Run*Scanner
// (startup tick + ticker select + safeTick wrap) in a single
// goroutine, then injects a panic on the first tick and
// asserts the goroutine survives to execute the second tick.
// If a future refactor drops the safeTick wrap from any of
// the four scanners (pm / inspection / spare / reconcile),
// the equivalent loop here will mirror that bug — but more
// importantly, this test acts as an executable contract for
// the wrap pattern itself.
func TestScannerLoopSurvivesPanic(t *testing.T) {
	// Quiet the logger; panic-recovery still logs but the
	// assertion is on the counter, not the log content.
	orig := log.Writer()
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(orig) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// drive a channel in place of time.Ticker so the test
	// doesn't sleep.
	ticks := make(chan time.Time, 4)

	var (
		calls atomic.Int32
		// firstPanic controls whether the next tick panics.
		// Set to true on entry, flipped to false after the
		// first tick fires, so the second tick proves the
		// goroutine survived.
		firstPanic atomic.Bool
	)
	firstPanic.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Startup tick — same shape as RunSLAScanner /
		// RunPMScanner / RunInspectionScanner /
		// RunSpareStockScanner / RunReconcileScanner.
		safeTick("pm", func() {
			calls.Add(1)
			if firstPanic.Swap(false) {
				panic("scanner tick injected panic")
			}
		})
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticks:
				safeTick("pm", func() {
					calls.Add(1)
					if firstPanic.Swap(false) {
						panic("scanner tick injected panic")
					}
				})
				_ = now
				return // exit after the second tick fires
			}
		}
	}()

	// First tick is the startup one (already executed). The
	// panic was swallowed by safeTick; the goroutine is still
	// blocked on the ticker select. Send the second tick.
	ticks <- time.Now()

	// Wait for the goroutine to finish. If safeTick were
	// missing, the panic would propagate out of `go func()`
	// and crash the test process — the wait below would
	// never return and the test would time out.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner loop did not survive panic (goroutine killed)")
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("want calls=2 (startup tick + one recovery tick), got %d", got)
	}
}
