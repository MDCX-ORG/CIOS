-- 008_inspection.sql: inspection_templates (M2 E2.7 P561 / PRMT-049).
--
-- Idempotent: CREATE TABLE IF NOT EXISTS, so re-running the
-- migration on a populated database is a no-op. The scanner
-- (core/inspection.go) opens a ticket when now >= next_due AND
-- enabled. Calendar-only triggers in M2; meter / runhours is
-- out of scope per the prompt (mirror PM spec-008 v0.3 Q12).
--
-- items is a JSON array of strings (the checklist). It is
-- carried through to the ticket's Runbook field at fire time
-- (no ticket schema change, per PRMT-049 §4 MUST NOT).
-- next_due index supports the scanner's "due-soonest-first"
-- walk.

CREATE TABLE IF NOT EXISTS inspection_templates (
    id         TEXT PRIMARY KEY,
    asset_path TEXT NOT NULL,
    title      TEXT NOT NULL,
    items      TEXT NOT NULL DEFAULT '[]',
    interval_ns BIGINT NOT NULL CHECK (interval_ns > 0),
    next_due   TIMESTAMPTZ NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS inspection_templates_due_idx
    ON inspection_templates (next_due ASC)
    WHERE enabled = TRUE;
