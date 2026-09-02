-- 014_maintenance_windows.sql: explicit maintenance-window table
-- (PRMT-096, E2.4 P532 / T42, spec-008 §4.2).
--
-- Goal: store operator-declared maintenance windows so the alarm
-- engine (cios-alarm) can suppress automatic ticket creation while
-- the asset is under maintenance. The table is the single source of
-- truth shared between cios-core (writes via /v1/maintenance/windows)
-- and cios-alarm (reads via SELECT in OpenTicket). Both services
-- already share the alarms / tickets tables; this is one more table
-- on the same DSN, no new infra.
--
-- Schema notes:
--   id         TEXT PK; "mw_" + 16 base32 chars (mirror newTicketID)
--   asset_path TEXT  ; crn of the target asset (or ancestor; the
--                     match uses prefix, see README §3 of PRMT-096)
--   starts_at  TIMESTAMPTZ ; window start (inclusive)
--   ends_at    TIMESTAMPTZ ; window end   (exclusive — now ∈ [start,end)
--                     matches the §2 active-window definition)
--   reason     TEXT NOT NULL DEFAULT '' ; human-readable note; never NULL
--                                                  so pgx scan -> string is
--                                                  safe (CHECK on row's
--                                                  existence + the column
--                                                  default covers empties).
--
-- Indexes:
--   maintenance_windows_active_idx is a btree on (starts_at, ends_at) so the
--     "now ∈ [start,end)" probe that ActiveWindowFor runs on every alarm
--     event is a single range scan (the planner also needs asset_path
--     in the predicate, but the index narrows the candidate set
--     dramatically on a busy site with thousands of windows).
--   maintenance_windows_asset_idx is a btree on (asset_path) so the
--     list endpoint can page-by-id cheaply.
--   maintenance_windows_probe_idx is a btree on (asset_path, starts_at,
--     ends_at) — PRMT-096 R2 F4: supports the ActiveWindowFor probe
--     predicate `starts_at <= $2 AND ends_at > $2 AND (asset_path = $1 OR
--     asset_path LIKE $1 || '.%')` directly, so the planner can seek
--     by asset_path and range-scan the time window without a full
--     scan + LIKE filter. Cheaper than the (starts_at, ends_at) index
--     once the asset_path predicate is selective.

CREATE TABLE IF NOT EXISTS maintenance_windows (
    id         TEXT PRIMARY KEY,
    asset_path TEXT NOT NULL,
    starts_at  TIMESTAMPTZ NOT NULL,
    ends_at    TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    CHECK (ends_at > starts_at)
);

CREATE INDEX IF NOT EXISTS maintenance_windows_active_idx
    ON maintenance_windows (starts_at, ends_at);

CREATE INDEX IF NOT EXISTS maintenance_windows_asset_idx
    ON maintenance_windows (asset_path);

CREATE INDEX IF NOT EXISTS maintenance_windows_probe_idx
    ON maintenance_windows (asset_path, starts_at, ends_at);