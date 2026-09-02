// Package main — freshness.go: pipeline gap alarms (DATA-RESILIENCE G6 / R-d).
//
// Tracks last-seen assets from telemetry batches. When an asset goes
// silent longer than -freshness-stale, emit a major "pipeline-gap"
// alarm event (Upsert + CE). When traffic returns, resolve it.
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/alarm"
	"github.com/yurimeng/cios/pkg/freshness"
)

const pipelineGapRule = "pipeline-gap"

// gapTracker remembers which assets currently have a gap alarm firing
// so we only emit transitions (not every tick).
type gapTracker struct {
	mu     sync.Mutex
	firing map[string]time.Time // asset → since
}

func newGapTracker() *gapTracker {
	return &gapTracker{firing: make(map[string]time.Time)}
}

// runFreshnessLoop polls the watch and persists gap/resolve events.
func runFreshnessLoop(
	ctx context.Context,
	w *freshness.Watch,
	trk *gapTracker,
	nc *nats.Conn,
	site string,
	st *alarm.Store,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			events := evaluateFreshness(w, trk, now)
			if len(events) == 0 {
				continue
			}
			upsertCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			for _, ev := range events {
				if err := publishCE(nc, site, ev); err != nil {
					log.Printf("cios-alarm: gap CE %s: %v", ev.AssetPath, err)
				}
				if err := st.Upsert(upsertCtx, ev); err != nil {
					log.Printf("cios-alarm: gap upsert %s: %v", ev.AssetPath, err)
					alarmResil.UpsertFailures.Inc()
				} else {
					log.Printf("cios-alarm: pipeline gap %s state=%s age=%s",
						ev.AssetPath, ev.State, now.Sub(ev.Since).Round(time.Second))
				}
			}
			cancel()
		}
	}
}

// evaluateFreshness is pure enough to unit-test: returns firing/resolved events.
func evaluateFreshness(w *freshness.Watch, trk *gapTracker, now time.Time) []alarm.Event {
	if w == nil || trk == nil {
		return nil
	}
	gaps := w.Gaps(now)
	gapSet := make(map[string]freshness.Gap, len(gaps))
	for _, g := range gaps {
		gapSet[g.AssetPath] = g
	}
	fresh := w.Fresh(now)

	var out []alarm.Event
	trk.mu.Lock()
	defer trk.mu.Unlock()

	// New or continuing gaps → firing event (first time only for CE noise:
	// we still Upsert firing each tick for summary refresh — only emit
	// transition events: new gap or resolve).
	for path, g := range gapSet {
		if _, already := trk.firing[path]; already {
			continue
		}
		since := g.LastSeen.Add(w.StaleAfter())
		if since.After(now) {
			since = now
		}
		trk.firing[path] = since
		out = append(out, alarm.Event{
			RuleName:   pipelineGapRule,
			AssetPath:  path,
			PointPath:  path + ".pipeline.heartbeat",
			Severity:   "major",
			State:      alarm.StateFiring,
			Summary:    fmt.Sprintf("pipeline gap: no telemetry for %s (stale > %s)", path, w.StaleAfter()),
			Since:      since,
			OccurredAt: now,
			Runbook:    "rb/pipeline-gap",
		})
	}
	// Resolved: was firing, now fresh
	for _, path := range fresh {
		since, was := trk.firing[path]
		if !was {
			continue
		}
		delete(trk.firing, path)
		out = append(out, alarm.Event{
			RuleName:   pipelineGapRule,
			AssetPath:  path,
			PointPath:  path + ".pipeline.heartbeat",
			Severity:   "major",
			State:      alarm.StateResolved,
			Summary:    fmt.Sprintf("pipeline gap resolved: telemetry resumed for %s", path),
			Since:      since,
			OccurredAt: now,
			Runbook:    "rb/pipeline-gap",
		})
	}
	return out
}

// touchFromHeartbeatLines records assets from cios_pipeline_heartbeat
// lines that promproj decode skips (unknown quantity).
func touchFromHeartbeatLines(w *freshness.Watch, lines []string, now time.Time) {
	if w == nil {
		return
	}
	for _, line := range lines {
		if path, ok := heartbeatPath(line); ok {
			w.Touch(path, now)
		}
	}
}

func heartbeatPath(line string) (string, bool) {
	if !strings.HasPrefix(line, "cios_pipeline_heartbeat{") {
		return "", false
	}
	// path="..."
	const key = `path="`
	i := strings.Index(line, key)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(key):]
	j := strings.Index(rest, `"`)
	if j <= 0 {
		return "", false
	}
	return rest[:j], true
}
