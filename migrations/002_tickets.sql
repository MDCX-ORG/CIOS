-- CIOS core store: tickets (M2 E2.3). Idempotent (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS tickets (
    id          TEXT        NOT NULL PRIMARY KEY,
    alarm_id    TEXT        NOT NULL DEFAULT '',
    asset_path  TEXT        NOT NULL,
    title       TEXT        NOT NULL DEFAULT '',
    severity    TEXT        NOT NULL,   -- critical|major|minor|info
    state       TEXT        NOT NULL,   -- open|acknowledged|resolved|closed
    assignee    TEXT        NOT NULL DEFAULT '',
    opened_at   TIMESTAMPTZ NOT NULL,
    acked_at    TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    closed_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS tickets_alarm_id_idx   ON tickets (alarm_id);
CREATE INDEX IF NOT EXISTS tickets_asset_path_idx ON tickets (asset_path);
