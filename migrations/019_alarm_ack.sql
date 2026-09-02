-- 019_alarm_ack.sql: alarm ack metadata (PRMT-230; spec-003 §4 firing→acked).
-- Idempotent (IF NOT EXISTS). acked_by = principal subject that acked;
-- acked_at = when. Both empty/NULL while firing.

ALTER TABLE alarms ADD COLUMN IF NOT EXISTS acked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE alarms ADD COLUMN IF NOT EXISTS acked_at TIMESTAMPTZ;
