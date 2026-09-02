-- 015_tenant_org.sql: tenants / orgs / tenant_audit substrate
-- (E3.1 / PRMT-184 / spec-001 v1.1 §5bis).
--
-- This is the relational substrate for tenant and org records
-- previously held only in STS token claims. The write paths
-- (PRMT-182 tier-write, PRMT-185 /v1/orgs, PRMT-186 migration)
-- land on top of these tables; this PRMT ships schema +
-- read-side store methods only.
--
-- Idempotent CREATE TABLE / CREATE INDEX so the ledger-driven
-- migrator (PRMT-087) can re-run it safely.

CREATE TABLE IF NOT EXISTS tenants (
    id             TEXT PRIMARY KEY,                 -- slug [a-z][a-z0-9-]{1,30} (validated at store boundary)
    display_name   TEXT NOT NULL,
    isolation_tier TEXT NOT NULL DEFAULT 'label' CHECK (isolation_tier IN ('label','row','db')),
    status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS orgs (
    id         TEXT PRIMARY KEY,                     -- internal id "og_" + 16 base32 (mirror newAuditID shape)
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    name       TEXT NOT NULL,                        -- slug [a-z][a-z0-9-]{1,30}
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);
CREATE INDEX IF NOT EXISTS orgs_tenant_idx ON orgs (tenant_id, name);

-- tenant_audit: append-only change log for tenant + org records
-- (spec-001 v1.1 §5bis "append-only 租户审计 (actor + 前后值)").
-- Shape mirrors asset_audit (006) with tenant_id in place of
-- path. The op vocabulary is fixed here so the downstream write
-- PRMTs (182/185/186) reuse identical tokens.
CREATE TABLE IF NOT EXISTS tenant_audit (
    id        TEXT PRIMARY KEY,                      -- "ta_" + 16 base32
    ts        TIMESTAMPTZ NOT NULL,
    principal TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    op        TEXT NOT NULL CHECK (op IN ('tenant_create','tier_change','tenant_status','org_create','org_rename','org_reattach','org_delete')),
    detail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS tenant_audit_tenant_idx ON tenant_audit (tenant_id, ts DESC);