// Package freshness tracks per-asset telemetry age for pipeline gap
// detection (DATA-RESILIENCE G6 / R-d).
//
// The gateway publishes on a fixed tick; if an asset stops showing up
// in decoded batches for longer than StaleAfter, the pipeline is
// considered gapped (NATS/VM/WAL silent loss, hung consumer, etc.).
package freshness

import (
	"sort"
	"sync"
	"time"
)

// DefaultStaleAfter is 10 minutes — ~ several gateway ticks under
// typical 15–30s intervals, short enough to catch outages quickly.
const DefaultStaleAfter = 10 * time.Minute

// Gap is one asset that has gone silent past the stale threshold.
type Gap struct {
	AssetPath string
	LastSeen  time.Time
	Age       time.Duration
}

// Watch records last-seen times for assets that have produced data.
type Watch struct {
	mu         sync.Mutex
	last       map[string]time.Time
	staleAfter time.Duration
}

// New returns a Watch. staleAfter <= 0 uses DefaultStaleAfter.
func New(staleAfter time.Duration) *Watch {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	return &Watch{
		last:       make(map[string]time.Time),
		staleAfter: staleAfter,
	}
}

// Touch records that assetPath was seen at t (batch timestamp or wall clock).
func (w *Watch) Touch(assetPath string, t time.Time) {
	if w == nil || assetPath == "" {
		return
	}
	if t.IsZero() {
		t = time.Now().UTC()
	} else {
		t = t.UTC()
	}
	w.mu.Lock()
	w.last[assetPath] = t
	w.mu.Unlock()
}

// Gaps returns assets last seen more than StaleAfter before now.
// Only assets that have been Touched at least once are considered
// (unknown assets are not "gapped" — they may not be deployed).
func (w *Watch) Gaps(now time.Time) []Gap {
	if w == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Gap
	for path, last := range w.last {
		age := now.Sub(last)
		if age > w.staleAfter {
			out = append(out, Gap{AssetPath: path, LastSeen: last, Age: age})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetPath < out[j].AssetPath })
	return out
}

// Fresh returns assets that are currently within StaleAfter of now
// (used to resolve prior gap alarms).
func (w *Watch) Fresh(now time.Time) []string {
	if w == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for path, last := range w.last {
		if now.Sub(last) <= w.staleAfter {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// StaleAfter returns the configured threshold.
func (w *Watch) StaleAfter() time.Duration {
	if w == nil {
		return DefaultStaleAfter
	}
	return w.staleAfter
}

// Len returns how many assets are tracked.
func (w *Watch) Len() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.last)
}
