-- 018_usage.sql: UsageRecord persistence (E3.2 / L102 / PRMT-195 / spec-010).
-- Natural key supports idempotent recompute. No money columns.

CREATE TABLE IF NOT EXISTS usage_records (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('energy', 'rack_hour')),
    tenant_id     TEXT NOT NULL DEFAULT '',
    org_id        TEXT NOT NULL DEFAULT '',
    site_id       TEXT NOT NULL DEFAULT '',
    asset_path    TEXT NOT NULL,
    period_start  TIMESTAMPTZ NOT NULL,
    period_end    TIMESTAMPTZ NOT NULL,
    granularity   TEXT NOT NULL CHECK (granularity IN ('daily', 'monthly')),
    quantity      DOUBLE PRECISION NOT NULL,
    unit          TEXT NOT NULL,
    UNIQUE (kind, asset_path, period_start, period_end, granularity)
);
CREATE INDEX IF NOT EXISTS usage_records_tenant_period_idx ON usage_records (tenant_id, period_start);
CREATE INDEX IF NOT EXISTS usage_records_site_idx ON usage_records (site_id);
