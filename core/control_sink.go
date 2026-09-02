// Package core — control_sink.go: southbound Set dispatch after policy
// (P722). Policy stays in setctl.go; this is the optional handoff to
// gateway/driver (or a test double). Nil sink ⇒ policy-only Accepted.
package core

import (
	"context"
	"sync"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
)

// ControlDispatch is one policy-passed Set ready for southbound delivery.
type ControlDispatch struct {
	Path            string
	AuditID         string
	Actor           string
	Class           RiskClass
	Value           float64
	TTL             time.Duration
	RequireReadback bool
}

// ControlDispatchResult is what a sink returns after attempting write.
type ControlDispatchResult struct {
	Accepted   bool
	Readback   float64
	ReadbackTs time.Time
	Detail     string
}

// ControlSink receives accepted Sets. Implementations may call
// driver.Write, publish NATS, or record for tests.
type ControlSink interface {
	DispatchControl(ctx context.Context, cmd ControlDispatch) (ControlDispatchResult, error)
}

// RecordingControlSink stores every dispatch for tests.
type RecordingControlSink struct {
	mu   sync.Mutex
	Cmds []ControlDispatch
	// Result, if non-nil, is returned for every dispatch.
	Result *ControlDispatchResult
	// Err, if non-nil, is returned for every dispatch.
	Err error
}

// DispatchControl implements ControlSink.
func (r *RecordingControlSink) DispatchControl(_ context.Context, cmd ControlDispatch) (ControlDispatchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Cmds = append(r.Cmds, cmd)
	if r.Err != nil {
		return ControlDispatchResult{}, r.Err
	}
	if r.Result != nil {
		return *r.Result, nil
	}
	return ControlDispatchResult{Accepted: true, Detail: "recorded"}, nil
}

// DriverControlSink maps ControlDispatch → driver.Write.
// Point is the full cpath; drivers that key by relative point need
// a richer adapter (gateway registry) — this is the direct seam.
type DriverControlSink struct {
	Driver driver.Driver
}

// DispatchControl implements ControlSink.
func (d DriverControlSink) DispatchControl(ctx context.Context, cmd ControlDispatch) (ControlDispatchResult, error) {
	if d.Driver == nil {
		return ControlDispatchResult{Accepted: false, Detail: "no driver"}, nil
	}
	res, err := d.Driver.Write(ctx, driver.ControlCommand{
		Point:     cmd.Path,
		Value:     cmd.Value,
		RequestID: cmd.AuditID,
		TTL:       cmd.TTL,
	})
	if err != nil {
		return ControlDispatchResult{Accepted: false, Detail: err.Error()}, err
	}
	return ControlDispatchResult{
		Accepted:   res.Accepted,
		Readback:   res.Readback,
		ReadbackTs: res.ReadbackTs,
	}, nil
}
