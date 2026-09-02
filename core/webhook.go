// Package core — webhook.go: ticket lifecycle events → outbound HTTP
// webhook (spec-008 §5 + spec-003 §1 CloudEvents 1.0 profile).
//
// PRMT-035 §1: cios-core has no NATS publisher (only cios-alarm
// does), so ticket notifications go out as a fire-and-forget HTTP
// POST to a single configured -ticket-webhook-url. Single URL,
// fail-soft (POST failure never blocks or rewinds the ticket
// response), no retry queue (M3). Signature/auth headers are out
// of scope (spec-008 v0.4).
//
// Event envelope (spec-003 §1.1, spec-008 §5):
//
//	id              UUIDv7 (envelope, NOT the ticket id)
//	specversion     "1.0"
//	source          "cios://<site>/cios-core"
//	type            io.cios.ticket.opened | io.cios.ticket.transitioned
//	subject         ticket.AssetPath
//	time            RFC3339 (UTC)
//	datacontenttype "application/json"
//	severity        ticket.Severity (CE extension, lower-case)
//	site            site extracted from ticket.AssetPath (CE extension)
//	data            {ticket_id, state, severity, title, alarm_id}
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ticketWebhookTimeout is the hard cap on a single outbound POST.
// The ticket handler has already returned (or is about to); this
// must NOT extend the request lifetime. 5s matches vmUpstreamTimeout
// (core/metrics.go).
const ticketWebhookTimeout = 5 * time.Second

// webhookWorkerCount / webhookQueueSize bound the async emit pool
// (PRMT-093, eval L5). The worker drains a buffered chan; a full
// chan → caller drops with a log line (never blocks the tick loop).
// 4 workers × 5s timeout ≈ 0.8 events/sec sustained ceiling, well
// past any realistic scanner cadence (SLA/PM/inspection/reconcile
// fire at minutes-to-hours intervals; burst from manual ticket
// creation is bounded by operator typing speed).
const (
	webhookWorkerCount = 4
	webhookQueueSize   = 256
)

// ticketEventJob is the unit of work on the async webhook pool.
// etype is copied, t is copied by-value (Ticket holds only strings,
// time.Time, and one *time.Time for EscalatedAt; we never mutate
// inside the worker, so the shallow copy is safe — see §2 contract
// "不捕获会被复用/修改的循环变量 (值拷贝)").
type ticketEventJob struct {
	url    string
	client *http.Client
	t      Ticket
	etype  string
}

// webhookPool is the package-level singleton driving async emit.
// Lazy-initialised on first async dispatch so tests that build a
// Server without ever calling SetTicketWebhookURL never spin
// workers. Close() drains at process exit; cmd/cios-core can call
// it from a defer but tests don't need to (process exit reclaims).
var (
	webhookPoolOnce sync.Once
	webhookPoolCh   chan ticketEventJob
	webhookPoolWG   sync.WaitGroup
	webhookPoolStop chan struct{}
)

// startWebhookPool spins up webhookWorkerCount worker goroutines
// that drain webhookPoolCh and call emitTicketEvent per job. Each
// worker has a defer recover() so a panic in one job can't kill the
// worker (or the other in-flight jobs on the same worker). Idempotent.
func startWebhookPool() {
	webhookPoolOnce.Do(func() {
		webhookPoolCh = make(chan ticketEventJob, webhookQueueSize)
		webhookPoolStop = make(chan struct{})
		for i := 0; i < webhookWorkerCount; i++ {
			webhookPoolWG.Add(1)
			go webhookWorker()
		}
	})
}

// webhookWorker is the inner loop: pop one job, run it under
// recover, repeat until the stop chan closes (only on Close()).
// The POST body is built and sent inline (no call to
// (*Server).emitTicketEvent) so the worker doesn't need a *Server
// receiver — Server's webhook state (url, client) is captured into
// the job at enqueue time (value-copy).
func webhookWorker() {
	defer webhookPoolWG.Done()
	for {
		select {
		case <-webhookPoolStop:
			return
		case job, ok := <-webhookPoolCh:
			if !ok {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("core: ticket webhook async panic: %v", r)
					}
				}()
				body, err := buildTicketEvent(job.t, job.etype)
				if err != nil {
					log.Printf("core: ticket webhook build: %v", err)
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), ticketWebhookTimeout)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.url, bytes.NewReader(body))
				if err != nil {
					log.Printf("core: ticket webhook request: %v", err)
					return
				}
				req.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
				resp, err := job.client.Do(req)
				if err != nil {
					log.Printf("core: ticket webhook post: %v", err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode/100 != 2 {
					log.Printf("core: ticket webhook status %d", resp.StatusCode)
					return
				}
			}()
		}
	}
}

// CloseTicketWebhookPool drains the in-flight async emit jobs and
// stops the workers. Intended for process shutdown; safe to call
// when the pool was never started (idempotent).
func CloseTicketWebhookPool() {
	if webhookPoolCh == nil {
		return
	}
	// Closing the channel signals workers to drain remaining
	// jobs and exit when empty.
	select {
	case <-webhookPoolStop:
		// already closed
	default:
		close(webhookPoolStop)
	}
	webhookPoolWG.Wait()
}

// ticketEventTypeOpened / Transitioned / Escalated are spec-008 §5
// type strings. Centralised as constants so a typo can't drift
// between the emitter and any future spec checker. Escalated is
// fired by the SLA scanner (PRMT-036) on first breach only.
const (
	ticketEventTypeOpened       = "io.cios.ticket.opened"
	ticketEventTypeTransitioned = "io.cios.ticket.transitioned"
	ticketEventTypeEscalated    = "io.cios.ticket.escalated"
)

// ceEnvelope is the CloudEvents 1.0 envelope we POST. Field tags
// match spec-003 §1.1; the order is irrelevant for parsing but is
// stable here for easier log diffing.
type ceEnvelope struct {
	SpecVersion     string       `json:"specversion"`
	ID              string       `json:"id"`
	Source          string       `json:"source"`
	Type            string       `json:"type"`
	Subject         string       `json:"subject"`
	Time            string       `json:"time"`
	DataContentType string       `json:"datacontenttype"`
	Severity        string       `json:"severity,omitempty"`
	Site            string       `json:"site,omitempty"`
	Data            ceTicketData `json:"data"`
}

// ceTicketData is the inner payload (spec-008 §5). Same shape for
// opened / transitioned; consumers switch on `type`.
type ceTicketData struct {
	TicketID string `json:"ticket_id"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	Title    string `json:"title,omitempty"`
	AlarmID  string `json:"alarm_id,omitempty"`
}

// uuidv7 is a minimal RFC 9562 UUIDv7 mint: 48-bit unix-ms in the
// high bits, 74 random bits in the tail, version nibble = 7,
// variant nibble = 0b10. Mirrors cmd/cios-alarm/main.go's uuidv7
// (PRMT-020 §5 "优先零新增" → no external uuid dep). Random bytes
// must be read BEFORE we OR in the version/variant bits; doing it
// the other way lets rand.Read clobber the fixed bits.
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
		// derived bytes (still unique per ms in practice).
		for i := 6; i < 16; i++ {
			b[i] = byte(now >> (uint(i-6) * 4))
		}
	}
	b[6] = 0x70 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

// siteOf extracts the site segment from an asset path. The full
// cpath.ParseAssetPath does more (type validation, parent checks),
// but for the envelope we only need a best-effort site label.
// Empty assetPath / parse failure → "" (PRMT-035 §4: source
// degrades to "cios://<empty>/cios-core" but the event still
// goes out, so the ticket is observable end-to-end).
func siteOf(assetPath string) string {
	if assetPath == "" {
		return ""
	}
	parts := strings.Split(assetPath, ".")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	return parts[0]
}

// buildTicketEvent assembles the CloudEvents envelope bytes for one
// ticket transition. Pulled out of emitTicketEvent so unit tests
// can assert the exact JSON shape (and that the id field is a
// freshly-minted UUIDv7, not the ticket id).
func buildTicketEvent(t Ticket, etype string) ([]byte, error) {
	if etype != ticketEventTypeOpened && etype != ticketEventTypeTransitioned && etype != ticketEventTypeEscalated {
		return nil, unknownTicketEventTypeError{got: etype}
	}
	site := siteOf(t.AssetPath)
	env := ceEnvelope{
		SpecVersion:     "1.0",
		ID:              uuidv7(),
		Source:          "cios://" + site + "/cios-core",
		Type:            etype,
		Subject:         t.AssetPath,
		Time:            time.Now().UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		Severity:        t.Severity,
		Site:            site,
		Data: ceTicketData{
			TicketID: t.ID,
			State:    t.State,
			Severity: t.Severity,
			Title:    t.Title,
			AlarmID:  t.AlarmID,
		},
	}
	return json.Marshal(env)
}

// unknownTicketEventTypeError is a typed error so buildTicketEvent's
// callers (and tests) can distinguish "we passed a bad type" from
// "marshal failed".
type unknownTicketEventTypeError struct{ got string }

func (e unknownTicketEventTypeError) Error() string {
	return "core: unknown ticket event type: " + e.got
}

// emitTicketEventAsync enqueues a ticket event for background
// dispatch via the bounded webhook pool. Contract (PRMT-093 §2):
//
//   - never blocks the tick loop. A full queue drops the job
//     with a log line; a tick that processes 100s of tickets
//     cannot be held up by a single hung endpoint.
//   - the worker recovers from panics, so a buggy upstream
//     library cannot kill the pool (mirrors PRMT-076).
//   - the Ticket is copied by value into the job struct so a
//     caller that mutates its local variable after the call
//     returns (e.g. a scanner that reuses `cur` on the next
//     iteration) cannot race the worker.
//
// Order of dispatch is NOT guaranteed (workers race for jobs).
// Acceptable per §2: events carry their own RFC3339 timestamp,
// so the receiver can reorder.
//
// Persistence stays synchronous (ticket Put/transition is still
// on the tick's hot path). Only the outbound webhook is async.
func (s *Server) emitTicketEventAsync(t Ticket, etype string) {
	urls := s.ticketWebhookURLs
	if len(urls) > 0 {
		startWebhookPool()
		// Fan-out (PRMT-200): one job per webhook channel. Fail-soft
		// per channel; queue full drops that channel only.
		for _, u := range urls {
			job := ticketEventJob{
				url:    u,
				client: s.httpClient,
				t:      t,
				etype:  etype,
			}
			select {
			case webhookPoolCh <- job:
			default:
				log.Printf("core: ticket webhook async queue full; dropping event %s for ticket %s url=%s", etype, t.ID, u)
			}
		}
	}
	// P783: email channel runs even when no webhooks are configured.
	s.emitTicketEmailAsync(t, etype)
}

// emitTicketEvent posts one CloudEvents envelope to every configured
// webhook URL (PRMT-035 + PRMT-200 fan-out) and optionally emails
// (P783 / L105). Contract:
//
//   - no URLs and no SMTP → no-op
//   - POST with 5s context timeout per URL
//   - On any error (build / post / non-2xx / SMTP): log.Printf, continue
//     other channels (fail-soft — never blocks ticket response)
func (s *Server) emitTicketEvent(t Ticket, etype string) {
	urls := s.ticketWebhookURLs
	if len(urls) > 0 {
		body, err := buildTicketEvent(t, etype)
		if err != nil {
			log.Printf("core: ticket webhook build: %v", err)
		} else {
			for _, u := range urls {
				s.postTicketWebhook(u, body)
			}
		}
	}
	// P783: email channel runs even when no webhooks are configured.
	s.emitTicketEmail(t, etype)
}

func (s *Server) postTicketWebhook(url string, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), ticketWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("core: ticket webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: ticketWebhookTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("core: ticket webhook post %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("core: ticket webhook status %d url=%s", resp.StatusCode, url)
		return
	}
}
