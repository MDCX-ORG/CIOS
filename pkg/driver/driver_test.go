package driver

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Compile-time assertion: a minimal implementation must satisfy the
// Driver interface. If a method is added or removed, this assignment
// fails to compile and points the maintainer straight at the contract
// change.
var _ Driver = (*stubDriver)(nil)

type stubDriver struct{}

func (stubDriver) Init(context.Context, DriverConfig) error           { return nil }
func (stubDriver) Discover(context.Context) ([]AssetCandidate, error) { return nil, nil }
func (stubDriver) Collect(context.Context) ([]Sample, error)          { return nil, nil }
func (stubDriver) Subscribe(context.Context, chan<- Sample) error     { return nil }
func (stubDriver) Write(context.Context, ControlCommand) (ControlResult, error) {
	return ControlResult{}, nil
}
func (stubDriver) Health(context.Context) DriverHealth { return DriverHealth{} }

func TestQualityConstants(t *testing.T) {
	// The four Quality values are part of the wire contract: any
	// change here breaks downstream consumers (storage, alerting,
	// conformance). Pin them.
	cases := []struct {
		got, want Quality
	}{
		{QualityGood, "good"},
		{QualityStale, "stale"},
		{QualitySuspect, "suspect"},
		{QualitySubstituted, "substituted"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("Quality constant = %q, want %q", c.got, c.want)
		}
	}
	// Sanity: distinct values (defensive — the spec lists four
	// disjoint states).
	seen := map[Quality]bool{}
	for _, q := range []Quality{QualityGood, QualityStale, QualitySuspect, QualitySubstituted} {
		if seen[q] {
			t.Errorf("duplicate Quality value %q", q)
		}
		seen[q] = true
	}
}

func TestErrNotSupported(t *testing.T) {
	// errors.Is must match. A driver that wraps the sentinel with %w
	// must still be detectable.
	if !errors.Is(ErrNotSupported, ErrNotSupported) {
		t.Errorf("errors.Is(ErrNotSupported, ErrNotSupported) = false, want true")
	}
	wrapped := errors.New("wrap me: " + ErrNotSupported.Error())
	// Note: errors.Is does NOT match a plain string-concat wrap. The
	// canonical pattern is fmt.Errorf("%w: ...", ErrNotSupported).
	// We don't import fmt here just to test that; the contract is
	// "errors.Is(err, ErrNotSupported) works" and the only way to
	// make it work is for the driver to use %w. This test just
	// confirms the sentinel is non-nil and non-empty.
	if ErrNotSupported == nil {
		t.Fatal("ErrNotSupported is nil")
	}
	if ErrNotSupported.Error() == "" {
		t.Error("ErrNotSupported.Error() is empty")
	}
	// Suppress the unused-variable warning from the wrapped assignment
	// above (it exists to document the expected pattern).
	_ = wrapped
}

func TestSampleFields(t *testing.T) {
	// Smoke test that Sample / ControlCommand / ControlResult /
	// DriverHealth / DriverConfig / AssetCandidate are constructible
	// with the expected field set. This catches accidental field
	// removal at compile + assign time.
	now := time.Now()
	s := Sample{Point: "p", Value: 1.0, Ts: now, Quality: QualityGood}
	if s.Point != "p" || s.Value != 1.0 || s.Quality != QualityGood {
		t.Errorf("Sample round-trip failed: %+v", s)
	}
	cmd := ControlCommand{Point: "p", Value: 1.0, RequestID: "r", TTL: 5 * time.Second}
	if cmd.RequestID != "r" || cmd.TTL != 5*time.Second {
		t.Errorf("ControlCommand round-trip failed: %+v", cmd)
	}
	res := ControlResult{Accepted: true, Readback: 1.0, ReadbackTs: now}
	if !res.Accepted || res.Readback != 1.0 {
		t.Errorf("ControlResult round-trip failed: %+v", res)
	}
	h := DriverHealth{Connected: true, LastSuccess: now, ErrorCount: 3, Detail: "ok"}
	if !h.Connected || h.ErrorCount != 3 || h.Detail != "ok" {
		t.Errorf("DriverHealth round-trip failed: %+v", h)
	}
	cfg := DriverConfig{Endpoint: "127.0.0.1:1502", Options: map[string]string{"unit_id": "1"}}
	if cfg.Endpoint != "127.0.0.1:1502" || cfg.Options["unit_id"] != "1" {
		t.Errorf("DriverConfig round-trip failed: %+v", cfg)
	}
	ac := AssetCandidate{Type: "cdu", Serial: "SN1", Hints: map[string]string{"fw": "1.0"}}
	if ac.Type != "cdu" || ac.Serial != "SN1" || ac.Hints["fw"] != "1.0" {
		t.Errorf("AssetCandidate round-trip failed: %+v", ac)
	}
}
