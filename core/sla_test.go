// SLA scanner tests (PRMT-036). Covers:
//   - slaWindows default table (spec-008 §3)
//   - isSLABreach for every severity / state / time combination
//   - scanSLA filtering: only open/acknowledged, only hasSLA,
//     only un-escalated, only breached → in the result
//   - idem potent: a ticket with EscalatedAt!=nil is filtered
//   - info severity: never a breach
//   - resolved/closed tickets: never a breach
//   - end-to-end through Server.scanSLATick: open a ticket,
//     rewind OpenedAt, drive one tick, assert the ticket comes
//     back with EscalatedAt set + the escalated webhook fired
//     (via the same httptest receiver that PRMT-035's tests use)
package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- slaWindows ----------------------------------------------------------

func TestSLAWindows_Table(t *testing.T) {
	cases := []struct {
		sev         string
		wantResp    time.Duration
		wantResolve time.Duration
		wantSLA     bool
	}{
		{"critical", 15 * time.Minute, 4 * time.Hour, true},
		{"major", 1 * time.Hour, 24 * time.Hour, true},
		{"minor", 8 * time.Hour, 72 * time.Hour, true},
		{"info", 0, 0, false},
		{"bogus", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.sev, func(t *testing.T) {
			resp, resolve, has := slaWindows(tc.sev)
			if has != tc.wantSLA {
				t.Errorf("hasSLA = %v, want %v", has, tc.wantSLA)
			}
			if has && (resp != tc.wantResp || resolve != tc.wantResolve) {
				t.Errorf("windows = (%v, %v), want (%v, %v)", resp, resolve, tc.wantResp, tc.wantResolve)
			}
		})
	}
}

// --- isSLABreach --------------------------------------------------------

// freshTicket builds a minimal ticket for breach tests. The
// caller decides state / openedAt / severity.
func freshTicket(state, sev string, openedAt time.Time) Ticket {
	return Ticket{
		ID:        "tk_AAAAAAAAAAAAAAAA",
		AssetPath: "site01.pod000.cdu000",
		Title:     "x",
		Severity:  sev,
		State:     state,
		OpenedAt:  openedAt,
	}
}

func TestIsSLABreach_OpenResponse(t *testing.T) {
	// critical response = 15m. Opened 20m ago, still open → breach.
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tk := freshTicket("open", "critical", t0)
	if !isSLABreach(tk, t0.Add(20*time.Minute)) {
		t.Errorf("open critical 20m: want breach")
	}
	// Opened 10m ago, still open → no breach.
	if isSLABreach(tk, t0.Add(10*time.Minute)) {
		t.Errorf("open critical 10m: want no breach")
	}
	// Exactly at 15m → no breach (strict greater-than).
	if isSLABreach(tk, t0.Add(15*time.Minute)) {
		t.Errorf("open critical 15m (boundary): want no breach (strict >)")
	}
}

func TestIsSLABreach_AcknowledgedResolve(t *testing.T) {
	// critical resolve = 4h. Opened 5h ago, acknowledged → breach
	// (per spec-008 §3, resolution breach is measured from opened_at,
	// not acked_at).
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tk := freshTicket("acknowledged", "critical", t0)
	if !isSLABreach(tk, t0.Add(5*time.Hour)) {
		t.Errorf("acknowledged critical 5h: want breach")
	}
	if isSLABreach(tk, t0.Add(3*time.Hour)) {
		t.Errorf("acknowledged critical 3h: want no breach")
	}
}

func TestIsSLABreach_TerminalNeverBreach(t *testing.T) {
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	for _, st := range []string{"resolved", "closed"} {
		tk := freshTicket(st, "critical", t0)
		if isSLABreach(tk, t0.Add(100*time.Hour)) {
			t.Errorf("%s critical 100h: must not breach (terminal state)", st)
		}
	}
}

func TestIsSLABreach_InfoNeverBreach(t *testing.T) {
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	for _, st := range []string{"open", "acknowledged"} {
		tk := freshTicket(st, "info", t0)
		if isSLABreach(tk, t0.Add(1000*time.Hour)) {
			t.Errorf("%s info 1000h: must not breach (info has no SLA)", st)
		}
	}
}

func TestIsSLABreach_MajorMinorWindows(t *testing.T) {
	// major: 1h response / 24h resolve; minor: 8h response / 72h resolve.
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// major open at 30m → no breach (response is 1h)
	if isSLABreach(freshTicket("open", "major", t0), t0.Add(30*time.Minute)) {
		t.Errorf("major open 30m: want no breach")
	}
	// major open at 2h → breach
	if !isSLABreach(freshTicket("open", "major", t0), t0.Add(2*time.Hour)) {
		t.Errorf("major open 2h: want breach")
	}
	// major acknowledged at 23h → no breach
	if isSLABreach(freshTicket("acknowledged", "major", t0), t0.Add(23*time.Hour)) {
		t.Errorf("major ack 23h: want no breach")
	}
	// major acknowledged at 25h → breach
	if !isSLABreach(freshTicket("acknowledged", "major", t0), t0.Add(25*time.Hour)) {
		t.Errorf("major ack 25h: want breach")
	}
	// minor open at 4h → no breach (response is 8h)
	if isSLABreach(freshTicket("open", "minor", t0), t0.Add(4*time.Hour)) {
		t.Errorf("minor open 4h: want no breach")
	}
	// minor open at 9h → breach
	if !isSLABreach(freshTicket("open", "minor", t0), t0.Add(9*time.Hour)) {
		t.Errorf("minor open 9h: want breach")
	}
	// minor acknowledged at 70h → no breach
	if isSLABreach(freshTicket("acknowledged", "minor", t0), t0.Add(70*time.Hour)) {
		t.Errorf("minor ack 70h: want no breach")
	}
	// minor acknowledged at 73h → breach
	if !isSLABreach(freshTicket("acknowledged", "minor", t0), t0.Add(73*time.Hour)) {
		t.Errorf("minor ack 73h: want breach")
	}
}

// --- scanSLA: filter logic ----------------------------------------------

func TestScanSLA_OnlyBreached(t *testing.T) {
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tickets := []Ticket{
		freshTicket("open", "critical", t0),                          // 0m, no breach
		freshTicket("open", "critical", t0.Add(-30*time.Minute)),     // 30m → breach
		freshTicket("open", "info", t0.Add(-100*time.Hour)),          // info, no breach
		freshTicket("resolved", "critical", t0.Add(-30*time.Minute)), // terminal
	}
	got := scanSLA(t0, tickets)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].State != "open" || got[0].Severity != "critical" {
		t.Errorf("got %+v, want open critical 30m", got[0])
	}
}

func TestScanSLA_EscalatedFilteredOut(t *testing.T) {
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// A ticket that was escalated 1h ago: still in breach, but
	// the scanner must NOT re-escalate it. scanSLA filters
	// EscalatedAt!=nil before the breach check.
	tk := freshTicket("open", "critical", t0.Add(-30*time.Minute))
	escTime := t0.Add(-1 * time.Hour)
	tk.EscalatedAt = &escTime
	got := scanSLA(t0, []Ticket{tk})
	if len(got) != 0 {
		t.Errorf("escalated ticket re-appeared: got %d, want 0", len(got))
	}
}

func TestScanSLA_InfoExempted(t *testing.T) {
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tk := freshTicket("open", "info", t0.Add(-1*time.Hour))
	got := scanSLA(t0, []Ticket{tk})
	if len(got) != 0 {
		t.Errorf("info open breached → 0 expected, got %d", len(got))
	}
}

// --- end-to-end through Server.scanSLATick ------------------------------

// slaTestEnv bundles a Server, a webhook receiver, and a mutex
// for the receiver's captured events. The receiver mirrors the
// shape of PRMT-035's TestEmitTicketEvent_POSTsAndPersistsEnvelope.
type slaTestEnv struct {
	srv      *Server
	receiver *httptest.Server
	mu       *sync.Mutex
	events   *[]ceEnvelope
}

// newSLATestServer wires a Server with a webhook receiver. The
// receiver captures every POSTed envelope so the test can assert
// that the scanner fired the escalated event.
func newSLATestServer(t *testing.T) *slaTestEnv {
	t.Helper()
	mu := &sync.Mutex{}
	events := &[]ceEnvelope{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var env ceEnvelope
		_ = json.Unmarshal(raw, &env)
		mu.Lock()
		*events = append(*events, env)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)

	root := moduleRoot(t)
	st, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	dict, err := loadDictForTest(root)
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	srv := NewServer(st, dict, "http://127.0.0.1:1")
	srv.SetTicketWebhookURL(receiver.URL, receiver.Client())
	return &slaTestEnv{srv: srv, receiver: receiver, mu: mu, events: events}
}

func TestScanSLATick_EscalatesBreached(t *testing.T) {
	env := newSLATestServer(t)
	// Seed two tickets: one that has breached response (open for
	// 30m as critical) and one that has not (open for 5m as
	// critical). Both go through the same PutTicket so the file
	// store JSON round-trips EscalatedAt correctly.
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	breach := Ticket{
		ID:        "tk_AAAAAAAAAAAAAAAA",
		AssetPath: "site01.pod000.cdu000",
		Title:     "leak",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-30 * time.Minute),
	}
	fresh := Ticket{
		ID:        "tk_BBBBBBBBBBBBBBBB",
		AssetPath: "site01.pod000.cdu001",
		Title:     "warm",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-5 * time.Minute),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), breach, 0); err != nil {
		t.Fatalf("put breach: %v", err)
	}
	if _, err := env.srv.st.PutTicket(context.Background(), fresh, 0); err != nil {
		t.Fatalf("put fresh: %v", err)
	}
	// Drive one tick at t0.
	env.srv.scanSLATick(context.Background(), t0)

	// 1. breach ticket: EscalatedAt must be set to <=t0.
	got, ok, err := env.srv.st.GetTicket(context.Background(), breach.ID)
	if err != nil || !ok {
		t.Fatalf("get breach: ok=%v err=%v", ok, err)
	}
	if got.EscalatedAt == nil {
		t.Errorf("breach ticket EscalatedAt still nil after tick")
	} else if got.EscalatedAt.After(t0) {
		t.Errorf("EscalatedAt = %v, want <= %v", got.EscalatedAt, t0)
	}
	// 2. fresh ticket: EscalatedAt must still be nil.
	got, _, _ = env.srv.st.GetTicket(context.Background(), fresh.ID)
	if got.EscalatedAt != nil {
		t.Errorf("fresh ticket EscalatedAt = %v, want nil", got.EscalatedAt)
	}
	// 3. webhook receiver must have seen exactly one escalated
	//    event (for the breach ticket; the fresh ticket fires
	//    nothing; no other source emits).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		env.mu.Lock()
		n := len(*env.events)
		env.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(*env.events) != 1 {
		t.Fatalf("events = %d, want 1 (only the breach ticket escalates)", len(*env.events))
	}
	e := (*env.events)[0]
	if e.Type != "io.cios.ticket.escalated" {
		t.Errorf("event type = %q, want io.cios.ticket.escalated", e.Type)
	}
	if e.Data.TicketID != breach.ID {
		t.Errorf("event ticket_id = %q, want %q", e.Data.TicketID, breach.ID)
	}
}

func TestScanSLATick_IdempotentNoResend(t *testing.T) {
	// Drive the same tick twice on a breach. Only one
	// io.cios.ticket.escalated event must be sent.
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	breach := Ticket{
		ID:        "tk_AAAAAAAAAAAAAAAA",
		AssetPath: "site01.pod000.cdu000",
		Title:     "leak",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-30 * time.Minute),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), breach, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	env.srv.scanSLATick(context.Background(), t0)
	env.srv.scanSLATick(context.Background(), t0.Add(1*time.Minute))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		env.mu.Lock()
		n := len(*env.events)
		env.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Wait a bit more in case a stray second event is in flight.
	time.Sleep(100 * time.Millisecond)
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(*env.events) != 1 {
		t.Errorf("events = %d, want 1 (idempotent: re-tick must not re-fire)", len(*env.events))
	}
}

func TestScanSLATick_InfoExempted(t *testing.T) {
	// An info ticket open for 100h must NOT escalate, even
	// though "100h > any response window" would naively suggest
	// a breach. info has no SLA per spec-008 §3.
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tk := Ticket{
		ID:        "tk_INFOTICKET00000",
		AssetPath: "site01.pod000.cdu000",
		Title:     "noise",
		Severity:  "info",
		State:     "open",
		OpenedAt:  t0.Add(-100 * time.Hour),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), tk, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	env.srv.scanSLATick(context.Background(), t0)

	got, _, _ := env.srv.st.GetTicket(context.Background(), tk.ID)
	if got.EscalatedAt != nil {
		t.Errorf("info ticket escalated: %v", got.EscalatedAt)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(*env.events) != 0 {
		t.Errorf("info ticket fired %d events, want 0", len(*env.events))
	}
}

func TestScanSLATick_ResolvedNeverEscalates(t *testing.T) {
	// A resolved critical ticket is terminal; even if it was
	// opened > resolve-window ago, the scanner must skip it.
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	resolvedAt := t0.Add(-1 * time.Hour)
	tk := Ticket{
		ID:         "tk_RESOLVED0000000",
		AssetPath:  "site01.pod000.cdu000",
		Title:      "fixed",
		Severity:   "critical",
		State:      "resolved",
		OpenedAt:   t0.Add(-10 * time.Hour),
		ResolvedAt: &resolvedAt,
	}
	if _, err := env.srv.st.PutTicket(context.Background(), tk, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	env.srv.scanSLATick(context.Background(), t0)

	got, _, _ := env.srv.st.GetTicket(context.Background(), tk.ID)
	if got.EscalatedAt != nil {
		t.Errorf("resolved ticket escalated: %v", got.EscalatedAt)
	}
}

func TestScanSLATick_FailSoftOnStoreError(t *testing.T) {
	// Wrap a Store that errors on ListTickets. scanSLATick must
	// log and return (not panic, not propagate).
	srv, ts := newTestServer(t)
	defer ts.Close()
	// Replace the store with a stub that always errors on
	// ListTickets. We do this by wrapping the real store in a
	// tiny shim that lives only for this test.
	bad := &failingListStore{Store: srv.st}
	srv.st = bad
	// No panic, no nil dereference; the tick returns and the
	// test reaches its end. (We don't assert on the log line
	// because log.Printf is captured to stderr only.)
	srv.scanSLATick(context.Background(), time.Now().UTC())
}

// failingListStore embeds a real Store and forces ListTickets (and a
// handful of tenant/org/role-binding mutators, kept below) to fail
// with errFailingListStore. Every other Store method is inherited
// from the embedded Store, so interface growth no longer breaks this
// stub (PRMT-236 root-cause fix).
type failingListStore struct {
	Store
}

func (f *failingListStore) ListTickets(_ context.Context) ([]Ticket, error) {
	return nil, errFailingListStore
}
func (f *failingListStore) CreateTenant(ctx context.Context, id, displayName, principal string) (Tenant, Org, error) {
	return Tenant{}, Org{}, errFailingListStore
}
func (f *failingListStore) DeleteTenant(ctx context.Context, id, principal string) error {
	return errFailingListStore
}
func (f *failingListStore) CreateOrg(ctx context.Context, tenantID, name, principal string) (Org, error) {
	return Org{}, errFailingListStore
}
func (f *failingListStore) RenameOrg(ctx context.Context, id, newName, principal string) error {
	return errFailingListStore
}
func (f *failingListStore) DeleteOrg(ctx context.Context, id, principal string) error {
	return errFailingListStore
}

// RoleBinding (PRMT-190-bis §4.2): the SLA scanner does not
// exercise the role-binding surface, but the Store interface
// gained these 3 methods. Forwarding the mutators to the same
// sentinel keeps this stub a drop-in for the SLA suite (the
// scanner's role-binding path, if ever reached in this test,
// would short-circuit on the sentinel the same way ListTickets
// already does — matching the 189 widening precedent).
func (f *failingListStore) PutRoleBinding(ctx context.Context, rb RoleBinding) error {
	return errFailingListStore
}
func (f *failingListStore) ListRoleBindings(_ context.Context, _ string) ([]RoleBinding, error) {
	return nil, errFailingListStore
}
func (f *failingListStore) ListAllRoleBindings(_ context.Context) ([]RoleBinding, error) {
	return nil, errFailingListStore
}
func (f *failingListStore) DeleteRoleBinding(_ context.Context, _, _ string) error {
	return errFailingListStore
}

type failingListStoreError struct{ msg string }

func (e failingListStoreError) Error() string { return e.msg }

var errFailingListStore = failingListStoreError{"sla_test: forced list error"}

// --- Leader-election gate (PRMT-065) -------------------------------------

// TestTryScannerLock_FileStoreAlwaysLeader locks in the fileStore
// half of the contract: single-instance semantics ⇒ always
// acquired=true with a no-op release. Mirrors what the production
// pgStore.TryScannerLock guarantees under "no other instance is
// running".
func TestTryScannerLock_FileStoreAlwaysLeader(t *testing.T) {
	root := moduleRoot(t)
	st, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	dict, err := loadDictForTest(root)
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	srv := NewServer(st, dict, "http://127.0.0.1:1")

	for _, name := range []string{"sla", "pm", "inspection", "spare", "reconcile", "report"} {
		ok, release, err := srv.st.TryScannerLock(context.Background(), name)
		if err != nil {
			t.Errorf("%s: TryScannerLock err = %v, want nil", name, err)
			continue
		}
		if !ok {
			t.Errorf("%s: TryScannerLock ok = false, want true (fileStore always-leader)", name)
			continue
		}
		if release == nil {
			t.Errorf("%s: TryScannerLock release = nil, want non-nil no-op", name)
			continue
		}
		// release must be safe to call (no-op) — invoking it
		// multiple times must not panic. We don't assert side
		// effects because there are none for fileStore.
		release()
		release()
	}
}

// scannerLockStub wraps a real Store and overrides TryScannerLock
// to a configurable outcome. Used to drive the leader-gate
// negative path (acquired=false → tick must no-op) without a
// real PostgreSQL dependency.
type scannerLockStub struct {
	Store
	// acquire is called by TryScannerLock. Returning (false, nil)
	// simulates "another instance leads"; returning (true, nil)
	// simulates "this instance leads".
	acquire func(name string) (bool, error)
	// releases counts invocations of the returned release closure
	// so the test can assert the deferred release actually fires
	// on the acquired path.
	releases *int
}

func (s *scannerLockStub) TryScannerLock(_ context.Context, name string) (bool, func(), error) {
	ok, err := s.acquire(name)
	if err != nil {
		return false, func() {}, err
	}
	if !ok {
		// Caller will not defer release; keep it a no-op so a
		// stray defer cannot mis-handle a non-acquired lock.
		return false, func() {}, nil
	}
	return true, func() { *s.releases++ }, nil
}

// TestScanSLATick_LeaderGateSkipsOnNotAcquired is the negative-
// path coverage for PRMT-065 §4: when another instance holds the
// "sla" advisory lock, this instance's scanSLATick must silently
// skip — no escalation, no webhook, no state mutation.
func TestScanSLATick_LeaderGateSkipsOnNotAcquired(t *testing.T) {
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	breach := Ticket{
		ID:        "tk_LEADERGATE00000",
		AssetPath: "site01.pod000.cdu000",
		Title:     "leak",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-30 * time.Minute),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), breach, 0); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Wrap the real store so TryScannerLock("sla") returns
	// (false, nil) — simulating "another instance leads". All
	// other Store methods pass through to the real fileStore so
	// the assertion below is meaningful: if the tick skipped,
	// the breach ticket's EscalatedAt stays nil and no webhook
	// event is captured.
	releases := 0
	env.srv.st = &scannerLockStub{
		Store: env.srv.st,
		acquire: func(name string) (bool, error) {
			if name != "sla" {
				t.Errorf("TryScannerLock called with %q, want %q", name, "sla")
			}
			return false, nil
		},
		releases: &releases,
	}

	env.srv.scanSLATick(context.Background(), t0)

	got, _, _ := env.srv.st.GetTicket(context.Background(), breach.ID)
	if got.EscalatedAt != nil {
		t.Errorf("breach ticket EscalatedAt = %v, want nil (lock not acquired, tick must skip)", got.EscalatedAt)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(*env.events) != 0 {
		t.Errorf("events = %d, want 0 (lock not acquired, tick must skip)", len(*env.events))
	}
	if releases != 0 {
		t.Errorf("release called %d times on a not-acquired lock, want 0", releases)
	}
}

// TestScanSLATick_LeaderGateReleasesOnAcquired covers the
// positive path: when this instance acquires the "sla" lock, the
// tick runs normally AND the deferred release closure fires once
// the tick completes.
func TestScanSLATick_LeaderGateReleasesOnAcquired(t *testing.T) {
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	breach := Ticket{
		ID:        "tk_LEADERGATE00001",
		AssetPath: "site01.pod000.cdu000",
		Title:     "leak",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-30 * time.Minute),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), breach, 0); err != nil {
		t.Fatalf("put: %v", err)
	}

	releases := 0
	env.srv.st = &scannerLockStub{
		Store: env.srv.st,
		acquire: func(name string) (bool, error) {
			return true, nil
		},
		releases: &releases,
	}

	env.srv.scanSLATick(context.Background(), t0)

	got, _, _ := env.srv.st.GetTicket(context.Background(), breach.ID)
	if got.EscalatedAt == nil {
		t.Errorf("breach ticket EscalatedAt = nil, want set (lock acquired, tick must escalate)")
	}
	if releases != 1 {
		t.Errorf("release called %d times, want 1 (defer release must fire after tick)", releases)
	}
}

// TestScanSLATick_LeaderGateLogAndSkipOnLockError covers the
// fail-soft contract for TryScannerLock error: a real store
// failure logs and returns without crashing. No assertion on the
// log line (stderr capture is unreliable across platforms); we
// only check that the tick returns cleanly without panic and
// without mutating the ticket.
func TestScanSLATick_LeaderGateLogAndSkipOnLockError(t *testing.T) {
	env := newSLATestServer(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	breach := Ticket{
		ID:        "tk_LEADERGATE00002",
		AssetPath: "site01.pod000.cdu000",
		Title:     "leak",
		Severity:  "critical",
		State:     "open",
		OpenedAt:  t0.Add(-30 * time.Minute),
	}
	if _, err := env.srv.st.PutTicket(context.Background(), breach, 0); err != nil {
		t.Fatalf("put: %v", err)
	}

	env.srv.st = &scannerLockStub{
		Store: env.srv.st,
		acquire: func(name string) (bool, error) {
			return false, errFailingListStore
		},
		releases: new(int),
	}

	// Must not panic, must not mutate the ticket.
	env.srv.scanSLATick(context.Background(), t0)
	got, _, _ := env.srv.st.GetTicket(context.Background(), breach.ID)
	if got.EscalatedAt != nil {
		t.Errorf("breach ticket EscalatedAt = %v, want nil (lock error, tick must skip)", got.EscalatedAt)
	}
}
