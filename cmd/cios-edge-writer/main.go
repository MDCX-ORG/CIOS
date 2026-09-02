// Command cios-edge-writer is the M1 NATS JetStream consumer that
// drains cios.tlm.<site>.<top_asset> subjects and forwards the
// Prometheus text lines to VictoriaMetrics. It is the second half
// of the PRMT-015 telemetry chain: the gateway publishes batches
// into JetStream (with a local WAL fallback), and cios-edge-writer
// does the durable consume + HTTP import into VM. Failure to post
// a batch to VM triggers a Nak so the message is redelivered.
//
// The binary is a single-purpose daemon: it has no config file,
// only command-line flags. Reconfiguration is a process restart.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/natspub"
	"github.com/yurimeng/cios/pkg/resilmetrics"
)

// edgeResil holds DATA-RESILIENCE G5 counters for this process.
var edgeResil = struct {
	NakTotal       resilmetrics.Counter
	PoisonDrops    resilmetrics.LabeledCounter // label: reason
	VMPostFailures resilmetrics.LabeledCounter // label: class
}{}

func writeEdgeMetrics(w io.Writer) {
	resilmetrics.WriteCounter(w, "cios_edge_writer_nak_total",
		"JetStream Naks issued for transient VM failures", edgeResil.NakTotal.Get())
	resilmetrics.WriteLabeledCounter(w, "cios_edge_writer_poison_drops_total",
		"Malformed or non-replayable messages Ack-dropped", "reason", &edgeResil.PoisonDrops)
	resilmetrics.WriteLabeledCounter(w, "cios_edge_writer_vm_post_failures_total",
		"VictoriaMetrics POST failures by class", "class", &edgeResil.VMPostFailures)
}

func main() {
	natsURL := flag.String("nats-url", "nats://localhost:4222", "NATS server URL")
	stream := flag.String("stream", "CIOS_TLM", "JetStream stream name")
	subject := flag.String("subject", "cios.tlm.>", "subscribe subject filter")
	vmURL := flag.String("vm-url", "", "VictoriaMetrics /api/v1/import/prometheus URL (required)")
	site := flag.String("site", "", "site filter; empty = accept all sites")
	consumer := flag.String("consumer", "edge-writer", "durable consumer name")
	metricsListen := flag.String("metrics-listen", "",
		"optional HTTP bind for GET /metrics (e.g. 127.0.0.1:9103); empty → env CIOS_EDGE_WRITER_METRICS_LISTEN")
	flag.Parse()

	if *vmURL == "" {
		fmt.Fprintln(os.Stderr, "cios-edge-writer: -vm-url is required")
		os.Exit(2)
	}
	maddr := strings.TrimSpace(*metricsListen)
	if maddr == "" {
		maddr = strings.TrimSpace(os.Getenv("CIOS_EDGE_WRITER_METRICS_LISTEN"))
	}
	stopMetrics, err := resilmetrics.Listen(maddr, writeEdgeMetrics)
	if err != nil {
		log.Fatalf("cios-edge-writer: metrics: %v", err)
	}
	defer stopMetrics()

	// DATA-RESILIENCE G2: reconnect forever; unexpected close → exit
	// so systemd/compose restarts a live consumer (not a zombie).
	var intentionalDrain atomic.Bool
	closed := make(chan struct{})
	opts := natspub.ConnectOpts("cios-edge-writer", func(*nats.Conn) {
		close(closed)
		if !intentionalDrain.Load() {
			log.Printf("cios-edge-writer: nats connection closed unexpectedly; exiting for restart")
			os.Exit(1)
		}
	})
	nc, err := nats.Connect(*natsURL, opts...)
	if err != nil {
		log.Fatalf("cios-edge-writer: nats connect: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("cios-edge-writer: jetstream: %v", err)
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// ctx is created up front so the per-message handler can bind
	// its outbound HTTP POST to it (PRMT-015b §4.7: handler POST
	// must respect shutdown). It is also the signal-watcher context
	// used by the graceful-exit block below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// R4 A 路：handler 提升到包级 makeHandler，使 R4 测试可以
	// 两次顺序投递同一 handler 闭包而不必反射拆 main()。逻辑与
	// 行为与原 handler := func(msg) {...} 完全等价——仅闭包变量
	// 改为显式参数。
	handler := makeHandler(ctx, httpClient, *vmURL, *site)

	// DATA-RESILIENCE G1: unlimited MaxDeliver + NakWithDelay on VM
	// failure (poison = decode/encoding only, handled inside handler).
	sub, err := js.Subscribe(*subject, handler,
		nats.Durable(*consumer),
		nats.ManualAck(),
		nats.MaxDeliver(natspub.TransientMaxDeliver),
	)
	if err != nil {
		log.Fatalf("cios-edge-writer: subscribe: %v", err)
	}
	defer func() {
		// Best-effort drain of the active subscription; ignore the
		// returned error because we are about to exit anyway.
		_ = sub.Unsubscribe()
	}()

	log.Printf("cios-edge-writer: subscribed to %s on stream %s (consumer=%s), forwarding to %s",
		*subject, *stream, *consumer, *vmURL)

	// Graceful exit on SIGINT/SIGTERM. Drain flushes in-flight acks
	// and then closes the connection. The runtime exits via the
	// deferred nc.Close(). We wait up to 10s for the nats.ClosedHandler
	// to fire, so Drain really finishes before we return.
	<-ctx.Done()
	log.Printf("cios-edge-writer: signal received, draining")
	intentionalDrain.Store(true)
	if err := nc.Drain(); err != nil {
		log.Printf("cios-edge-writer: drain: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		log.Printf("cios-edge-writer: drain did not complete within 10s")
	}
}

// errBuildPost / errDoPost tag the two failure modes postBatchToVM
// can return. The handler uses errors.Is to choose between the two
// drop-reason labels that pre-PRMT-030 dashboards key on
// ("build-POST-failed" vs "POST-network-failed"). Keeping the labels
// unchanged is a PRMT-030 §B.6 requirement.
var (
	errBuildPost = errors.New("edge-writer: build POST request failed")
	errDoPost    = errors.New("edge-writer: POST round-trip failed")
)

// makeHandler is the per-message handler, lifted out of main() in
// R4 so the test file can drive it directly (A 路: 两次顺序投递
// 同一 handler 闭包，验证 "next message is still processed"). The
// body is identical to the pre-R4 main()-local closure modulo
// reading the now-explicit parameters instead of closed-over
// variables.
func makeHandler(ctx context.Context, hc *http.Client, vmURL, siteFilter string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		// PRMT-030 §B / CONC-022: panic-isolation at the handler
		// boundary, mirroring cmd/cios-alarm/main.go. A panic here
		// would otherwise surface to nats-io's dispatcher and
		// could tear down the subscription; we recover, log, and
		// fall through (the handler has not Ack'd, so JetStream
		// will redeliver; poison-cap kicks in via DropIfPoison).
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			log.Printf("cios-edge-writer: handler panic: %v\n%s", r, debug.Stack())
		}()
		var batch natspub.TelemetryBatch
		if err := json.Unmarshal(msg.Data, &batch); err != nil {
			// Malformed payload — no point redelivering. Ack and move on.
			log.Printf("cios-edge-writer: decode: %v (acking)", err)
			edgeResil.PoisonDrops.With("decode").Inc()
			_ = msg.Ack()
			return
		}
		if batch.Encoding != "promtext" {
			// Future proto3 frames will be handled by a sibling
			// consumer; for now drop unknown encodings with a log.
			log.Printf("cios-edge-writer: unknown encoding %q (acking)", batch.Encoding)
			edgeResil.PoisonDrops.With("encoding").Inc()
			_ = msg.Ack()
			return
		}
		if siteFilter != "" && batch.Site != siteFilter {
			// Not for this site; ack so we don't redeliver forever.
			_ = msg.Ack()
			return
		}
		body := strings.Join(batch.Lines, "\n")
		status, err := postBatchToVM(ctx, hc, vmURL, body)
		if err != nil {
			// Build / Do failure — keep the prior reason labels so
			// operator dashboards (which key on "build-POST-failed"
			// vs "POST-network-failed") don't have to learn a new
			// vocabulary. postBatchToVM tags the error with
			// errBuildPost / errDoPost via errors.Is below.
			reason := "POST-network-failed"
			if errors.Is(err, errBuildPost) {
				reason = "build-POST-failed"
			}
			dc := natspub.DeliveryCount(msg)
			log.Printf("cios-edge-writer: POST %s: %v (nak-delay deliveries=%d reason=%s)", vmURL, err, dc, reason)
			edgeResil.VMPostFailures.With(reason).Inc()
			edgeResil.NakTotal.Inc()
			// DATA-RESILIENCE G1: transient outage → NakWithDelay, never Ack-drop.
			if nerr := natspub.NakTransient(msg); nerr != nil {
				log.Printf("cios-edge-writer: nak: %v", nerr)
			}
			return
		}
		if status/100 != 2 {
			// postBatchToVM already logged body+status. Transient
			// VM unavailability is NOT poison (G1) — delay redelivery.
			dc := natspub.DeliveryCount(msg)
			log.Printf("cios-edge-writer: POST non-2xx status=%d (nak-delay deliveries=%d)", status, dc)
			edgeResil.VMPostFailures.With("POST-non-2xx").Inc()
			edgeResil.NakTotal.Inc()
			if nerr := natspub.NakTransient(msg); nerr != nil {
				log.Printf("cios-edge-writer: nak: %v", nerr)
			}
			return
		}
		if err := msg.Ack(); err != nil {
			log.Printf("cios-edge-writer: ack: %v", err)
		}
	}
}

// postBatchToVM is the IO helper extracted from the prior 74-line
// handler closure (PRMT-030 §B / FUNC-019). It owns the build /
// Do / read-body / drain / close sequence and reports the outcome as
//
//	(status, nil) — round-trip completed (caller still drains & closes).
//	(0, errBuildPost) — http.NewRequestWithContext returned an error.
//	(0, errDoPost)    — Client.Do returned an error (network failure).
//
// Non-2xx status is returned as (status, nil); the handler inspects
// status/100 itself and decides Nak vs drop via natspub.DropIfPoison.
//
// R4 (architect ruling): postBatchToVM is allowed log side-effects.
// On non-2xx it emits a single line with body + status + "(naking)"
// marker so the operator's pre-PRMT-030 log-grep pattern
// (POST %s: status %d, body: %q (naking)) stays valid. The
// helper therefore does not return body bytes — that information
// is in the log.
func postBatchToVM(ctx context.Context, hc *http.Client, vmURL, body string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vmURL, strings.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errBuildPost, err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errDoPost, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Match the prior log shape: read up to 256 bytes of the
		// body so an operator can see why VM rejected the import.
		buf := make([]byte, 256)
		n, _ := io.ReadFull(resp.Body, buf)
		log.Printf("cios-edge-writer: POST %s: status %d, body: %q (naking)", vmURL, resp.StatusCode, buf[:n])
		return resp.StatusCode, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
