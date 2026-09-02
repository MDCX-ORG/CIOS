-- 010_ticket_audit.sql: ticket_audit table (M2 E2.3 + ops forensics /
-- PRMT-061). Append-only change log mirroring asset_audit (PRMT-045)
-- for the ticket state machine.
--
-- Every successful ticket CREATE / state TRANSITION / assignee UPDATE
-- appends one row via core.appendTicketAudit. No UPDATE / DELETE API
-- is exposed on this table (audit integrity per spec-008 §13.2).
--
-- op is constrained to the three known values so the audit reader
-- cannot see a typo'd op. from_state / to_state are nullable for
-- the "created" op (no prior state) and the "assigned" op (assignee
-- change is not a state-machine transition).

CREATE TABLE IF NOT EXISTS ticket_audit (
    id         TEXT PRIMARY KEY,
    ticket_id  TEXT NOT NULL,
    op         TEXT NOT NULL CHECK (op IN ('created','transitioned','assigned')),
    from_state TEXT,
    to_state   TEXT,
    who        TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ticket_audit_ticket_id_idx
    ON ticket_audit (ticket_id, at ASC);