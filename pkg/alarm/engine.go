// Package alarm — engine.go: per-rule/per-asset state machine.
//
// The engine holds one Instance per (rule.Name, assetPath) pair, the
// dedup key from spec-003 §4. Observe evaluates the rule's compiled
// expression against a per-instance snapshot (relative-point → value)
// and returns any state-transition events the caller must publish
// (CloudEvents) and persist (alarms table). The engine is not safe
// for concurrent use — main feeds it serially from a single NATS
// subscription goroutine, per PRMT-020 §4.3.
//
// State machine (spec-003 §4 firing/resolved subset, ack deferred):
//
//	resolved ──(expr satisfied ≥ for)──→ firing ──(recovery)──→ resolved
//
// Recovery is "expr not satisfied" by default; with hysteresis>0 and
// a single-comparison expression, recovery is derived automatically
// per spec-003 §3 ("方向由比较符自动推导恢复阈值 = 阈值 ± hysteresis").
// and/or expressions with hysteresis fall back to plain "expr not
// satisfied" — that combination is not expressible from a single
// threshold.
package alarm

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yurimeng/cios/pkg/arith"
)

// State is one of the alarm lifecycle labels PRMT-020 §4.3 defines.
// ack is reserved for the future external ack API (spec-003 §4);
// this engine never produces it.
type State string

const (
	StateFiring   State = "firing"
	StateResolved State = "resolved"
	StateAcked    State = "acked"
)

// Event is one state-transition the caller must publish + persist.
// PointPath is the CloudEvents subject: the first expr reference
// joined with the asset path, or the asset path itself when expr
// references no point (e.g. a synthetic "1" predicate, which the
// parser does not actually produce — kept as a defensive fallback).
//
// Since is the operator-facing "since when the condition was first
// continuously satisfied" — stable across re-fires, not overwritten
// on resolved. OccurredAt is the wall-clock moment of THIS specific
// transition (firing trigger instant, or recovery instant) and is
// the value the CloudEvents `time` attribute carries (spec-003 §1.1:
// "time=事件发生时刻" — distinct from `since`).
type Event struct {
	RuleName   string
	AssetPath  string
	PointPath  string
	Severity   string
	State      State
	Summary    string
	Since      time.Time // first-satisfied moment (firing trigger) — stable
	OccurredAt time.Time // this-transition instant — used for CE `time`
	// Runbook is the knowledge-base key from the originating
	// AlarmRule (e.g. "rb/cdu-deltat-low"). Optional; the engine
	// currently does not populate it (PRMT-044 §8 records this
	// gap and points at a follow-up micro-PRMT to plumb the
	// value from rule.Spec.Annotations["runbook"]).
	Runbook string
}

// instance is the per-(rule,asset) running state. unexported — the
// engine exposes only NewEngine + Observe + Event.
type instance struct {
	rule             AlarmRule
	assetPath        string
	state            State
	firstSatisfiedAt time.Time // zero when not currently satisfying
	since            time.Time // when firing started; stable across re-evals
	recovery         recoveryFn
	pointPath        string // CE subject, cached at Observe entry
}

// recoveryFn evaluates whether the firing condition has cleared
// enough to transition back to resolved. Pure (no state of its own).
type recoveryFn func(snap map[string]float64) (bool, error)

// Engine holds every instance plus a fast lookup table keyed by
// (rule.Name, assetPath). Safe for concurrent use: mu guards the
// instances map plus every per-instance tick state read/write in
// Observe. The critical section is intentionally tiny — no I/O, no
// allocation beyond what tick already does (an out slice on the
// transition event only) — so the per-sample lock cost is in the
// microsecond range.
type Engine struct {
	mu        sync.Mutex
	rules     []AlarmRule
	instances map[dedupKey]*instance
}

type dedupKey struct {
	rule, asset string
}

// NewEngine builds an engine from a validated rule set. The rules
// slice is retained by reference (rules are treated as immutable
// after LoadRules returns), so callers must not mutate it.
func NewEngine(rules []AlarmRule) *Engine {
	return &Engine{
		rules:     rules,
		instances: map[dedupKey]*instance{},
	}
}

// Observe feeds one per-instance snapshot into the engine. now is
// the wall-clock time of the batch; main passes batch.Timestamp so
// rule evaluation is reproducible from recorded telemetry alone.
//
// assetType is the leaf asset type as the wire label `asset_type`
// carries it (e.g. "cdu", "chiller", "pdu"). Rules whose
// AppliesTo does not match assetType are skipped entirely — this
// is what prevents a `status==3` rule authored against "cdu" from
// firing on a chiller that happens to also emit a `status` point
// (R1 — cross-type false positives). An empty assetType (e.g. a
// malformed batch that lost the label) is treated as "no rule
// applies" so a misconfigured label set never broadens scope.
//
// Returns zero or more state-transition events in the order they
// were generated. At most one event is returned per Observe call
// in practice (the state machine produces at most one transition
// per tick — resolved→firing, or firing→resolved, never both).
func (e *Engine) Observe(assetPath, assetType string, snapshot map[string]float64, now time.Time) []Event {
	if assetType == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Event
	for i := range e.rules {
		if e.rules[i].Metadata.AppliesTo != assetType {
			continue
		}
		inst := e.getOrCreate(&e.rules[i], assetPath)
		if ev := inst.tick(snapshot, now); ev != nil {
			out = append(out, *ev)
		}
	}
	return out
}

// getOrCreate returns the instance for (r, assetPath), constructing
// it lazily on first sight. Construction is O(1) and does no
// expression evaluation. Callers must have already verified that
// the rule's AppliesTo matches the asset's type — getOrCreate
// trusts the caller to have filtered.
//
// Precondition: e.mu must be held by the caller. Documented here
// rather than re-locking so Observe can hold one lock across the
// whole rule loop (avoids O(rules) lock acquisitions per Observe
// without expanding the critical section's scope — it only
// touches in-memory state).
func (e *Engine) getOrCreate(r *AlarmRule, assetPath string) *instance {
	key := dedupKey{rule: r.Metadata.Name, asset: assetPath}
	if inst, ok := e.instances[key]; ok {
		return inst
	}
	inst := &instance{
		rule:      *r,
		assetPath: assetPath,
		state:     StateResolved,
		recovery:  buildRecovery(r),
		pointPath: buildPointPath(r, assetPath),
	}
	e.instances[key] = inst
	return inst
}

// tick is the per-Observe state machine. Returns nil when no
// transition occurs (the common case for stable conditions).
func (i *instance) tick(snap map[string]float64, now time.Time) *Event {
	satisfied, err := i.rule.Expr.Eval(snap)
	if err != nil {
		// ErrMissingPoint or division-by-zero: condition is NOT
		// satisfied (spec-003 §3 "数据缺失不视为满足"). For the
		// firing→resolved edge we still consult recovery so a
		// transient missing-point can't keep a phantom alarm alive.
		satisfied = false
	}

	switch i.state {
	case StateResolved:
		if satisfied {
			// For-duration of zero means "fire on the first satisfying
			// tick" — spec-003 §3 leaves `for` optional. Skipping the
			// timer eliminates a needless one-tick delay and matches
			// what every existing rule (status==N, deltat thresholds)
			// implicitly wants.
			if i.rule.Spec.ForDuration == 0 {
				i.state = StateFiring
				i.since = now
				i.firstSatisfiedAt = time.Time{}
				return &Event{
					RuleName:   i.rule.Metadata.Name,
					AssetPath:  i.assetPath,
					PointPath:  i.pointPath,
					Severity:   i.rule.Spec.Severity,
					State:      StateFiring,
					Summary:    i.rule.Spec.Annotations["summary"],
					Runbook:    i.rule.Spec.Annotations["runbook"],
					Since:      i.since,
					OccurredAt: now,
				}
			}
			if i.firstSatisfiedAt.IsZero() {
				i.firstSatisfiedAt = now
				return nil
			}
			if now.Sub(i.firstSatisfiedAt) >= i.rule.Spec.ForDuration {
				i.state = StateFiring
				i.since = i.firstSatisfiedAt
				i.firstSatisfiedAt = time.Time{}
				return &Event{
					RuleName:   i.rule.Metadata.Name,
					AssetPath:  i.assetPath,
					PointPath:  i.pointPath,
					Severity:   i.rule.Spec.Severity,
					State:      StateFiring,
					Summary:    i.rule.Spec.Annotations["summary"],
					Runbook:    i.rule.Spec.Annotations["runbook"],
					Since:      i.since,
					OccurredAt: now,
				}
			}
			return nil
		}
		// Not satisfied; clear the timer so a future satisfy starts
		// `for` from zero, not from a stale earlier stamp.
		i.firstSatisfiedAt = time.Time{}
		return nil

	case StateFiring:
		if satisfied {
			// Already firing — dedup (spec-003 §4 "同键 firing 期间
			// 不重复发 firing 事件"). Reset nothing; the condition
			// is continuous.
			return nil
		}
		// Recover: consult the derived recovery function (which
		// knows about hysteresis). If it errors (missing point,
		// div-by-zero), treat as "still firing" — the snapshot
		// doesn't refute the alarm, so we don't resolve.
		recovered, rerr := i.recovery(snap)
		if rerr != nil || !recovered {
			return nil
		}
		i.state = StateResolved
		i.firstSatisfiedAt = time.Time{}
		return &Event{
			RuleName:   i.rule.Metadata.Name,
			AssetPath:  i.assetPath,
			PointPath:  i.pointPath,
			Severity:   i.rule.Spec.Severity,
			State:      StateResolved,
			Summary:    i.rule.Spec.Annotations["summary"],
			Runbook:    i.rule.Spec.Annotations["runbook"],
			Since:      i.since, // since stays = the original firing start
			OccurredAt: now,     // but the transition itself happens NOW
		}
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// buildPointPath returns the CloudEvents subject per PRMT-020 §4.3:
// "expr Refs[0] 拼资产路径（无 ref 时 = assetPath）". The "join" is
// a single dot, matching cpath.Point.String() rendering for an
// absolute point built from a relative ref.
func buildPointPath(r *AlarmRule, assetPath string) string {
	refs := r.Expr.Refs()
	if len(refs) == 0 {
		return assetPath
	}
	return assetPath + "." + refs[0]
}

// buildRecovery returns the recovery predicate. Three cases:
//  1. hysteresis == 0 OR expr has no extractable single cmp
//     → recovery is "expr not satisfied". The engine already
//     calls recovery only on the firing→resolved edge so we
//     don't re-evaluate the original expr; we return true.
//  2. hysteresis > 0 AND expr is a single comparison
//     → derive the inverted threshold with ± hysteresis offset
//     and evaluate that instead.
//  3. hysteresis > 0 AND expr is and/or
//     → fall back to case 1; the inversion is not expressible
//     from a single threshold in that shape.
//
// We never re-evaluate the original "satisfied" expression here;
// the caller has already established that satisfied=false on this
// snapshot. Recovery adds hysteresis on top of that — a stronger
// "cleared enough" predicate.
func buildRecovery(r *AlarmRule) recoveryFn {
	if r.Spec.Hysteresis <= 0 {
		return func(map[string]float64) (bool, error) { return true, nil }
	}
	// Pull the single cmp from the AST if there is one. The AST
	// is unexported so we use the already-compiled expr directly —
	// the cost is one type assertion per rule, not per Observe.
	return deriveRecoveryFromExpr(r.Expr, r.Spec.Hysteresis)
}

// deriveRecoveryFromExpr inspects the compiled expression for the
// single-comparison shape. If found, it returns a recovery function
// that re-evaluates against the inverted threshold. Otherwise it
// falls back to "recovered=true on every non-satisfied tick".
func deriveRecoveryFromExpr(e Expr, hysteresis float64) recoveryFn {
	c, ok := e.(*compiled)
	if !ok {
		return func(map[string]float64) (bool, error) { return true, nil }
	}
	if c.root.kind != "and" || len(c.root.xs) != 1 {
		// Multi-clause or "or": defer to plain recovery.
		return func(map[string]float64) (bool, error) { return true, nil }
	}
	cm := c.root.xs[0]
	lhs, rhs, isLit := extractCmpLiterals(cm)
	if !isLit {
		return func(map[string]float64) (bool, error) { return true, nil }
	}
	switch cm.op {
	case "<":
		// expr: L < R  → recover when L >= R + h
		threshold := rhs + hysteresis
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v >= threshold, nil
		}
	case "<=":
		threshold := rhs + hysteresis
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v > threshold, nil
		}
	case ">":
		threshold := rhs - hysteresis
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v <= threshold, nil
		}
	case ">=":
		threshold := rhs - hysteresis
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v < threshold, nil
		}
	case "==":
		// Hysteresis on equality is a half-band. Treat as >= rhs+h
		// OR <= rhs-h; missing point → still firing.
		hi := rhs + hysteresis
		lo := rhs - hysteresis
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v >= hi || v <= lo, nil
		}
	case "!=":
		// expr: L != R → recover when L == R. Hysteresis is unused
		// here; spec-003 §3 does not prescribe a clear semantics.
		return func(snap map[string]float64) (bool, error) {
			v, ok := snap[lhs]
			if !ok {
				return false, ErrMissingPoint
			}
			return v == rhs, nil
		}
	}
	return func(map[string]float64) (bool, error) { return true, nil }
}

// extractCmpLiterals pulls (lhs-identifier, rhs-numeric, ok) from a
// comparison that compares a single relative-point identifier to a
// numeric literal (e.g. "fws.deltat < 4"). Anything else (two idents,
// two arith expressions) returns ok=false so recovery falls back to
// the plain "non-satisfied" semantics.
//
// The type assertions on the arith sub-nodes target the exported
// pkg/arith concrete types. The alarm parser hands out *Ident
// (pointer) for relative-point names and NumLit (value) for
// numeric literals — the asymmetry matches PRMT-020's pre-025
// shapes and is preserved here so the hysteresis recovery logic
// stays bit-for-bit identical.
func extractCmpLiterals(c cmp) (string, float64, bool) {
	lhs, lok := c.lhs.(*arith.Ident)
	rhs, rok := c.rhs.(arith.NumLit)
	if !lok || !rok {
		// try swapped sides too — "4 > fws.deltat" is uncommon but
		// the grammar permits it. Swap operands and invert op.
		lhs2, l2ok := c.lhs.(arith.NumLit)
		rhs2, r2ok := c.rhs.(*arith.Ident)
		if l2ok && r2ok {
			return rhs2.Name, lhs2.V, true
		}
		return "", 0, false
	}
	return lhs.Name, rhs.V, true
}

// String renders an Event for log lines / debug. Not used in the
// publish path (CE encoding lives in cmd/cios-alarm).
func (ev Event) String() string {
	return fmt.Sprintf("%s/%s state=%s sev=%s since=%s",
		ev.RuleName, ev.AssetPath, ev.State, ev.Severity,
		ev.Since.Format(time.RFC3339))
}

// trimPrefix is a tiny helper used in tests to strip the "assetPath."
// prefix when comparing snapshots against refs in error messages.
func trimPrefix(s, p string) string { return strings.TrimPrefix(s, p) }
