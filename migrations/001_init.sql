-- CIOS core store: assets + alarms.
-- Idempotent (IF NOT EXISTS). Run inside a transaction by NewPGStore.

CREATE TABLE IF NOT EXISTS assets (
    path             TEXT        NOT NULL PRIMARY KEY,
    resource_version BIGINT      NOT NULL DEFAULT 1,
    spec             JSONB       NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS alarms (
    id        TEXT        NOT NULL PRIMARY KEY,
    path      TEXT        NOT NULL,
    severity  TEXT        NOT NULL,  -- critical|major|minor|info
    state     TEXT        NOT NULL,  -- firing|acked|resolved
    summary   TEXT        NOT NULL DEFAULT '',
    since     TIMESTAMPTZ NOT NULL
);
