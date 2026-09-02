package alarm

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// ruleForTest is a small helper that builds a single AlarmRule from
// the minimum fields the engine cares about (expr + for + severity +
// hysteresis). AppliesTo is fixed to "cdu" so the rule validates
// against testdata.LoadDict() if a test ever wants to round-trip
// through LoadRules.
func ruleForTest(t *testing.T, name, expr, severity string, forDur time.Duration, hysteresis float64) AlarmRule {
	t.Helper()
	e, err := ParseExpr(expr)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", expr, err)
	}
	return AlarmRule{
		Kind: "AlarmRule",
		Metadata: ruleMetadata{
			Name:      name,
			AppliesTo: "cdu",
		},
		Spec: ruleSpec{
			Severity:    severity,
			Hysteresis:  hysteresis,
			ForDuration: forDur,
			Expr:        expr,
			Annotations: map[string]string{"summary": "test: " + name},
		},
		Expr: e,
	}
}

// expectEvents asserts that evs is exactly one event with the given
// state on a particular rule/asset pair. Returns the event for
// further assertions in the caller.
func expectEvents(t *testing.T, evs []Event, rule, asset string, want State) Event {
	t.Helper()
	if len(evs) != 1 {
		t.Fatalf("len(events)=%d want 1 (%+v)", len(evs), evs)
	}
	ev := evs[0]
	if ev.RuleName != rule || ev.AssetPath != asset || ev.State != want {
		t.Fatalf("event = %+v, want rule=%s asset=%s state=%s", ev, rule, asset, want)
	}
	return ev
}

// ---- for 防抖 --------------------------------------------------------------

func TestEngine_ForDebounce_FiresAfterDelay(t *testing.T) {
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 5*time.Minute, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Tick 1: satisfied, but for not elapsed → no event.
	if ev := eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0); len(ev) != 0 {
		t.Fatalf("tick1: want 0 events, got %d (%+v)", len(ev), ev)
	}
	// Tick 2: 4 minutes later, still satisfied, still inside for → no event.
	if ev := eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(4*time.Minute)); len(ev) != 0 {
		t.Fatalf("tick2: want 0 events, got %d", len(ev))
	}
	// Tick 3: 5+ minutes since t0, fires.
	ev := expectEvents(t, eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(5*time.Minute)), "deltab-low", "sgp01.pod002.cdu000", StateFiring)
	// since = first satisfied moment = t0, not the firing moment.
	if !ev.Since.Equal(t0) {
		t.Fatalf("since=%v, want %v (first satisfied)", ev.Since, t0)
	}
}

func TestEngine_ForDebounce_ResetsOnInterruption(t *testing.T) {
	// Satisfied for 3 min, then unsatisfied, then satisfied again
	// for 3 min — for-timer should restart at the second satisfied tick.
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 5*time.Minute, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 3 min satisfied.
	eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0)
	eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(3*time.Minute))
	// Interruption: condition clears.
	eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 5}, t0.Add(4*time.Minute))
	// Re-satisfy: t0+4 min, run for 3 more min.
	eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(5*time.Minute))
	eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(7*time.Minute))
	// Total wall-clock since re-satisfy = 7-5 = 2 min < 5 min → no fire.
	if ev := eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(8*time.Minute)); len(ev) != 0 {
		t.Fatalf("interrupted-then-resatisfied: expected no fire at 3min, got %+v", ev)
	}
	// 3 more min → total 5 min since re-satisfy → fires.
	ev := expectEvents(t, eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(10*time.Minute)), "deltab-low", "sgp01.pod002.cdu000", StateFiring)
	if !ev.Since.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("since=%v want %v (re-satisfied moment)", ev.Since, t0.Add(5*time.Minute))
	}
}

// ---- firing 去重 ---------------------------------------------------------

func TestEngine_FiringDedupe(t *testing.T) {
	r := ruleForTest(t, "status-fault", "status == 3", "critical", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// First satisfied tick: fires immediately (for=0).
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0), "status-fault", "cdu000", StateFiring)
	// Subsequent satisfied ticks: deduped.
	for i := 1; i < 5; i++ {
		ev := eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0.Add(time.Duration(i)*time.Minute))
		if len(ev) != 0 {
			t.Fatalf("dedup tick %d: want 0 events, got %+v", i, ev)
		}
	}
}

// ---- recovery & hysteresis -------------------------------------------------

func TestEngine_RecoveryNoHysteresis(t *testing.T) {
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Fires on first sample.
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0), "deltab-low", "cdu000", StateFiring)
	// Condition clears → resolves on the very next tick (no for, no hysteresis).
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 5}, t0.Add(time.Second)), "deltab-low", "cdu000", StateResolved)
}

func TestEngine_RecoveryWithHysteresis_LessThan(t *testing.T) {
	// expr: fws.deltat < 4, hysteresis: 0.5 → recover when >= 4.5
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0.5)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0), "deltab-low", "cdu000", StateFiring)
	// 4.4 — original expr is unsatisfied (4.4 >= 4), but recovery
	// wants >= 4.5 → still firing.
	if ev := eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 4.4}, t0.Add(time.Second)); len(ev) != 0 {
		t.Fatalf("4.4 with h=0.5: want still firing, got %+v", ev)
	}
	// 4.5 — recovery clears.
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 4.5}, t0.Add(2*time.Second)), "deltab-low", "cdu000", StateResolved)
}

func TestEngine_RecoveryWithHysteresis_GreaterThan(t *testing.T) {
	// expr: fws.deltat > 10, hysteresis: 1 → recover when <= 9
	r := ruleForTest(t, "deltab-high", "fws.deltat > 10", "major", 0, 1)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 12}, t0), "deltab-high", "cdu000", StateFiring)
	// 9.5 — expr unsatisfied (9.5 > 10 false), recovery wants <= 9 → still firing.
	if ev := eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 9.5}, t0.Add(time.Second)); len(ev) != 0 {
		t.Fatalf("9.5 with h=1: want still firing, got %+v", ev)
	}
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 9}, t0.Add(2*time.Second)), "deltab-high", "cdu000", StateResolved)
}

func TestEngine_RecoveryHysteresisMissingPointStaysFiring(t *testing.T) {
	// If the snapshot loses the point, the firing→resolved edge
	// must NOT fire — we can't confirm the alarm has cleared.
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0.5)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0), "deltab-low", "cdu000", StateFiring)
	if ev := eng.Observe("cdu000", "cdu", map[string]float64{}, t0.Add(time.Second)); len(ev) != 0 {
		t.Fatalf("missing-point recovery: want still firing, got %+v", ev)
	}
}

// ---- 缺数据不满足 + 清 firstSatisfiedAt ---------------------------------

func TestEngine_MissingPointClearsForTimer(t *testing.T) {
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 5*time.Minute, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 3 min satisfied.
	eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0)
	eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(3*time.Minute))
	// Snapshot drops the point — for-timer should reset.
	eng.Observe("cdu000", "cdu", map[string]float64{}, t0.Add(4*time.Minute))
	// Point comes back: only 1 min of continuous satisfaction left,
	// 1 min < 5 min → no fire.
	if ev := eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(5*time.Minute)); len(ev) != 0 {
		t.Fatalf("after missing-point: want no fire, got %+v", ev)
	}
}

// ---- dedup key: 多规则 × 多实例 ---------------------------------------------

func TestEngine_DedupKey_PerRuleAndAsset(t *testing.T) {
	r1 := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	r2 := ruleForTest(t, "status-fault", "status == 3", "critical", 0, 0)
	eng := NewEngine([]AlarmRule{r1, r2})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := map[string]float64{"fws.deltat": 3, "status": 3}
	// Both rules fire on cdu000 on first tick.
	evs := eng.Observe("cdu000", "cdu", snap, t0)
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d (%+v)", len(evs), evs)
	}
	// Different asset: dedup key (r1, cdu001) is fresh — fires again.
	evs = eng.Observe("cdu001", "cdu", snap, t0)
	if len(evs) != 2 {
		t.Fatalf("cdu001: want 2 fresh events, got %d", len(evs))
	}
	// Same asset again: dedup — both rules already firing → no events.
	if ev := eng.Observe("cdu000", "cdu", snap, t0.Add(time.Second)); len(ev) != 0 {
		t.Fatalf("dedup: want 0 events, got %+v", ev)
	}
}

// ---- re-fire after resolved ------------------------------------------------

func TestEngine_RefireAfterResolved(t *testing.T) {
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0), "deltab-low", "cdu000", StateFiring)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 5}, t0.Add(time.Second)), "deltab-low", "cdu000", StateResolved)
	// Re-fire: new firing should have since = THIS satisfied tick,
	// not the original firing moment.
	t1 := t0.Add(time.Hour)
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t1), "deltab-low", "cdu000", StateFiring)
	if !ev.Since.Equal(t1) {
		t.Fatalf("refire since=%v want %v", ev.Since, t1)
	}
}

// ---- CE subject (Event.PointPath) ----------------------------------------

func TestEngine_PointPath_BuiltFromFirstRef(t *testing.T) {
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := expectEvents(t, eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0), "deltab-low", "sgp01.pod002.cdu000", StateFiring)
	want := "sgp01.pod002.cdu000.fws.deltat"
	if ev.PointPath != want {
		t.Fatalf("PointPath=%q want %q", ev.PointPath, want)
	}
}

func TestEngine_PointPath_MultiRef(t *testing.T) {
	// Two refs → first wins (PRMT-020 §4.3: "Refs[0]").
	r := ruleForTest(t, "sup-return-temp", "fws.supply.temp - fws.return.temp > 5", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := expectEvents(t, eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{
		"fws.supply.temp": 30,
		"fws.return.temp": 24,
	}, t0), "sup-return-temp", "sgp01.pod002.cdu000", StateFiring)
	if ev.PointPath != "sgp01.pod002.cdu000.fws.supply.temp" {
		t.Fatalf("PointPath=%q", ev.PointPath)
	}
}

// ---- severity / summary propagation ---------------------------------------

func TestEngine_EventMetadata(t *testing.T) {
	e, err := ParseExpr("status == 3")
	if err != nil {
		t.Fatal(err)
	}
	r := AlarmRule{
		Kind: "AlarmRule",
		Metadata: ruleMetadata{
			Name:      "status-fault",
			AppliesTo: "cdu",
		},
		Spec: ruleSpec{
			Severity:    "critical",
			Hysteresis:  0,
			ForDuration: 0,
			Expr:        "status == 3",
			Annotations: map[string]string{"summary": "CDU in fault"},
		},
		Expr: e,
	}
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0), "status-fault", "cdu000", StateFiring)
	if ev.Severity != "critical" {
		t.Fatalf("severity=%q", ev.Severity)
	}
	if ev.Summary != "CDU in fault" {
		t.Fatalf("summary=%q", ev.Summary)
	}
	if !strings.HasPrefix(ev.PointPath, "cdu000.") {
		t.Fatalf("PointPath=%q", ev.PointPath)
	}
}

// ---- R1 regression: AppliesTo must filter at Observe boundary ----
//
// Before the fix, Observe fed every rule against every asset, and
// shared relative-point names (status, leak, fault) caused a rule
// authored against cdu to fire on a chiller or pdu that happened to
// emit the same point. The minimal fix is: only build/evaluate an
// Instance for rules whose AppliesTo matches the asset's
// `asset_type` label. This test pins the behaviour.

func TestEngine_AppliesToFilter_RejectsCrossType(t *testing.T) {
	// Rule is authored against cdu. The chiller emits a `status` point
	// too — and crucially, the snapshot value satisfies the rule
	// (status == 3). The old (buggy) Observe would fire on chiller
	// instances. The fixed Observe must not.
	r := ruleForTest(t, "status-fault", "status == 3", "critical", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Chiller emits status=3 — rule does NOT apply.
	if ev := eng.Observe("sgp01.pod002.chiller000", "chiller", map[string]float64{"status": 3}, t0); len(ev) != 0 {
		t.Fatalf("chiller with status=3 must not fire cdu rule: %+v", ev)
	}
	// PDU emits status=3 — same.
	if ev := eng.Observe("sgp01.pod002.pdu000", "pdu", map[string]float64{"status": 3}, t0); len(ev) != 0 {
		t.Fatalf("pdu with status=3 must not fire cdu rule: %+v", ev)
	}
	// And: chiller must not have built an Instance at all. A second
	// satisfying tick on the SAME chiller should also produce 0 events
	// (no instance → no state machine → no transition).
	if ev := eng.Observe("sgp01.pod002.chiller000", "chiller", map[string]float64{"status": 3}, t0.Add(time.Minute)); len(ev) != 0 {
		t.Fatalf("chiller 2nd tick: still must not fire: %+v", ev)
	}
	// Sanity: cdu with the same status value DOES fire.
	expectEvents(t, eng.Observe("sgp01.pod002.cdu000", "cdu", map[string]float64{"status": 3}, t0), "status-fault", "sgp01.pod002.cdu000", StateFiring)
}

func TestEngine_AppliesToFilter_EmptyAssetTypeIsNoOp(t *testing.T) {
	// A malformed batch that lost the asset_type label must NOT
	// silently broaden scope — empty assetType short-circuits to
	// "no rule applies". (The real gateway never emits this, but
	// the safety property is worth pinning.)
	r := ruleForTest(t, "status-fault", "status == 3", "critical", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if ev := eng.Observe("sgp01.pod002.cdu000", "", map[string]float64{"status": 3}, t0); len(ev) != 0 {
		t.Fatalf("empty assetType: must not fire, got %+v", ev)
	}
}

func TestEngine_OccurredAt_EqualsTransitionInstant(t *testing.T) {
	// R2 regression: OccurredAt must be the moment of THIS
	// transition (the `now` argument), not Since (which is the
	// first-satisfied moment for firing, and which stays equal to
	// the original firing-start for resolved). Before the fix,
	// publishCE used ev.Since, so firing.time == resolved.time.
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tFire := t0.Add(5 * time.Minute)
	tRes := t0.Add(10 * time.Minute)

	firing := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, tFire), "deltab-low", "cdu000", StateFiring)
	if !firing.OccurredAt.Equal(tFire) {
		t.Fatalf("firing OccurredAt=%v want %v", firing.OccurredAt, tFire)
	}
	if !firing.Since.Equal(tFire) {
		t.Fatalf("firing Since=%v want %v (first satisfied = fire instant for for=0)", firing.Since, tFire)
	}

	resolved := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 5}, tRes), "deltab-low", "cdu000", StateResolved)
	if !resolved.OccurredAt.Equal(tRes) {
		t.Fatalf("resolved OccurredAt=%v want %v (the recovery instant)", resolved.OccurredAt, tRes)
	}
	// Since on a resolved event = the ORIGINAL firing start (tFire),
	// not the recovery instant — this is the operator-facing
	// "alarm lasted from tFire to tRes" view.
	if !resolved.Since.Equal(tFire) {
		t.Fatalf("resolved Since=%v want %v (stays = original firing start)", resolved.Since, tFire)
	}
	// And the two CE `time` values (OccurredAt) must NOT be equal.
	if firing.OccurredAt.Equal(resolved.OccurredAt) {
		t.Fatalf("firing.time == resolved.time: %v (R2 regression)", firing.OccurredAt)
	}
}

func TestEngine_OccurredAt_FiringAfterFor(t *testing.T) {
	// When for>0, Since = first-satisfied (the timer start) and
	// OccurredAt = the moment for elapsed (the firing trigger).
	// These are distinct, and that's the whole point of separating
	// the two fields.
	r := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 5*time.Minute, 0)
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tFirst := t0
	tFire := t0.Add(5 * time.Minute)
	eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, tFirst) // start timer
	firing := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, tFire), "deltab-low", "cdu000", StateFiring)
	if !firing.Since.Equal(tFirst) {
		t.Fatalf("firing Since=%v want %v (first satisfied)", firing.Since, tFirst)
	}
	if !firing.OccurredAt.Equal(tFire) {
		t.Fatalf("firing OccurredAt=%v want %v (for elapsed)", firing.OccurredAt, tFire)
	}
}

// ---- Runbook propagation (PRMT-047 E2.8 来源链收口) -----------------------

// ruleForTestWithRunbook builds a rule whose Annotations carry both
// "summary" and "runbook" keys (the PRMT-044 schema). Keeps the test
// data honest — summary and runbook are independent annotations.
func ruleForTestWithRunbook(t *testing.T, name, expr, runbook string) AlarmRule {
	t.Helper()
	e, err := ParseExpr(expr)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", expr, err)
	}
	return AlarmRule{
		Kind: "AlarmRule",
		Metadata: ruleMetadata{
			Name:      name,
			AppliesTo: "cdu",
		},
		Spec: ruleSpec{
			Severity:    "critical",
			Hysteresis:  0,
			ForDuration: 0,
			Expr:        expr,
			Annotations: map[string]string{
				"summary": "test: " + name,
				"runbook": runbook,
			},
		},
		Expr: e,
	}
}

func TestEngine_Runbook_FiringPropagatesFromAnnotation(t *testing.T) {
	// Rule has runbook="cdu-leak" → firing Event.Runbook must equal
	// the annotation value (this is the whole point of PRMT-047).
	r := ruleForTestWithRunbook(t, "cdu-leak-low", "status == 3", "cdu-leak")
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0), "cdu-leak-low", "cdu000", StateFiring)
	if ev.Runbook != "cdu-leak" {
		t.Fatalf("firing Runbook=%q want %q (annotation propagated)", ev.Runbook, "cdu-leak")
	}
}

func TestEngine_Runbook_ResolvedAlsoPropagates(t *testing.T) {
	// The resolved transition also carries Runbook — same rule, same
	// annotation source. Keeps the three construction points symmetric.
	r := ruleForTestWithRunbook(t, "cdu-leak-low", "status == 3", "cdu-leak")
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0), "cdu-leak-low", "cdu000", StateFiring)
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 1}, t0.Add(time.Second)), "cdu-leak-low", "cdu000", StateResolved)
	if ev.Runbook != "cdu-leak" {
		t.Fatalf("resolved Runbook=%q want %q", ev.Runbook, "cdu-leak")
	}
}

func TestEngine_Runbook_AbsentAnnotationYieldsEmpty(t *testing.T) {
	// No runbook annotation → Event.Runbook must be "" (zero value
	// from map[string]string miss). Backward-compatible with rules
	// authored before PRMT-044 / PRMT-047.
	r := ruleForTest(t, "cdu-leak-low", "status == 3", "critical", 0, 0)
	// ruleForTest only sets "summary"; ensure no "runbook" key sneaks in.
	if _, ok := r.Spec.Annotations["runbook"]; ok {
		t.Fatalf("ruleForTest should not populate runbook key")
	}
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"status": 3}, t0), "cdu-leak-low", "cdu000", StateFiring)
	if ev.Runbook != "" {
		t.Fatalf("absent annotation: Runbook=%q want \"\"", ev.Runbook)
	}
}

func TestEngine_Runbook_AfterForDebouncePropagates(t *testing.T) {
	// The for-elapsed firing path (second Event construction point)
	// must also plumb the annotation. Exercises the case where the
	// first Satisfied tick is silent and the firing fires later.
	e, err := ParseExpr("fws.deltat < 4")
	if err != nil {
		t.Fatal(err)
	}
	r := AlarmRule{
		Kind: "AlarmRule",
		Metadata: ruleMetadata{
			Name:      "cdu-deltat-low",
			AppliesTo: "cdu",
		},
		Spec: ruleSpec{
			Severity:    "minor",
			Hysteresis:  0,
			ForDuration: 5 * time.Minute,
			Expr:        "fws.deltat < 4",
			Annotations: map[string]string{
				"summary": "deltat low",
				"runbook": "cdu-deltat-low",
			},
		},
		Expr: e,
	}
	eng := NewEngine([]AlarmRule{r})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// First satisfied tick — silent (for timer starts).
	if ev := eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0); len(ev) != 0 {
		t.Fatalf("tick1: want 0 events, got %+v", ev)
	}
	// After for elapses — fires, Runbook must be present.
	ev := expectEvents(t, eng.Observe("cdu000", "cdu", map[string]float64{"fws.deltat": 3}, t0.Add(5*time.Minute)), "cdu-deltat-low", "cdu000", StateFiring)
	if ev.Runbook != "cdu-deltat-low" {
		t.Fatalf("for-elapsed firing Runbook=%q want %q", ev.Runbook, "cdu-deltat-low")
	}
}

// ---- PRMT-089: concurrent Observe (-race) ----------------------------------
//
// Verifies that Engine.Observe is safe under concurrent use: many
// goroutines Observe the same and different assets in parallel, the
// race detector stays clean, and the engine remains in a state a
// follow-up single-threaded Observe can still reason about (e.g.
// the same (rule, asset) key never appears twice in the instances
// map). The pre-PRMT-089 engine would fail this test under -race
// because the instances map was read+written without synchronisation.

// TestEngine_Observe_ConcurrentSameAndDifferentAssets fans out N
// goroutines that each Observe a mix of overlapping (same rule +
// asset) and disjoint (different asset) keys. After the swarm joins
// we make one serial, final Observe on each (rule, asset) pair to
// confirm dedup is intact: a key that fired in the storm must
// produce a dedup-suppressed event on the same snapshot, and a key
// that never fired must still be resolvable to its first transition.
func TestEngine_Observe_ConcurrentSameAndDifferentAssets(t *testing.T) {
	r1 := ruleForTest(t, "status-fault", "status == 3", "critical", 0, 0)
	r2 := ruleForTest(t, "deltab-low", "fws.deltat < 4", "minor", 0, 0)
	eng := NewEngine([]AlarmRule{r1, r2})

	const goroutines = 32
	const iters = 200
	assets := []string{"cdu000", "cdu001", "cdu002", "cdu003"}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < iters; i++ {
				asset := assets[(g+i)%len(assets)]
				// Vary the snapshot so we cover both satisfying and
				// non-satisfying values; the state machine itself
				// is single-threaded, the property under test is
				// the map-level concurrency safety.
				snap := map[string]float64{"status": 3, "fws.deltat": 3}
				if (g+i)%5 == 0 {
					snap["status"] = 1
					snap["fws.deltat"] = 5
				}
				// Use a fresh time per call so re-fires are possible.
				now := base.Add(time.Duration(g*iters+i) * time.Second)
				_ = eng.Observe(asset, "cdu", snap, now)
			}
		}(g)
	}
	wg.Wait()

	// Post-condition: a single-threaded follow-up Observe on a key
	// that fired during the storm must dedup. We don't pin which
	// keys fired (depends on scheduler) — we only assert that
	// the engine is still in a consistent, deduped state. The
	// critical property is that the race detector (running via
	// `go test -race`) reports no races, and the engine still
	// functions correctly under single-threaded Observe.
	for _, asset := range assets {
		ev := eng.Observe(asset, "cdu", map[string]float64{"status": 3, "fws.deltat": 3}, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		// Either 0 events (already firing → dedup) or 2 events (one
		// for each rule) — never any other count, never a panic.
		if len(ev) != 0 && len(ev) != 2 {
			t.Fatalf("post-storm %s: want 0 or 2 events, got %d (%+v)", asset, len(ev), ev)
		}
	}
}
