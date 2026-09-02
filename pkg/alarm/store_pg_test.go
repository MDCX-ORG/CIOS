package alarm

// PRMT-099 R10b — PG-backed runtime tests for OpenTicket.
//
// The original PRMT-099 R10 implementation only had a source-string
// grep test (TestOpenTicket_SuppressionShape) which locks the SQL
// fragments but does NOT drive the OpenTicket state machine at
// runtime. The PRMT-099 §6 Review Record flagged this as R10.2:
// "三分支（existing / mw / inserted）及其优先级无任何 PG-backed 运行时
// 测试覆盖". This file adds that coverage.
//
// All tests are gated on CIOS_PG_DSN. The fixture applies ONLY
// the migration files OpenTicket's CTE actually touches — see
// withAlarmPG for the list. The list is the minimum required for
// clean-DB semantic equivalence: 002 creates tickets (without
// runbook), 005 adds the tickets.runbook column that the CTE
// INSERTs into, 014 creates maintenance_windows (the suppression
// probe target). Each test TRUNCATEs the two tables on entry so
// they don't pollute each other.
//
// Cases covered:
//   - TestOpenTicket_InsertsWhenNoExistingNoMw:  inserted (ticket row created)
//   - TestOpenTicket_SkipsWhenExisting:          existing (no log, no insert)
//   - TestOpenTicket_SuppressesWhenActiveMw:     mw (log fires, no insert)
//   - TestOpenTicket_ExistingWinsOverMw:         existing+mw → existing (no log)

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alarmPgTestEnv bundles the per-test transaction context and the
// shared *pgxpool.Pool. The transaction is opened on a short-lived
// conn from the pool and rolled back on t.Cleanup; subsequent
// OpenTicket calls go through Store.pool (the same pool), so the
// BEGIN/ROLLBACK session state is visible to them as long as the
// conn that issued BEGIN is the one currently checked out — which
// MaxConns=1 guarantees under serialised query traffic.
//
// We deliberately do NOT pin a conn via pool.Acquire here: pinning
// a conn would deadlock Store.pool.QueryRow (it would block on
// Pool.Acquire waiting for the pinned conn's release). The fix
// is to keep MaxConns=1 and let both the BEGIN/ROLLBACK and the
// production queries serialise through the same single conn —
// they're sequential within a test by construction.
type alarmPgTestEnv struct {
	Ctx  context.Context
	Pool *pgxpool.Pool
}

func alarmPgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		t.Skip("CIOS_PG_DSN not set — R10b.2 PG-backed runtime tests require a live PG (PRMT-099 §0)")
	}
	return dsn
}

func alarmMigrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// pkg/alarm is at <root>/pkg/alarm, migrations/ is at <root>/migrations
	return filepath.Join(wd, "..", "..", "migrations")
}

func withAlarmPG(t *testing.T) (*Store, *alarmPgTestEnv) {
	t.Helper()
	dsn := alarmPgDSN(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1 // serialise queries through a single conn
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Apply migrations (idempotent — IF NOT EXISTS in the SQL).
	// 005_ticket_runbook.sql is required: OpenTicket's CTE INSERT
	// (store.go:209) and the test seed INSERTs both write the
	// `runbook` column, which 005 adds via ALTER TABLE … ADD COLUMN
	// IF NOT EXISTS. Without 005, the very first insert on a clean
	// DB fails with `column "runbook" does not exist`. R10c.
	for _, f := range []string{"002_tickets.sql", "005_ticket_runbook.sql", "014_maintenance_windows.sql"} {
		raw, err := os.ReadFile(filepath.Join(alarmMigrationsDir(t), f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("migrate %s: %v", f, err)
		}
	}

	// Test isolation strategy: TRUNCATE the tables each test owns
	// so the fixture does not depend on ROLLBACK being wired
	// correctly across pgxpool's per-conn state-machine. The
	// transactional approach (BEGIN ... ROLLBACK) is fragile here
	// because pgxpool may reset the conn on release; using
	// TRUNCATE is idempotent and avoids inter-test bleed entirely.
	if _, err := pool.Exec(ctx,
		"TRUNCATE tickets RESTART IDENTITY CASCADE; TRUNCATE maintenance_windows CASCADE",
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// OpenTicket consumes Store.pool directly. Build the Store
	// against the same pool so OpenTicket's queries share the
	// pool (and therefore the table state).
	s := &Store{pool: pool}
	return s, &alarmPgTestEnv{Ctx: ctx, Pool: pool}
}

// captureLog runs fn while redirecting the default logger to a
// buffer, returning whatever was written. Used to assert that
// OpenTicket emits (or does NOT emit) the suppression log line.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

func ticketCount(t *testing.T, env *alarmPgTestEnv, alarmID string) int {
	t.Helper()
	var n int
	if err := env.Pool.QueryRow(env.Ctx,
		"SELECT count(*) FROM tickets WHERE alarm_id = $1 AND state <> 'closed'",
		alarmID,
	).Scan(&n); err != nil {
		t.Fatalf("count tickets: %v", err)
	}
	return n
}

// --- the four branch cases -------------------------------------------------

// TestOpenTicket_InsertsWhenNoExistingNoMw covers the canonical hot
// path: no pre-existing non-closed ticket, no active maintenance
// window, INSERT must run.
func TestOpenTicket_InsertsWhenNoExistingNoMw(t *testing.T) {
	s, env := withAlarmPG(t)
	ev := Event{
		RuleName:  "r10b-insert",
		AssetPath: "sgp01.pod002.cdu001",
		Severity:  "major",
		Summary:   "insert",
	}
	out := captureLog(t, func() {
		if err := s.OpenTicket(env.Ctx, ev); err != nil {
			t.Fatalf("OpenTicket: %v", err)
		}
	})
	if got := ticketCount(t, env, eventID(ev.RuleName, ev.AssetPath)); got != 1 {
		t.Fatalf("ticket count = %d, want 1", got)
	}
	if out != "" {
		t.Fatalf("expected no log, got %q", out)
	}
}

// TestOpenTicket_SkipsWhenExisting covers the idempotent path: a
// non-closed ticket for this alarm already exists; OpenTicket
// must return nil, must NOT insert another row, and must NOT
// emit the suppression log line.
func TestOpenTicket_SkipsWhenExisting(t *testing.T) {
	s, env := withAlarmPG(t)
	ev := Event{
		RuleName:  "r10b-existing",
		AssetPath: "sgp01.pod002.cdu002",
		Severity:  "major",
		Summary:   "existing",
	}
	alarmID := eventID(ev.RuleName, ev.AssetPath)
	// Seed an existing non-closed ticket for this alarm.
	if _, err := env.Pool.Exec(env.Ctx, `
		INSERT INTO tickets (id, alarm_id, asset_path, title, severity, state, opened_at, runbook)
		VALUES ('tk_R10BEXIST000000', $1, 'sgp01.pod002.cdu002', 'pre', 'major', 'open', now(), '')
	`, alarmID); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	if got := ticketCount(t, env, alarmID); got != 1 {
		t.Fatalf("seed: ticket count = %d, want 1", got)
	}

	out := captureLog(t, func() {
		if err := s.OpenTicket(env.Ctx, ev); err != nil {
			t.Fatalf("OpenTicket: %v", err)
		}
	})
	// No new row, no suppression log.
	if got := ticketCount(t, env, alarmID); got != 1 {
		t.Fatalf("after skip: ticket count = %d, want 1 (no new row)", got)
	}
	if out != "" {
		t.Fatalf("expected no log on existing-skip, got %q", out)
	}
}

// TestOpenTicket_SuppressesWhenActiveMw covers the PRMT-096
// suppression path: no existing ticket, but an active maintenance
// window covers the asset. OpenTicket must return nil, must NOT
// insert a row, and MUST emit the suppression log line naming
// the window id.
func TestOpenTicket_SuppressesWhenActiveMw(t *testing.T) {
	s, env := withAlarmPG(t)
	// Seed an active window covering the asset.
	if _, err := env.Pool.Exec(env.Ctx, `
		INSERT INTO maintenance_windows (id, asset_path, starts_at, ends_at, reason)
		VALUES ('mw_r10b_only', 'sgp01.pod002.cdu003', now() - interval '1 hour', now() + interval '1 hour', 'r10b')
	`); err != nil {
		t.Fatalf("seed mw: %v", err)
	}
	ev := Event{
		RuleName:  "r10b-mw",
		AssetPath: "sgp01.pod002.cdu003",
		Severity:  "major",
		Summary:   "mw",
	}
	alarmID := eventID(ev.RuleName, ev.AssetPath)

	out := captureLog(t, func() {
		if err := s.OpenTicket(env.Ctx, ev); err != nil {
			t.Fatalf("OpenTicket: %v", err)
		}
	})
	if got := ticketCount(t, env, alarmID); got != 0 {
		t.Fatalf("after suppress: ticket count = %d, want 0", got)
	}
	wantLog := "cios-alarm: suppressed auto-ticket for sgp01.pod002.cdu003 (maintenance window mw_r10b_only)"
	if !strings.Contains(out, wantLog) {
		t.Fatalf("suppression log mismatch:\n  got:  %q\n  want: %q (substring)", out, wantLog)
	}
}

// TestOpenTicket_ExistingWinsOverMw is the regression case from
// PRMT-099 R10 Review Record Finding R10.1: a non-closed ticket
// exists AND an active maintenance window covers the asset. The
// original 3-RTT code short-circuited on existing (silent skip,
// no log). The new CTE must preserve that priority — 'existing'
// wins, no suppression log is emitted, no new row is inserted.
// Without the outer ORDER BY pri LIMIT 1 the UNION ALL could
// surface the 'mw' row first and silently flip the log on.
func TestOpenTicket_ExistingWinsOverMw(t *testing.T) {
	s, env := withAlarmPG(t)
	ev := Event{
		RuleName:  "r10b-both",
		AssetPath: "sgp01.pod002.cdu004",
		Severity:  "major",
		Summary:   "both",
	}
	alarmID := eventID(ev.RuleName, ev.AssetPath)

	// Seed both: a pre-existing non-closed ticket AND an active mw.
	if _, err := env.Pool.Exec(env.Ctx, `
		INSERT INTO tickets (id, alarm_id, asset_path, title, severity, state, opened_at, runbook)
		VALUES ('tk_R10BBOTH0000000', $1, 'sgp01.pod002.cdu004', 'pre', 'major', 'open', now(), '')
	`, alarmID); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if _, err := env.Pool.Exec(env.Ctx, `
		INSERT INTO maintenance_windows (id, asset_path, starts_at, ends_at, reason)
		VALUES ('mw_r10b_both', 'sgp01.pod002.cdu004', now() - interval '1 hour', now() + interval '1 hour', 'r10b-both')
	`); err != nil {
		t.Fatalf("seed mw: %v", err)
	}

	out := captureLog(t, func() {
		if err := s.OpenTicket(env.Ctx, ev); err != nil {
			t.Fatalf("OpenTicket: %v", err)
		}
	})
	if got := ticketCount(t, env, alarmID); got != 1 {
		t.Fatalf("after both-hit: ticket count = %d, want 1 (no insert)", got)
	}
	if out != "" {
		t.Fatalf("expected NO suppression log when existing wins (R10.1), got %q", out)
	}
	// Belt-and-suspenders: verify the pre-existing ticket is
	// untouched (a row that mistakenly got UPDATED would not be
	// caught by the count alone).
	var title string
	if err := env.Pool.QueryRow(env.Ctx,
		"SELECT title FROM tickets WHERE id = 'tk_R10BBOTH0000000'",
	).Scan(&title); err != nil {
		t.Fatalf("re-read existing: %v", err)
	}
	if title != "pre" {
		t.Fatalf("existing ticket title = %q, want %q (must be untouched)", title, "pre")
	}
}
