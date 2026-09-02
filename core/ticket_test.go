// Ticket data-layer tests (PRMT-032). The fileStore tests run
// unconditionally; the pgStore tests follow the same t.Skip
// convention as pg_store_test.go (CIOS_PG_DSN gates them). The
// pgStore tests call the package-private production helpers
// (putTicket / getTicket / listTickets) directly with a
// tx-pinned *pgxpool.Conn — same convention as PRMT-016b.
package core

import (
	"context"
	"os"
	"testing"
	"time"
)

// withPGTickets applies the full production migration set so
// putTicket columns (escalated_at, runbook, resource_version, …)
// exist. Historically only 001+002 ran and drifted from SQL helpers
// (P795 / CODE-SCAN).
func withPGTickets(t *testing.T) *pgTestEnv {
	t.Helper()
	return withPG(t)
}

// sampleTicket returns a deterministic ticket for tests.
func sampleTicket(id, assetPath, state string, opened time.Time) Ticket {
	return Ticket{
		ID:        id,
		AssetPath: assetPath,
		Title:     "t-" + id,
		Severity:  "major",
		State:     state,
		OpenedAt:  opened,
	}
}

// --- fileStore tests (always run) ---------------------------------------

func TestStore_PutTicketCreateAndOverwrite(t *testing.T) {
	st, _ := newStore(t)
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	if _, err := st.PutTicket(context.Background(), sampleTicket("T1", "site01.pod000.cdu000", "open", now), 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.GetTicket(context.Background(), "T1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.State != "open" || got.Title != "t-T1" {
		t.Errorf("got %+v", got)
	}
	// Overwrite with same ID → state updates.
	if _, err := st.PutTicket(context.Background(), sampleTicket("T1", "site01.pod000.cdu000", "acknowledged", now), 0); err != nil {
		t.Fatalf("put overwrite: %v", err)
	}
	got, _, _ = st.GetTicket(context.Background(), "T1")
	if got.State != "acknowledged" {
		t.Errorf("overwrite state = %q, want acknowledged", got.State)
	}
}

func TestStore_GetTicketNotFound(t *testing.T) {
	st, _ := newStore(t)
	_, ok, err := st.GetTicket(context.Background(), "nope")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
}

func TestStore_ListTicketsSortedByOpenedAtDesc(t *testing.T) {
	st, _ := newStore(t)
	t0 := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		id   string
		open time.Time
	}{
		{"OLD", t0},
		{"NEW", t0.Add(2 * time.Hour)},
		{"MID", t0.Add(1 * time.Hour)},
	} {
		if _, err := st.PutTicket(context.Background(), sampleTicket(c.id, "site01.pod000.cdu000", "open", c.open), 0); err != nil {
			t.Fatalf("put %s: %v", c.id, err)
		}
	}
	got, err := st.ListTickets(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].ID != "NEW" || got[1].ID != "MID" || got[2].ID != "OLD" {
		t.Errorf("order: %+v", got)
	}
}

func TestStore_ListTicketsEmptyIsNonNil(t *testing.T) {
	st, _ := newStore(t)
	got, err := st.ListTickets(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Errorf("empty list returned nil; want []Ticket{}")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestStore_TicketRoundTripOnDisk(t *testing.T) {
	st, p := newStore(t)
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	if _, err := st.PutTicket(context.Background(), sampleTicket("T1", "site01.pod000.cdu000", "open", now), 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Reload from disk in a separate handle.
	st2, err := NewFileStore(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok, err := st2.GetTicket(context.Background(), "T1")
	if err != nil || !ok {
		t.Fatalf("reload get: ok=%v err=%v", ok, err)
	}
	if got.ID != "T1" || got.AssetPath != "site01.pod000.cdu000" {
		t.Errorf("reloaded = %+v", got)
	}
}

// --- pgStore tests (CIOS_PG_DSN gated) ----------------------------------

func TestPG_PutTicketUpsertAndList(t *testing.T) {
	env := withPGTickets(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	if _, err := putTicket(env.Ctx, env.Conn, sampleTicket("PG1", "site01.pod000.cdu000", "open", t0), 0); err != nil {
		t.Fatalf("putTicket create: %v", err)
	}
	// Overwrite with new state.
	if _, err := putTicket(env.Ctx, env.Conn, sampleTicket("PG1", "site01.pod000.cdu000", "acknowledged", t0), 0); err != nil {
		t.Fatalf("putTicket update: %v", err)
	}
	got, ok, err := getTicket(env.Ctx, env.Conn, "PG1")
	if err != nil || !ok {
		t.Fatalf("getTicket: ok=%v err=%v", ok, err)
	}
	if got.State != "acknowledged" {
		t.Errorf("state = %q, want acknowledged", got.State)
	}
	if !got.OpenedAt.Equal(t0) {
		t.Errorf("opened_at = %v, want %v", got.OpenedAt, t0)
	}
	// Insert another, list should be OpenedAt desc.
	if _, err := putTicket(env.Ctx, env.Conn, sampleTicket("PG2", "site01.pod000.cdu000", "open", t0.Add(1*time.Hour)), 0); err != nil {
		t.Fatalf("putTicket PG2: %v", err)
	}
	all, err := listTickets(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("listTickets: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if all[0].ID != "PG2" || all[1].ID != "PG1" {
		t.Errorf("order: %+v", all)
	}
}

func TestPG_GetTicketNotFound(t *testing.T) {
	env := withPGTickets(t)
	_, ok, err := getTicket(env.Ctx, env.Conn, "missing")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
}

func TestPG_TicketNullableTimestamps(t *testing.T) {
	env := withPGTickets(t)
	t0 := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	ack := t0.Add(5 * time.Minute)
	if _, err := putTicket(env.Ctx, env.Conn, Ticket{
		ID:        "N1",
		AssetPath: "site01.pod000.cdu000",
		Severity:  "minor",
		State:     "acknowledged",
		OpenedAt:  t0,
		AckedAt:   &ack,
	}, 0); err != nil {
		t.Fatalf("putTicket: %v", err)
	}
	got, ok, err := getTicket(env.Ctx, env.Conn, "N1")
	if err != nil || !ok {
		t.Fatalf("getTicket: ok=%v err=%v", ok, err)
	}
	if got.AckedAt == nil || !got.AckedAt.Equal(ack) {
		t.Errorf("AckedAt = %v, want %v", got.AckedAt, ack)
	}
	if got.ResolvedAt != nil || got.ClosedAt != nil {
		t.Errorf("nullable timestamps not nil: %+v", got)
	}
}

// --- cross-implementation consistency ----------------------------------

// TestTicket_FileVsPG_OrderingConsistency seeds the same three
// tickets (with the same OpenedAt ordering) into both
// implementations and asserts the ListTickets ordering matches.
// Skips on the PG side when CIOS_PG_DSN is not set.
func TestTicket_FileVsPG_OrderingConsistency(t *testing.T) {
	fst, _ := newStore(t)
	t0 := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	cases := []Ticket{
		sampleTicket("A", "site01.pod000.cdu000", "open", t0),
		sampleTicket("B", "site01.pod000.cdu000", "open", t0.Add(1*time.Hour)),
		sampleTicket("C", "site01.pod000.cdu000", "open", t0.Add(2*time.Hour)),
	}
	for _, c := range cases {
		if _, err := fst.PutTicket(context.Background(), c, 0); err != nil {
			t.Fatalf("file put %s: %v", c.ID, err)
		}
	}
	fGot, err := fst.ListTickets(context.Background())
	if err != nil {
		t.Fatalf("file list: %v", err)
	}

	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		t.Log("CIOS_PG_DSN not set — skipping PG cross-check")
		return
	}
	env := withPGTickets(t)
	for _, c := range cases {
		if _, err := putTicket(env.Ctx, env.Conn, c, 0); err != nil {
			t.Fatalf("pg put %s: %v", c.ID, err)
		}
	}
	pGot, err := listTickets(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("pg list: %v", err)
	}
	if len(fGot) != len(pGot) {
		t.Fatalf("len mismatch: file=%d pg=%d", len(fGot), len(pGot))
	}
	for i := range fGot {
		if fGot[i].ID != pGot[i].ID {
			t.Errorf("[%d] file=%q pg=%q", i, fGot[i].ID, pGot[i].ID)
		}
	}
}
