// Package core — sla.go: ticket SLA timer + breach scanner
// (PRMT-036 / M2 E2.3 P504). Backed by spec-008 §3 (default
// windows by severity) and §5 (`io.cios.ticket.escalated` event
// type). The scanner runs as a background goroutine that ticks on
// a configurable interval; on every tick it lists every
// non-terminal, non-already-escalated ticket and fires the
// escalated event + sets `EscalatedAt` for each breached ticket
// (once per ticket, ever — see Idempotency below).
//
// Idempotency: an escalated ticket carries `EscalatedAt != nil`.
// The scanner filters on that field before considering the
// ticket, so re-ticks are no-ops. `EscalatedAt` is persisted via
// `Store.PutTicket` (the same upsert path used by ticket HTTP
// handlers), which is idempotent by ID. The webhook emission is
// also fail-soft (core/webhook.go:emitTicketEvent) and never
// rewinds the ticket response — but the scanner is not in a
// request path, so the only consequence of a webhook failure is
// a missing downstream notification, never data corruption.
//
// Concurrency: runSLAScanner is the single owner of the timer
// goroutine; the Store's internal mutex serialises ticket
// updates. Multiple cios-core instances would race on the
// escalated_at column (each would set its own now()), but
// spec-008 does not yet specify multi-instance leader election
// for this scanner — that is M3 (per-tenant SLA + leader
// election per L?).
package core

import (
	"context"
	"errors"
	"log"
	"time"
)

// SLA default windows (spec-008 §3, L69 Q3). The contract is
// "open without ack beyond resp" = response breach, and
// "acknowledged without resolve beyond resolve" = resolution
// breach. `info` has no SLA (best-effort) and is never
// escalated.
//
// These are package-level constants so the unit tests can assert
// the exact numbers (PRMT-036 §6 acceptance: 窗口 + 各 severity
// 超时/未超时 + 幂等 + info 豁免).
const (
	slaResponseCritical = 15 * time.Minute
	slaResponseMajor    = 1 * time.Hour
	slaResponseMinor    = 8 * time.Hour

	slaResolveCritical = 4 * time.Hour
	slaResolveMajor    = 24 * time.Hour
	slaResolveMinor    = 72 * time.Hour
)

// slaWindows returns the response and resolution timeouts for the
// given severity, plus a hasSLA flag. `info` returns hasSLA=false
// (best-effort, never escalated per spec-008 §3). Unknown
// severities also return hasSLA=false so a misconfigured ticket
// silently no-ops rather than escalating on a bogus rule.
//
// PRMT-036 §4: default values are constants; per-tenant overrides
// are M3 (the function signature leaves room for a future
// `override func(severity) (time.Duration, time.Duration, bool)`
// parameter without breaking the call sites).
func slaWindows(severity string) (resp, resolve time.Duration, hasSLA bool) {
	switch severity {
	case "critical":
		return slaResponseCritical, slaResolveCritical, true
	case "major":
		return slaResponseMajor, slaResolveMajor, true
	case "minor":
		return slaResponseMinor, slaResolveMinor, true
	case "info":
		return 0, 0, false
	}
	return 0, 0, false
}

// isSLABreach reports whether t has breached its SLA at the
// reference time `now`. The reference time is the scanner's tick
// time (callers pass it in so tests can pin the clock). The
// contract is "open without ack beyond resp" or "acknowledged
// without resolve beyond resolve" — terminal states (resolved /
// closed) are not breaches, and `info` is never a breach.
//
// hasSLA=false → never a breach. EscalatedAt!=nil → not a
// candidate (idempotency guard at the call site; this function
// is pure and does not read EscalatedAt so the unit tests can
// cover the timing logic without depending on the marker).
func isSLABreach(t Ticket, now time.Time) bool {
	if t.State == "resolved" || t.State == "closed" {
		return false
	}
	resp, resolve, hasSLA := slaWindows(t.Severity)
	if !hasSLA {
		return false
	}
	switch t.State {
	case "open":
		// spec-008 §3: response window is "time to acknowledged",
		// measured from opened_at. While the ticket is still open
		// and unacknowledged, the response clock keeps running.
		return now.Sub(t.OpenedAt) > resp
	case "acknowledged":
		// spec-008 §3: resolution window is "time to resolved",
		// measured from opened_at (NOT from acked_at — the §3
		// metric is "MTTR = resolved − opened" so the breach
		// reference is the same).
		return now.Sub(t.OpenedAt) > resolve
	}
	return false
}

// scanSLA walks the provided ticket slice and returns the
// subset that has breached its SLA at `now`. Pure function — no
// Store access, no side effects — so the unit tests cover the
// logic deterministically. The scanner (runSLAScanner) does the
// I/O; this function does the decision.
//
// Tickets with EscalatedAt!=nil are filtered out (idempotency):
// a ticket that was escalated on an earlier tick must not be
// returned again, even if it is still in a breach state. The
// PutTicket that set EscalatedAt is the persistence guarantee;
// this filter is the in-memory short-circuit.
func scanSLA(now time.Time, tickets []Ticket) []Ticket {
	var out []Ticket
	for _, t := range tickets {
		if t.EscalatedAt != nil {
			continue
		}
		if !isSLABreach(t, now) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// RunSLAScanner is the long-lived background goroutine. It
// ticks on `interval` (typically 60s, set by -sla-scan-interval)
// and processes any newly-breached tickets on each tick. The
// loop exits cleanly on `<-ctx.Done()` so cmd/cios-core can
// wire it to the same shutdown context as ListenAndServe.
//
// Failure handling: every per-ticket operation (Store.GetTicket,
// Store.PutTicket, emitTicketEvent) is wrapped in a fail-soft
// log. A single bad ticket must not stop the scanner — the
// next tick will retry. This is what "fail-soft" means in the
// prompt: errors are logged, never propagated, never fatal.
func (s *Server) RunSLAScanner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		// Belt-and-braces: a zero or negative interval would
		// busy-loop. -sla-scan-interval's default (60s) is
		// already positive, but a misconfiguration (e.g. via
		// a future config-file loader) must not melt the box.
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run one scan at startup so a freshly-restored cios-core
	// picks up any tickets that breached while it was down.
	// safeTick (PRMT-076) so a panic in scanSLATick can't kill
	// the long-lived goroutine.
	safeTick("sla", func() { s.scanSLATick(ctx, time.Now().UTC()) })
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			safeTick("sla", func() { s.scanSLATick(ctx, now.UTC()) })
		}
	}
}

// scanSLATick is one iteration of the scanner: list every
// ticket, run scanSLA, and for each breach re-load the ticket
// from the store (the in-memory list might be stale by the time
// we act on it) and escalate it. Pulled out of runSLAScanner so
// tests can drive a single tick deterministically without
// touching the ticker.
func (s *Server) scanSLATick(ctx context.Context, now time.Time) {
	// PRMT-066: record the tick outcome for /v1/health/scanners.
	// The local err is captured by the deferred closure so a
	// return at any depth — lock failure, leader skip, list
	// error, per-ticket error — still produces a registry entry.
	var tickErr error
	defer func() {
		s.recordScanner("sla", now, tickErr)
	}()
	// Multi-instance leader election (PRMT-065 / T43): at most
	// one cios-core instance may execute the SLA tick for this
	// tick window. The pg advisory lock is session-scoped and
	// released when the tick ends (release is deferred). On
	// error we log + skip (fail-soft, next tick will retry); on
	// !acquired we silently skip — another instance leads.
	ok, release, err := s.st.TryScannerLock(ctx, "sla")
	if err != nil {
		log.Printf("core: sla scanner: try lock: %v", err)
		tickErr = err
		return
	}
	if !ok {
		return
	}
	defer release()
	all, err := s.st.ListTickets(ctx)
	if err != nil {
		log.Printf("core: sla scanner: list tickets: %v", err)
		tickErr = err
		return
	}
	if err != nil {
		log.Printf("core: sla scanner: list tickets: %v", err)
		return
	}
	breached := scanSLA(now, all)
	for _, t := range breached {
		// Re-read by ID so we act on the current state, not a
		// snapshot from the bulk list. A ticket that was
		// acknowledged between list and this point is no
		// longer a candidate and PutTicket (which is a full
		// upsert) must not regress it.
		cur, ok, err := s.st.GetTicket(ctx, t.ID)
		if err != nil {
			log.Printf("core: sla scanner: get %s: %v", t.ID, err)
			continue
		}
		if !ok {
			// Vanished between list and get. Fail-soft: just
			// skip; the next tick will reconcile.
			continue
		}
		if cur.EscalatedAt != nil {
			continue
		}
		if !isSLABreach(cur, now) {
			continue
		}
		// Mark the breach time and persist BEFORE firing the
		// event. If PutTicket fails, we skip the event too —
		// the webhook is informational and a missing event is
		// better than a duplicate event on the next tick.
		// PRMT-082: optimistic CAS — use the version we just
		// read so a racing transition between get and put 409s
		// instead of silently overwriting the loser's state.
		nowCopy := now
		cur.EscalatedAt = &nowCopy
		if _, err := s.st.PutTicket(ctx, cur, cur.ResourceVersion); err != nil {
			if errors.Is(err, ErrVersionConflict) {
				// Lost the race to a concurrent transition
				// (operator acked the ticket while the
				// scanner was preparing to escalate). Skip
				// and let the next tick re-evaluate.
				log.Printf("core: sla scanner: put %s: version conflict, skipping", cur.ID)
				continue
			}
			log.Printf("core: sla scanner: put %s: %v", cur.ID, err)
			continue
		}
		// The webhook POST is fire-and-forget. emitTicketEvent
		// short-circuits when no URL is configured, so tests
		// that build a Server without SetTicketWebhookURL
		// observe a no-op here.
		s.emitTicketEventAsync(cur, ticketEventTypeEscalated)
	}
}
