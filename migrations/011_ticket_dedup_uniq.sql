-- CIOS core store: tickets dedup — partial unique index
-- (PRMT-081, eval M4 schema-layer backstop).
--
-- Goal: enforce "one non-closed ticket per non-empty alarm_id" at
-- the SQL layer. The application layer (alarm.Store.OpenTicket,
-- core/spares.go, core/reconcile.go) already does a check-then-insert
-- dedup dance; this index is the backstop that catches any racing
-- concurrent insert that slips between the SELECT and the INSERT.
--
-- WHERE clause:
--   alarm_id <> ''       — manual tickets (alarm_id='') are NOT
--                          covered; operators can open unlimited
--                          manual tickets for the same asset.
--   state <> 'closed'    — once a ticket is closed a fresh ticket
--                          for the same alarm_id is allowed (the
--                          state machine prevents reopening, but a
--                          re-firing alarm may legitimately open a
--                          new active ticket after the old one
--                          closed). This is the same "active =
--                          state != 'closed'" semantics the dedup
--                          scanners use (alarm.Store.OpenTicket
--                          existsQ; core/spares.hasOpenLowStockTicket;
--                          core/reconcile.hasOpenDriftTicket).
--
-- Named so pg_store.putTicket can detect a 23505 raised on this
-- specific index (vs the tickets_pkey ON CONFLICT (id) path) and
-- surface ErrDuplicateActiveTicket to the caller.

CREATE UNIQUE INDEX IF NOT EXISTS tickets_alarm_id_active_uniq
    ON tickets (alarm_id)
    WHERE alarm_id <> '' AND state <> 'closed';
