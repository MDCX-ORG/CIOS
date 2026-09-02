-- 005_ticket_runbook.sql: ticket.runbook knowledge-base key
-- (M2 E2.8 P571 / PRMT-044).
--
-- Auto-opened tickets carry the originating AlarmRule's runbook
-- key (e.g. "rb/cdu-deltat-low"); manual + PM tickets carry an
-- empty string. The default '' keeps the existing rows
-- consistent with the new column. The column is nullable in
-- the wire format but stored as TEXT NOT NULL DEFAULT '' so
-- the fileStore / pgStore schemas both round-trip cleanly.

ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS runbook TEXT NOT NULL DEFAULT '';