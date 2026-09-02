// Command cios-alarm is the M1 E1.6 alarm engine. It subscribes
// to the cios.tlm.<site>.> JetStream subjects produced by the
// gateway, evaluates each rule from -rules-dir against the
// per-instance snapshot, drives the firing/resolved state machine
// (spec-003 §4), and for every transition publishes a CloudEvents
// 1.0 JSON message to cios.evt.<site>.alarm AND upserts the alarms
// row in PostgreSQL.
//
// The binary owns its own PG connection (spec-006 §1.1 — alarm
// must not depend on cios-core's lifecycle). The alarms table
// itself is created by cios-core's NewPGStore running
// migrations/001_init.sql, so on a fresh database the operator
// must start cios-core at least once before cios-alarm.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/alarm"
	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/freshness"
	"github.com/yurimeng/cios/pkg/natspub"
	"github.com/yurimeng/cios/pkg/promproj"
	"github.com/yurimeng/cios/pkg/resilmetrics"
)

// alarmResil holds DATA-RESILIENCE G5 counters.
var alarmResil = struct {
	NakTotal       resilmetrics.Counter
	PoisonDrops    resilmetrics.LabeledCounter // reason
	UpsertFailures resilmetrics.Counter
}{}

func writeAlarmMetrics(w io.Writer) {
	resilmetrics.WriteCounter(w, "cios_alarm_nak_total",
		"JetStream Naks issued for transient PG upsert failures", alarmResil.NakTotal.Get())
	resilmetrics.WriteLabeledCounter(w, "cios_alarm_poison_drops_total",
		"Malformed telemetry batches Ack-dropped", "reason", &alarmResil.PoisonDrops)
	resilmetrics.WriteCounter(w, "cios_alarm_upsert_failures_total",
		"Alarm Upsert failures (before NakWithDelay)", alarmResil.UpsertFailures.Get())
}

func main() {
	natsURL := flag.String("nats-url", "nats://localhost:4222", "NATS server URL")
	site := flag.String("site", "", "site code (required, e.g. sgp01)")
	rulesDir := flag.String("rules-dir", "", "directory of AlarmRule YAML files (required)")
	protocolDir := flag.String("protocol-dir", "", "protocol/ directory containing types/quantities/locations.yaml (required)")
	pgDSN := flag.String("pg-dsn", "", "PostgreSQL DSN for the alarms table (required)")
	consumer := flag.String("consumer", "cios-alarm", "JetStream durable consumer name")
	autoTicket := flag.Bool("auto-ticket", false, "auto-open a ticket on alarm firing (M2 E2.3)")
	metricsListen := flag.String("metrics-listen", "",
		"optional HTTP bind for GET /metrics (e.g. 127.0.0.1:9104); empty → env CIOS_ALARM_METRICS_LISTEN")
	freshnessStale := flag.Duration("freshness-stale", freshness.DefaultStaleAfter,
		"pipeline gap threshold: no telemetry for this long → major pipeline-gap alarm (DATA-RESILIENCE G6)")
	flag.Parse()

	if *site == "" || *rulesDir == "" || *protocolDir == "" || *pgDSN == "" {
		fmt.Fprintln(os.Stderr, "cios-alarm: -site, -rules-dir, -protocol-dir, -pg-dsn are all required")
		os.Exit(2)
	}
	maddr := strings.TrimSpace(*metricsListen)
	if maddr == "" {
		maddr = strings.TrimSpace(os.Getenv("CIOS_ALARM_METRICS_LISTEN"))
	}
	stopMetrics, err := resilmetrics.Listen(maddr, writeAlarmMetrics)
	if err != nil {
		log.Fatalf("cios-alarm: metrics: %v", err)
	}
	defer stopMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dict, err := cpath.LoadDict(*protocolDir)
	if err != nil {
		log.Fatalf("cios-alarm: load dict: %v", err)
	}
	rules, err := alarm.LoadRules(*rulesDir, dict)
	if err != nil {
		log.Fatalf("cios-alarm: load rules: %v", err)
	}
	if len(rules) == 0 {
		log.Printf("cios-alarm: warning: no AlarmRule YAMLs loaded from %s", *rulesDir)
	}
	engine := alarm.NewEngine(rules)
	freshWatch := freshness.New(*freshnessStale)
	gapTrk := newGapTracker()

	st, err := alarm.NewStore(ctx, *pgDSN)
	if err != nil {
		log.Fatalf("cios-alarm: pg store: %v", err)
	}
	defer st.Close()

	// DATA-RESILIENCE G2: reconnect forever; unexpected close → exit.
	var intentionalDrain atomic.Bool
	closed := make(chan struct{})
	opts := natspub.ConnectOpts("cios-alarm", func(*nats.Conn) {
		close(closed)
		if !intentionalDrain.Load() {
			log.Printf("cios-alarm: nats connection closed unexpectedly; exiting for restart")
			os.Exit(1)
		}
	})
	nc, err := nats.Connect(*natsURL, opts...)
	if err != nil {
		log.Fatalf("cios-alarm: nats connect: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("cios-alarm: jetstream: %v", err)
	}

	// DATA-RESILIENCE G1: unlimited MaxDeliver + NakWithDelay on PG
	// upsert failure. Parse/encoding poison still Acks immediately
	// in the handler (not redelivered).
	sub, err := js.Subscribe("cios.tlm."+*site+".>", makeHandler(nc, *site, dict, engine, st, *autoTicket, freshWatch),
		nats.Durable(*consumer),
		nats.ManualAck(),
		nats.MaxDeliver(natspub.TransientMaxDeliver),
	)
	if err != nil {
		log.Fatalf("cios-alarm: subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	go runFreshnessLoop(ctx, freshWatch, gapTrk, nc, *site, st, 30*time.Second)

	log.Printf("cios-alarm: site=%s rules=%d consumer=%s freshness-stale=%s subscribed to cios.tlm.%s.>",
		*site, len(rules), *consumer, *freshnessStale, *site)

	<-ctx.Done()
	log.Printf("cios-alarm: signal received, draining")
	intentionalDrain.Store(true)
	if err := nc.Drain(); err != nil {
		log.Printf("cios-alarm: drain: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		log.Printf("cios-alarm: drain did not complete within 10s")
	}
}

// makeHandler returns the per-message handler that decodes a
// telemetry batch, feeds the engine, and publishes/persists any
// state-transition events. Pulled out so the body of main stays
// linear. autoTicket gates the alarm→ticket bridge (PRMT-034).
func makeHandler(nc *nats.Conn, site string, dict *cpath.Dict, engine *alarm.Engine, st *alarm.Store, autoTicket bool, fresh *freshness.Watch) nats.MsgHandler {
	return func(msg *nats.Msg) {
		// PRMT-076: panic-isolation at the handler boundary. A
		// panic here would surface to nats-io's dispatcher and
		// could tear down the subscription; on top of that
		// Go's default behaviour is to crash the whole process.
		// Recover, log, and deliberately skip Ack so JetStream
		// redelivers the message on the next pull (relying on
		// engine idempotency; PRMT-077's Nak provides the
		// explicit per-message rejection path).
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			log.Printf("cios-alarm: handler panic: %v\n%s", r, debug.Stack())
		}()
		var batch natspub.TelemetryBatch
		if err := json.Unmarshal(msg.Data, &batch); err != nil {
			log.Printf("cios-alarm: decode: %v (acking)", err)
			alarmResil.PoisonDrops.With("decode").Inc()
			_ = msg.Ack()
			return
		}
		if batch.Encoding != "promtext" {
			log.Printf("cios-alarm: unknown encoding %q (acking)", batch.Encoding)
			alarmResil.PoisonDrops.With("encoding").Inc()
			_ = msg.Ack()
			return
		}
		now := batch.Timestamp
		if now.IsZero() {
			now = time.Now().UTC()
		}

		// group samples by asset path; each asset becomes one snapshot.
		snapshots := decodeBatch(batch, dict)
		var allEvents []alarm.Event
		for assetPath, entry := range snapshots {
			if fresh != nil {
				fresh.Touch(assetPath, now)
			}
			allEvents = append(allEvents, engine.Observe(assetPath, entry.assetType, entry.snapshot, now)...)
		}
		// Heartbeat lines (G6) are unknown quantities to promproj — touch via path label.
		if fresh != nil {
			touchFromHeartbeatLines(fresh, batch.Lines, now)
		}

		// Use the daemon-wide ctx (cancelled on SIGINT) so an
		// in-flight Upsert aborts cleanly during shutdown. We do
		// NOT bind it to msg.Context — nats.Msg has no such method,
		// and a context per-message would also race with the Ack
		// we issue on the line below.
		upsertCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		persistFailed := processEvents(allEvents, processDeps{
			publishCE:  func(ev alarm.Event) error { return publishCE(nc, site, ev) },
			upsert:     func(ev alarm.Event) error { return st.Upsert(upsertCtx, ev) },
			openTicket: autoTicketOpenTicket(upsertCtx, st, autoTicket),
		})
		// PRMT-077 (eval H2): if any Upsert failed, Nak so JetStream
		// redelivers; Upsert is by-key (eventID = sha256(rule|asset))
		// so a redelivery is idempotent — same id, ON CONFLICT
		// rewrites the same fields. publishCE / OpenTicket failures
		// are best-effort and DO NOT influence the Ack decision
		// (Nak'ing on a NATS blip would amplify transient flakiness
		// into repeated alarm-event replays).
		if persistFailed {
			// DATA-RESILIENCE G1: PG/upsert outage is transient —
			// NakWithDelay, never Ack-drop as "poison".
			dc := natspub.DeliveryCount(msg)
			log.Printf("cios-alarm: upsert persist failed subject=%s deliveries=%d (nak-delay)", msg.Subject, dc)
			alarmResil.UpsertFailures.Inc()
			alarmResil.NakTotal.Inc()
			if nerr := natspub.NakTransient(msg); nerr != nil {
				log.Printf("cios-alarm: nak: %v", nerr)
			}
		} else {
			_ = msg.Ack()
		}
	}
}

// autoTicketOpenTicket returns the openTicket dep wired to the store
// when autoTicket is true, or a no-op when false (preserves the
// pre-PRMT-077 "only fire-state events open tickets" gate). Pulled
// out as a small constructor so processEvents doesn't have to know
// about the autoTicket flag — and so the test wiring stays a single
// closure literal.
func autoTicketOpenTicket(ctx context.Context, st *alarm.Store, autoTicket bool) func(alarm.Event) error {
	if !autoTicket {
		return nil
	}
	return func(ev alarm.Event) error {
		if ev.State != alarm.StateFiring {
			return nil
		}
		return st.OpenTicket(ctx, ev)
	}
}

// processDeps wires the three side-effects processEvents performs
// per event. Each is a function value so the loop body stays
// identical to the production wiring while remaining testable with
// closures that return canned errors (see main_test.go — PRMT-077).
type processDeps struct {
	publishCE  func(alarm.Event) error
	upsert     func(alarm.Event) error
	openTicket func(alarm.Event) error // nil → skip
}

// processEvents runs the per-event side-effects (publish CE, upsert,
// open ticket) and returns true iff any *Upsert* failed. publishCE
// and OpenTicket failures are logged but do not flip the return
// value: Upsert is the only durable persistence step (alarms table
// is the source of truth for the current state per spec-003 §4), and
// the other two are best-effort — a transient NATS blip or a
// ticket-dedup race is cheaper to swallow than to redeliver the
// whole batch over.
//
// persistFailed accumulates across the loop: even if some events
// Upserted successfully, a single failure means the batch as a whole
// must be redelivered (the alarm rows we DID write are idempotent on
// redelivery, per Upsert's ON CONFLICT semantics — PRMT-077 §2).
func processEvents(events []alarm.Event, deps processDeps) bool {
	persistFailed := false
	for _, ev := range events {
		if deps.publishCE != nil {
			if err := deps.publishCE(ev); err != nil {
				log.Printf("cios-alarm: publish CE %s: %v", ev.RuleName, err)
			}
		}
		if deps.upsert != nil {
			if err := deps.upsert(ev); err != nil {
				log.Printf("cios-alarm: upsert %s: %v", ev.RuleName, err)
				persistFailed = true
			}
		}
		if deps.openTicket != nil {
			if err := deps.openTicket(ev); err != nil {
				log.Printf("cios-alarm: open-ticket %s: %v", ev.RuleName, err)
			}
		}
	}
	return persistFailed
}

// --- promtext decoding -----------------------------------------------------
//
// promtext is one Prometheus exposition line per sample. The text→
// (labels, value) and labels→relative-point steps live in
// pkg/promproj (ParseLine, RelPoint) so this daemon and the gateway
// share one projection authority — a label-order drift on the
// producer side surfaces here as a parse error, not silent data
// loss (spec-002 §7, L23). PRMT-024 collapsed the once-private
// parsePromLine/quantityFromMetric/buildRelPoint/parseLabels/
// findMatchingBrace/parseFloat helpers back into promproj.
//
// What this layer still owns:
//   - reading the four fields the engine cares about (path, quality,
//     relPoint, value) out of the promproj-shaped result;
//   - the suspect/bad drop policy (PRMT-020 §4.5);
//   - per-asset bucketing into snapshotEntry.

// decoded is one sample reduced to the minimum the engine needs.
type decoded struct {
	assetPath string // e.g. "sgp01.pod002.cdu000"
	assetType string // e.g. "cdu" — from the wire `asset_type` label
	relPoint  string // e.g. "fws.deltat" or "status"
	value     float64
	quality   string
}

// snapshotEntry is one asset's worth of decoded data: the leaf
// asset type (so the engine can filter by AppliesTo) plus the
// relative-point → value map. Decoded at decodeBatch time so the
// handler can feed it straight into engine.Observe without
// re-walking labels.
type snapshotEntry struct {
	assetType string
	snapshot  map[string]float64
}

// decodeBatch parses every line, drops malformed/suspect samples,
// and groups the survivors by assetPath into relative-point
// snapshots. Lines that reference an unknown quantity (or fail
// validation) are logged at debug-equivalent (info) level so a
// misconfigured gateway surfaces in the daemon log without
// wedging the subscription.
func decodeBatch(batch natspub.TelemetryBatch, dict *cpath.Dict) map[string]snapshotEntry {
	out := map[string]snapshotEntry{}
	for _, line := range batch.Lines {
		d, err := decodeLine(line, dict)
		if err != nil {
			// Unknown quantity is common during a rolling deploy; not
			// worth warning per-line. Log once-per-tick noise is fine
			// because a noisy batch will be obvious in the log.
			log.Printf("cios-alarm: skip line: %v (line=%q)", err, line)
			continue
		}
		if d.quality == "suspect" || d.quality == "bad" {
			// PRMT-020 §4.5: only non-suspect samples enter the
			// snapshot. spec-003 §3 "数据缺失不视为满足" — a
			// suspect sample is the moral equivalent of missing.
			continue
		}
		entry, ok := out[d.assetPath]
		if !ok {
			entry = snapshotEntry{assetType: d.assetType, snapshot: map[string]float64{}}
			out[d.assetPath] = entry
		}
		entry.snapshot[d.relPoint] = d.value
		out[d.assetPath] = entry
	}
	return out
}

// decodeLine wires promproj.ParseLine + promproj.RelPoint into the
// four-field `decoded` shape this binary's engine consumes. It is
// the thin glue that used to be parsePromLine; the actual parsing,
// label/escape handling, and quantity-from-metric inverse all live
// in promproj now.
func decodeLine(line string, dict *cpath.Dict) (decoded, error) {
	labels, value, err := promproj.ParseLine(line, dict)
	if err != nil {
		return decoded{}, err
	}
	if labels["path"] == "" {
		return decoded{}, fmt.Errorf("missing path label")
	}
	rel, err := promproj.RelPoint(labels, dict)
	if err != nil {
		return decoded{}, err
	}
	return decoded{
		assetPath: labels["path"],
		assetType: labels["asset_type"],
		relPoint:  rel,
		value:     value,
		quality:   labels["quality"],
	}, nil
}

// --- CloudEvents publish + UUIDv7 ------------------------------------------

// uuidv7 is a minimal UUIDv7 implementation: 48-bit unix-ms time
// in the high bits, 74 bits of cryptographic randomness. We don't
// reuse the spec out of an avoidance of new dependencies (PRMT-020
// §5 "优先零新增").
//
// Order matters: read random bytes FIRST, then OR in the version
// (high nibble of byte 6 = 0x7) and variant (high nibble of byte 8
// = 0b10). Doing it in the other order lets rand.Read clobber
// the fixed bits — a bug we caught in unit tests.
func uuidv7() string {
	var b [16]byte
	now := time.Now().UnixMilli()
	b[0] = byte(now >> 40)
	b[1] = byte(now >> 32)
	b[2] = byte(now >> 24)
	b[3] = byte(now >> 16)
	b[4] = byte(now >> 8)
	b[5] = byte(now)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failure is exceptional; fall back to time-
		// derived bytes — still unique per nanosecond at worst.
		for i := 6; i < 16; i++ {
			b[i] = byte(now >> (uint(i) * 4))
		}
	}
	b[6] = 0x70 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// ceData is the inner JSON object (spec-003 §1.1). The fields
// here are what an alarm consumer reads; CE extension attributes
// (severity, site) live at the top level.
type ceData struct {
	Rule    string `json:"rule"`
	Summary string `json:"summary"`
	State   string `json:"state"`
}

// publishCE builds the CloudEvents envelope, marshals to JSON, and
// publishes to cios.evt.<site>.alarm. We use a plain (non-JetStream)
// Publish because alarm events are downstream of durable telemetry
// — replaying a stale firing is worse than losing a transient one
// that the next tick will re-produce.
func publishCE(nc *nats.Conn, site string, ev alarm.Event) error {
	body, err := buildCEBody(site, ev)
	if err != nil {
		return err
	}
	subject := "cios.evt." + site + ".alarm"
	return nc.Publish(subject, body)
}

// buildCEBody assembles the CloudEvents JSON for one event. Pulled
// out of publishCE so unit tests can verify the envelope shape —
// and, in particular, that the `time` field reflects the
// transition instant (ev.OccurredAt) and not the first-satisfied
// moment (ev.Since), per R2. Returns the marshaled bytes; the id
// is freshly generated per call.
func buildCEBody(site string, ev alarm.Event) ([]byte, error) {
	var etype string
	switch ev.State {
	case alarm.StateFiring:
		etype = "io.cios.alarm.firing"
	case alarm.StateResolved:
		etype = "io.cios.alarm.resolved"
	default:
		// ack is the only other state; not produced by this engine.
		return nil, fmt.Errorf("publishCE: unexpected state %q", ev.State)
	}
	envelope := map[string]interface{}{
		"specversion":     "1.0",
		"id":              uuidv7(),
		"source":          "cios://" + site + "/cios-alarm",
		"type":            etype,
		"subject":         ev.PointPath,
		"time":            ev.OccurredAt.UTC().Format(time.RFC3339),
		"datacontenttype": "application/json",
		"severity":        ev.Severity,
		"site":            site,
		"data": ceData{
			Rule:    ev.RuleName,
			Summary: ev.Summary,
			State:   string(ev.State),
		},
	}
	return json.Marshal(envelope)
}
