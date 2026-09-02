-- CIOS core store: SLA escalation column on tickets (M2 E2.3 P504).
-- Idempotent (IF NOT EXISTS) so re-applying the migration is safe.
-- A NULL escalated_at means "never breached"; the first breach sets it
-- to the breach time and the scanner short-circuits on subsequent
-- ticks (PRMT-036 spec-008 §3).

ALTER TABLE tickets ADD COLUMN IF NOT EXISTS escalated_at TIMESTAMPTZ;
