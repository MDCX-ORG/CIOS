// Package core — notify_email.go: ticket lifecycle → SMTP email channel
// (P783 / L105; second channel after webhook PRMT-200).
//
// Contract mirrors webhook fail-soft:
//   - empty Host or empty To → no-op
//   - send failure only logs; never blocks / rewinds ticket response
//   - no retry queue (M3/M4 MVP)
//   - fixed recipient list from config (subscription model deferred)
//
// Template is plain-text only (no HTML). Subject + body derive from
// the same ticket fields carried in the CloudEvents data payload.
package core

import (
	"fmt"
	"log"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// TicketSMTPConfig is the ops-facing email channel config (P783).
// Host + From + ≥1 To required to enable; User/Pass optional PLAIN auth.
type TicketSMTPConfig struct {
	Host string
	Port int // 0 → 587
	From string
	To   []string
	User string
	Pass string
}

// smtpSendMail is the outbound send seam (stdlib by default; tests replace).
var smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	return smtp.SendMail(addr, a, from, to, msg)
}

// SetTicketSMTP configures the email notification channel. cfg==nil
// or Host empty or To empty disables. Invalid entries in To are dropped.
func (s *Server) SetTicketSMTP(cfg *TicketSMTPConfig) {
	if cfg == nil {
		s.ticketSMTP = nil
		return
	}
	host := strings.TrimSpace(cfg.Host)
	from := strings.TrimSpace(cfg.From)
	if host == "" || from == "" {
		s.ticketSMTP = nil
		return
	}
	port := cfg.Port
	if port <= 0 {
		port = 587
	}
	to := make([]string, 0, len(cfg.To))
	seen := map[string]struct{}{}
	for _, t := range cfg.To {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		to = append(to, t)
	}
	if len(to) == 0 {
		s.ticketSMTP = nil
		return
	}
	s.ticketSMTP = &TicketSMTPConfig{
		Host: host,
		Port: port,
		From: from,
		To:   to,
		User: strings.TrimSpace(cfg.User),
		Pass: cfg.Pass, // keep as-is (may be empty)
	}
}

// emitTicketEmail sends one plain-text email for a ticket lifecycle
// event. Fail-soft: all errors log only.
func (s *Server) emitTicketEmail(t Ticket, etype string) {
	cfg := s.ticketSMTP
	if cfg == nil {
		return
	}
	subject, body, err := buildTicketEmail(t, etype)
	if err != nil {
		log.Printf("core: ticket email build: %v", err)
		return
	}
	msg := formatRFC822(cfg.From, cfg.To, subject, body)
	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)
	var auth smtp.Auth
	if cfg.User != "" {
		auth = smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
	}
	if err := smtpSendMail(addr, auth, cfg.From, cfg.To, msg); err != nil {
		log.Printf("core: ticket email send: %v", err)
		return
	}
}

// emitTicketEmailAsync runs emitTicketEmail off the caller's path
// (scanners). Panic-isolated; does not share the webhook worker pool.
func (s *Server) emitTicketEmailAsync(t Ticket, etype string) {
	if s.ticketSMTP == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("core: ticket email async panic: %v", r)
			}
		}()
		s.emitTicketEmail(t, etype)
	}()
}

// buildTicketEmail produces subject + plain body. Unknown etype → error
// (same vocabulary as buildTicketEvent).
func buildTicketEmail(t Ticket, etype string) (subject, body string, err error) {
	if etype != ticketEventTypeOpened && etype != ticketEventTypeTransitioned && etype != ticketEventTypeEscalated {
		return "", "", unknownTicketEventTypeError{got: etype}
	}
	kind := strings.TrimPrefix(etype, "io.cios.ticket.")
	title := t.Title
	if title == "" {
		title = t.ID
	}
	subject = fmt.Sprintf("[CIOS] ticket %s: %s", kind, title)
	var b strings.Builder
	b.WriteString("CIOS ticket notification\n")
	b.WriteString("=======================\n")
	b.WriteString("event:     " + etype + "\n")
	b.WriteString("ticket_id: " + t.ID + "\n")
	b.WriteString("state:     " + t.State + "\n")
	b.WriteString("severity:  " + t.Severity + "\n")
	b.WriteString("title:     " + t.Title + "\n")
	if t.AlarmID != "" {
		b.WriteString("alarm_id:  " + t.AlarmID + "\n")
	}
	if t.AssetPath != "" {
		b.WriteString("asset:     " + t.AssetPath + "\n")
	}
	b.WriteString("time:      " + time.Now().UTC().Format(time.RFC3339) + "\n")
	return subject, b.String(), nil
}

// formatRFC822 builds a minimal text/plain message. Header values are
// stripped of CR/LF to avoid header injection from ticket fields.
func formatRFC822(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(strings.Join(to, ", ")) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
