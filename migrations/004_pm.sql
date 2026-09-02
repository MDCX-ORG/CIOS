-- 004_pm.sql: pm_schedules table (M2 E2.4 P531 / PRMT-043).
--
-- Idempotent: CREATE TABLE IF NOT EXISTS, so re-running the
-- migration on a populated database is a no-op. The scanner
-- (core/pm.go) opens a ticket when now >= next_due AND enabled.
-- Calendar-only triggers in M2; meter (runhours) is stubbed per
-- spec-008 v0.3 Q12.

CREATE TABLE IF NOT EXISTS pm_schedules (
    id            TEXT PRIMARY KEY,
    asset_path    TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'calendar',
    interval_days INTEGER NOT NULL CHECK (interval_days > 0),
    last_run      TIMESTAMPTZ,
    next_due      TIMESTAMPTZ NOT NULL,
    title         TEXT NOT NULL,
    severity      TEXT NOT NULL CHECK (severity IN ('critical','major','minor','info')),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS pm_schedules_due_idx
    ON pm_schedules (next_due ASC)
    WHERE enabled = TRUE;