// Package core — tenant.go: id generators and slug validator for the
// tenant / org substrate (E3.1 / PRMT-184 / spec-001 v1.1 §5bis).
//
// This file holds only:
//   - newOrgID         — "og_" + 16 base32 chars (mirror newAuditID's shape)
//   - newTenantAuditID — "ta_" + 16 base32 chars (mirror newAuditID's shape)
//   - validTenantSlug  — boundary validator for tenant / org name slugs
//
// It does NOT carry any Store method, mutation, or HTTP handler — those
// arrive with their consuming PRMTs (182 tier-write, 185 /v1/orgs,
// 186 migration). The validators and ID generators defined here are
// the contract the write paths will plug into.
package core

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// newOrgID produces "og_" + 16 base32 chars. Mirror of newAuditID
// (core/assets.go) byte-for-byte in scheme (same crypto/rand source,
// same base32 alphabet with no padding, same 10 random bytes → 16
// chars), only the prefix differs so the namespace is distinguishable
// in logs and on disk.
func newOrgID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "og_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// newRoleBindingID produces "rb_" + 16 base32 chars. Mirror of
// newOrgID / newAuditID byte-for-byte in scheme (same crypto/rand
// source, same base32 alphabet with no padding, same 10 random
// bytes → 16 chars). Used by Store.PutRoleBinding as the row PK on
// the role_bindings table (PRMT-190-bis §4.1; id matches the
// "rb_" prefix pinned in migrations/017_role_bindings.sql).
func newRoleBindingID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "rb_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// newTenantAuditID produces "ta_" + 16 base32 chars. Mirror of
// newAuditID (core/assets.go) — same scheme, distinct prefix.
// Used by tenant_audit inserts (PRMT-184 §4.4).
//
// NOTE: the prefix "ta_" is intentionally distinct from ticket_audit's
// "tk_"/"ta_" historically used by TicketAudit.ID — that struct uses
// newTicketID's prefix machinery and is unrelated to tenant_audit
// (the TenantAudit struct here has its own ID space; the table
// column types are TEXT so the namespace collision is benign).
func newTenantAuditID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "ta_" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

// validTenantSlug reports whether s matches [a-z][a-z0-9-]{1,30}.
// spec-001 §5bis.1 id grammar; same charset as site codes (the slug
// space is shared so tenants and sites look uniform to operators).
//
// The regex shape is equivalent to: first char must be lowercase
// letter; remaining 1–30 chars may be lowercase letter, digit, or
// dash. That gives a total length of 2..31 chars. We validate by
// hand here (no regex compile) since this is on the store-write
// boundary and runs once per write.
//
// This is the boundary check the downstream mutators (PRMT-182 /
// 185 / 186) will call before they hit the SQL UNIQUE on tenants.id
// or the (tenant_id, name) UNIQUE on orgs.
func validTenantSlug(s string) bool {
	if len(s) < 2 || len(s) > 31 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
