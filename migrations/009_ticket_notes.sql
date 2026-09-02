-- 009_ticket_notes.sql: ticket_notes table (M2 E2.3 / PRMT-060).
--
-- Append-only note stream on a ticket. The processing record IS one
-- of the corpora that M4 (autonomous ops) trains on, so lossless
-- append-only is non-negotiable: there is no UPDATE / DELETE path
-- on this table (notes are evidence).
--
-- ID is "tn_" + 16 base32 chars (mirror newTicketID's shape,
-- distinct prefix). ticket_id is a TEXT FK to tickets.id — we do
-- NOT add a hard FK constraint because the production ticket table
-- is shared with the file store (no DDL), and a missing FK on
-- development sandboxes would block boot. The application layer is
-- the only writer, so referential integrity is the writer's job.
--
-- The author is the principal at the time of write, or "anonymous"
-- when no Authorization header is present (PRMT-045 / 060
-- alignment: anonymous is the only anonymous label — same shape as
-- AssetAudit.Principal). body is the operator's text. at is the
-- server-side timestamp.

CREATE TABLE IF NOT EXISTS ticket_notes (
    id        TEXT PRIMARY KEY,
    ticket_id TEXT NOT NULL,
    author    TEXT NOT NULL,
    body      TEXT NOT NULL,
    at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ticket_notes_ticket_id_idx
    ON ticket_notes (ticket_id, at ASC);
