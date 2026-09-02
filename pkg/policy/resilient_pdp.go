// pkg/policy/resilient_pdp.go — retry + circuit-breaker decorator
// for any PDP (PRMT-112).
//
// Wraps an inner PDP (typically NewOPAPDP) and adds bounded retry
// with exponential backoff + jitter, plus a three-state circuit
// breaker (closed / open / half-open). Designed for transient
// transport failures and 5xx from the OPA sidecar.
//
// Fail-closed contract (PRMT-104 §5, L81 red line): the middleware
// in pkg/apigw already treats any non-nil err from Decision as
// deny. The decorator must NEVER convert a transport failure into
// allow. Deny verdicts returned by the inner PDP are NORMAL
// results and must not be retried, must not count as failures,
// and must not affect the breaker.
package policy

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"
)

// RetryConfig configures the resilient decorator. All fields are
// required to be valid: MaxAttempts >= 1, BaseBackoff > 0,
// MaxBackoff >= BaseBackoff, FailureThresh >= 1, OpenCooldown > 0.
// NewResilientPDP does not validate; callers (the wiring layer)
// pick sensible defaults. No env reads (PRMT-112 §6).
type RetryConfig struct {
	MaxAttempts   int           // total attempts incl. first; >= 1; recommended 3
	BaseBackoff   time.Duration // initial backoff; recommended 50ms
	MaxBackoff    time.Duration // backoff cap; recommended 500ms
	FailureThresh int           // consecutive failures to open; recommended 5
	OpenCooldown  time.Duration // open → half-open cooldown; recommended 2s
}

// ErrCircuitOpen is returned by Decision while the breaker is
// open. Callers treat any non-nil err as deny (fail-closed).
var ErrCircuitOpen = errors.New("policy: PDP circuit open")

// NewResilientPDP wraps inner with retry + breaker semantics. The
// returned PDP is safe for concurrent use.
//
// retryable judgement: any err != nil from inner is treated as a
// retryable transport failure. (allow, nil) — including false,
// i.e. a deny — is a successful answer and is returned verbatim
// with no retry and no breaker accounting.
func NewResilientPDP(inner PDP, cfg RetryConfig) PDP {
	return &resilientPDP{
		inner: inner,
		cfg:   cfg,
		// Per-instance RNG so callers don't share a global lock.
		// Non-cryptographic; only used for jitter.
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

type resilientPDP struct {
	inner PDP
	cfg   RetryConfig

	mu         sync.Mutex
	state      breakerState
	consecFail int
	openedAt   time.Time
	halfOpenIn bool // a probe is already in-flight
	rng        *rand.Rand
}

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// Decision consults the inner PDP with retry + breaker.
//
// Order of operations:
//  1. Check breaker. If open and cooldown elapsed → transition to
//     half-open and allow this caller to be the probe. If open and
//     cooldown not yet elapsed → (false, ErrCircuitOpen).
//  2. If half-open, allow at most one in-flight probe (halfOpenIn).
//     Others get (false, ErrCircuitOpen).
//  3. Loop up to cfg.MaxAttempts: call inner. If err == nil →
//     return (allow, nil). On err: backoff + jitter, then retry
//     unless ctx is done.
//  4. After attempts exhausted: return (false, lastErr).
//
// Breaker accounting is done ONCE per Decision (terminal outcome):
//   - success on any attempt → close (or stay closed), reset consecFail.
//   - all attempts failed → if we are a half-open probe, reopen
//     and restart the cooldown. Otherwise (closed) increment
//     consecFail; if it reaches FailureThresh, open.
func (r *resilientPDP) Decision(ctx context.Context, in Input) (bool, error) {
	cfg := r.cfg

	// Gate: are we allowed to issue a call at all?
	probe, err := r.beforeCall(time.Now())
	if err != nil {
		return false, err
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			// Don't count a cancelled-by-caller attempt as a failure
			// against the breaker; the inner PDP never ran (or its
			// error was wrapped context.Canceled).
			return false, ctx.Err()
		}

		allow, callErr := r.inner.Decision(ctx, in)
		if callErr == nil {
			r.afterCall(true, probe)
			return allow, nil
		}
		lastErr = callErr

		// If this was our final attempt, account the Decision-level
		// failure and bail.
		if attempt == cfg.MaxAttempts {
			r.afterCall(false, probe)
			return false, lastErr
		}

		// Backoff with jitter, respecting ctx cancellation.
		sleep := r.backoff(attempt)
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			// Cancelled mid-backoff. We didn't complete a full
			// Decision; do not poison the breaker on the caller's
			// behalf. Return ctx.Err so the middleware sees the
			// cancellation, not a synthetic transport error.
			return false, ctx.Err()
		case <-t.C:
		}
	}

	// Unreachable: the loop above either returns or continues.
	return false, lastErr
}

// beforeCall consults the breaker and reserves a slot for this
// caller. Returns probe=true if this caller should run a probe
// (half-open transition) — the breaker state machine expects
// exactly one such caller per cooldown cycle. Returns err != nil
// (ErrCircuitOpen) if the breaker is open and the caller is not
// the probe.
func (r *resilientPDP) beforeCall(now time.Time) (probe bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.state {
	case breakerClosed:
		return false, nil
	case breakerOpen:
		if now.Sub(r.openedAt) < r.cfg.OpenCooldown {
			return false, ErrCircuitOpen
		}
		// Cooldown elapsed → move to half-open and reserve the
		// probe slot for this caller.
		r.state = breakerHalfOpen
		r.halfOpenIn = true
		return true, nil
	case breakerHalfOpen:
		if r.halfOpenIn {
			// A probe is already in flight; everyone else is denied
			// without touching inner.
			return false, ErrCircuitOpen
		}
		// No in-flight probe — let this caller be one.
		r.halfOpenIn = true
		return false, nil
	}
	return false, nil
}

// afterCall updates breaker state given the outcome of the inner
// call. success=true means inner returned (_, nil). probe mirrors
// the value returned by beforeCall (only meaningful in half-open).
func (r *resilientPDP) afterCall(success bool, probe bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.state {
	case breakerClosed:
		if success {
			r.consecFail = 0
			return
		}
		r.consecFail++
		if r.consecFail >= r.cfg.FailureThresh {
			r.state = breakerOpen
			r.openedAt = time.Now()
			r.consecFail = 0
		}
	case breakerHalfOpen:
		// Release the probe slot in any case.
		r.halfOpenIn = false
		if success {
			r.state = breakerClosed
			r.consecFail = 0
			return
		}
		// Probe failed → reopen, restart cooldown.
		r.state = breakerOpen
		r.openedAt = time.Now()
		r.consecFail = 0
	case breakerOpen:
		// Should not happen — beforeCall would have rejected — but
		// if it does, leave state alone.
	}
}

// backoff returns the sleep duration for the gap between
// `attempt` and `attempt+1`. attempt is 1-indexed (1 = the
// backoff after the first failure). Formula per PRMT-112 §4:
//
//	min(MaxBackoff, BaseBackoff * 2^(attempt-1)) + jitter
//
// jitter is uniformly drawn from [0, BaseBackoff).
func (r *resilientPDP) backoff(attempt int) time.Duration {
	cfg := r.cfg
	base := cfg.BaseBackoff
	if base <= 0 {
		base = 1 * time.Millisecond
	}
	// 2^(attempt-1) can overflow quickly; clamp the shift.
	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}
	d := base << shift
	if d <= 0 || d > cfg.MaxBackoff {
		d = cfg.MaxBackoff
	}

	r.mu.Lock()
	j := time.Duration(r.rng.Int63n(int64(base)))
	r.mu.Unlock()
	return d + j
}
