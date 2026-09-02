package natspub

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/wal"
)

// JetStreamContext is the subset of nats.JetStreamContext the
// Publisher uses. It is exposed as an interface so tests can pass
// a mock without spinning up a real nats-server (PRMT-015 §4.3).
type JetStreamContext interface {
	Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error)
}

// Publisher publishes TelemetryBatch to NATS JetStream with WAL
// fallback. A single instance is safe for concurrent use: each
// Publish serialises the JSON encode and the NATS / WAL writes.
type Publisher struct {
	js  JetStreamContext
	wal *wal.WAL

	// Metrics optional (DATA-RESILIENCE G5); nil hooks are no-ops.
	OnPublishFail func()
	OnWALFrame    func()
	OnWALFull     func()

	// mu guards the encode+publish path so two concurrent Publish
	// calls do not interleave their JSON encoding (we don't actually
	// share buffers, but the lock also makes WAL Replay + Write
	// atomic with respect to Publish).
	mu sync.Mutex
}

// New returns a Publisher. wal may be nil, in which case publish
// failures propagate to the caller (no buffering).
func New(js JetStreamContext, w *wal.WAL) *Publisher {
	return &Publisher{js: js, wal: w}
}

// WALBytes returns current WAL size for the resilmetrics gauge.
func (p *Publisher) WALBytes() int64 {
	if p == nil || p.wal == nil {
		return 0
	}
	n, err := p.wal.Size()
	if err != nil {
		return 0
	}
	return n
}

// Publish serialises b as JSON and publishes to b.Subject().
// Order of operations:
//  1. If wal != nil and wal.Len() > 0, attempt Replay first; on
//     replay error we log and continue (the live publish below is
//     still attempted — we never let a stuck WAL block real-time
//     traffic; the replay will retry next tick).
//  2. Encode the current batch as JSON and Publish to NATS.
//  3. If Publish fails and wal != nil, Write the encoded payload
//     to the WAL. Returns nil if either step succeeded.
//
// Returns an error only if both NATS Publish and the WAL fallback
// (or the absence of a WAL) failed.
func (p *Publisher) Publish(ctx context.Context, b TelemetryBatch) error {
	if p.js == nil {
		return errors.New("natspub: nil JetStream context")
	}

	// Check ctx up front so we don't bother encoding if the caller
	// is already shutting down.
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := json.Marshal(b)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Step 1: drain WAL first so the live tick is delivered in
	// order with any backlog. Replay truncates the WAL on full
	// success, so the next Publish starts with a clean slate.
	//
	// F1 fix (PRMT-015 R1): the buffered frames may belong to
	// different top_assets from earlier ticks. The original code
	// published every frame to b.Subject() (the current tick's
	// subject), which silently violated spec-006 §2.2 routing
	// whenever NATS was down across a top_asset boundary. Decode
	// each frame back to a TelemetryBatch and use ITS Subject() so
	// the gateway recovers with the same per-asset partitioning
	// it produced.
	if p.wal != nil {
		n, lerr := p.wal.Len()
		if lerr != nil {
			log.Printf("natspub: wal len: %v", lerr)
		} else if n > 0 {
			if rerr := p.wal.Replay(func(frame []byte) error {
				var parsed TelemetryBatch
				if uerr := json.Unmarshal(frame, &parsed); uerr != nil {
					// A frame that cannot be parsed is a
					// permanent failure for that entry — skip it
					// and let the live publish proceed. The
					// alternative (return the error, stop replay)
					// would wedge the WAL on a single bad frame.
					log.Printf("natspub: wal replay: skip undecodable frame: %v", uerr)
					return nil
				}
				_, perr := p.js.Publish(parsed.Subject(), frame)
				return perr
			}); rerr != nil {
				log.Printf("natspub: wal replay failed: %v (will retry next tick)", rerr)
			}
		}
	}

	// Step 2: live publish.
	if _, perr := p.js.Publish(b.Subject(), payload); perr == nil {
		return nil
	} else {
		if p.OnPublishFail != nil {
			p.OnPublishFail()
		}
		// Step 3: fallback to WAL.
		if p.wal == nil {
			return perr
		}
		if werr := p.wal.Write(payload); werr != nil {
			if errors.Is(werr, wal.ErrWALFull) && p.OnWALFull != nil {
				p.OnWALFull()
			}
			return errors.Join(perr, werr)
		}
		if p.OnWALFrame != nil {
			p.OnWALFrame()
		}
		return nil
	}
}
