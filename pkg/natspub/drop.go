// Package natspub — drop.go: JetStream delivery helpers for NATS handlers.
//
// DATA-RESILIENCE (docs/runbooks/telemetry-data-resilience.md G1):
//   - Parse/encoding poison → Ack and drop immediately (never redeliver).
//   - Transient downstream failures (VM 5xx, PG down) → NakWithDelay
//     with backoff; do NOT treat delivery count as "poison".
//   - PoisonDeliverCap remains for DropIfPoison (legacy / defensive) but
//     edge-writer and cios-alarm transport paths no longer Ack-drop on cap.
//
// Subscribe options for consumers that need outage tolerance:
//
//	nats.MaxDeliver(TransientMaxDeliver)  // unlimited (-1)
//	ManualAck + NakWithDelay(NakBackoff(dc))
package natspub

import (
	"time"

	"github.com/nats-io/nats.go"
)

// PoisonDeliverCap is the historical JetStream max-delivery cap (K19).
// Still used by DropIfPoison for callers that want a hard count-based drop.
// Transport/outage paths MUST NOT use this as a drop trigger (G1).
const PoisonDeliverCap = 5

// TransientMaxDeliver is MaxDeliver for outage-tolerant consumers.
// -1 = unlimited redeliveries (stream MaxAge is the real retention bound).
const TransientMaxDeliver = -1

// NakBackoff returns the NakWithDelay for the given 1-based delivery count.
// Ladder: 5s → 15s → 30s → 1m → 2m (cap). Keeps JetStream from burning
// MaxDeliver in seconds during a multi-minute VM/PG outage (G1).
func NakBackoff(deliveries int) time.Duration {
	if deliveries < 1 {
		deliveries = 1
	}
	switch {
	case deliveries <= 1:
		return 5 * time.Second
	case deliveries == 2:
		return 15 * time.Second
	case deliveries == 3:
		return 30 * time.Second
	case deliveries == 4:
		return time.Minute
	default:
		return 2 * time.Minute
	}
}

// DeliveryCount returns JetStream NumDelivered (1 on first delivery).
// Non-JS / error → 1 (safe: first-delay backoff, never drop).
func DeliveryCount(msg *nats.Msg) int {
	if msg == nil {
		return 1
	}
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return 1
	}
	dc := int(meta.NumDelivered)
	if dc < 1 {
		return 1
	}
	return dc
}

// DropIfPoison reports whether the given message has reached the
// historical poison-cap. Prefer not using this for transport failures
// (G1); keep for tests and any parse-path that still wants a count stop.
//
// Metadata missing → drop=false (safe direction).
func DropIfPoison(msg *nats.Msg) (dropped bool, deliveries int) {
	if msg == nil {
		return false, 0
	}
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return false, 0
	}
	dc := int(meta.NumDelivered)
	if dc >= PoisonDeliverCap {
		return true, dc
	}
	return false, dc
}

// NakTransient Naks with backoff for a transient downstream failure.
// Logs nothing — caller logs context. Returns NakWithDelay error if any.
func NakTransient(msg *nats.Msg) error {
	if msg == nil {
		return nil
	}
	dc := DeliveryCount(msg)
	return msg.NakWithDelay(NakBackoff(dc))
}
