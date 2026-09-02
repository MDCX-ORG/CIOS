// pgStore is the PostgreSQL-backed implementation of Store (L55②
// replacement, PRMT-016). It satisfies the same Store contract as
// fileStore and is selected by NewServerFromConfig when
// ServerConfig.DSN is non-empty. The file path of fileStore is
// untouched: NewFileStore still works exactly as before.
//
// The wire format on disk is two tables (assets, alarms) created
// by migrations/001_init.sql. NewPGStore reads and applies that
// file idempotently at construction time, so a fresh database is
// usable immediately. There is no separate "migration" concept in
// this prompt (see §6 MUST NOT — no migration framework).
//
// G1 fix (PRMT-016 R1) and PRMT-016b R1: the six Store-method SQL
// paths live in package-private helper functions that take a
// querier interface. *pgxpool.Pool (production), *pgxpool.Conn
// (test transaction pin) and pgx.Tx (production single-statement
// transactions opened by the *pgStore methods) all satisfy it.
// The integration tests in pg_store_test.go call the same helper
// functions as production, so there is no SQL duplicate to drift.
package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. querier + migration helpers  — DSN bootstrap, schema_migrations ledger
//   2. *pgStore: assets / alarms    — core CRUD on the two M0 tables
//   3. *pgStore: tickets            — PutTicket/GetTicket/ListTickets (PRMT-032)
//   4. *pgStore: PM / audit / spare — M2 entities (PRMT-043, 045, 048)
//   5. *pgStore: inspection/notes   — PRMT-049, PRMT-060 (assignee + notes)
//   6. *pgStore: mwindow / scanner  — PRMT-096 + TryScannerLock (PRMT-065)
//   7. shared SQL helpers           — package-private querier helpers + tx
// ─────────────────────────────────────────────────────────────────────────────

// querier is the subset of pgx shared by *pgxpool.Pool, *pgxpool.Conn
// and pgx.Tx. The store helpers take a querier so the SAME SQL runs
// in production (against the pool/tx) and in tests (against a
// tx-scoped conn) — no duplicated query strings.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// --- migration helpers (production only) ---------------------------------

// MigrationFiles is the ordered list of SQL files NewPGStore applies.
// PG integration test helpers MUST use this same list (or a strict
// prefix that still includes every column/table the production SQL
// helpers touch) — partial fixtures that lag the SQL are how
// CODE-SCAN / P795 pg-parity went red (missing escalated_at, spare_parts).
var MigrationFiles = []string{
	"001_init.sql",
	"002_tickets.sql",
	"003_ticket_sla.sql",
	"004_pm.sql",
	"005_ticket_runbook.sql",
	"006_asset_audit.sql",
	"007_spares.sql",
	"008_inspection.sql",
	"009_ticket_notes.sql",
	"010_ticket_audit.sql",
	"011_ticket_dedup_uniq.sql",
	"012_ticket_version.sql",
	"013_ticket_alarm_checks.sql",
	"014_maintenance_windows.sql",
	"015_tenant_org.sql",
	"016_site_org.sql",
	"017_role_bindings.sql",
	"018_usage.sql",
	"019_alarm_ack.sql",
	"020_set_audit.sql",
}

// schemaMigrationsDDL is the schema for the migration ledger
// (PRMT-087). One row per applied migration; future non-idempotent
// migrations rely on this so a fresh boot does not re-run them.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
`

// NewPGStore opens a pgxpool, runs migrations/001_init.sql,
// migrations/002_tickets.sql, and migrations/003_ticket_sla.sql in
// a transaction, and returns a ready-to-use Store. migrationsDir
// is the path to the migrations/ directory; the files inside it
// are read verbatim and EXEC'd.
//
// PRMT-087: a schema_migrations ledger tracks which migration
// files have already been applied. On each boot we CREATE TABLE
// IF NOT EXISTS the ledger, then for each entry in migFiles check
// whether the version (the file name) is already recorded; if not,
// we run the SQL in a single transaction together with the ledger
// INSERT, then commit. Already-applied migrations are skipped
// outright. migFiles remains the authoritative list of KNOWN
// migrations; a new file in migrations/ that is not added to
// migFiles will still be silently ignored (this is intentional —
// the slice is the contract; the ledger is just "already
// applied").
//
// The pool is closed by calling Close on the returned Store —
// callers that want to shut the pool down should type-assert to
// *pgStore and call pool.Close, or use a deferred pgxpool close
// at a higher level.
func NewPGStore(ctx context.Context, dsn string, migrationsDir string) (Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("core: pg store: empty DSN")
	}
	if migrationsDir == "" {
		return nil, fmt.Errorf("core: pg store: empty migrations dir")
	}
	// MigrationFiles is the single source of truth (also used by PG test helpers).
	migFiles := MigrationFiles
	migs := make([]string, 0, len(migFiles))
	for _, f := range migFiles {
		p := filepath.Join(migrationsDir, f)
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("core: pg store: read %s: %w", p, err)
		}
		migs = append(migs, string(raw))
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: connect: %w", err)
	}
	// Best-effort probe so a misconfigured DSN surfaces at boot, not
	// on the first request.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("core: pg store: ping: %w", err)
	}

	// Bootstrap the ledger. CREATE TABLE IF NOT EXISTS is
	// idempotent so a fresh DB and an existing DB both land in
	// the same state.
	if _, err := pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("core: pg store: create schema_migrations: %w", err)
	}

	for i, m := range migs {
		applied, err := migrationApplied(ctx, pool, migFiles[i])
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("core: pg store: probe %s: %w", migFiles[i], err)
		}
		if applied {
			continue
		}
		if err := runMigration(ctx, pool, m, migFiles[i]); err != nil {
			pool.Close()
			return nil, fmt.Errorf("core: pg store: migrate %s: %w", migFiles[i], err)
		}
	}
	return &pgStore{pool: pool}, nil
}

// migrationApplied reports whether version is already in the
// schema_migrations ledger. A miss means the migration still
// needs to run. We do this on a fresh connection (not a held
// transaction) so the read is short and the subsequent
// BEGIN/COMMIT for runMigration is a separate, clean tx.
func migrationApplied(ctx context.Context, pool *pgxpool.Pool, version string) (bool, error) {
	var one int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM schema_migrations WHERE version = $1`, version,
	).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// runMigration applies the SQL and records the version in
// schema_migrations inside a single transaction. On any failure
// the transaction is rolled back and the error propagated — the
// caller closes the pool. The version is the migration file
// name (e.g. "001_init.sql"); the ledger key is a TEXT column
// keyed on it.
func runMigration(ctx context.Context, pool *pgxpool.Pool, sqlText string, version string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations(version) VALUES ($1)`, version,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- *pgStore (production wrapper) ---------------------------------------

// pgStore implements Store using a PostgreSQL connection pool.
// The methods are thin wrappers that open a transaction when
// needed and forward to the package-private helpers below. The
// helpers carry the actual SQL.
type pgStore struct {
	pool *pgxpool.Pool
}

func (s *pgStore) PutAsset(ctx context.Context, a Asset, expectVersion int64) (Asset, error) {
	if expectVersion == 0 {
		// Single-statement upsert: no explicit transaction needed.
		return putAsset(ctx, s.pool, a, expectVersion)
	}
	// Optimistic update + conflict re-read must share one
	// transaction so the version conflict re-read sees the same
	// snapshot the UPDATE saw (PRMT-016b §1 TOCTOU fix).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("core: pg store: put: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, perr := putAsset(ctx, tx, a, expectVersion)
	if perr != nil {
		// On ErrVersionConflict we still want to commit the (empty)
		// transaction so the SELECT the helper issued inside the
		// helper is properly bounded; the deferred Rollback is
		// suppressed by a successful Commit. On any other error
		// the deferred Rollback fires and we propagate perr.
		if errors.Is(perr, ErrVersionConflict) {
			_ = tx.Commit(ctx)
		}
		return out, perr
	}
	if err := tx.Commit(ctx); err != nil {
		return Asset{}, fmt.Errorf("core: pg store: put: commit: %w", err)
	}
	return out, nil
}

func (s *pgStore) GetAsset(ctx context.Context, path string) (Asset, bool, error) {
	return getAsset(ctx, s.pool, path)
}

func (s *pgStore) ListAssets(ctx context.Context) ([]Asset, error) {
	return listAssets(ctx, s.pool)
}

func (s *pgStore) DeleteAsset(ctx context.Context, path string, cascade bool) (int, error) {
	// EXISTS / count / DELETE all share one transaction so a
	// concurrent insert cannot slip a child between probe and
	// delete (PRMT-016b §1 TOCTOU fix).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("core: pg store: delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, derr := deleteAsset(ctx, tx, path, cascade)
	if derr != nil {
		// ErrHasChildren / other errors: rollback via the deferred
		// Rollback and propagate.
		return n, derr
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("core: pg store: delete: commit: %w", err)
	}
	return n, nil
}

func (s *pgStore) ListAlarms(ctx context.Context) ([]Alarm, error) {
	return listAlarms(ctx, s.pool)
}

func (s *pgStore) AckAlarm(ctx context.Context, id, actor string) (Alarm, bool, error) {
	return ackAlarm(ctx, s.pool, id, actor)
}

func (s *pgStore) SeedAlarms(ctx context.Context, in []Alarm) error {
	if len(in) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: seed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := seedAlarms(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutTicket implements Store. expectVersion==0 → single-statement
// INSERT…ON CONFLICT upsert (auto-opener path; mirrors putAsset's
// expectVersion=0 branch). expectVersion>0 → optimistic lock: open
// a transaction so the conflict re-read sees the same snapshot the
// UPDATE saw (PRMT-016b §1 TOCTOU fix). 0 rows affected on the
// optimistic path → ErrVersionConflict; the current row is
// surfaced (if it exists) via a re-read on the SAME querier so the
// caller can build a 409 response. Each successful write advances
// resource_version by 1.
func (s *pgStore) PutTicket(ctx context.Context, t Ticket, expectVersion int64) (Ticket, error) {
	if expectVersion == 0 {
		return putTicket(ctx, s.pool, t, expectVersion)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, fmt.Errorf("core: pg store: put ticket: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, perr := putTicket(ctx, tx, t, expectVersion)
	if perr != nil {
		// On ErrVersionConflict we still want to commit the (empty)
		// transaction so the SELECT the helper issued inside the
		// helper is properly bounded; the deferred Rollback is
		// suppressed by a successful Commit. On any other error
		// the deferred Rollback fires and we propagate perr.
		if errors.Is(perr, ErrVersionConflict) {
			_ = tx.Commit(ctx)
		}
		return out, perr
	}
	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, fmt.Errorf("core: pg store: put ticket: commit: %w", err)
	}
	return out, nil
}

func (s *pgStore) GetTicket(ctx context.Context, id string) (Ticket, bool, error) {
	return getTicket(ctx, s.pool, id)
}

func (s *pgStore) ListTickets(ctx context.Context) ([]Ticket, error) {
	return listTickets(ctx, s.pool)
}

func (s *pgStore) PutPMSchedule(ctx context.Context, p PMSchedule) error {
	return putPMSchedule(ctx, s.pool, p)
}

func (s *pgStore) GetPMSchedule(ctx context.Context, id string) (PMSchedule, bool, error) {
	return getPMSchedule(ctx, s.pool, id)
}

func (s *pgStore) ListPMSchedules(ctx context.Context) ([]PMSchedule, error) {
	return listPMSchedules(ctx, s.pool)
}

func (s *pgStore) AppendAssetAudit(ctx context.Context, a AssetAudit) error {
	return appendAssetAudit(ctx, s.pool, a)
}

func (s *pgStore) ListAssetAudits(ctx context.Context, path string) ([]AssetAudit, error) {
	return listAssetAudits(ctx, s.pool, path)
}

// PutSpare idempotently upserts by ID (full-field overwrite).
// SQL UNIQUE on sku may surface as a unique-violation; we wrap
// it so callers see ErrSKUExists (mapped to 422 by the HTTP layer).
func (s *pgStore) PutSpare(ctx context.Context, sp SparePart) error {
	return putSpare(ctx, s.pool, sp)
}

func (s *pgStore) GetSpare(ctx context.Context, id string) (SparePart, bool, error) {
	return getSpare(ctx, s.pool, id)
}

func (s *pgStore) ListSpares(ctx context.Context) ([]SparePart, error) {
	return listSpares(ctx, s.pool)
}

// AdjustSpare opens one transaction so the txn append and the
// qty update share a snapshot. qty+delta<0 → ErrInsufficientStock;
// the UNIQUE/sku constraint is enforced by the schema.
func (s *pgStore) AdjustSpare(ctx context.Context, id string, delta int, ticketID string, at time.Time) (SparePart, SpareTxn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sp, txn, err := adjustSpare(ctx, tx, id, delta, ticketID, at)
	if err != nil {
		return sp, txn, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare: commit: %w", err)
	}
	return sp, txn, nil
}

func (s *pgStore) ListSpareTxns(ctx context.Context, spareID string) ([]SpareTxn, error) {
	return listSpareTxns(ctx, s.pool, spareID)
}

// --- Inspection (PRMT-049) ----------------------------------------------

// PutInspectionTemplate is upsert by ID (full-field overwrite on
// conflict). The interval is stored as BIGINT nanoseconds
// (interval_ns) — Postgres has no native duration type, and ns
// preserves the full Go time.Duration range without lossy casts.
func (s *pgStore) PutInspectionTemplate(ctx context.Context, it InspectionTemplate) error {
	return putInspectionTemplate(ctx, s.pool, it)
}

func (s *pgStore) GetInspectionTemplate(ctx context.Context, id string) (InspectionTemplate, bool, error) {
	return getInspectionTemplate(ctx, s.pool, id)
}

func (s *pgStore) ListInspectionTemplates(ctx context.Context) ([]InspectionTemplate, error) {
	return listInspectionTemplates(ctx, s.pool)
}

// --- Ticket notes (PRMT-060) ---------------------------------------------

func (s *pgStore) AppendTicketNote(ctx context.Context, n TicketNote) error {
	return appendTicketNote(ctx, s.pool, n)
}

func (s *pgStore) ListTicketNotes(ctx context.Context, ticketID string) ([]TicketNote, error) {
	return listTicketNotes(ctx, s.pool, ticketID)
}

// UpdateTicketAssignee returns the updated ticket. Missing ticket
// → (Ticket{}, false, nil); failure other than missing → error.
func (s *pgStore) UpdateTicketAssignee(ctx context.Context, ticketID, assignee string) (Ticket, bool, error) {
	return updateTicketAssignee(ctx, s.pool, ticketID, assignee)
}

// --- Ticket audit (PRMT-061) --------------------------------------------

func (s *pgStore) AppendTicketAudit(ctx context.Context, a TicketAudit) error {
	return appendTicketAudit(ctx, s.pool, a)
}

func (s *pgStore) ListTicketAudits(ctx context.Context, ticketID string) ([]TicketAudit, error) {
	return listTicketAudits(ctx, s.pool, ticketID)
}

func (s *pgStore) AppendSetAudit(ctx context.Context, a SetAudit) error {
	return appendSetAuditPG(ctx, s.pool, a)
}

func (s *pgStore) ListSetAudits(ctx context.Context) ([]SetAudit, error) {
	return listSetAuditsPG(ctx, s.pool)
}

// --- MaintenanceWindow (PRMT-096) ----------------------------------------

// PutMaintenanceWindow is upsert by ID (full-field overwrite on
// conflict). Mirrors PutInspectionTemplate / PutPMSchedule.
func (s *pgStore) PutMaintenanceWindow(ctx context.Context, m MaintenanceWindow) error {
	return putMaintenanceWindow(ctx, s.pool, m)
}

func (s *pgStore) GetMaintenanceWindow(ctx context.Context, id string) (MaintenanceWindow, bool, error) {
	return getMaintenanceWindow(ctx, s.pool, id)
}

func (s *pgStore) ListMaintenanceWindows(ctx context.Context) ([]MaintenanceWindow, error) {
	return listMaintenanceWindows(ctx, s.pool)
}

func (s *pgStore) DeleteMaintenanceWindow(ctx context.Context, id string) (bool, error) {
	return deleteMaintenanceWindow(ctx, s.pool, id)
}

// ActiveWindowFor returns the first window whose [StartsAt, EndsAt)
// contains now AND whose asset_path matches the alarming path
// (== OR "."-prefixed ancestor). cios-alarm calls this on every
// firing transition before opening a ticket (PRMT-096 §2 / §4).
// One single SQL query keeps the per-alarm overhead to a single
// round-trip — a hit means OpenTicket skips ticket creation and
// logs the suppression (pkg/alarm/store.go).
//
// Prefix match uses ($1 = asset_path OR path LIKE asset_path || '.%')
// so a parent path "site01.pod000" matches "site01.pod000.cdu000"
// but NOT "site01.pod0009.cdu000". Asset paths are constrained to
// [a-z0-9.] (pkg/cpath), so LIKE wildcards '_' / '%' cannot appear
// in the value; if the charset ever loosens, switch to
// starts_with(path, asset_path||'.') for safety (mirrors
// deleteAsset's comment).
func (s *pgStore) ActiveWindowFor(ctx context.Context, assetPath string, now time.Time) (MaintenanceWindow, bool, error) {
	return activeWindowFor(ctx, s.pool, assetPath, now)
}

// TryScannerLock implements Store. Acquires a session-scoped
// advisory lock keyed by hashtext('cios.scanner.' || name) so
// each scanner (sla/pm/inspection/spare/reconcile/report) is
// independent — one scanner's lock does not block another.
// pg_try_advisory_lock returns true on the very first caller
// per (key, session) pair; concurrent callers on a different
// session get false and skip the tick. The lock is bound to the
// session that issued it, so it is automatically released when
// the underlying connection is returned to the pool — the
// release closure still calls pg_advisory_unlock on the same
// session-scoped connection so the lock is freed the moment the
// tick ends rather than waiting for the connection to be
// recycled. PRMT-065 §2.
//
// Implementation note: we open a dedicated *pgxpool.Conn for
// each TryScannerLock call so the lock and its matching unlock
// run on the same session. The conn is held until release is
// invoked (or until ctx cancellation); the conn release is the
// "release" of the underlying session resource.
func (s *pgStore) TryScannerLock(ctx context.Context, name string) (bool, func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, func() {}, fmt.Errorf("core: pg store: acquire conn for scanner lock %q: %w", name, err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('cios.scanner.' || $1))`, name).Scan(&acquired); err != nil {
		conn.Release()
		return false, func() {}, fmt.Errorf("core: pg store: pg_try_advisory_lock %q: %w", name, err)
	}
	if !acquired {
		// Another session holds the lock for this scanner; this
		// tick should be skipped. Release the conn back to the
		// pool immediately — the caller's release closure is
		// still a no-op so a stray defer cannot mis-handle a
		// lock that was never acquired.
		conn.Release()
		return false, func() {}, nil
	}
	release := func() {
		// Best-effort: if the unlock fails we still want to
		// return the conn to the pool. The lock is session-
		// scoped so an unlock failure only matters if the same
		// session later re-acquires — pool recycling handles
		// that by giving us a fresh session.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('cios.scanner.' || $1))`, name)
		conn.Release()
	}
	return true, release, nil
}

// --- Tenant / Org / TenantAudit (PRMT-184, spec-001 v1.1 §5bis) ---
//
// Read-side Store methods only. Mutators (Create / Update /
// TierChange / StatusChange / Org Create / Rename / Re-attach /
// Delete) arrive with their consuming PRMTs (182 / 185 / 186).

func (s *pgStore) GetTenant(ctx context.Context, id string) (Tenant, bool, error) {
	return getTenant(ctx, s.pool, id)
}

func (s *pgStore) ListTenants(ctx context.Context) ([]Tenant, error) {
	return listTenants(ctx, s.pool)
}

// CreateTenant (L109 P804) inserts tenant + default org in one transaction.
func (s *pgStore) CreateTenant(ctx context.Context, id, displayName, principal string) (Tenant, Org, error) {
	id = strings.TrimSpace(id)
	displayName = strings.TrimSpace(displayName)
	if !validTenantSlug(id) {
		return Tenant{}, Org{}, fmt.Errorf("core: create tenant: invalid slug")
	}
	if displayName == "" {
		return Tenant{}, Org{}, fmt.Errorf("core: create tenant: display_name required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Tenant{}, Org{}, fmt.Errorf("core: pg store: create tenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UTC()
	t := Tenant{
		ID:            id,
		DisplayName:   displayName,
		IsolationTier: "label",
		Status:        "active",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenants (id, display_name, isolation_tier, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, t.ID, t.DisplayName, t.IsolationTier, t.Status, t.CreatedAt, t.UpdatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Tenant{}, Org{}, ErrTenantExists
		}
		return Tenant{}, Org{}, fmt.Errorf("core: pg store: create tenant: %w", err)
	}
	if err := appendTenantAudit(ctx, tx, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  id,
		Op:        "tenant_create",
		Detail:    displayName,
	}); err != nil {
		return Tenant{}, Org{}, err
	}
	o, err := createOrg(ctx, tx, id, DefaultOrgName, principal)
	if err != nil {
		return Tenant{}, Org{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, Org{}, fmt.Errorf("core: pg store: create tenant: commit: %w", err)
	}
	return t, o, nil
}

func (s *pgStore) GetOrg(ctx context.Context, id string) (Org, bool, error) {
	return getOrg(ctx, s.pool, id)
}

func (s *pgStore) ListOrgs(ctx context.Context, tenantID string) ([]Org, error) {
	return listOrgs(ctx, s.pool, tenantID)
}

// ListOrgsAll implements Store. Single SQL over all orgs; groups in Go.
// Columns and Scan order match listOrgs (id, tenant_id, name, created_at).
// ORDER BY tenant_id, name guarantees name ASC per tenant — no Go sort.
func (s *pgStore) ListOrgsAll(ctx context.Context) (map[string][]Org, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, name, created_at
		  FROM orgs
		 ORDER BY tenant_id, name
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list orgs all: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]Org)
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.TenantID, &o.Name, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan org: %w", err)
		}
		out[o.TenantID] = append(out[o.TenantID], o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: pg store: list orgs all: %w", err)
	}
	return out, nil
}

// CreateOrg (PRMT-185 §4.1) inserts one org under tenantID and
// appends one tenant_audit op="org_create" in the same transaction
// so a crash between the row write and the audit insert cannot
// leave a torn record (mirrors UpdateTenantTier's txn shape). The
// slug re-check is the defensive boundary (the HTTP handler also
// checks). (tenant_id, name) UNIQUE 23505 → ErrOrgNameConflict;
// tenants FK 23503 → wrapped not-found (mapped to 404 by handler).
func (s *pgStore) CreateOrg(ctx context.Context, tenantID, name, principal string) (Org, error) {
	if !validTenantSlug(name) {
		return Org{}, fmt.Errorf("core: create org: invalid slug")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Org{}, fmt.Errorf("core: pg store: create org: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := createOrg(ctx, tx, tenantID, name, principal)
	if err != nil {
		return Org{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Org{}, fmt.Errorf("core: pg store: create org: commit: %w", err)
	}
	return o, nil
}

// RenameOrg (PRMT-185 §4.1) updates orgs.name and appends one
// tenant_audit op="org_rename" detail "<old>→<new>" in the same
// transaction. Equal-name is a no-op with zero audit rows (mirrors
// UpdateTenantTier equal-tier branch). (tenant_id, name) UNIQUE
// 23505 → ErrOrgNameConflict; absent id → wrapped not-found.
func (s *pgStore) RenameOrg(ctx context.Context, id, newName, principal string) error {
	if !validTenantSlug(newName) {
		return fmt.Errorf("core: rename org: invalid slug")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: rename org: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := renameOrg(ctx, tx, id, newName, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: rename org: commit: %w", err)
	}
	return nil
}

// DeleteOrg (PRMT-185 §4.1, R5) checks CountSitesByOrg (PRMT-189)
// inside the transaction so a racing AttachSiteToOrg cannot slip a
// site mapping between the count and the delete. count > 0 →
// ErrOrgOwnsResources, NO delete, NO audit. count == 0 → delete +
// one tenant_audit op="org_delete". Applies uniformly to `default`
// and every other org (spec-001 §5bis.2). Absent id → wrapped
// not-found.
func (s *pgStore) DeleteOrg(ctx context.Context, id, principal string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: delete org: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := deleteOrg(ctx, tx, id, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: delete org: commit: %w", err)
	}
	return nil
}

func (s *pgStore) AppendTenantAudit(ctx context.Context, a TenantAudit) error {
	return appendTenantAudit(ctx, s.pool, a)
}

func (s *pgStore) ListTenantAudits(ctx context.Context, tenantID string) ([]TenantAudit, error) {
	return listTenantAudits(ctx, s.pool, tenantID)
}

// UpdateTenantTier (PRMT-182) raises a tenant's isolation_tier
// one-way-up and records the outcome to tenant_audit atomically.
// Opens one transaction so the SELECT FOR UPDATE guard, the tenants
// UPDATE, and the tenant_audit INSERT share a snapshot (no torn
// state on crash — mirrors AdjustSpare's txn shape). Downgrade
// writes one REFUSED audit row and returns ErrTierDowngrade without
// touching tenants; equal target is a no-op with zero audit rows.
func (s *pgStore) UpdateTenantTier(ctx context.Context, id, target, principal string) error {
	targetRank, ok := tenantTierRank(target)
	if !ok {
		return tenantTierValidationError
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: update tenant tier: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := updateTenantTier(ctx, tx, id, target, targetRank, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: update tenant tier: commit: %w", err)
	}
	return nil
}

// --- SiteOrg (PRMT-189, spec-001 v1.1 §5bis.2 site→Org mapping) ---

// GetSiteOrg implements Store. pgx.ErrNoRows →
// (SiteOrg{}, false, nil) — mirrors getAsset.
func (s *pgStore) GetSiteOrg(ctx context.Context, site string) (SiteOrg, bool, error) {
	return getSiteOrg(ctx, s.pool, site)
}

// ListSiteOrgs implements Store. Returns all site→org mappings
// sorted by site ASC for a stable order across runs (mirrors
// listTenants). Empty table yields []SiteOrg{} (never nil).
func (s *pgStore) ListSiteOrgs(ctx context.Context) ([]SiteOrg, error) {
	return listSiteOrgs(ctx, s.pool)
}

// CountSitesByOrg implements Store. Returns the exact number of
// sites mapped to orgID (0 when none) — drives the 185 R5 delete
// guard.
func (s *pgStore) CountSitesByOrg(ctx context.Context, orgID string) (int, error) {
	return countSitesByOrg(ctx, s.pool, orgID)
}

// AttachSiteToOrg implements Store (PRMT-189). Validates the slug
// against validSiteSlug and the org FK against orgs.id; on create
// OR actual re-home, opens one transaction so the upsert and the
// tenant_audit INSERT share a snapshot (mirrors AdjustSpare /
// UpdateTenantTier). On the idempotent no-op path the tx does
// neither write.
// --- RoleBinding (PRMT-190-bis §4.2; spec-004 §6bis, R3) ---

// PutRoleBinding implements Store. Upsert on (subject, scope) via
// INSERT…ON CONFLICT DO UPDATE; origin and updated_at always
// advance; created_at is preserved on conflict (the SQL DEFAULT
// carries it forward — we only overwrite origin + updated_at on the
// conflict branch). Empty subject or scope is rejected at the
// boundary so the row cannot violate the schema.
func (s *pgStore) PutRoleBinding(ctx context.Context, rb RoleBinding) error {
	if rb.Subject == "" || rb.Scope == "" {
		return fmt.Errorf("core: put role binding: subject and scope required")
	}
	if rb.Origin == "" {
		rb.Origin = "legacy"
	}
	if rb.ID == "" {
		rb.ID = newRoleBindingID()
	}
	return putRoleBinding(ctx, s.pool, rb)
}

// ListRoleBindings implements Store. Returns rows for one subject
// in Scope ASC order. Unknown / empty subject yields a non-nil
// empty slice so the loader sees an empty list, not nil.
func (s *pgStore) ListRoleBindings(ctx context.Context, subject string) ([]RoleBinding, error) {
	return listRoleBindings(ctx, s.pool, subject)
}

// ListAllRoleBindings implements Store. Returns every row in
// (Subject ASC, Scope ASC) order — the stable order PRMT-186
// rewrites against. Empty table yields []RoleBinding{} (never nil).
func (s *pgStore) ListAllRoleBindings(ctx context.Context) ([]RoleBinding, error) {
	return listAllRoleBindings(ctx, s.pool)
}

// DeleteRoleBinding implements Store. Single-statement
// parameterised DELETE on (subject, scope). Idempotent no-op
// when the row is absent (rowcount == 0 is not an error;
// migration-only primitive, PRMT-186 §3 widening).
func (s *pgStore) DeleteRoleBinding(ctx context.Context, subject, scope string) error {
	if subject == "" || scope == "" {
		return fmt.Errorf("core: delete role binding: subject and scope required")
	}
	return deleteRoleBinding(ctx, s.pool, subject, scope)
}

func (s *pgStore) AttachSiteToOrg(ctx context.Context, site, orgID, principal string) error {
	if !validSiteSlug(site) {
		return siteSlugError
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: attach site to org: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := attachSiteToOrg(ctx, tx, site, orgID, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: attach site to org: commit: %w", err)
	}
	return nil
}

// DetachSiteFromOrg implements Store (PRMT-220).
func (s *pgStore) DetachSiteFromOrg(ctx context.Context, site, principal string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: detach site from org: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := detachSiteFromOrg(ctx, tx, site, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: detach site from org: commit: %w", err)
	}
	return nil
}

// DeleteTenant implements Store (PRMT-220).
func (s *pgStore) DeleteTenant(ctx context.Context, id, principal string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("core: pg store: delete tenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := deleteTenant(ctx, tx, id, principal); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("core: pg store: delete tenant: commit: %w", err)
	}
	return nil
}

// --- shared SQL helpers (used by production AND tests) -------------------

// putAsset implements the Store.PutAsset contract. expectVersion==0
// → INSERT…ON CONFLICT DO UPDATE; expectVersion>0 → optimistic
// UPDATE. 0 rows affected on the optimistic path is
// ErrVersionConflict with the current row surfaced (if it exists)
// via a re-read on the SAME querier so the read sees the same
// transaction snapshot.
func putAsset(ctx context.Context, q querier, a Asset, expectVersion int64) (Asset, error) {
	specBytes, err := json.Marshal(a.Spec)
	if err != nil {
		return Asset{}, fmt.Errorf("core: pg store: marshal spec: %w", err)
	}

	if expectVersion == 0 {
		row := q.QueryRow(ctx, `
			INSERT INTO assets(path, resource_version, spec, created_at, updated_at)
			VALUES ($1, 1, $2::jsonb, NOW(), NOW())
			ON CONFLICT (path) DO UPDATE
			  SET spec = EXCLUDED.spec,
			      resource_version = assets.resource_version + 1,
			      updated_at = NOW()
			RETURNING resource_version, created_at, updated_at
		`, a.Path, specBytes)
		var v int64
		var created, updated time.Time
		if err := row.Scan(&v, &created, &updated); err != nil {
			return Asset{}, fmt.Errorf("core: pg store: put: %w", err)
		}
		a.ResourceVersion = v
		a.CreatedAt = created
		a.UpdatedAt = updated
		return a, nil
	}

	row := q.QueryRow(ctx, `
		UPDATE assets
		   SET spec = $2::jsonb,
		       resource_version = resource_version + 1,
		       updated_at = NOW()
		 WHERE path = $1 AND resource_version = $3
		RETURNING resource_version, created_at, updated_at
	`, a.Path, specBytes, expectVersion)
	var v int64
	var created, updated time.Time
	if err := row.Scan(&v, &created, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Re-read on the SAME querier to keep this in the
			// caller's transaction. If the row is missing too,
			// surface a bare ErrVersionConflict.
			cur, _, gerr := getAsset(ctx, q, a.Path)
			if gerr != nil {
				return Asset{}, ErrVersionConflict
			}
			return cur, ErrVersionConflict
		}
		return Asset{}, fmt.Errorf("core: pg store: put: %w", err)
	}
	a.ResourceVersion = v
	a.CreatedAt = created
	a.UpdatedAt = updated
	return a, nil
}

// getAsset: not-found returns (Asset{}, false, nil). Only unmarshal
// the spec column when it actually holds bytes (an empty
// []byte would otherwise return an unmarshal error).
func getAsset(ctx context.Context, q querier, path string) (Asset, bool, error) {
	row := q.QueryRow(ctx, `
		SELECT path, resource_version, spec, created_at, updated_at
		  FROM assets WHERE path = $1
	`, path)
	var a Asset
	var specBytes []byte
	if err := row.Scan(&a.Path, &a.ResourceVersion, &specBytes, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, false, nil
		}
		return Asset{}, false, fmt.Errorf("core: pg store: get: %w", err)
	}
	if len(specBytes) > 0 {
		if err := json.Unmarshal(specBytes, &a.Spec); err != nil {
			return Asset{}, false, fmt.Errorf("core: pg store: get: unmarshal spec: %w", err)
		}
	}
	return a, true, nil
}

// listAssets returns assets in path order. An empty table yields
// a non-nil empty slice so the JSON encoding is `[]` (matching
// fileStore).
func listAssets(ctx context.Context, q querier) ([]Asset, error) {
	rows, err := q.Query(ctx, `
		SELECT path, resource_version, spec, created_at, updated_at
		  FROM assets ORDER BY path ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list: %w", err)
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		var a Asset
		var specBytes []byte
		if err := rows.Scan(&a.Path, &a.ResourceVersion, &specBytes, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: list: %w", err)
		}
		if len(specBytes) > 0 {
			if err := json.Unmarshal(specBytes, &a.Spec); err != nil {
				return nil, fmt.Errorf("core: pg store: list: unmarshal spec: %w", err)
			}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: pg store: list: %w", err)
	}
	return out, nil
}

// deleteAsset mirrors fileStore.hasChildren: a child is any asset
// whose path matches target LIKE target||'.%'. The EXISTS/count/
// DELETE share the caller's transaction (opened by *pgStore) so
// concurrent inserts cannot slip a child between probe and delete.
//
// path LIKE $1 || '.%' is safe because asset paths are constrained
// to ^[a-z0-9.]+$ (pkg/cpath) — no LIKE wildcard chars (_ or %)
// can occur. If that charset ever loosens, switch to
// starts_with(path, $1||'.').
func deleteAsset(ctx context.Context, q querier, path string, cascade bool) (int, error) {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE path = $1)`, path,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("core: pg store: delete: probe: %w", err)
	}

	var nKids int64
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM assets WHERE path LIKE $1`, path+".%",
	).Scan(&nKids); err != nil {
		return 0, fmt.Errorf("core: pg store: delete: kids: %w", err)
	}

	if !exists && nKids == 0 {
		return 0, nil
	}
	if nKids > 0 && !cascade {
		return int(nKids), ErrHasChildren
	}

	tag, err := q.Exec(ctx,
		`DELETE FROM assets WHERE path = $1 OR path LIKE $2`,
		path, path+".%",
	)
	if err != nil {
		return 0, fmt.Errorf("core: pg store: delete: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// listAlarms returns alarms in severity-rank + since-desc order.
// An empty table yields a non-nil empty slice so the JSON encoding
// is `[]` (matching fileStore).
func listAlarms(ctx context.Context, q querier) ([]Alarm, error) {
	rows, err := q.Query(ctx, `
		SELECT id, path, severity, state, summary, since, acked_by, acked_at
		  FROM alarms
		 ORDER BY CASE severity
		           WHEN 'critical' THEN 0
		           WHEN 'major'    THEN 1
		           WHEN 'minor'    THEN 2
		           WHEN 'info'     THEN 3
		           ELSE 4 END ASC,
		          since DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list alarms: %w", err)
	}
	defer rows.Close()
	out := []Alarm{}
	for rows.Next() {
		var a Alarm
		if err := rows.Scan(&a.ID, &a.Path, &a.Severity, &a.State, &a.Summary, &a.Since, &a.AckedBy, &a.AckedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: list alarms: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: pg store: list alarms: %w", err)
	}
	return out, nil
}

// ackAlarm is a single-statement CAS: the WHERE state='firing' guard
// makes concurrent acks race-safe without a transaction (loser sees
// 0 rows and falls into the re-read to distinguish 404 from 409).
func ackAlarm(ctx context.Context, q querier, id, actor string) (Alarm, bool, error) {
	var a Alarm
	err := q.QueryRow(ctx, `
		UPDATE alarms
		   SET state = 'acked', acked_by = $2, acked_at = now()
		 WHERE id = $1 AND state = 'firing'
		RETURNING id, path, severity, state, summary, since, acked_by, acked_at
	`, id, actor).Scan(&a.ID, &a.Path, &a.Severity, &a.State, &a.Summary, &a.Since, &a.AckedBy, &a.AckedAt)
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Alarm{}, false, fmt.Errorf("core: pg store: ack alarm: %w", err)
	}
	// 0 rows: missing vs not-firing.
	err = q.QueryRow(ctx, `
		SELECT id, path, severity, state, summary, since, acked_by, acked_at
		  FROM alarms WHERE id = $1
	`, id).Scan(&a.ID, &a.Path, &a.Severity, &a.State, &a.Summary, &a.Since, &a.AckedBy, &a.AckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Alarm{}, false, nil
	}
	if err != nil {
		return Alarm{}, false, fmt.Errorf("core: pg store: ack alarm re-read: %w", err)
	}
	return a, true, ErrAlarmNotAckable
}

// seedAlarms upserts each input alarm. The "since" column is left
// alone on conflict so an idempotent re-seed preserves the
// original first-seen time. Caller is responsible for opening a
// transaction if atomicity across rows is required (production's
// *pgStore.SeedAlarms does).
func seedAlarms(ctx context.Context, q querier, in []Alarm) error {
	if len(in) == 0 {
		return nil
	}
	for _, a := range in {
		_, err := q.Exec(ctx, `
			INSERT INTO alarms(id, path, severity, state, summary, since)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE
			  SET path     = EXCLUDED.path,
			      severity = EXCLUDED.severity,
			      state    = EXCLUDED.state,
			      summary  = EXCLUDED.summary
		`, a.ID, a.Path, a.Severity, a.State, a.Summary, a.Since)
		if err != nil {
			return fmt.Errorf("core: pg store: seed: %w", err)
		}
	}
	return nil
}

// putTicket writes a ticket. expectVersion==0 → single-statement
// INSERT…ON CONFLICT upsert (full-field overwrite on conflict;
// mirrors the asset path). expectVersion>0 → optimistic UPDATE:
// only succeeds when the in-row resource_version matches; 0
// rows affected is ErrVersionConflict with the current row
// surfaced via a same-querier re-read so the caller can build
// a 409 response. Each successful write advances
// resource_version by 1 (mirrors assets.resource_version,
// PRMT-016b / PRMT-082).
//
// PRMT-081: a 23505 raised on the tickets_alarm_id_active_uniq
// partial-unique index surfaces as ErrDuplicateActiveTicket — a
// racing concurrent insert slipped between the application-layer
// dedup SELECT and this INSERT. Callers (alarm.Store.OpenTicket
// et al.) treat it as a no-op (idempotent skip): the existing
// ticket row IS the authoritative one.
func putTicket(ctx context.Context, q querier, t Ticket, expectVersion int64) (Ticket, error) {
	if expectVersion == 0 {
		row := q.QueryRow(ctx, `
			INSERT INTO tickets(
				id, alarm_id, asset_path, title, severity, state, assignee,
				opened_at, acked_at, resolved_at, closed_at, escalated_at, runbook,
				resource_version
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1)
			ON CONFLICT (id) DO UPDATE
			  SET alarm_id         = EXCLUDED.alarm_id,
			      asset_path       = EXCLUDED.asset_path,
			      title            = EXCLUDED.title,
			      severity         = EXCLUDED.severity,
			      state            = EXCLUDED.state,
			      assignee         = EXCLUDED.assignee,
			      acked_at         = EXCLUDED.acked_at,
			      resolved_at      = EXCLUDED.resolved_at,
			      closed_at        = EXCLUDED.closed_at,
			      escalated_at     = EXCLUDED.escalated_at,
			      runbook          = EXCLUDED.runbook,
			      resource_version = tickets.resource_version + 1
			RETURNING resource_version
		`, t.ID, t.AlarmID, t.AssetPath, t.Title, t.Severity, t.State, t.Assignee,
			t.OpenedAt, nullTime(t.AckedAt), nullTime(t.ResolvedAt), nullTime(t.ClosedAt), nullTime(t.EscalatedAt),
			t.Runbook,
		)
		var v int64
		if err := row.Scan(&v); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "tickets_alarm_id_active_uniq" {
				return Ticket{}, ErrDuplicateActiveTicket
			}
			return Ticket{}, fmt.Errorf("core: pg store: put ticket: %w", err)
		}
		t.ResourceVersion = v
		return t, nil
	}
	// Optimistic path.
	row := q.QueryRow(ctx, `
		UPDATE tickets
		   SET alarm_id         = $2,
		       asset_path       = $3,
		       title            = $4,
		       severity         = $5,
		       state            = $6,
		       assignee         = $7,
		       opened_at        = $8,
		       acked_at         = $9,
		       resolved_at      = $10,
		       closed_at        = $11,
		       escalated_at     = $12,
		       runbook          = $13,
		       resource_version = resource_version + 1
		 WHERE id = $1 AND resource_version = $14
		RETURNING resource_version
	`, t.ID, t.AlarmID, t.AssetPath, t.Title, t.Severity, t.State, t.Assignee,
		t.OpenedAt, nullTime(t.AckedAt), nullTime(t.ResolvedAt), nullTime(t.ClosedAt), nullTime(t.EscalatedAt),
		t.Runbook, expectVersion,
	)
	var v int64
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Re-read on the SAME querier to keep this in the
			// caller's transaction. If the row is missing too,
			// surface a bare ErrVersionConflict.
			cur, _, gerr := getTicket(ctx, q, t.ID)
			if gerr != nil {
				return Ticket{}, ErrVersionConflict
			}
			return cur, ErrVersionConflict
		}
		return Ticket{}, fmt.Errorf("core: pg store: put ticket: %w", err)
	}
	t.ResourceVersion = v
	return t, nil
}

// getTicket: not-found returns (Ticket{}, false, nil).
func getTicket(ctx context.Context, q querier, id string) (Ticket, bool, error) {
	row := q.QueryRow(ctx, `
		SELECT id, alarm_id, asset_path, title, severity, state, assignee,
		       opened_at, acked_at, resolved_at, closed_at, escalated_at, runbook,
		       resource_version
		  FROM tickets WHERE id = $1
	`, id)
	var t Ticket
	var acked, resolved, closed, escalated sql.NullTime
	if err := row.Scan(
		&t.ID, &t.AlarmID, &t.AssetPath, &t.Title, &t.Severity, &t.State, &t.Assignee,
		&t.OpenedAt, &acked, &resolved, &closed, &escalated, &t.Runbook, &t.ResourceVersion,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, false, nil
		}
		return Ticket{}, false, fmt.Errorf("core: pg store: get ticket: %w", err)
	}
	t.AckedAt = timePtr(acked)
	t.ResolvedAt = timePtr(resolved)
	t.ClosedAt = timePtr(closed)
	t.EscalatedAt = timePtr(escalated)
	return t, true, nil
}

// listTickets returns tickets in OpenedAt desc order. An empty
// table yields a non-nil empty slice so the JSON encoding is `[]`
// (matching fileStore).
func listTickets(ctx context.Context, q querier) ([]Ticket, error) {
	rows, err := q.Query(ctx, `
		SELECT id, alarm_id, asset_path, title, severity, state, assignee,
		       opened_at, acked_at, resolved_at, closed_at, escalated_at, runbook,
		       resource_version
		  FROM tickets
		 ORDER BY opened_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list tickets: %w", err)
	}
	defer rows.Close()
	out := []Ticket{}
	for rows.Next() {
		var t Ticket
		var acked, resolved, closed, escalated sql.NullTime
		if err := rows.Scan(
			&t.ID, &t.AlarmID, &t.AssetPath, &t.Title, &t.Severity, &t.State, &t.Assignee,
			&t.OpenedAt, &acked, &resolved, &closed, &escalated, &t.Runbook, &t.ResourceVersion,
		); err != nil {
			return nil, fmt.Errorf("core: pg store: list tickets: %w", err)
		}
		t.AckedAt = timePtr(acked)
		t.ResolvedAt = timePtr(resolved)
		t.ClosedAt = timePtr(closed)
		t.EscalatedAt = timePtr(escalated)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: pg store: list tickets: %w", err)
	}
	return out, nil
}

// --- PM schedule (PRMT-043) ----------------------------------------------

// putPMSchedule upserts by ID. Matches PutTicket's idempotent
// semantics so re-applying a stored schedule is safe.
func putPMSchedule(ctx context.Context, q querier, p PMSchedule) error {
	_, err := q.Exec(ctx, `
		INSERT INTO pm_schedules (id, asset_path, kind, interval_days,
		                          last_run, next_due, title, severity, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
		    asset_path    = EXCLUDED.asset_path,
		    kind          = EXCLUDED.kind,
		    interval_days = EXCLUDED.interval_days,
		    last_run      = EXCLUDED.last_run,
		    next_due      = EXCLUDED.next_due,
		    title         = EXCLUDED.title,
		    severity      = EXCLUDED.severity,
		    enabled       = EXCLUDED.enabled
	`, p.ID, p.AssetPath, p.Kind, p.IntervalDays,
		nullTime(p.LastRun), p.NextDue, p.Title, p.Severity, p.Enabled)
	if err != nil {
		return fmt.Errorf("core: pg store: put pm schedule: %w", err)
	}
	return nil
}

func getPMSchedule(ctx context.Context, q querier, id string) (PMSchedule, bool, error) {
	var p PMSchedule
	var lastRun sql.NullTime
	err := q.QueryRow(ctx, `
		SELECT id, asset_path, kind, interval_days, last_run, next_due,
		       title, severity, enabled
		  FROM pm_schedules WHERE id = $1
	`, id).Scan(&p.ID, &p.AssetPath, &p.Kind, &p.IntervalDays,
		&lastRun, &p.NextDue, &p.Title, &p.Severity, &p.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PMSchedule{}, false, nil
		}
		return PMSchedule{}, false, fmt.Errorf("core: pg store: get pm schedule: %w", err)
	}
	p.LastRun = timePtr(lastRun)
	return p, true, nil
}

func listPMSchedules(ctx context.Context, q querier) ([]PMSchedule, error) {
	rows, err := q.Query(ctx, `
		SELECT id, asset_path, kind, interval_days, last_run, next_due,
		       title, severity, enabled
		  FROM pm_schedules
		 ORDER BY next_due ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list pm schedules: %w", err)
	}
	defer rows.Close()
	out := []PMSchedule{}
	for rows.Next() {
		var p PMSchedule
		var lastRun sql.NullTime
		if err := rows.Scan(&p.ID, &p.AssetPath, &p.Kind, &p.IntervalDays,
			&lastRun, &p.NextDue, &p.Title, &p.Severity, &p.Enabled); err != nil {
			return nil, fmt.Errorf("core: pg store: scan pm schedule: %w", err)
		}
		p.LastRun = timePtr(lastRun)
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Asset audit (PRMT-045) ----------------------------------------------

// appendAssetAudit inserts one audit row. Append-only by design:
// there is no update/delete path on asset_audit (audit integrity
// per Implementation Plan §E2.1 "保留期/签名 = M3"). Failure
// is propagated to the caller (the HTTP layer treats it as
// best-effort and logs).
func appendAssetAudit(ctx context.Context, q querier, a AssetAudit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO asset_audit (id, ts, principal, path, op, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, a.ID, a.TS, a.Principal, a.Path, a.Op, a.Detail)
	if err != nil {
		return fmt.Errorf("core: pg store: append asset audit: %w", err)
	}
	return nil
}

// listAssetAudits returns audit rows for one path in TS desc
// order. Empty / unknown path → non-nil empty slice.
func listAssetAudits(ctx context.Context, q querier, path string) ([]AssetAudit, error) {
	rows, err := q.Query(ctx, `
		SELECT id, ts, principal, path, op, detail
		  FROM asset_audit
		 WHERE path = $1
		 ORDER BY ts DESC, id ASC
	`, path)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list asset audits: %w", err)
	}
	defer rows.Close()
	out := []AssetAudit{}
	for rows.Next() {
		var a AssetAudit
		if err := rows.Scan(&a.ID, &a.TS, &a.Principal, &a.Path, &a.Op, &a.Detail); err != nil {
			return nil, fmt.Errorf("core: pg store: scan asset audit: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Inspection SQL helpers (PRMT-049) -----------------------------------

// putInspectionTemplate upserts by ID. items is a JSON array of
// strings, serialised with the Go json package; the column type
// is TEXT so a malformed stored value will surface as a Go-side
// unmarshal error in getInspectionTemplate, not a SQL parse
// error. interval_ns is BIGINT (nanoseconds) so the round-trip
// is lossless for any time.Duration.
func putInspectionTemplate(ctx context.Context, q querier, it InspectionTemplate) error {
	itemsBytes, err := json.Marshal(it.Items)
	if err != nil {
		return fmt.Errorf("core: pg store: marshal items: %w", err)
	}
	intervalNs := int64(it.Interval)
	_, err = q.Exec(ctx, `
		INSERT INTO inspection_templates (id, asset_path, title, items, interval_ns, next_due, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
		    asset_path  = EXCLUDED.asset_path,
		    title       = EXCLUDED.title,
		    items       = EXCLUDED.items,
		    interval_ns = EXCLUDED.interval_ns,
		    next_due    = EXCLUDED.next_due,
		    enabled     = EXCLUDED.enabled
	`, it.ID, it.AssetPath, it.Title, string(itemsBytes), intervalNs, it.NextDue, it.Enabled)
	if err != nil {
		return fmt.Errorf("core: pg store: put inspection template: %w", err)
	}
	return nil
}

// getInspectionTemplate: not-found returns
// (InspectionTemplate{}, false, nil).
func getInspectionTemplate(ctx context.Context, q querier, id string) (InspectionTemplate, bool, error) {
	var it InspectionTemplate
	var itemsStr string
	var intervalNs int64
	err := q.QueryRow(ctx, `
		SELECT id, asset_path, title, items, interval_ns, next_due, enabled
		  FROM inspection_templates WHERE id = $1
	`, id).Scan(&it.ID, &it.AssetPath, &it.Title, &itemsStr, &intervalNs, &it.NextDue, &it.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InspectionTemplate{}, false, nil
		}
		return InspectionTemplate{}, false, fmt.Errorf("core: pg store: get inspection template: %w", err)
	}
	if itemsStr != "" {
		if err := json.Unmarshal([]byte(itemsStr), &it.Items); err != nil {
			return InspectionTemplate{}, false, fmt.Errorf("core: pg store: get inspection template unmarshal items: %w", err)
		}
	}
	it.Interval = time.Duration(intervalNs)
	return it, true, nil
}

// listInspectionTemplates returns templates in next_due asc
// order (soonest-due first). Empty table → non-nil empty slice.
func listInspectionTemplates(ctx context.Context, q querier) ([]InspectionTemplate, error) {
	rows, err := q.Query(ctx, `
		SELECT id, asset_path, title, items, interval_ns, next_due, enabled
		  FROM inspection_templates
		 ORDER BY next_due ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list inspection templates: %w", err)
	}
	defer rows.Close()
	out := []InspectionTemplate{}
	for rows.Next() {
		var it InspectionTemplate
		var itemsStr string
		var intervalNs int64
		if err := rows.Scan(&it.ID, &it.AssetPath, &it.Title, &itemsStr, &intervalNs, &it.NextDue, &it.Enabled); err != nil {
			return nil, fmt.Errorf("core: pg store: scan inspection template: %w", err)
		}
		if itemsStr != "" {
			if err := json.Unmarshal([]byte(itemsStr), &it.Items); err != nil {
				return nil, fmt.Errorf("core: pg store: list inspection templates unmarshal items: %w", err)
			}
		}
		it.Interval = time.Duration(intervalNs)
		out = append(out, it)
	}
	return out, rows.Err()
}

// nullTime returns a sql.NullTime usable as a $N arg. We use this
// rather than passing a *time.Time directly so a nil pointer
// encodes as SQL NULL (the driver maps *time.Time to TIMESTAMPTZ
// but a nil *time.Time is already NULL; this helper exists for
// clarity at every call site).
func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// --- Spare (PRMT-048) ---------------------------------------------------

// putSpare upserts by ID. SKU uniqueness is the schema's job —
// a duplicate INSERT raises a pgconn.PgError with Code 23505
// which we surface as ErrSKUExists so the HTTP layer can 422.
func putSpare(ctx context.Context, q querier, sp SparePart) error {
	_, err := q.Exec(ctx, `
		INSERT INTO spare_parts (id, sku, name, qty, min_qty, location)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
		    sku      = EXCLUDED.sku,
		    name     = EXCLUDED.name,
		    qty      = EXCLUDED.qty,
		    min_qty  = EXCLUDED.min_qty,
		    location = EXCLUDED.location
	`, sp.ID, sp.SKU, sp.Name, sp.Qty, sp.MinQty, sp.Location)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrSKUExists
		}
		return fmt.Errorf("core: pg store: put spare: %w", err)
	}
	return nil
}

// getSpare: not-found returns (SparePart{}, false, nil).
func getSpare(ctx context.Context, q querier, id string) (SparePart, bool, error) {
	var sp SparePart
	err := q.QueryRow(ctx, `
		SELECT id, sku, name, qty, min_qty, location
		  FROM spare_parts WHERE id = $1
	`, id).Scan(&sp.ID, &sp.SKU, &sp.Name, &sp.Qty, &sp.MinQty, &sp.Location)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SparePart{}, false, nil
		}
		return SparePart{}, false, fmt.Errorf("core: pg store: get spare: %w", err)
	}
	return sp, true, nil
}

// listSpares returns spares in ID order. Empty table yields a
// non-nil empty slice so the JSON encoding is `[]`.
func listSpares(ctx context.Context, q querier) ([]SparePart, error) {
	rows, err := q.Query(ctx, `
		SELECT id, sku, name, qty, min_qty, location
		  FROM spare_parts ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list spares: %w", err)
	}
	defer rows.Close()
	out := []SparePart{}
	for rows.Next() {
		var sp SparePart
		if err := rows.Scan(&sp.ID, &sp.SKU, &sp.Name, &sp.Qty, &sp.MinQty, &sp.Location); err != nil {
			return nil, fmt.Errorf("core: pg store: scan spare: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// adjustSpare writes one txn and updates qty atomically. Caller
// (production *pgStore.AdjustSpare) opens the transaction; tests
// call this helper directly inside their own pin.
//
// Two-step read-modify-write: SELECT … FOR UPDATE locks the row
// so a concurrent :adjust cannot drive qty<0 between the check
// and the UPDATE. delta==0 is rejected by the schema CHECK; we
// guard earlier in the HTTP layer for a friendlier 400.
func adjustSpare(ctx context.Context, q querier, id string, delta int, ticketID string, at time.Time) (SparePart, SpareTxn, error) {
	var curQty int
	if err := q.QueryRow(ctx,
		`SELECT qty FROM spare_parts WHERE id = $1 FOR UPDATE`, id,
	).Scan(&curQty); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SparePart{}, SpareTxn{}, fmt.Errorf("core: adjust spare: not found")
		}
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare lock: %w", err)
	}
	if curQty+delta < 0 {
		return SparePart{}, SpareTxn{}, ErrInsufficientStock
	}
	if _, err := q.Exec(ctx,
		`UPDATE spare_parts SET qty = qty + $2 WHERE id = $1`, id, delta,
	); err != nil {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare update: %w", err)
	}
	txn := SpareTxn{
		ID:       newSpareTxnID(),
		SpareID:  id,
		Delta:    delta,
		TicketID: ticketID,
		At:       at,
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO spare_txns (id, spare_id, delta, ticket_id, at)
		VALUES ($1, $2, $3, $4, $5)
	`, txn.ID, txn.SpareID, txn.Delta, txn.TicketID, txn.At); err != nil {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare txn: %w", err)
	}
	// Re-read on the SAME querier so the caller sees its own write
	// (FOR UPDATE + the UPDATE both run inside the caller's tx).
	sp, _, err := getSpare(ctx, q, id)
	if err != nil {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: pg store: adjust spare reread: %w", err)
	}
	return sp, txn, nil
}

// listSpareTxns returns the txn log for one spare_id, newest-first.
// Empty / unknown id → non-nil empty slice.
func listSpareTxns(ctx context.Context, q querier, spareID string) ([]SpareTxn, error) {
	rows, err := q.Query(ctx, `
		SELECT id, spare_id, delta, ticket_id, at
		  FROM spare_txns
		 WHERE spare_id = $1
		 ORDER BY at DESC, id ASC
	`, spareID)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list spare txns: %w", err)
	}
	defer rows.Close()
	out := []SpareTxn{}
	for rows.Next() {
		var t SpareTxn
		if err := rows.Scan(&t.ID, &t.SpareID, &t.Delta, &t.TicketID, &t.At); err != nil {
			return nil, fmt.Errorf("core: pg store: scan spare txn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// timePtr returns a *time.Time from a sql.NullTime (nil when
// !Valid). The inverse of nullTime.
func timePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	v := n.Time
	return &v
}

// --- Ticket notes (PRMT-060) ---------------------------------------------

// appendTicketNote inserts one ticket_notes row. Append-only —
// there is no update/delete helper by design.
func appendTicketNote(ctx context.Context, q querier, n TicketNote) error {
	_, err := q.Exec(ctx, `
		INSERT INTO ticket_notes (id, ticket_id, author, body, at)
		VALUES ($1, $2, $3, $4, $5)
	`, n.ID, n.TicketID, n.Author, n.Body, n.At)
	if err != nil {
		return fmt.Errorf("core: pg store: append ticket note: %w", err)
	}
	return nil
}

// listTicketNotes returns notes for one ticket in At ASC order
// (oldest first). Empty / unknown id → non-nil empty slice.
func listTicketNotes(ctx context.Context, q querier, ticketID string) ([]TicketNote, error) {
	rows, err := q.Query(ctx, `
		SELECT id, ticket_id, author, body, at
		  FROM ticket_notes
		 WHERE ticket_id = $1
		 ORDER BY at ASC, id ASC
	`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list ticket notes: %w", err)
	}
	defer rows.Close()
	out := []TicketNote{}
	for rows.Next() {
		var n TicketNote
		if err := rows.Scan(&n.ID, &n.TicketID, &n.Author, &n.Body, &n.At); err != nil {
			return nil, fmt.Errorf("core: pg store: scan ticket note: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// updateTicketAssignee mutates only the assignee column on the
// named ticket. 0 rows updated → ticket missing → (Ticket{}, false,
// nil). 1 row updated → re-read on the SAME querier so the caller
// sees the canonical row (opened_at, severity, etc. are not
// touched).
func updateTicketAssignee(ctx context.Context, q querier, ticketID, assignee string) (Ticket, bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tickets SET assignee = $2 WHERE id = $1
	`, ticketID, assignee)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("core: pg store: update ticket assignee: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Ticket{}, false, nil
	}
	return getTicket(ctx, q, ticketID)
}

// --- Ticket audit helpers (PRMT-061) ------------------------------------

// appendTicketAudit inserts one ticket_audit row. Append-only by
// design — there is no update/delete helper on this table.
func appendTicketAudit(ctx context.Context, q querier, a TicketAudit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO ticket_audit (id, ticket_id, op, from_state, to_state, who, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, a.ID, a.TicketID, a.Op, nullStr(a.FromState), nullStr(a.ToState), a.Who, a.At)
	if err != nil {
		return fmt.Errorf("core: pg store: append ticket audit: %w", err)
	}
	return nil
}

// listTicketAudits returns audit rows for one ticket in At ASC
// order (oldest first). Empty / unknown id → non-nil empty slice.
func listTicketAudits(ctx context.Context, q querier, ticketID string) ([]TicketAudit, error) {
	rows, err := q.Query(ctx, `
		SELECT id, ticket_id, op, from_state, to_state, who, at
		  FROM ticket_audit
		 WHERE ticket_id = $1
		 ORDER BY at ASC, id ASC
	`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list ticket audits: %w", err)
	}
	defer rows.Close()
	out := []TicketAudit{}
	for rows.Next() {
		var a TicketAudit
		var fromState, toState sql.NullString
		if err := rows.Scan(&a.ID, &a.TicketID, &a.Op, &fromState, &toState, &a.Who, &a.At); err != nil {
			return nil, fmt.Errorf("core: pg store: scan ticket audit: %w", err)
		}
		if fromState.Valid {
			a.FromState = fromState.String
		}
		if toState.Valid {
			a.ToState = toState.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Set audit helpers (PRMT-234) ---------------------------------------

// appendSetAuditPG inserts one set_audit row. Append-only by design.
func appendSetAuditPG(ctx context.Context, q querier, a SetAudit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO set_audit (id, ts, path, risk_class, value, actor,
		                       second_approver, readback_required, note, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, a.ID, a.At, a.Path, string(a.Class), a.Value, a.Actor,
		a.Second, a.Readback, a.Note, a.RequestID)
	if err != nil {
		return fmt.Errorf("core: pg store: append set audit: %w", err)
	}
	return nil
}

// listSetAuditsPG returns all set_audit rows newest-first (ts DESC,
// id DESC) — same order as fileStore.ListSetAudits.
func listSetAuditsPG(ctx context.Context, q querier) ([]SetAudit, error) {
	rows, err := q.Query(ctx, `
		SELECT id, ts, path, risk_class, value, actor,
		       second_approver, readback_required, note, request_id
		  FROM set_audit
		 ORDER BY ts DESC, id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list set audits: %w", err)
	}
	defer rows.Close()
	out := []SetAudit{}
	for rows.Next() {
		var a SetAudit
		var class string
		if err := rows.Scan(&a.ID, &a.At, &a.Path, &class, &a.Value, &a.Actor,
			&a.Second, &a.Readback, &a.Note, &a.RequestID); err != nil {
			return nil, fmt.Errorf("core: pg store: scan set audit: %w", err)
		}
		a.Class = RiskClass(class)
		out = append(out, a)
	}
	return out, rows.Err()
}

// nullStr returns a sql.NullString usable as a $N arg; empty
// string encodes as SQL NULL so the CHECK column accepts it
// without rejecting "" as a non-NULL value.
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// --- MaintenanceWindow SQL helpers (PRMT-096) ---------------------------

// putMaintenanceWindow upserts by ID. Mirrors putInspectionTemplate
// (full-field overwrite on conflict).
func putMaintenanceWindow(ctx context.Context, q querier, m MaintenanceWindow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO maintenance_windows (id, asset_path, starts_at, ends_at, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
		    asset_path = EXCLUDED.asset_path,
		    starts_at  = EXCLUDED.starts_at,
		    ends_at    = EXCLUDED.ends_at,
		    reason     = EXCLUDED.reason
	`, m.ID, m.AssetPath, m.StartsAt, m.EndsAt, m.Reason)
	if err != nil {
		return fmt.Errorf("core: pg store: put maintenance window: %w", err)
	}
	return nil
}

// getMaintenanceWindow: not-found returns (MaintenanceWindow{}, false, nil).
func getMaintenanceWindow(ctx context.Context, q querier, id string) (MaintenanceWindow, bool, error) {
	var m MaintenanceWindow
	err := q.QueryRow(ctx, `
		SELECT id, asset_path, starts_at, ends_at, reason
		  FROM maintenance_windows WHERE id = $1
	`, id).Scan(&m.ID, &m.AssetPath, &m.StartsAt, &m.EndsAt, &m.Reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MaintenanceWindow{}, false, nil
		}
		return MaintenanceWindow{}, false, fmt.Errorf("core: pg store: get maintenance window: %w", err)
	}
	return m, true, nil
}

// listMaintenanceWindows returns windows in StartsAt asc order
// (soonest-starting first). Empty table → non-nil empty slice.
func listMaintenanceWindows(ctx context.Context, q querier) ([]MaintenanceWindow, error) {
	rows, err := q.Query(ctx, `
		SELECT id, asset_path, starts_at, ends_at, reason
		  FROM maintenance_windows
		 ORDER BY starts_at ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list maintenance windows: %w", err)
	}
	defer rows.Close()
	out := []MaintenanceWindow{}
	for rows.Next() {
		var m MaintenanceWindow
		if err := rows.Scan(&m.ID, &m.AssetPath, &m.StartsAt, &m.EndsAt, &m.Reason); err != nil {
			return nil, fmt.Errorf("core: pg store: scan maintenance window: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// deleteMaintenanceWindow removes by ID. Returns (true, nil) when
// a row was actually deleted, (false, nil) on a no-op miss — the
// HTTP handler turns that into 404 (mirrors PutSpare's pattern).
func deleteMaintenanceWindow(ctx context.Context, q querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM maintenance_windows WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("core: pg store: delete maintenance window: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// activeWindowFor is the single-round-trip probe invoked by
// cios-alarm on every firing event (PRMT-096 §2 / §4). It returns
// the first window whose [starts_at, ends_at) contains now AND
// whose asset_path matches the alarming path (== OR "."-prefixed
// ancestor). The ORDER BY keeps the result deterministic when
// multiple windows overlap on the same asset (rare but legal;
// the operator UI shows all of them, the alarm engine suppresses
// based on whichever comes first).
func activeWindowFor(ctx context.Context, q querier, assetPath string, now time.Time) (MaintenanceWindow, bool, error) {
	var m MaintenanceWindow
	err := q.QueryRow(ctx, `
		SELECT id, asset_path, starts_at, ends_at, reason
		  FROM maintenance_windows
		 WHERE starts_at <= $2
		   AND ends_at   >  $2
		   AND (asset_path = $1 OR asset_path LIKE $1 || '.%')
		 ORDER BY starts_at ASC, id ASC
		 LIMIT 1
	`, assetPath, now).Scan(&m.ID, &m.AssetPath, &m.StartsAt, &m.EndsAt, &m.Reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MaintenanceWindow{}, false, nil
		}
		return MaintenanceWindow{}, false, fmt.Errorf("core: pg store: active window for: %w", err)
	}
	return m, true, nil
}

// --- Tenant / Org / TenantAudit SQL helpers (PRMT-184) -------------------

// getTenant reads one tenant by ID. pgx.ErrNoRows →
// (Tenant{}, false, nil) — mirrors getAsset. Any other error is
// wrapped with the operation name so logs trace the source.
func getTenant(ctx context.Context, q querier, id string) (Tenant, bool, error) {
	row := q.QueryRow(ctx, `
		SELECT id, display_name, isolation_tier, status, created_at, updated_at
		  FROM tenants WHERE id = $1
	`, id)
	var t Tenant
	if err := row.Scan(&t.ID, &t.DisplayName, &t.IsolationTier, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, false, nil
		}
		return Tenant{}, false, fmt.Errorf("core: pg store: get tenant: %w", err)
	}
	return t, true, nil
}

// listTenants returns every tenant in ID ASC order (stable across
// runs; matches List*Tenants' fileStore counterpart). Empty
// table yields []Tenant{} (never nil) so JSON encoding is `[]`.
func listTenants(ctx context.Context, q querier) ([]Tenant, error) {
	rows, err := q.Query(ctx, `
		SELECT id, display_name, isolation_tier, status, created_at, updated_at
		  FROM tenants
		 ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list tenants: %w", err)
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.DisplayName, &t.IsolationTier, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan tenant: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// getOrg reads one org by ID. pgx.ErrNoRows →
// (Org{}, false, nil) — mirrors getAsset.
func getOrg(ctx context.Context, q querier, id string) (Org, bool, error) {
	row := q.QueryRow(ctx, `
		SELECT id, tenant_id, name, created_at
		  FROM orgs WHERE id = $1
	`, id)
	var o Org
	if err := row.Scan(&o.ID, &o.TenantID, &o.Name, &o.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Org{}, false, nil
		}
		return Org{}, false, fmt.Errorf("core: pg store: get org: %w", err)
	}
	return o, true, nil
}

// listOrgs returns the orgs of one tenant in name ASC order
// (stable across runs). Empty / unknown tenant_id yields
// []Org{} (never nil) so JSON encoding is `[]`.
func listOrgs(ctx context.Context, q querier, tenantID string) ([]Org, error) {
	rows, err := q.Query(ctx, `
		SELECT id, tenant_id, name, created_at
		  FROM orgs
		 WHERE tenant_id = $1
		 ORDER BY name ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list orgs: %w", err)
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.TenantID, &o.Name, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan org: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// createOrg implements the Store.CreateOrg contract inside the
// caller's transaction (PRMT-185 §4.1). Server-generated id; the
// SQL UNIQUE (tenant_id, name) surfaces a 23505 we map to
// ErrOrgNameConflict; tenants(id) FK violation surfaces a 23503 we
// map to a wrapped not-found. On success, appends ONE tenant_audit
// op="org_create" detail "<id>:<name>" on the same querier.
func createOrg(ctx context.Context, q querier, tenantID, name, principal string) (Org, error) {
	now := time.Now().UTC()
	o := Org{
		ID:        newOrgID(),
		TenantID:  tenantID,
		Name:      name,
		CreatedAt: now,
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO orgs (id, tenant_id, name, created_at)
		VALUES ($1, $2, $3, $4)
	`, o.ID, o.TenantID, o.Name, o.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505":
				return Org{}, ErrOrgNameConflict
			case "23503":
				return Org{}, fmt.Errorf("core: create org: tenant not found")
			}
		}
		return Org{}, fmt.Errorf("core: pg store: create org: %w", err)
	}
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  tenantID,
		Op:        "org_create",
		Detail:    o.ID + ":" + name,
	}); err != nil {
		return Org{}, err
	}
	return o, nil
}

// renameOrg implements the Store.RenameOrg contract inside the
// caller's transaction (PRMT-185 §4.1). SELECT FOR UPDATE the row
// so a concurrent RenameOrg cannot race the (tenant_id, name) UNIQUE
// check. Equal-name is a no-op with zero audit rows; absent id →
// wrapped not-found; UNIQUE 23505 → ErrOrgNameConflict.
func renameOrg(ctx context.Context, q querier, id, newName, principal string) error {
	var (
		curTenant string
		curName   string
	)
	if err := q.QueryRow(ctx,
		`SELECT tenant_id, name FROM orgs WHERE id = $1 FOR UPDATE`, id,
	).Scan(&curTenant, &curName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: rename org: not found")
		}
		return fmt.Errorf("core: pg store: rename org lock: %w", err)
	}
	if curName == newName {
		// Idempotent no-op (mirrors UpdateTenantTier equal-tier branch).
		return nil
	}
	now := time.Now().UTC()
	if _, err := q.Exec(ctx,
		`UPDATE orgs SET name = $2 WHERE id = $1`, id, newName,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrOrgNameConflict
		}
		return fmt.Errorf("core: pg store: rename org: %w", err)
	}
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  curTenant,
		Op:        "org_rename",
		Detail:    curName + "→" + newName,
	}); err != nil {
		return err
	}
	return nil
}

// deleteOrg implements the Store.DeleteOrg contract inside the
// caller's transaction (PRMT-185 §4.1, R5). SELECT FOR UPDATE the
// org row, then count site_orgs by org_id, then either refuse
// (ErrOrgOwnsResources, NO delete/audit) or DELETE + ONE tenant_audit
// op="org_delete" detail "<name>". The check uses the same querier
// as the delete so the snapshot is consistent inside the tx.
func deleteOrg(ctx context.Context, q querier, id, principal string) error {
	var (
		curTenant string
		curName   string
	)
	if err := q.QueryRow(ctx,
		`SELECT tenant_id, name FROM orgs WHERE id = $1 FOR UPDATE`, id,
	).Scan(&curTenant, &curName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: delete org: not found")
		}
		return fmt.Errorf("core: pg store: delete org lock: %w", err)
	}
	var n int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM site_orgs WHERE org_id = $1`, id,
	).Scan(&n); err != nil {
		return fmt.Errorf("core: pg store: delete org count: %w", err)
	}
	if n > 0 {
		return ErrOrgOwnsResources
	}
	now := time.Now().UTC()
	if _, err := q.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, id); err != nil {
		return fmt.Errorf("core: pg store: delete org: %w", err)
	}
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  curTenant,
		Op:        "org_delete",
		Detail:    curName,
	}); err != nil {
		return err
	}
	return nil
}

// appendTenantAudit inserts one tenant_audit row. Append-only by
// design: there is no update/delete path on tenant_audit (audit
// integrity per spec-001 v1.1 §5bis). Failure is propagated to
// the caller; the same pattern is used by appendAssetAudit.
func appendTenantAudit(ctx context.Context, q querier, a TenantAudit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tenant_audit (id, ts, principal, tenant_id, op, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, a.ID, a.TS, a.Principal, a.TenantID, a.Op, a.Detail)
	if err != nil {
		return fmt.Errorf("core: pg store: append tenant audit: %w", err)
	}
	return nil
}

// updateTenantTier implements the Store.UpdateTenantTier contract
// inside the caller's transaction (PRMT-182). targetRank is the
// caller's already-validated rank for target; the helper still
// re-reads isolation_tier under SELECT FOR UPDATE so it sees the
// authoritative current rank and can refuse a downgrade atomically.
// Returns ErrTierDowngrade (with one REFUSED tenant_audit row
// already written) when targetRank < curRank; tenantTierValidationError
// when the stored tier is outside the {label,row,db} allowlist
// (store corruption); a wrapped not-found when the row vanished.
// Equal ranks return nil with zero audit rows; upgrade writes one
// tenant_audit row and updates tenants.isolation_tier + updated_at.
func updateTenantTier(ctx context.Context, q querier, id, target string, targetRank int, principal string) error {
	var curTier string
	if err := q.QueryRow(ctx,
		`SELECT isolation_tier FROM tenants WHERE id = $1 FOR UPDATE`, id,
	).Scan(&curTier); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: update tenant tier: not found")
		}
		return fmt.Errorf("core: pg store: update tenant tier lock: %w", err)
	}
	curRank, ok := tenantTierRank(curTier)
	if !ok {
		// Stored tier outside allowlist = store corruption. Surface
		// as not-found so the caller treats the row as unusable
		// (mirrors the fileStore branch).
		return fmt.Errorf("core: update tenant tier: not found")
	}
	now := time.Now().UTC()
	if targetRank == curRank {
		// Idempotent no-op per PRMT-182 §Resolved #1: no audit row.
		return nil
	}
	if targetRank < curRank {
		// Downgrade refused: write ONE REFUSED audit row, do NOT
		// touch tenants.isolation_tier.
		if err := appendTenantAudit(ctx, q, TenantAudit{
			ID:        newTenantAuditID(),
			TS:        now,
			Principal: principal,
			TenantID:  id,
			Op:        "tier_change",
			Detail:    curTier + "→" + target + " REFUSED",
		}); err != nil {
			return err
		}
		return ErrTierDowngrade
	}
	// Upgrade path: update the row AND append ONE audit row. Both
	// run on the same querier (caller's tx) so a crash leaves
	// either both or neither.
	if _, err := q.Exec(ctx,
		`UPDATE tenants SET isolation_tier = $2, updated_at = $3 WHERE id = $1`,
		id, target, now,
	); err != nil {
		return fmt.Errorf("core: pg store: update tenant tier: %w", err)
	}
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  id,
		Op:        "tier_change",
		Detail:    curTier + "→" + target,
	}); err != nil {
		return err
	}
	return nil
}

// listTenantAudits returns audit rows for one tenant_id in TS
// DESC order (newest first; mirrors listAssetAudits). Empty /
// unknown tenant_id yields []TenantAudit{} (never nil).
func listTenantAudits(ctx context.Context, q querier, tenantID string) ([]TenantAudit, error) {
	rows, err := q.Query(ctx, `
		SELECT id, ts, principal, tenant_id, op, detail
		  FROM tenant_audit
		 WHERE tenant_id = $1
		 ORDER BY ts DESC, id ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list tenant audits: %w", err)
	}
	defer rows.Close()
	out := []TenantAudit{}
	for rows.Next() {
		var a TenantAudit
		if err := rows.Scan(&a.ID, &a.TS, &a.Principal, &a.TenantID, &a.Op, &a.Detail); err != nil {
			return nil, fmt.Errorf("core: pg store: scan tenant audit: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- SiteOrg SQL helpers (PRMT-189, spec-001 v1.1 §5bis.2) ---

// getSiteOrg reads one site→org mapping by site slug. pgx.ErrNoRows
// → (SiteOrg{}, false, nil) — mirrors getOrg / getAsset.
func getSiteOrg(ctx context.Context, q querier, site string) (SiteOrg, bool, error) {
	row := q.QueryRow(ctx, `
		SELECT site, org_id, created_at, updated_at
		  FROM site_orgs WHERE site = $1
	`, site)
	var so SiteOrg
	if err := row.Scan(&so.Site, &so.OrgID, &so.CreatedAt, &so.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SiteOrg{}, false, nil
		}
		return SiteOrg{}, false, fmt.Errorf("core: pg store: get site org: %w", err)
	}
	return so, true, nil
}

// listSiteOrgs returns every site→org mapping in site ASC order
// (stable across runs). Empty table yields []SiteOrg{} (never nil)
// so JSON encoding is `[]`.
func listSiteOrgs(ctx context.Context, q querier) ([]SiteOrg, error) {
	rows, err := q.Query(ctx, `
		SELECT site, org_id, created_at, updated_at
		  FROM site_orgs
		 ORDER BY site ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list site orgs: %w", err)
	}
	defer rows.Close()
	out := []SiteOrg{}
	for rows.Next() {
		var so SiteOrg
		if err := rows.Scan(&so.Site, &so.OrgID, &so.CreatedAt, &so.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan site org: %w", err)
		}
		out = append(out, so)
	}
	return out, rows.Err()
}

// countSitesByOrg returns the exact number of sites mapped to
// orgID (0 when none). Backed by the site_orgs_org_idx index so
// the count is an index-only scan.
func countSitesByOrg(ctx context.Context, q querier, orgID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM site_orgs WHERE org_id = $1`, orgID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("core: pg store: count sites by org: %w", err)
	}
	return n, nil
}

// attachSiteToOrg implements the Store.AttachSiteToOrg contract
// inside the caller's transaction (PRMT-189). It validates the org
// FK by SELECTing orgs.tenant_id, then performs an idempotent
// upsert of site_orgs and writes ONE tenant_audit op='org_reattach'
// row on create or actual re-home. On the idempotent no-op path
// (existing site→same org) neither write runs. On the create /
// re-home path both writes share the caller's transaction so a
// crash leaves either both or neither (mirrors AdjustSpare /
// UpdateTenantTier single-tx semantics).
//
// The 015 CHECK vocabulary is NOT altered; org_reattach is one of
// the seven tokens fixed in migrations/015_tenant_org.sql. PRMT-189
// reuses that token (per architect A1 §8 ruling on R7).
func attachSiteToOrg(ctx context.Context, q querier, site, orgID, principal string) error {
	// 1. Validate the org FK and resolve its TenantID for the audit row.
	var tenantID string
	if err := q.QueryRow(ctx,
		`SELECT tenant_id FROM orgs WHERE id = $1`, orgID,
	).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: attach site to org: not found")
		}
		return fmt.Errorf("core: pg store: attach site to org: org lookup: %w", err)
	}
	// 2. Idempotent no-op: if the site is already mapped to this
	// org, neither write runs (mirrors fileStore branch).
	var curOrgID string
	err := q.QueryRow(ctx,
		`SELECT org_id FROM site_orgs WHERE site = $1`, site,
	).Scan(&curOrgID)
	switch {
	case err == nil:
		if curOrgID == orgID {
			return nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		// First attach — fall through.
	default:
		return fmt.Errorf("core: pg store: attach site to org: site lookup: %w", err)
	}
	now := time.Now().UTC()
	var detail string
	if err == nil {
		detail = site + ": " + curOrgID + "→" + orgID
	} else {
		detail = site + "→" + orgID
	}
	// 3. Upsert site_orgs. ON CONFLICT (site) DO UPDATE keeps the
	// original created_at; updated_at always advances to now().
	if _, err := q.Exec(ctx, `
		INSERT INTO site_orgs (site, org_id, created_at, updated_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (site) DO UPDATE
		  SET org_id     = EXCLUDED.org_id,
		      updated_at = EXCLUDED.updated_at
	`, site, orgID, now); err != nil {
		return fmt.Errorf("core: pg store: attach site to org: upsert: %w", err)
	}
	// 4. Append the single tenant_audit row on the same querier
	// (caller's tx). Op='org_reattach' is in the 015 CHECK list.
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  tenantID,
		Op:        "org_reattach",
		Detail:    detail,
	}); err != nil {
		return err
	}
	return nil
}

// detachSiteFromOrg deletes site_orgs row + one org_reattach audit
// detail "<site>: <orgID>→" (PRMT-220). Unmapped → wrapped not-found.
func detachSiteFromOrg(ctx context.Context, q querier, site, principal string) error {
	var orgID string
	if err := q.QueryRow(ctx,
		`SELECT org_id FROM site_orgs WHERE site = $1 FOR UPDATE`, site,
	).Scan(&orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: detach site from org: not found")
		}
		return fmt.Errorf("core: pg store: detach site from org: lock: %w", err)
	}
	var tenantID string
	if err := q.QueryRow(ctx,
		`SELECT tenant_id FROM orgs WHERE id = $1`, orgID,
	).Scan(&tenantID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("core: pg store: detach site from org: org lookup: %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM site_orgs WHERE site = $1`, site); err != nil {
		return fmt.Errorf("core: pg store: detach site from org: %w", err)
	}
	if tenantID != "" {
		if err := appendTenantAudit(ctx, q, TenantAudit{
			ID:        newTenantAuditID(),
			TS:        time.Now().UTC(),
			Principal: principal,
			TenantID:  tenantID,
			Op:        "org_reattach",
			Detail:    site + ": " + orgID + "→",
		}); err != nil {
			return err
		}
	}
	return nil
}

// deleteTenant removes a tenant with zero orgs (PRMT-220).
// ≥1 org → ErrTenantOwnsOrgs. Else DELETE + tenant_status "deleted".
func deleteTenant(ctx context.Context, q querier, id, principal string) error {
	var exists string
	if err := q.QueryRow(ctx,
		`SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, id,
	).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("core: delete tenant: not found")
		}
		return fmt.Errorf("core: pg store: delete tenant lock: %w", err)
	}
	var n int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM orgs WHERE tenant_id = $1`, id,
	).Scan(&n); err != nil {
		return fmt.Errorf("core: pg store: delete tenant count: %w", err)
	}
	if n > 0 {
		return ErrTenantOwnsOrgs
	}
	if _, err := q.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id); err != nil {
		return fmt.Errorf("core: pg store: delete tenant: %w", err)
	}
	if err := appendTenantAudit(ctx, q, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        time.Now().UTC(),
		Principal: principal,
		TenantID:  id,
		Op:        "tenant_status",
		Detail:    "deleted",
	}); err != nil {
		return err
	}
	return nil
}

// --- RoleBinding SQL helpers (PRMT-190-bis §4.2; spec-004 §6bis, R3) ---

// putRoleBinding upserts one row. ON CONFLICT (subject, scope) DO
// UPDATE advances origin + updated_at only — created_at is
// preserved (the SQL DEFAULT carries it forward). Caller has
// already filled rb.ID, rb.Origin, rb.Subject, rb.Scope and
// validated the non-empty subjects; the helper stamps created_at +
// updated_at to time.Now().UTC() on insert and only updated_at on
// update.
//
// Single-statement / no explicit tx needed (PRMT-016b mirror: a
// single SQL statement is atomic in PG; the conflict branch lives
// inside the same statement).
func putRoleBinding(ctx context.Context, q querier, rb RoleBinding) error {
	now := time.Now().UTC()
	_, err := q.Exec(ctx, `
		INSERT INTO role_bindings (id, subject, scope, origin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (subject, scope) DO UPDATE
		  SET origin     = EXCLUDED.origin,
		      updated_at = EXCLUDED.updated_at
	`, rb.ID, rb.Subject, rb.Scope, rb.Origin, now)
	if err != nil {
		return fmt.Errorf("core: pg store: put role binding: %w", err)
	}
	return nil
}

// listRoleBindings returns the rows for one subject in Scope ASC
// order. Unknown / empty subject yields []RoleBinding{} (never nil).
func listRoleBindings(ctx context.Context, q querier, subject string) ([]RoleBinding, error) {
	rows, err := q.Query(ctx, `
		SELECT id, subject, scope, origin, created_at, updated_at
		  FROM role_bindings
		 WHERE subject = $1
		 ORDER BY scope ASC
	`, subject)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list role bindings: %w", err)
	}
	defer rows.Close()
	out := []RoleBinding{}
	for rows.Next() {
		var rb RoleBinding
		if err := rows.Scan(&rb.ID, &rb.Subject, &rb.Scope, &rb.Origin, &rb.CreatedAt, &rb.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan role binding: %w", err)
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// listAllRoleBindings returns every row in (Subject ASC, Scope ASC)
// order — the stable order PRMT-186 rewrites against. Empty table
// yields []RoleBinding{} (never nil) so the JSON encoding is `[]`.
func listAllRoleBindings(ctx context.Context, q querier) ([]RoleBinding, error) {
	rows, err := q.Query(ctx, `
		SELECT id, subject, scope, origin, created_at, updated_at
		  FROM role_bindings
		 ORDER BY subject ASC, scope ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("core: pg store: list all role bindings: %w", err)
	}
	defer rows.Close()
	out := []RoleBinding{}
	for rows.Next() {
		var rb RoleBinding
		if err := rows.Scan(&rb.ID, &rb.Subject, &rb.Scope, &rb.Origin, &rb.CreatedAt, &rb.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: pg store: scan role binding: %w", err)
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// deleteRoleBinding removes the (subject, scope) row; idempotent
// no-op when absent (rowcount == 0). Mirrors the SQL UNIQUE
// constraint on (subject, scope) in migrations/017_role_bindings.sql.
// Used only by the v1.1 migration to retire legacy rows after the
// replacement crn row is written (PRMT-186 §3 widening).
func deleteRoleBinding(ctx context.Context, q querier, subject, scope string) error {
	_, err := q.Exec(ctx, `
		DELETE FROM role_bindings
		 WHERE subject = $1 AND scope = $2
	`, subject, scope)
	if err != nil {
		return fmt.Errorf("core: pg store: delete role binding: %w", err)
	}
	return nil
}

// UpsertUsage implements Store (PRMT-195). Natural-key ON CONFLICT.
func (s *pgStore) UpsertUsage(ctx context.Context, rec UsageRecord) (UsageRecord, error) {
	if rec.ID == "" {
		rec.ID = newUsageID()
	}
	// On conflict reuse existing id so callers keep a stable PK.
	const q = `
INSERT INTO usage_records (
  id, kind, tenant_id, org_id, site_id, asset_path,
  period_start, period_end, granularity, quantity, unit
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (kind, asset_path, period_start, period_end, granularity) DO UPDATE SET
  tenant_id = EXCLUDED.tenant_id,
  org_id = EXCLUDED.org_id,
  site_id = EXCLUDED.site_id,
  quantity = EXCLUDED.quantity,
  unit = EXCLUDED.unit
RETURNING id, kind, tenant_id, org_id, site_id, asset_path,
  period_start, period_end, granularity, quantity, unit`
	var out UsageRecord
	err := s.pool.QueryRow(ctx, q,
		rec.ID, string(rec.Kind), rec.TenantID, rec.OrgID, rec.SiteID, rec.AssetPath,
		rec.PeriodStart, rec.PeriodEnd, string(rec.Granularity), rec.Quantity, rec.Unit,
	).Scan(
		&out.ID, &out.Kind, &out.TenantID, &out.OrgID, &out.SiteID, &out.AssetPath,
		&out.PeriodStart, &out.PeriodEnd, &out.Granularity, &out.Quantity, &out.Unit,
	)
	if err != nil {
		return UsageRecord{}, fmt.Errorf("core: upsert usage: %w", err)
	}
	return out, nil
}

// ListUsage implements Store (PRMT-195).
func (s *pgStore) ListUsage(ctx context.Context, f UsageListFilter) ([]UsageRecord, error) {
	q := `SELECT id, kind, tenant_id, org_id, site_id, asset_path,
 period_start, period_end, granularity, quantity, unit
FROM usage_records WHERE 1=1`
	args := []any{}
	n := 1
	if f.TenantID != "" {
		q += fmt.Sprintf(" AND tenant_id = $%d", n)
		args = append(args, f.TenantID)
		n++
	}
	if f.SiteID != "" {
		q += fmt.Sprintf(" AND site_id = $%d", n)
		args = append(args, f.SiteID)
		n++
	}
	if f.Kind != "" {
		q += fmt.Sprintf(" AND kind = $%d", n)
		args = append(args, string(f.Kind))
		n++
	}
	if f.Granularity != "" {
		q += fmt.Sprintf(" AND granularity = $%d", n)
		args = append(args, string(f.Granularity))
		n++
	}
	if !f.PeriodEnd.IsZero() {
		q += fmt.Sprintf(" AND period_start < $%d", n)
		args = append(args, f.PeriodEnd)
		n++
	}
	if !f.PeriodStart.IsZero() {
		q += fmt.Sprintf(" AND period_end > $%d", n)
		args = append(args, f.PeriodStart)
		n++
	}
	q += " ORDER BY period_start, asset_path, kind"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("core: list usage: %w", err)
	}
	defer rows.Close()
	out := make([]UsageRecord, 0)
	for rows.Next() {
		var rec UsageRecord
		if err := rows.Scan(
			&rec.ID, &rec.Kind, &rec.TenantID, &rec.OrgID, &rec.SiteID, &rec.AssetPath,
			&rec.PeriodStart, &rec.PeriodEnd, &rec.Granularity, &rec.Quantity, &rec.Unit,
		); err != nil {
			return nil, fmt.Errorf("core: list usage scan: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
