// Package natspub defines the wire payload (TelemetryBatch) and
// publishes it to NATS JetStream with a local write-ahead-log
// fallback (spec-006 §2.1, §2.2, §3.1). The payload is JSON for
// the M1 first cut; the Encoding field tags the wire format so a
// future protobuf migration (PRMT-015b) can ship a second value
// without breaking older consumers.
package natspub

import "time"

// TelemetryBatch is the NATS wire payload for one gateway
// collection tick. One batch = one tick's worth of samples for one
// top-level asset (e.g. one pod). The Lines field carries
// Prometheus text exposition lines when Encoding == "promtext".
type TelemetryBatch struct {
	Site      string    `json:"site"`
	TopAsset  string    `json:"top_asset"` // first two segments of any sample path, e.g. "sgp01.pod002"
	Timestamp time.Time `json:"timestamp"`
	Encoding  string    `json:"encoding"` // "promtext" (M1); "proto3" reserved for PRMT-015b
	Lines     []string  `json:"lines"`    // one Prometheus exposition line per sample
}

// Subject returns the NATS subject for this batch per spec-006 §2.2.
// Format: cios.tlm.<site>.<top_asset>
func (b TelemetryBatch) Subject() string {
	return "cios.tlm." + b.Site + "." + b.TopAsset
}
