package main

import (
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/alarm"
	"github.com/yurimeng/cios/pkg/freshness"
)

func TestEvaluateFreshness_FireAndResolve(t *testing.T) {
	w := freshness.New(10 * time.Minute)
	trk := newGapTracker()
	t0 := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	w.Touch("sgp01.pod000.cdu000", t0)

	// Not yet stale
	if ev := evaluateFreshness(w, trk, t0.Add(5*time.Minute)); len(ev) != 0 {
		t.Fatalf("early events=%+v", ev)
	}

	// Stale → firing
	ev := evaluateFreshness(w, trk, t0.Add(11*time.Minute))
	if len(ev) != 1 || ev[0].State != alarm.StateFiring || ev[0].RuleName != pipelineGapRule {
		t.Fatalf("fire=%+v", ev)
	}

	// Still stale → no re-fire
	if ev := evaluateFreshness(w, trk, t0.Add(12*time.Minute)); len(ev) != 0 {
		t.Fatalf("repeat fire=%+v", ev)
	}

	// Resume → resolved
	w.Touch("sgp01.pod000.cdu000", t0.Add(13*time.Minute))
	ev = evaluateFreshness(w, trk, t0.Add(13*time.Minute))
	if len(ev) != 1 || ev[0].State != alarm.StateResolved {
		t.Fatalf("resolve=%+v", ev)
	}
}

func TestHeartbeatPath(t *testing.T) {
	line := `cios_pipeline_heartbeat{site="sgp01",path="sgp01.pod000.cdu000",top_asset="sgp01.pod000",asset_type="cdu"} 1 1720000000000`
	path, ok := heartbeatPath(line)
	if !ok || path != "sgp01.pod000.cdu000" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
	if _, ok := heartbeatPath(`cios_temp{path="x"} 1 1`); ok {
		t.Fatal("want reject non-heartbeat")
	}
}
