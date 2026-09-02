// Integration tests for pgStore. They run only when the
// CIOS_PG_DSN environment variable is set; otherwise they call
// t.Skip so the suite is CI-friendly without a live database.
//
// Each test runs inside its own transaction (BEGIN; …; ROLLBACK)
// so the schema mutations from one test never leak into another.
// A pgxpool with MaxConns=1 is used so BEGIN/ROLLBACK stay on a
// single physical socket — the test cannot accidentally read
// writes from a sibling test that landed on a different conn.
//
// PRMT-016b R1: the tests no longer maintain a SQL duplicate of
// pgStore. They call the package-private production helpers
// (putAsset / getAsset / listAssets / deleteAsset / listAlarms /
// seedAlarms) directly with a tx-pinned *pgxpool.Conn as the
// querier — the same helpers production *pgStore forwards to.
// Any future SQL change in pg_store.go is therefore reflected in
// the tests automatically.
package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgDSN returns the DSN from CIOS_PG_DSN or skips the test.
// Per the prompt's MUST list, no DSN = t.Skip, never t.Fatal.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		t.Skip("CIOS_PG_DSN not set")
	}
	return dsn
}

// migrationsDir locates the migrations/ directory relative to
// the module root. Tests run from the core/ directory, so we
// walk up one level using the same moduleRoot helper that
// server_test.go exposes.
func migrationsDir(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	return filepath.Join(root, "migrations")
}

// pgTestEnv bundles the per-test transaction context and the
// pinned *pgxpool.Conn the production helpers will see as the
// querier. The transaction is rolled back by t.Cleanup.
type pgTestEnv struct {
	Ctx  context.Context
	Conn *pgxpool.Conn
}

// applyPGMigrations runs the named SQL files (relative to migrations/)
// on conn inside the caller's open transaction. files default to
// MigrationFiles when empty.
func applyPGMigrations(t *testing.T, ctx context.Context, conn *pgxpool.Conn, files []string) {
	t.Helper()
	if len(files) == 0 {
		files = MigrationFiles
	}
	dir := migrationsDir(t)
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("migrate %s: %v", f, err)
		}
	}
}

// openPGTx acquires a MaxConns=1 pool, BEGINs a transaction, and
// registers ROLLBACK + release cleanup. Caller applies migrations.
func openPGTx(t *testing.T) *pgTestEnv {
	t.Helper()
	dsn := pgDSN(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { conn.Release() })

	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "ROLLBACK")
	})
	return &pgTestEnv{Ctx: ctx, Conn: conn}
}

// withPG spins up a single-connection pool, begins a transaction,
// applies the full production migration set (MigrationFiles), and
// returns the test env. The conn is released and the transaction
// rolled back when t.Cleanup fires.
//
// P795 / CODE-SCAN: partial fixtures (001 only) drifted from
// production SQL helpers (spares, tickets columns). Full set is the
// parity contract.
//
// After migrate, TRUNCATE all application tables inside the test
// transaction so committed leftovers from NewPGStore-style tests
// (e.g. TestMigrateV11_PGParity) cannot pollute row counts / PKs.
// TRUNCATE is transactional — ROLLBACK restores shared-dev data.
func withPG(t *testing.T) *pgTestEnv {
	t.Helper()
	env := openPGTx(t)
	applyPGMigrations(t, env.Ctx, env.Conn, nil)
	// Order: children first is not required with CASCADE.
	if _, err := env.Conn.Exec(env.Ctx, `
		TRUNCATE TABLE
			usage_records,
			role_bindings,
			site_orgs,
			tenant_audit,
			orgs,
			tenants,
			maintenance_windows,
			ticket_audit,
			ticket_notes,
			inspection_templates,
			spare_txns,
			spare_parts,
			asset_audit,
			pm_schedules,
			tickets,
			alarms,
			assets
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate app tables: %v", err)
	}
	return env
}

// --- tests ---------------------------------------------------------------

func TestPG_PutAssetCreateAndUpdate(t *testing.T) {
	env := withPG(t)

	a := Asset{Path: "site01.pod000.cdu000", Spec: map[string]any{"type": "cdu"}}
	got, err := putAsset(env.Ctx, env.Conn, a, 0)
	if err != nil {
		t.Fatalf("putAsset create: %v", err)
	}
	if got.ResourceVersion != 1 {
		t.Errorf("create version = %d, want 1", got.ResourceVersion)
	}
	if got.Path != a.Path {
		t.Errorf("path = %q, want %q", got.Path, a.Path)
	}
	if got.Spec["type"] != "cdu" {
		t.Errorf("spec.type = %v, want cdu", got.Spec["type"])
	}

	a.Spec = map[string]any{"type": "cdu", "label": "A"}
	got2, err := putAsset(env.Ctx, env.Conn, a, 0)
	if err != nil {
		t.Fatalf("putAsset update: %v", err)
	}
	if got2.ResourceVersion != 2 {
		t.Errorf("update version = %d, want 2", got2.ResourceVersion)
	}
	if got2.Spec["label"] != "A" {
		t.Errorf("update spec.label = %v, want A", got2.Spec["label"])
	}
	if !got2.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("created_at changed: %v → %v", got.CreatedAt, got2.CreatedAt)
	}
	if got2.UpdatedAt.Before(got.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v → %v", got.UpdatedAt, got2.UpdatedAt)
	}
}

func TestPG_PutAssetVersionConflict(t *testing.T) {
	env := withPG(t)

	a := Asset{Path: "site01.pod000.cdu001", Spec: map[string]any{"type": "cdu"}}
	first, err := putAsset(env.Ctx, env.Conn, a, 0)
	if err != nil {
		t.Fatalf("putAsset create: %v", err)
	}

	got, err := putAsset(env.Ctx, env.Conn, a, 99)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	if got.ResourceVersion != first.ResourceVersion {
		t.Errorf("conflict surface version = %d, want %d", got.ResourceVersion, first.ResourceVersion)
	}
}

func TestPG_GetAssetFoundAndNotFound(t *testing.T) {
	env := withPG(t)

	if _, ok, err := getAsset(env.Ctx, env.Conn, "does.not.exist"); err != nil {
		t.Fatalf("getAsset missing: %v", err)
	} else if ok {
		t.Errorf("getAsset missing returned ok=true")
	}

	a := Asset{Path: "site01.pod001.cdu000", Spec: map[string]any{"type": "cdu"}}
	if _, err := putAsset(env.Ctx, env.Conn, a, 0); err != nil {
		t.Fatalf("putAsset: %v", err)
	}
	got, ok, err := getAsset(env.Ctx, env.Conn, "site01.pod001.cdu000")
	if err != nil {
		t.Fatalf("getAsset present: %v", err)
	}
	if !ok {
		t.Fatalf("getAsset present returned ok=false")
	}
	if got.Path != "site01.pod001.cdu000" {
		t.Errorf("path = %q", got.Path)
	}
}

func TestPG_ListAssetsOrder(t *testing.T) {
	env := withPG(t)
	inserts := []string{
		"site02.pod000.cdu000",
		"site01.pod001.cdu000",
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
	}
	for _, p := range inserts {
		if _, err := putAsset(env.Ctx, env.Conn, Asset{Path: p, Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
			t.Fatalf("putAsset %s: %v", p, err)
		}
	}
	got, err := listAssets(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("listAssets: %v", err)
	}
	if len(got) != len(inserts) {
		t.Fatalf("len = %d, want %d", len(got), len(inserts))
	}
	want := []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site01.pod001.cdu000",
		"site02.pod000.cdu000",
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i].Path, want[i])
		}
	}
}

func TestPG_DeleteAssetNoChildren(t *testing.T) {
	env := withPG(t)
	if _, err := putAsset(env.Ctx, env.Conn, Asset{Path: "site01.pod000.cdu099", Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
		t.Fatalf("putAsset: %v", err)
	}

	n, err := deleteAsset(env.Ctx, env.Conn, "site01.pod000.cdu099", false)
	if err != nil {
		t.Fatalf("deleteAsset: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	n, err = deleteAsset(env.Ctx, env.Conn, "site01.pod000.cdu099", false)
	if err != nil {
		t.Fatalf("deleteAsset second: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted (second) = %d, want 0", n)
	}
}

func TestPG_DeleteAssetChildrenNoCascade(t *testing.T) {
	env := withPG(t)
	for _, p := range []string{
		"site01.pod099.cdu000",
		"site01.pod099.cdu001",
	} {
		if _, err := putAsset(env.Ctx, env.Conn, Asset{Path: p, Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
			t.Fatalf("putAsset %s: %v", p, err)
		}
	}

	n, err := deleteAsset(env.Ctx, env.Conn, "site01.pod099", false)
	if !errors.Is(err, ErrHasChildren) {
		t.Fatalf("err = %v, want ErrHasChildren", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
}

func TestPG_DeleteAssetCascade(t *testing.T) {
	env := withPG(t)
	for _, p := range []string{
		"site01.pod098.cdu000",
		"site01.pod098.cdu001",
		"site01.pod098.meter000",
	} {
		if _, err := putAsset(env.Ctx, env.Conn, Asset{Path: p, Spec: map[string]any{"type": "cdu"}}, 0); err != nil {
			t.Fatalf("putAsset %s: %v", p, err)
		}
	}

	n, err := deleteAsset(env.Ctx, env.Conn, "site01.pod098", true)
	if err != nil {
		t.Fatalf("deleteAsset cascade: %v", err)
	}
	// The pod itself is not present, but the 3 descendants are.
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
}

func TestPG_SeedAlarmsIdempotentKeepsSince(t *testing.T) {
	env := withPG(t)

	original := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := seedAlarms(env.Ctx, env.Conn, []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "x", Since: original},
	}); err != nil {
		t.Fatalf("seedAlarms first: %v", err)
	}

	updated := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := seedAlarms(env.Ctx, env.Conn, []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "major", State: "acked", Summary: "y", Since: updated},
	}); err != nil {
		t.Fatalf("seedAlarms second: %v", err)
	}

	all, err := listAlarms(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("listAlarms: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1", len(all))
	}
	if !all[0].Since.Equal(original) {
		t.Errorf("Since = %v, want %v (idempotent re-seed must preserve)", all[0].Since, original)
	}
	if all[0].Severity != "major" || all[0].State != "acked" || all[0].Summary != "y" {
		t.Errorf("updated fields not refreshed: %+v", all[0])
	}
}

func TestPG_ListAlarmsOrdering(t *testing.T) {
	env := withPG(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := seedAlarms(env.Ctx, env.Conn, []Alarm{
		{ID: "I-OLD", Path: "p", Severity: "info", State: "firing", Summary: "", Since: t0.Add(1 * time.Hour)},
		{ID: "C-OLD", Path: "p", Severity: "critical", State: "firing", Summary: "", Since: t0},
		{ID: "M-NEW", Path: "p", Severity: "major", State: "firing", Summary: "", Since: t0.Add(2 * time.Hour)},
		{ID: "C-NEW", Path: "p", Severity: "critical", State: "firing", Summary: "", Since: t0.Add(3 * time.Hour)},
	}); err != nil {
		t.Fatalf("seedAlarms: %v", err)
	}
	got, err := listAlarms(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("listAlarms: %v", err)
	}
	wantIDs := []string{"C-NEW", "C-OLD", "M-NEW", "I-OLD"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len = %d, want %d", len(got), len(wantIDs))
	}
	for i := range wantIDs {
		if got[i].ID != wantIDs[i] {
			t.Errorf("[%d] = %q, want %q (severity rank → since desc)", i, got[i].ID, wantIDs[i])
		}
	}
}

// --- PRMT-087: schema_migrations ledger ----------------------------------

// pgMigrationPool returns a single-connection pool wired to the
// test DSN. The pool is closed by t.Cleanup. The schema_migrations
// table is created (idempotent) before the test runs so we always
// operate against a known state.
func pgMigrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := pgDSN(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	return pool
}

// TestPG_SchemaMigrationsAppliesAndRecords covers the happy path:
// a migration that has not yet been recorded is applied (its SQL
// runs) and the version is added to the ledger inside the same
// transaction. A subsequent probe reports it as already applied.
func TestPG_SchemaMigrationsAppliesAndRecords(t *testing.T) {
	pool := pgMigrationPool(t)
	ctx := context.Background()

	const version = "087_test_apply.sql"
	// Make sure the test starts clean — earlier runs may have
	// recorded the version. The DDL is CREATE TABLE IF NOT
	// EXISTS, but the ledger is an insert-once table.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		t.Fatalf("clear ledger: %v", err)
	}

	applied, err := migrationApplied(ctx, pool, version)
	if err != nil {
		t.Fatalf("migrationApplied pre: %v", err)
	}
	if applied {
		t.Fatalf("pre-applied = true, want false (ledger was just cleared)")
	}

	// Idempotent DDL: CREATE TABLE IF NOT EXISTS. The presence
	// of the side-effect (assets table existing) after the call
	// is what proves the SQL ran; the ledger row proves the
	// INSERT was committed atomically.
	const ddlt = `CREATE TABLE IF NOT EXISTS pg_store_test_087_assets (path text)`
	if err := runMigration(ctx, pool, ddlt, version); err != nil {
		t.Fatalf("runMigration: %v", err)
	}

	applied, err = migrationApplied(ctx, pool, version)
	if err != nil {
		t.Fatalf("migrationApplied post: %v", err)
	}
	if !applied {
		t.Errorf("post-applied = false, want true (ledger row missing after runMigration)")
	}

	// Belt-and-braces: the table from the migration DDL must
	// exist on the same conn-side. Query the catalog.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		"pg_store_test_087_assets",
	).Scan(&exists); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	if !exists {
		t.Errorf("assets table not created by runMigration")
	}
}

// TestPG_SchemaMigrationsSkipAlreadyApplied exercises the
// "already applied" branch: once a version is in the ledger,
// NewPGStore's loop (modeled here by migrationApplied) must
// report it as applied so the SQL is NOT re-run. We can't
// observe the no-op SQL directly, but we can prove the ledger
// short-circuits the call path.
func TestPG_SchemaMigrationsSkipAlreadyApplied(t *testing.T) {
	pool := pgMigrationPool(t)
	ctx := context.Background()

	const version = "087_test_skip.sql"
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		t.Fatalf("clear ledger: %v", err)
	}

	// First call: not yet applied.
	applied, err := migrationApplied(ctx, pool, version)
	if err != nil {
		t.Fatalf("first migrationApplied: %v", err)
	}
	if applied {
		t.Fatalf("first applied = true, want false")
	}

	// Record the version.
	if err := runMigration(ctx, pool, `-- noop`, version); err != nil {
		t.Fatalf("runMigration record: %v", err)
	}

	// Second call: must report applied.
	applied, err = migrationApplied(ctx, pool, version)
	if err != nil {
		t.Fatalf("second migrationApplied: %v", err)
	}
	if !applied {
		t.Errorf("second applied = false, want true (ledger row should be visible)")
	}
}

// TestPG_SchemaMigrationsFailureRollsBack covers the contract
// that a failing migration rolls back its transaction (so the
// SQL is NOT applied) AND the ledger row is NOT inserted (so
// the next boot will retry). We force a failure with a
// deliberate syntax error.
func TestPG_SchemaMigrationsFailureRollsBack(t *testing.T) {
	pool := pgMigrationPool(t)
	ctx := context.Background()

	const version = "087_test_fail.sql"
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		t.Fatalf("clear ledger: %v", err)
	}

	// Bad SQL: `THIS IS NOT VALID SQL` — Postgres will reject it.
	bad := "THIS IS NOT VALID SQL"
	if err := runMigration(ctx, pool, bad, version); err == nil {
		t.Fatalf("runMigration bad SQL: err = nil, want non-nil")
	}

	// Ledger row must NOT be present (transaction rolled back).
	applied, err := migrationApplied(ctx, pool, version)
	if err != nil {
		t.Fatalf("migrationApplied after fail: %v", err)
	}
	if applied {
		t.Errorf("applied = true after failed migration; the ledger INSERT should have rolled back")
	}
}

// --- PRMT-088: tickets/alarms severity·state CHECK ----------------------

// withPGChecks spins up a tx with the full production migration set
// so alarms + tickets CHECK constraints (013) are present alongside
// every column putTicket/putAlarm require. Partial 001+002+013
// fixtures are insufficient once later ALTERs land.
func withPGChecks(t *testing.T) *pgTestEnv {
	t.Helper()
	return withPG(t)
}

// TestPG_TicketAlarmChecks_RejectsOutOfSet proves the DB rejects
// out-of-set severity (on tickets AND alarms) and out-of-set state
// (on tickets). The CHECK is the defensive layer: the HTTP path
// validates too, but anything bypassing HTTP (manual SQL, future
// background workers) must be blocked here.
func TestPG_TicketAlarmChecks_RejectsOutOfSet(t *testing.T) {
	env := withPGChecks(t)
	ctx := env.Ctx

	cases := []struct {
		name  string
		query string
	}{
		{"tickets severity bogus", `INSERT INTO tickets(id, asset_path, severity, state, opened_at) VALUES ('TB1','p','emergency','open',NOW())`},
		{"tickets state bogus", `INSERT INTO tickets(id, asset_path, severity, state, opened_at) VALUES ('TB2','p','major','cancelled',NOW())`},
		{"alarms severity bogus", `INSERT INTO alarms(id, path, severity, state, summary, since) VALUES ('AB1','p','emergency','firing','',NOW())`},
	}
	for _, tc := range cases {
		_, err := env.Conn.Exec(ctx, tc.query)
		if err == nil {
			t.Errorf("%s: err = nil, want CHECK violation", tc.name)
		}
	}
}

// TestPG_TicketAlarmChecks_AcceptsInSet proves every value from the
// spec-003 severity set and the spec-008 state set is accepted by
// the DB. This guards against an over-zealous migration that drops
// or mistypes a member of the enum.
func TestPG_TicketAlarmChecks_AcceptsInSet(t *testing.T) {
	env := withPGChecks(t)
	ctx := env.Ctx

	// tickets.severity — every member of spec-003 §2.
	for i, sev := range []string{"critical", "major", "minor", "info"} {
		id := "TS" + string(rune('A'+i))
		if _, err := env.Conn.Exec(ctx,
			`INSERT INTO tickets(id, asset_path, severity, state, opened_at) VALUES ($1,'p',$2,'open',NOW())`,
			id, sev,
		); err != nil {
			t.Errorf("tickets.severity=%q insert: %v", sev, err)
		}
	}
	// tickets.state — every member of spec-008 §2.
	for i, st := range []string{"open", "acknowledged", "resolved", "closed"} {
		id := "TT" + string(rune('A'+i))
		if _, err := env.Conn.Exec(ctx,
			`INSERT INTO tickets(id, asset_path, severity, state, opened_at) VALUES ($1,'p','major',$2,NOW())`,
			id, st,
		); err != nil {
			t.Errorf("tickets.state=%q insert: %v", st, err)
		}
	}
	// alarms.severity — every member of spec-003 §2.
	for i, sev := range []string{"critical", "major", "minor", "info"} {
		id := "AS" + string(rune('A'+i))
		if _, err := env.Conn.Exec(ctx,
			`INSERT INTO alarms(id, path, severity, state, summary, since) VALUES ($1,'p',$2,'firing','',NOW())`,
			id, sev,
		); err != nil {
			t.Errorf("alarms.severity=%q insert: %v", sev, err)
		}
	}
}

// --- PRMT-230: alarm ack (migration 019) ---------------------------------

func TestPG_AckAlarm_FiringToAcked(t *testing.T) {
	env := withPG(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := seedAlarms(env.Ctx, env.Conn, []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "hot", Since: since},
	}); err != nil {
		t.Fatalf("seedAlarms: %v", err)
	}

	got, found, err := ackAlarm(env.Ctx, env.Conn, "A1", "svc:ci-operator")
	if err != nil || !found {
		t.Fatalf("ackAlarm = (_, %v, %v), want (row, true, nil)", found, err)
	}
	if got.State != "acked" || got.AckedBy != "svc:ci-operator" || got.AckedAt == nil {
		t.Fatalf("returned row = %+v, want acked/svc:ci-operator/non-nil AckedAt", got)
	}

	all, err := listAlarms(env.Ctx, env.Conn)
	if err != nil {
		t.Fatalf("listAlarms: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1", len(all))
	}
	if all[0].State != "acked" || all[0].AckedBy != "svc:ci-operator" || all[0].AckedAt == nil {
		t.Errorf("re-listed row = %+v, want acked/svc:ci-operator/non-nil AckedAt", all[0])
	}
}

func TestPG_AckAlarm_Conflict(t *testing.T) {
	env := withPG(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := seedAlarms(env.Ctx, env.Conn, []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "hot", Since: since},
	}); err != nil {
		t.Fatalf("seedAlarms: %v", err)
	}
	if _, _, err := ackAlarm(env.Ctx, env.Conn, "A1", "svc:ci-operator"); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	got, found, err := ackAlarm(env.Ctx, env.Conn, "A1", "svc:ci-operator")
	if !found || !errors.Is(err, ErrAlarmNotAckable) {
		t.Fatalf("re-ack = (_, %v, %v), want (row, true, ErrAlarmNotAckable)", found, err)
	}
	if got.State != "acked" {
		t.Errorf("returned row State = %q, want acked (409 detail source)", got.State)
	}
}

func TestPG_AckAlarm_NotFound(t *testing.T) {
	env := withPG(t)
	got, found, err := ackAlarm(env.Ctx, env.Conn, "MISSING", "svc:ci-operator")
	if found || err != nil {
		t.Fatalf("ackAlarm(MISSING) = (%+v, %v, %v), want (_, false, nil)", got, found, err)
	}
}
