-- 007_spares.sql: spare_parts + spare_txns (M2 E2.5 P541 / PRMT-048).
--
-- Minimal spare domain: catalog + current stock + append-only txn
-- log. No procurement / supplier / price columns by design
-- (PRMT-048 §1). The txn log is the source of truth for stock
-- movement; spare_parts.qty is a cached aggregate kept in sync by
-- the :adjust endpoint inside one transaction (pg) or one mutex
-- section (file).
--
-- SKU is unique to prevent duplicate catalog entries on retry. The
-- spare_id index supports GET /v1/spares/{id} and the recent-txns
-- lookup for the per-id view. CHECK on delta<>0 prevents the txn
-- log from accumulating no-op rows.

CREATE TABLE IF NOT EXISTS spare_parts (
    id        TEXT PRIMARY KEY,
    sku       TEXT NOT NULL UNIQUE,
    name      TEXT NOT NULL,
    qty       INTEGER NOT NULL CHECK (qty >= 0),
    min_qty   INTEGER NOT NULL CHECK (min_qty >= 0),
    location  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS spare_txns (
    id        TEXT PRIMARY KEY,
    spare_id  TEXT NOT NULL REFERENCES spare_parts(id) ON DELETE CASCADE,
    delta     INTEGER NOT NULL CHECK (delta <> 0),
    ticket_id TEXT NOT NULL DEFAULT '',
    at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS spare_txns_spare_id_idx
    ON spare_txns (spare_id, at DESC);
