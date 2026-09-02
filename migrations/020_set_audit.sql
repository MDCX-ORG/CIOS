-- 020_set_audit.sql: set_audit table (PRMT-234; spec-006 §5.4 control
-- write audit). Append-only change log for PUT /v1/points/{path}:set —
-- one row per accepted control write. Mirrors ticket_audit (010) /
-- tenant_audit (015): no UPDATE / DELETE API is exposed on this table.
-- Previously these records lived in a process-memory slice (core/setctl.go
-- MVP) and were lost on restart.

CREATE TABLE IF NOT EXISTS set_audit (
    id                TEXT PRIMARY KEY,
    ts                TIMESTAMPTZ NOT NULL,
    path              TEXT NOT NULL,
    risk_class        TEXT NOT NULL CHECK (risk_class IN ('a','b','c')),
    value             DOUBLE PRECISION NOT NULL,
    actor             TEXT NOT NULL,
    second_approver   TEXT NOT NULL DEFAULT '',
    readback_required BOOLEAN NOT NULL,
    note              TEXT NOT NULL DEFAULT '',
    request_id        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS set_audit_ts_idx ON set_audit (ts DESC, id DESC);
