-- 017_role_bindings.sql: persisted RBAC scope grants
-- (E3.1 / PRMT-190-bis / spec-004 §6bis; R3).
--
-- The static token config (config/rbac.*.yaml) remains the authn seed
-- (token → subject/role); these rows are the scope grants the v1.1
-- migration (PRMT-186) mechanically rewrites (dot-glob → crn). origin
-- mirrors PRMT-190's scopeOrigin vocabulary ('legacy' pre-crn dot-glob
-- | 'crn' native) so the window-closure flag in core/rbac.go can
-- reject legacy-origin matches without re-parsing scope strings.
--
-- Idempotent CREATE TABLE / CREATE INDEX so the ledger-driven migrator
-- (PRMT-087) can re-run it safely. origin is constrained to the two
-- values PRMT-190 already fixed (auth.go L30-L33); no new enum token
-- is introduced (protocol/types.yaml 只增不扩).

CREATE TABLE IF NOT EXISTS role_bindings (
    id         TEXT PRIMARY KEY,                    -- "rb_" + 16 base32 (mirror newOrgID/newAuditID scheme; see core/tenant.go newRoleBindingID)
    subject    TEXT NOT NULL,                       -- matches Principal.Subject (authn seed)
    scope      TEXT NOT NULL,                       -- dot-glob or crn scope pattern
    origin     TEXT NOT NULL DEFAULT 'legacy' CHECK (origin IN ('legacy','crn')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (subject, scope)
);
CREATE INDEX IF NOT EXISTS role_bindings_subject_idx ON role_bindings (subject);