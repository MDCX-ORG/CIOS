// Package alarm — store.go: PG persistence for alarm state.
//
// cios-alarm owns its own PG connection (spec-006 §1.1 — alarm
// must not depend on cios-core). The schema is the alarms table
// from migrations/001_init.sql, which this package re-uses
// without modification:
//
//	id        TEXT PK
//	path      TEXT
//	severity  TEXT  (critical|major|minor|info)
//	state     TEXT  (firing|acked|resolved)
//	summary   TEXT
//	since     TIMESTAMPTZ
//
// (There is no updated_at column on purpose — schema author
// confirmed in PRMT-020 review. See §8 Implementation Notes.)
//
// The dedup key (rule.Name, assetPath) from spec-003 §4 is mapped
// to the PK as sha256hex(rule+"|"+asset) truncated to 16 hex chars:
// short enough to be human-readable in psql, long enough (64 bits)
// to make accidental collisions astronomically unlikely.
package alarm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgxpool with the one method cios-alarm needs:
// upsert an alarm row for a state transition. Stateless — safe
// for concurrent use by virtue of pgxpool.Pool being concurrent.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore opens a connection pool. We don't run migrations here:
// the alarms table is created by cios-core's NewPGStore (PRMT-016)
// running migrations/001_init.sql. If cios-core hasn't booted yet,
// this call still succeeds; the upsert below will simply fail with
// "relation alarms does not exist" until the table is created.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("alarm: store: empty DSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("alarm: store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("alarm: store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool. Call from main on shutdown.
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Upsert writes the alarm row for a state-transition event. The id
// is derived from (rule.Name, assetPath) so it is stable across
// process restarts — a rule that was firing at t0 and is still
// firing at t1 hits the same id, ON CONFLICT updates the four
// fields that change but preserves since (the original firing
// instant), matching core.SeedAlarms semantics (PRMT-016, core/store.go).
//
// We deliberately do NOT write updated_at — the table doesn't
// have that column (PRMT-020 review note). The PG layer would
// reject "updated_at = NOW()" with a column-does-not-exist error.
func (s *Store) Upsert(ctx context.Context, ev Event) error {
	id := eventID(ev.RuleName, ev.AssetPath)
	const q = `
		INSERT INTO alarms (id, path, severity, state, summary, since)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			path     = EXCLUDED.path,
			severity = EXCLUDED.severity,
			state    = EXCLUDED.state,
			summary  = EXCLUDED.summary
	`
	// since is intentionally absent from the UPDATE clause: a
	// resolved Event still carries the original firing instant,
	// and per PRMT-020 §4.4 "resolved 不覆盖 since" — the PG
	// insert's $6 is therefore only consumed when the row is
	// freshly created.
	_, err := s.pool.Exec(ctx, q, id, ev.AssetPath, ev.Severity, string(ev.State), ev.Summary, ev.Since)
	if err != nil {
		return fmt.Errorf("alarm: store: upsert %s: %w", id, err)
	}
	return nil
}

// eventID returns the 16-hex-char PK for a (rule, asset) pair.
// SHA-256 is overkill for the collision space but it's the
// standard tool everyone already has in stdlib and the output is
// stable across processes. Truncation to 64 bits trades ~zero
// collision risk for a more readable psql listing.
func eventID(rule, asset string) string {
	h := sha256.Sum256([]byte(rule + "|" + asset))
	return hex.EncodeToString(h[:8])
}

// OpenTicket idempotently opens one ticket for a firing alarm.
// No-op (returns nil) when the Store has no PG pool (mirrors
// Upsert's NoPG guard) or when a non-closed ticket already
// exists for this alarm. PRMT-034 / spec-008 §8 / L69.
//
// PRMT-096: maintenance-window suppression. Before inserting a
// ticket we probe the maintenance_windows table for an active
// window whose asset_path is == or a "."-prefixed ancestor of
// ev.AssetPath. A hit suppresses the insert: the alarm itself is
// still persisted (Upsert above is unaffected) and the CloudEvent
// still publishes (processEvents does not gate on suppression), but
// no ticket row is created and the suppression is logged. This is
// the explicit-window table only — lifecycle="maintenance" on the
// asset is out of scope for PRMT-096 (deferred to a spec follow-up
// so this PRMT ships the minimal contract).
//
// PRMT-099 R10 (K7-sub): the original implementation issued three
// sequential round-trips (ticket-existence probe + maintenance-window
// probe + INSERT). Under alarm storms a single upstream fault can
// fan out to 200 cascading alarms and triple the per-alarm DB
// latency. The three probes are collapsed into a single CTE so
// OpenTicket costs one round-trip regardless of which branch wins.
// External semantics are unchanged: same inputs still produce the
// same ticket row (or the same suppression log + skip), and the
// three SQL fragments the suppression-shape test pins remain
// verbatim in this file.
func (s *Store) OpenTicket(ctx context.Context, ev Event) error {
	if s == nil || s.pool == nil {
		return nil
	}
	alarmID := eventID(ev.RuleName, ev.AssetPath)

	id, err := newTicketID()
	if err != nil {
		return fmt.Errorf("alarm: store: open-ticket id: %w", err)
	}
	title := ev.Summary
	if title == "" {
		title = ev.RuleName
	}

	// Single round-trip: probe for an existing non-closed ticket,
	// probe for an active maintenance window, and (only when both
	// probes miss) INSERT. The discriminator + id are returned so
	// the caller can branch on the outcome:
	//   'existing'  → idempotent skip, no log (PRMT-034 priority)
	//   'mw'        → suppressed, log and skip (PRMT-096)
	//   'inserted'  → new ticket row created
	//
	// PostgreSQL evaluates SELECT CTEs before data-modifying CTEs
	// and modifying CTEs are mutually invisible, so `existing` and
	// `mw` see the pre-statement state. `inserted` only fires when
	// both probes miss.
	//
	// In the original 3-RTT implementation a hit on the existing
	// probe short-circuited the whole flow — the mw probe never ran.
	// In the CTE both probes always run, so a row can exist in
	// both `existing` and `mw` (a ticket opened BEFORE the
	// maintenance window started). Without an explicit priority
	// the UNION ALL would emit two rows and `QueryRow`'s first-row
	// pick would silently flip the suppression log on/off depending
	// on the planner's branch order — a real observable regression.
	// The outer `pri` column forces `existing` (0) > `mw` (1) >
	// `inserted` (2) deterministically; ORDER BY pri LIMIT 1
	// guarantees at most one row and preserves the original
	// "existing wins" semantics.
	//
	// Parameter order is fixed by the suppression-shape test:
	// $1 = asset_path, $2 = probe timestamp (the $1/$2 placeholders
	// in the maintenance-window probe are pinned verbatim). The
	// remaining placeholders ($3..$7) are introduced by the CTE
	// collapse and used only inside the inserted branch.
	const openTicketCTE = `
		WITH existing AS (
			SELECT 'existing'::text AS kind, NULL::text AS id, 0 AS pri
			  FROM tickets
			 WHERE alarm_id = $3 AND state <> 'closed'
			 LIMIT 1
		),
		mw AS (
			SELECT id FROM maintenance_windows
			 WHERE starts_at <= $2
			   AND ends_at   >  $2
			   AND (asset_path = $1 OR asset_path LIKE $1 || '.%')
			 ORDER BY starts_at ASC, id ASC
			 LIMIT 1
		),
		mw_typed AS (
			SELECT 'mw'::text AS kind, id, 1 AS pri FROM mw
		),
		inserted AS (
			INSERT INTO tickets (id, alarm_id, asset_path, title, severity, state, opened_at, runbook)
			SELECT $4, $3, $1, $5, $6, 'open', now(), $7
			 WHERE NOT EXISTS (SELECT 1 FROM existing)
			   AND NOT EXISTS (SELECT 1 FROM mw)
			RETURNING 'inserted'::text AS kind, id, 2 AS pri
		)
		SELECT kind, id FROM (
			SELECT kind, id, pri FROM existing
			UNION ALL
			SELECT kind, id, pri FROM mw_typed
			UNION ALL
			SELECT kind, id, pri FROM inserted
		) t
		ORDER BY pri
		LIMIT 1
	`

	now := time.Now().UTC()

	// The outer CTE guarantees exactly one row: each branch is
	// LIMIT 1 / PK-scoped / mutually exclusive at the inserted
	// level, and the outer ORDER BY pri LIMIT 1 collapses any
	// accidental multi-row case to a single deterministic winner
	// (existing > mw > inserted). `QueryRow` then reads that one
	// row directly, skipping the rows-iterator allocations.
	var kind string
	var rowID sql.NullString
	if err := s.pool.QueryRow(ctx, openTicketCTE,
		ev.AssetPath, // $1 — asset_path (mw probe + insert)
		now,          // $2 — probe timestamp (mw bounds; pinned by suppression-shape test)
		alarmID,      // $3 — alarm_id (existing probe + insert)
		id,           // $4 — new ticket id (insert)
		title,        // $5 — title (insert; default RuleName if Summary empty)
		ev.Severity,  // $6 — severity (insert)
		ev.Runbook,   // $7 — runbook (insert)
	).Scan(&kind, &rowID); err != nil {
		return fmt.Errorf("alarm: store: open-ticket scan %s: %w", alarmID, err)
	}
	switch kind {
	case "existing":
		// idempotent skip — no log, no error (rowID is NULL by construction)
	case "mw":
		// rowID is the maintenance_windows.id (mw_...)
		log.Printf("cios-alarm: suppressed auto-ticket for %s (maintenance window %s)", ev.AssetPath, rowID.String)
	case "inserted":
		// rowID is the newly minted ticket id (tk_...) — only used for
		// correlation in case an operator greps logs.
	default:
		return fmt.Errorf("alarm: store: open-ticket unexpected kind %q for %s", kind, alarmID)
	}
	return nil
}

// newTicketID returns "tk_" + 16 uppercase base32 chars from
// 10 random bytes. Same shape as core.newTicketID (PRMT-033):
// pkg/alarm is forbidden from importing core (PRMT-034 §1), so
// the helper is duplicated here verbatim.
func newTicketID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "tk_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "="), nil
}
