-- 016_site_org.sql: site→org mapping substrate
-- (E3.1 / PRMT-189 / spec-001 v1.1 §5bis.2).
--
-- Site→Org is a persisted, re-assignable, audited mapping ("site 挂 Org，
-- 可改挂，改挂记审计"). Core has NO sites table — sites derive from the
-- first segment of asset paths (cpath.AssetPath.Site), so the mapping
-- references a site slug (not an FK to a sites table) plus an org FK.
--
-- This PRMT ships the mapping table + Store read methods + the idempotent
-- "attach site to org" primitive. Backfill of existing sites is PRMT-186's
-- job; cluster→org stays spec-side (no Cluster store; fleet is E3.7).
--
-- Audit rows for re-home go into the EXISTING tenant_audit table from
-- PRMT-184 with op='org_reattach' (one of the seven op tokens fixed in
-- 015_tenant_org.sql). No new audit op token is introduced here, and
-- 015's CHECK vocabulary is NOT altered (protocol/types.yaml 只增不扩).
--
-- Idempotent CREATE TABLE / CREATE INDEX so the ledger-driven migrator
-- (PRMT-087) can re-run it safely.

CREATE TABLE IF NOT EXISTS site_orgs (
    site       TEXT PRIMARY KEY,                    -- site slug, grammar [a-z]{2,8}[0-9]{2} (spec-001 §2, validated at store boundary)
    org_id     TEXT NOT NULL REFERENCES orgs(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS site_orgs_org_idx ON site_orgs (org_id);