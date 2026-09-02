package core

import (
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildTicketEmail_Shape(t *testing.T) {
	subj, body, err := buildTicketEmail(Ticket{
		ID:        "tk_test1",
		State:     "open",
		Severity:  "critical",
		Title:     "CDU flow low",
		AlarmID:   "al_1",
		AssetPath: "sgp01.pod001.cdu000",
	}, ticketEventTypeOpened)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subj, "opened") || !strings.Contains(subj, "CDU flow low") {
		t.Fatalf("subject=%q", subj)
	}
	for _, want := range []string{"tk_test1", "critical", "al_1", "sgp01.pod001.cdu000", ticketEventTypeOpened} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestBuildTicketEmail_UnknownType(t *testing.T) {
	_, _, err := buildTicketEmail(Ticket{ID: "x"}, "io.cios.ticket.bogus")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFormatRFC822_SanitizesHeaders(t *testing.T) {
	msg := string(formatRFC822("from@x", []string{"to@x"}, "sub\r\nX-Inject: 1", "body"))
	// CR/LF stripped so subject cannot open a new header line.
	if strings.Contains(msg, "Subject: sub\r\n") || strings.Contains(msg, "Subject: sub\n") {
		t.Fatalf("CRLF in subject survived: %q", msg)
	}
	if !strings.Contains(msg, "Subject: sub  X-Inject: 1") {
		t.Fatalf("expected flattened subject, got: %q", msg)
	}
	if !strings.Contains(msg, "Content-Type: text/plain") {
		t.Fatalf("missing content-type: %q", msg)
	}
}

func TestEmitTicketEmail_NoopWhenUnset(t *testing.T) {
	prev := smtpSendMail
	defer func() { smtpSendMail = prev }()
	called := 0
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		called++
		return nil
	}
	s, ts := newTestServer(t)
	defer ts.Close()
	s.emitTicketEmail(Ticket{ID: "tk"}, ticketEventTypeOpened)
	if called != 0 {
		t.Fatalf("send called %d times without config", called)
	}
}

func TestEmitTicketEmail_SendsWhenConfigured(t *testing.T) {
	prev := smtpSendMail
	defer func() { smtpSendMail = prev }()

	var (
		mu   sync.Mutex
		gotA string
		gotF string
		gotT []string
		gotM string
	)
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		gotA, gotF, gotT, gotM = addr, from, append([]string(nil), to...), string(msg)
		return nil
	}

	s, ts := newTestServer(t)
	defer ts.Close()
	s.SetTicketSMTP(&TicketSMTPConfig{
		Host: "smtp.example",
		Port: 2525,
		From: "cios@example.com",
		To:   []string{"ops@example.com", " ops@example.com ", "oncall@example.com"},
		User: "u",
		Pass: "p",
	})
	s.emitTicketEmail(Ticket{
		ID:       "tk_ab",
		State:    "open",
		Severity: "warning",
		Title:    "valve stuck",
	}, ticketEventTypeTransitioned)

	mu.Lock()
	defer mu.Unlock()
	if gotA != "smtp.example:2525" {
		t.Fatalf("addr=%q", gotA)
	}
	if gotF != "cios@example.com" {
		t.Fatalf("from=%q", gotF)
	}
	if len(gotT) != 2 || gotT[0] != "ops@example.com" || gotT[1] != "oncall@example.com" {
		t.Fatalf("to=%v", gotT)
	}
	if !strings.Contains(gotM, "tk_ab") || !strings.Contains(gotM, "transitioned") {
		t.Fatalf("msg=%q", gotM)
	}
}

func TestEmitTicketEmail_FailSoft(t *testing.T) {
	prev := smtpSendMail
	defer func() { smtpSendMail = prev }()
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		return errSMTPSimulated
	}
	s, ts := newTestServer(t)
	defer ts.Close()
	s.SetTicketSMTP(&TicketSMTPConfig{
		Host: "smtp.example",
		From: "a@b.c",
		To:   []string{"x@y.z"},
	})
	// Must not panic (void, fail-soft).
	s.emitTicketEmail(Ticket{ID: "tk", Title: "t"}, ticketEventTypeEscalated)
}

var errSMTPSimulated = errString("smtp down")

type errString string

func (e errString) Error() string { return string(e) }

func TestEmitTicketEvent_AlsoEmails(t *testing.T) {
	// Sync emit path used by ticket create/transition must hit SMTP
	// even with zero webhooks.
	prev := smtpSendMail
	defer func() { smtpSendMail = prev }()
	var n int
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		n++
		return nil
	}
	s, ts := newTestServer(t)
	defer ts.Close()
	s.SetTicketSMTP(&TicketSMTPConfig{
		Host: "h", From: "f@e", To: []string{"t@e"},
	})
	s.emitTicketEvent(Ticket{ID: "tk1", Title: "x", State: "open", Severity: "info"}, ticketEventTypeOpened)
	if n != 1 {
		t.Fatalf("email sends=%d want 1", n)
	}
}

func TestEmitTicketEventAsync_AlsoEmails(t *testing.T) {
	prev := smtpSendMail
	defer func() { smtpSendMail = prev }()
	done := make(chan struct{}, 1)
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		done <- struct{}{}
		return nil
	}
	s, ts := newTestServer(t)
	defer ts.Close()
	s.SetTicketSMTP(&TicketSMTPConfig{
		Host: "h", From: "f@e", To: []string{"t@e"},
	})
	s.emitTicketEventAsync(Ticket{ID: "tk2", Title: "y", State: "open", Severity: "info"}, ticketEventTypeOpened)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("async email not sent")
	}
}

func TestSetTicketSMTP_DisablesOnEmptyTo(t *testing.T) {
	s, ts := newTestServer(t)
	defer ts.Close()
	s.SetTicketSMTP(&TicketSMTPConfig{Host: "h", From: "f@e", To: []string{"  ", ""}})
	if s.ticketSMTP != nil {
		t.Fatal("expected disabled")
	}
}
