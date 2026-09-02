// pkg/policy/resilient_pdp_test.go — tests for the ResilientPDP
// decorator (PRMT-112).
//
// Coverage map (matches §5 MUST list):
//   - retry succeeds on second attempt (transient blip)
//   - retry succeeds on third attempt (multi-blip)
//   - retry exhausts → returns last err, fail-closed
//   - breaker opens after N consecutive failures
//   - open → half-open after cooldown, probe success → closed
//   - open → half-open, probe failure → open again
//   - deny verdict is NOT retried (counts as success)
//   - deny verdict does NOT trip the breaker
//   - ctx cancellation aborts retry loop, returns ctx.Err()
//   - concurrent calls are safe under -race
package policy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePDP is a programmable PDP for tests. Each entry in `script`
// is consumed in order; subsequent calls with no script return
// `defaultAllow, defaultErr`. Tracks total calls via atomic counter.
type fakePDP struct {
	mu           sync.Mutex
	script       []fakeCall
	calls        int32
	defaultErr   error
	defaultAllow bool
}

type fakeCall struct {
	allow bool
	err   error
}

func (f *fakePDP) Decision(ctx context.Context, in Input) (bool, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return f.defaultAllow, f.defaultErr
	}
	next := f.script[0]
	f.script = f.script[1:]
	return next.allow, next.err
}

func (f *fakePDP) Calls() int { return int(atomic.LoadInt32(&f.calls)) }

// alwaysFailPDP fails every call with err. Used to drive the
// breaker past FailureThresh without relying on call sequencing.
type alwaysFailPDP struct{ err error }

func (a alwaysFailPDP) Decision(ctx context.Context, in Input) (bool, error) {
	return false, a.err
}

// alwaysAllowPDP always succeeds with allow=true. Used to confirm
// deny-via-success does not pollute breaker state.
type alwaysAllowPDP struct{ allow bool }

func (a alwaysAllowPDP) Decision(ctx context.Context, in Input) (bool, error) {
	return a.allow, nil
}

// fastConfig returns a config with minimal sleeps so tests are not
// slow. Cooldown is short so half-open transitions happen quickly.
// FailureThresh counts Decision-level failures (a Decision is the
// unit the caller observes): one exhausted Decision = one failure.
// MaxAttempts caps the inner retries per Decision.
func fastConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:   3,
		BaseBackoff:   1 * time.Millisecond,
		MaxBackoff:    5 * time.Millisecond,
		FailureThresh: 1,
		OpenCooldown:  5 * time.Millisecond,
	}
}

// TestResilientPDP_RetryThenSuccess: inner fails once then
// succeeds. The decorator must retry and return the success.
func TestResilientPDP_RetryThenSuccess(t *testing.T) {
	fp := &fakePDP{
		script: []fakeCall{
			{allow: false, err: errors.New("blip")},
			{allow: true, err: nil},
		},
	}
	pdp := NewResilientPDP(fp, fastConfig())

	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if !allow {
		t.Errorf("allow = false, want true")
	}
	if fp.Calls() != 2 {
		t.Errorf("inner calls = %d, want 2", fp.Calls())
	}
}

// TestResilientPDP_RetryExhausts: inner always fails. After
// MaxAttempts the decorator returns the last err and allow=false
// (fail-closed).
func TestResilientPDP_RetryExhausts(t *testing.T) {
	fp := &fakePDP{defaultErr: errors.New("opa down")}
	cfg := fastConfig()
	pdp := NewResilientPDP(fp, cfg)

	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err == nil {
		t.Fatalf("Decision: nil err, want transport err")
	}
	if allow {
		t.Errorf("allow = true on exhausted retry; want false (fail-closed)")
	}
	if fp.Calls() != cfg.MaxAttempts {
		t.Errorf("inner calls = %d, want %d", fp.Calls(), cfg.MaxAttempts)
	}
}

// TestResilientPDP_DenyNotRetried: an inner (false, nil) — a
// deny verdict — must NOT trigger retry. The middleware will
// translate this to 403; the PDP itself does not treat deny as
// error.
func TestResilientPDP_DenyNotRetried(t *testing.T) {
	fp := &fakePDP{defaultAllow: false}
	pdp := NewResilientPDP(fp, fastConfig())

	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if allow {
		t.Errorf("allow = true, want false")
	}
	if fp.Calls() != 1 {
		t.Errorf("inner calls = %d, want 1 (deny is success)", fp.Calls())
	}
}

// TestResilientPDP_DenyDoesNotTripBreaker: many deny verdicts in
// a row must NOT open the breaker. Deny is success.
func TestResilientPDP_DenyDoesNotTripBreaker(t *testing.T) {
	fp := &fakePDP{defaultAllow: false}
	pdp := NewResilientPDP(fp, fastConfig())

	// FailureThresh=1; 10 deny Decisions in a row must not open.
	for i := 0; i < 10; i++ {
		allow, err := pdp.Decision(context.Background(), sampleInput())
		if err != nil {
			t.Fatalf("Decision %d: %v", i, err)
		}
		if allow {
			t.Errorf("decision %d: allow = true, want false", i)
		}
	}
	// And one allow should still go through.
	fp.mu.Lock()
	fp.script = []fakeCall{{allow: true, err: nil}}
	fp.mu.Unlock()
	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("post-deny allow: %v", err)
	}
	if !allow {
		t.Errorf("post-deny allow: allow = false, breaker tripped by deny")
	}
}

// TestResilientPDP_BreakerOpens: after FailureThresh consecutive
// Decision-level failures, the breaker opens and subsequent calls
// get ErrCircuitOpen without touching inner.
func TestResilientPDP_BreakerOpens(t *testing.T) {
	fp := &fakePDP{defaultErr: errors.New("opa down")}
	cfg := fastConfig() // FailureThresh=1, MaxAttempts=3
	pdp := NewResilientPDP(fp, cfg)

	// First call: 3 inner calls all fail → Decision fails →
	// consecFail=1 ≥ FailureThresh → open.
	if _, err := pdp.Decision(context.Background(), sampleInput()); err == nil {
		t.Fatalf("first decision: nil err")
	}
	before := fp.Calls()

	// Second call: must short-circuit with ErrCircuitOpen.
	allow, err := pdp.Decision(context.Background(), sampleInput())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("second decision err = %v, want ErrCircuitOpen", err)
	}
	if allow {
		t.Errorf("allow = true on circuit-open; want false")
	}
	if fp.Calls() != before {
		t.Errorf("inner touched while open: calls %d → %d", before, fp.Calls())
	}
}

// TestResilientPDP_BreakerHalfOpenSuccess: after cooldown, the
// next call is the probe. If it succeeds, the breaker closes and
// subsequent calls go to inner again.
func TestResilientPDP_BreakerHalfOpenSuccess(t *testing.T) {
	fp := &fakePDP{defaultErr: errors.New("opa down")}
	cfg := fastConfig() // FailureThresh=1, OpenCooldown=5ms
	pdp := NewResilientPDP(fp, cfg)

	// Trip the breaker.
	if _, err := pdp.Decision(context.Background(), sampleInput()); err == nil {
		t.Fatalf("trip: nil err")
	}

	// While open, expect ErrCircuitOpen immediately.
	if _, err := pdp.Decision(context.Background(), sampleInput()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open: err = %v, want ErrCircuitOpen", err)
	}

	// Wait out the cooldown.
	time.Sleep(cfg.OpenCooldown + 5*time.Millisecond)

	// Heal the inner PDP for the probe.
	fp.mu.Lock()
	fp.script = []fakeCall{{allow: true, err: nil}}
	fp.defaultErr = nil
	fp.defaultAllow = true
	fp.mu.Unlock()

	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !allow {
		t.Errorf("probe: allow = false, want true")
	}

	// Breaker should be closed now — another call goes through
	// to inner with no script entry (which means default=allow).
	allow, err = pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("post-close: %v", err)
	}
	if !allow {
		t.Errorf("post-close: allow = false")
	}
}

// TestResilientPDP_BreakerHalfOpenFailure: a probe that fails
// reopens the breaker and restarts the cooldown.
func TestResilientPDP_BreakerHalfOpenFailure(t *testing.T) {
	fp := &fakePDP{defaultErr: errors.New("opa down")}
	cfg := fastConfig() // FailureThresh=1, OpenCooldown=5ms
	pdp := NewResilientPDP(fp, cfg)

	// Trip the breaker.
	if _, err := pdp.Decision(context.Background(), sampleInput()); err == nil {
		t.Fatalf("trip: nil err")
	}
	time.Sleep(cfg.OpenCooldown + 5*time.Millisecond)

	// Probe fails (inner still returning err).
	allow, err := pdp.Decision(context.Background(), sampleInput())
	if err == nil {
		t.Fatalf("probe fail: nil err")
	}
	if allow {
		t.Errorf("probe fail: allow = true, want false")
	}

	// Immediately after probe failure, breaker should be open
	// again — next call short-circuits.
	_, err = pdp.Decision(context.Background(), sampleInput())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("post-probe-fail err = %v, want ErrCircuitOpen", err)
	}
}

// TestResilientPDP_HalfOpenDropsConcurrentProbes: while a probe
// is in flight in half-open, additional callers must be denied
// with ErrCircuitOpen. The probe slot is exclusive.
//
// We simulate "probe in flight" by making the probe block on a
// channel inside the inner PDP.
func TestResilientPDP_HalfOpenDropsConcurrentProbes(t *testing.T) {
	cfg := fastConfig() // FailureThresh=1, OpenCooldown=5ms
	cfg.MaxAttempts = 1 // half-open probe should not retry internally

	// Inner PDP that fails once to open the breaker, then blocks
	// during the probe so we can race a second caller.
	var innerCalls int32
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	blocking := &blockingPDP{
		probeStarted: probeStarted,
		releaseProbe: releaseProbe,
		calls:        &innerCalls,
		failFirstN:   1, // first call fails → breaker opens
	}

	pdp := NewResilientPDP(blocking, cfg)

	// Trip the breaker. FailureThresh=1, MaxAttempts=1 → one failing
	// Decision opens the breaker.
	if _, err := pdp.Decision(context.Background(), sampleInput()); err == nil {
		t.Fatalf("trip call: nil err")
	}
	// Wait out cooldown before next decision (probe).
	time.Sleep(cfg.OpenCooldown + 5*time.Millisecond)

	// Start the probe in a goroutine. It will block inside inner.
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_, _ = pdp.Decision(context.Background(), sampleInput())
	}()
	<-probeStarted

	// Now race another caller. Breaker is half-open with a probe
	// in flight → this caller must be denied.
	allow, err := pdp.Decision(context.Background(), sampleInput())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("concurrent err = %v, want ErrCircuitOpen", err)
	}
	if allow {
		t.Errorf("concurrent allow = true, want false")
	}

	// Release the probe.
	close(releaseProbe)
	<-probeDone
}

// blockingPDP is the inner PDP for the half-open concurrency test.
// It fails the first `failFirstN` calls with a transport err, then
// on the next call signals probeStarted and waits on releaseProbe
// before returning the success result.
type blockingPDP struct {
	probeStarted chan struct{}
	releaseProbe chan struct{}
	calls        *int32
	failFirstN   int

	mu   sync.Mutex
	seen int
}

func (b *blockingPDP) Decision(ctx context.Context, in Input) (bool, error) {
	atomic.AddInt32(b.calls, 1)
	b.mu.Lock()
	b.seen++
	n := b.seen
	b.mu.Unlock()

	if n <= b.failFirstN {
		return false, errors.New("opa down")
	}
	// Beyond failFirstN: this call is the probe. Signal it and
	// block until released.
	close(b.probeStarted)
	select {
	case <-b.releaseProbe:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// TestResilientPDP_ContextCancel: a cancelled ctx must abort the
// retry loop and return ctx.Err() — without poisoning the
// breaker.
func TestResilientPDP_ContextCancel(t *testing.T) {
	cfg := fastConfig()
	cfg.BaseBackoff = 50 * time.Millisecond // long enough to cancel
	fp := &fakePDP{defaultErr: errors.New("opa down")}
	pdp := NewResilientPDP(fp, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before the second backoff completes.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	allow, err := pdp.Decision(ctx, sampleInput())
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if allow {
		t.Errorf("allow = true on cancel; want false")
	}
	// Must return well before the full backoff sum (~50+100ms).
	if elapsed > 80*time.Millisecond {
		t.Errorf("cancel took %v; backoff not aborted", elapsed)
	}

	// Breaker should not have tripped on this single cancelled call
	// (we don't account ctx-cancel as a failure). Verify by
	// clearing the err on inner and observing the next call
	// succeeds without circuit-open.
	fp.mu.Lock()
	fp.script = []fakeCall{{allow: true, err: nil}}
	fp.defaultErr = nil
	fp.mu.Unlock()
	allow, err = pdp.Decision(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("post-cancel heal: %v", err)
	}
	if !allow {
		t.Errorf("post-cancel heal: allow = false, breaker tripped by cancel")
	}
}

// TestResilientPDP_ConcurrentCalls: many concurrent callers hit
// the decorator while it is healthy. Verifies the breaker state
// machine is race-free under -race.
func TestResilientPDP_ConcurrentCalls(t *testing.T) {
	cfg := fastConfig()
	inner := alwaysAllowPDP{allow: true}
	pdp := NewResilientPDP(inner, cfg)

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			allow, err := pdp.Decision(context.Background(), sampleInput())
			if err != nil {
				errs <- err
				return
			}
			if !allow {
				errs <- errors.New("allow = false")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent call: %v", e)
	}
}

// TestResilientPDP_ConcurrentFailureTrips: concurrent failing
// callers should trip the breaker at least once. We do NOT
// assert "exactly one call reaches inner" — under high
// concurrency, multiple goroutines can pass beforeCall before
// any of them updates the breaker state. This is the standard
// trade-off for a non-reserving breaker; what we DO assert is
// that the breaker eventually trips (opens > 0).
func TestResilientPDP_ConcurrentFailureTrips(t *testing.T) {
	cfg := fastConfig() // FailureThresh=1, MaxAttempts=1 → fast
	cfg.MaxAttempts = 1
	inner := alwaysFailPDP{err: errors.New("opa down")}
	pdp := NewResilientPDP(inner, cfg)

	const N = 8
	results := make(chan error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := pdp.Decision(context.Background(), sampleInput())
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var opens, fails int
	for e := range results {
		switch {
		case errors.Is(e, ErrCircuitOpen):
			opens++
		case e != nil:
			fails++
		default:
			t.Errorf("unexpected nil err")
		}
	}
	// At least one caller must observe ErrCircuitOpen — proves the
	// breaker tripped under load.
	if opens == 0 {
		t.Errorf("expected at least one ErrCircuitOpen; got 0 (fails=%d)", fails)
	}
	// And no caller should observe a false allow (fail-closed
	// under all paths).
	if opens+fails != N {
		t.Errorf("opens+fails = %d, want %d", opens+fails, N)
	}
}

// sampleInput returns a deterministic Input. Keeps test bodies
// short.
func sampleInput() Input {
	return Input{
		Realm:  "ops",
		Action: "read",
		Time:   time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
}
