-- 013_ticket_alarm_checks.sql: severity / state CHECK constraints
-- (PRMT-088, eval L3 schema-layer backstop).
--
-- Goal: enforce the spec-003 (severity) and spec-008 (state) enums
-- at the DB layer so non-API write paths (ad-hoc psql, future
-- background jobs, mistaken bulk loads) cannot insert out-of-set
-- values that the HTTP layer would have rejected. Defense in
-- depth — not a replacement for the Go-side validation.
--
-- Enums (authoritative, do NOT widen without a spec update):
--   severity ∈ {'critical','major','minor','info'}     -- spec-003 §2
--   state    ∈ {'open','acknowledged','resolved','closed'}  -- spec-008 §2
--
-- Idempotency: PostgreSQL does not support ADD CONSTRAINT IF NOT
-- EXISTS, so each CHECK is wrapped in a DO $$ ... EXCEPTION /
-- pg_constraint lookup. The first pattern (pg_constraint probe)
-- is preferred because it leaves a clean error path if the
-- migration ever runs against a pre-populated database with
-- out-of-set values (the ALTER TABLE will fail with a 23514 and
-- the operator can clean the data before re-running).
--
-- Pre-existing data note: if any row in tickets/alarms already
-- carries a value outside the enumerated set, ALTER TABLE ... ADD
-- CONSTRAINT will fail (PostgreSQL scans the full table before
-- adding the CHECK). At M2 the library is empty/controlled so this
-- is acceptable; production deployment will need a pre-migration
-- SELECT to surface any offending rows first.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'tickets_severity_check'
    ) THEN
        ALTER TABLE tickets
            ADD CONSTRAINT tickets_severity_check
            CHECK (severity IN ('critical','major','minor','info'));
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'tickets_state_check'
    ) THEN
        ALTER TABLE tickets
            ADD CONSTRAINT tickets_state_check
            CHECK (state IN ('open','acknowledged','resolved','closed'));
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'alarms_severity_check'
    ) THEN
        ALTER TABLE alarms
            ADD CONSTRAINT alarms_severity_check
            CHECK (severity IN ('critical','major','minor','info'));
    END IF;
END
$$;
