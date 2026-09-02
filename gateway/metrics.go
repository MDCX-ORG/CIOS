// Package gateway — metrics.go: DATA-RESILIENCE G5 / R-c counters.
package gateway

import (
	"io"

	"github.com/yurimeng/cios/pkg/resilmetrics"
)

// GatewayResilMetrics holds process-local resilience counters.
type GatewayResilMetrics struct {
	PublishFailures resilmetrics.Counter
	WALFrames       resilmetrics.Counter
	WALFullDrops    resilmetrics.Counter
	WALBytes        resilmetrics.Gauge
}

func (m *GatewayResilMetrics) writePrometheus(w io.Writer) {
	if m == nil {
		return
	}
	resilmetrics.WriteCounter(w, "cios_gateway_publish_failures_total",
		"NATS JetStream publish failures (before WAL fallback)", m.PublishFailures.Get())
	resilmetrics.WriteCounter(w, "cios_gateway_wal_frames_total",
		"Frames successfully written to the local NATS fallback WAL", m.WALFrames.Get())
	resilmetrics.WriteCounter(w, "cios_gateway_wal_full_drops_total",
		"Live frames rejected because the WAL is full (ErrWALFull)", m.WALFullDrops.Get())
	resilmetrics.WriteGauge(w, "cios_gateway_wal_bytes",
		"Current WAL file size in bytes", m.WALBytes.Get())
}
