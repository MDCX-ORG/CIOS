-- 006_asset_audit.sql: asset_audit table (M2 E2.1 P512 / PRMT-045).
--
-- Append-only change log: every PUT / :lifecycle / DELETE that
-- succeeds records one row. No UPDATE / DELETE API on this
-- table (audit integrity). Op is constrained to the three
-- known values so the audit reader cannot see a typo'd op.

CREATE TABLE IF NOT EXISTS asset_audit (
    id        TEXT PRIMARY KEY,
    ts        TIMESTAMPTZ NOT NULL,
    principal TEXT NOT NULL,
    path      TEXT NOT NULL,
    op        TEXT NOT NULL CHECK (op IN ('put','lifecycle','delete')),
    detail    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS asset_audit_path_idx
    ON asset_audit (path, ts DESC);