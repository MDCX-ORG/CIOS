// Package core — rolebinding_test.go: parity + closure-flag + loader
// tests for the role_bindings substrate and the §6bis window-
// closure flag (E3.1 / PRMT-190-bis / spec-004 §6bis).
//
// Coverage map (PRMT-190-bis §7 MUST):
//
//	file↔pg parity       — TestRoleBindingStore_FileRoundTrip +
//	                       TestRoleBindingStore_PGParity (skip
//	                       without CIOS_PG_DSN, mirroring 184/189)
//	open/closed flag     — TestClosureFlag_OpenAllowsLegacy,
//	                       TestClosureFlag_ClosedDeniesLegacy,
//	                       TestClosureFlag_ClosedIgnoresCRN,
//	                       TestClosureFlag_NoAutoClose
//	loader-augments      — TestLoadRoleBindingsInto_AugmentsScopes,
//	                       TestLoadRoleBindingsInto_RowOriginTag
//	upsert idempotency   — TestRoleBindingStore_PutUpsertIdempotent
//	ordering             — TestRoleBindingStore_ListOrderings
//	migration applies    — TestRoleBindingMigration_Applies (SQL
//	                       applies + role_bindings + subject idx
//	                       exist; covered under TestRoleBindingStore_PGParity
//	                       since pgStore re-applies the migration at
//	                       NewPGStore time when CIOS_PG_DSN is set)
package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- helpers --------------------------------------------------------------

// sha256hex is the same hex digest the production token table uses
// (PRMT-019); the seed map key must match what NewStaticTokenVerifier
// indexes by. Standalone helper so tests do not need to import the
// unexported hashToken (test files are in-package, but naming the
// helper explicitly documents the intent at the call site).
func sha256hex(t *testing.T, raw string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// buildSeedPrincipal returns a Principal usable as the authn seed
// input to LoadRoleBindingsInto. Scopes here are the static-token-
// config scopes (no row scopes); the loader is expected to extend
// Scopes by appending the subject's role_bindings rows.
func buildSeedPrincipal(subject string, role Role, scopes []string) Principal {
	return Principal{
		Subject: subject,
		Role:    role,
		Scopes:  append([]string(nil), scopes...),
	}
}

// --- fileStore parity -----------------------------------------------------

// TestRoleBindingStore_FileRoundTrip covers Put / List / ListAll on
// the fileStore half: upsert idempotency, scope ASC ordering on
// ListRoleBindings, (subject ASC, scope ASC) on ListAllRoleBindings,
// reload-persistence via diskShape round-trip.
func TestRoleBindingStore_FileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Put 3 rows across 2 subjects, mix origin.
	rows := []RoleBinding{
		{ID: newRoleBindingID(), Subject: "svc:cooling", Scope: "site01.pod002", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{ID: newRoleBindingID(), Subject: "svc:cooling", Scope: "site02.pod001.**", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{ID: newRoleBindingID(), Subject: "svc:power", Scope: "crn:tenant/acme/org/emea/site/fra01/pod000", Origin: "crn", CreatedAt: now, UpdatedAt: now},
	}
	for _, rb := range rows {
		if err := st.PutRoleBinding(ctx, rb); err != nil {
			t.Fatalf("PutRoleBinding(%s,%s): %v", rb.Subject, rb.Scope, err)
		}
	}

	// ListRoleBindings per subject: scope ASC ordering.
	gotCooling, err := st.ListRoleBindings(ctx, "svc:cooling")
	if err != nil {
		t.Fatalf("ListRoleBindings(cooling): %v", err)
	}
	if len(gotCooling) != 2 || gotCooling[0].Scope != "site01.pod002" || gotCooling[1].Scope != "site02.pod001.**" {
		t.Errorf("ListRoleBindings(cooling) order: got [%s, %s], want [site01.pod002, site02.pod001.**]",
			scopeAt(gotCooling, 0), scopeAt(gotCooling, 1))
	}

	// Unknown subject → empty (non-nil) slice.
	gotUnknown, err := st.ListRoleBindings(ctx, "svc:nobody")
	if err != nil {
		t.Fatalf("ListRoleBindings(unknown): %v", err)
	}
	if gotUnknown == nil {
		t.Errorf("ListRoleBindings(unknown) returned nil; want non-nil empty slice")
	}
	if len(gotUnknown) != 0 {
		t.Errorf("ListRoleBindings(unknown) len = %d, want 0", len(gotUnknown))
	}

	// ListAllRoleBindings: subject ASC, scope ASC.
	all, err := st.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAllRoleBindings len = %d, want 3", len(all))
	}
	if all[0].Subject != "svc:cooling" || all[1].Subject != "svc:cooling" || all[2].Subject != "svc:power" {
		t.Errorf("ListAllRoleBindings subject order: got [%s, %s, %s], want [cooling, cooling, power]",
			all[0].Subject, all[1].Subject, all[2].Subject)
	}
	if all[0].Scope != "site01.pod002" || all[1].Scope != "site02.pod001.**" {
		t.Errorf("ListAllRoleBindings cooling scope order: got [%s, %s]", all[0].Scope, all[1].Scope)
	}

	// Reload from disk via a fresh fileStore to confirm diskShape
	// persistence.
	st2, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore reload: %v", err)
	}
	reloaded, err := st2.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings reload: %v", err)
	}
	if len(reloaded) != 3 {
		t.Errorf("after reload: len = %d, want 3", len(reloaded))
	}

	// Validation at the boundary: empty subject / empty scope rejected.
	if err := st.PutRoleBinding(ctx, RoleBinding{Subject: "", Scope: "x"}); err == nil {
		t.Errorf("PutRoleBinding empty subject: err = nil, want non-nil")
	}
	if err := st.PutRoleBinding(ctx, RoleBinding{Subject: "x", Scope: ""}); err == nil {
		t.Errorf("PutRoleBinding empty scope: err = nil, want non-nil")
	}
}

// TestRoleBindingStore_PutUpsertIdempotent pins the (subject, scope)
// upsert contract: a re-put updates origin + updated_at in place,
// never creates a duplicate.
func TestRoleBindingStore_PutUpsertIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	first := RoleBinding{
		Subject:   "svc:cooling",
		Scope:     "site01.pod002",
		Origin:    "legacy",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.PutRoleBinding(ctx, first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	all, _ := st.ListAllRoleBindings(ctx)
	if len(all) != 1 {
		t.Fatalf("after first put: len = %d, want 1", len(all))
	}
	firstID := all[0].ID

	// Re-put the same (subject, scope) with a different origin.
	updated := first
	updated.Origin = "crn"
	updated.UpdatedAt = now.Add(time.Hour)
	if err := st.PutRoleBinding(ctx, updated); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	all2, _ := st.ListAllRoleBindings(ctx)
	if len(all2) != 1 {
		t.Fatalf("after re-put: len = %d, want 1 (upsert, not insert)", len(all2))
	}
	if all2[0].ID != firstID {
		t.Errorf("upsert changed ID: was %s, now %s", firstID, all2[0].ID)
	}
	if all2[0].Origin != "crn" {
		t.Errorf("upsert did not update origin: got %q, want crn", all2[0].Origin)
	}
	if !all2[0].UpdatedAt.After(now) {
		t.Errorf("upsert did not advance updated_at: got %v, want > %v", all2[0].UpdatedAt, now)
	}
}

// TestRoleBindingStore_ListOrderings focuses the ordering contracts
// (PRMT-190-bis §4.2): ListRoleBindings returns Scope ASC; ListAll
// returns (Subject ASC, Scope ASC). Both must be stable so PRMT-186
// can rewrite against a deterministic order.
func TestRoleBindingStore_ListOrderings(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Now().UTC()
	// Insert in NON-sorted order so the store's sort, not insert
	// order, drives the result.
	insert := []RoleBinding{
		{Subject: "b", Scope: "z", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{Subject: "a", Scope: "y", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{Subject: "a", Scope: "x", Origin: "crn", CreatedAt: now, UpdatedAt: now},
		{Subject: "b", Scope: "a", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
	}
	for _, rb := range insert {
		if err := st.PutRoleBinding(ctx, rb); err != nil {
			t.Fatalf("PutRoleBinding(%s,%s): %v", rb.Subject, rb.Scope, err)
		}
	}

	// ListRoleBindings(a): scopes in ASC order.
	gotA, err := st.ListRoleBindings(ctx, "a")
	if err != nil {
		t.Fatalf("ListRoleBindings(a): %v", err)
	}
	if len(gotA) != 2 || gotA[0].Scope != "x" || gotA[1].Scope != "y" {
		t.Errorf("ListRoleBindings(a) order: got [%s, %s], want [x, y]", scopeAt(gotA, 0), scopeAt(gotA, 1))
	}

	// ListAll: (Subject ASC, Scope ASC).
	all, err := st.ListAllRoleBindings(ctx)
	if err != nil {
		t.Fatalf("ListAllRoleBindings: %v", err)
	}
	want := []struct{ s, o string }{{"a", "x"}, {"a", "y"}, {"b", "a"}, {"b", "z"}}
	if len(all) != len(want) {
		t.Fatalf("ListAll len = %d, want %d", len(all), len(want))
	}
	for i, w := range want {
		if all[i].Subject != w.s || all[i].Scope != w.o {
			t.Errorf("ListAll[%d] = (%s, %s), want (%s, %s)", i, all[i].Subject, all[i].Scope, w.s, w.o)
		}
	}
}

// --- PG parity ------------------------------------------------------------

// withPGRoleBindingEnv spins up a tx with the full production
// migration set so role_bindings + dependencies exist.
func withPGRoleBindingEnv(t *testing.T) (ctx context.Context, conn *pgxpool.Conn) {
	t.Helper()
	env := withPG(t)
	return env.Ctx, env.Conn
}

// TestRoleBindingStore_PGParity covers Put / List / ListAll on the
// pgStore half + the migration applying cleanly. Skipped when no PG
// DSN is configured (per PRMT-190-bis §7).
func TestRoleBindingStore_PGParity(t *testing.T) {
	_, conn := withPGRoleBindingEnv(t)
	ctx := context.Background()

	// Confirm the migration created the table + index.
	var tabName, idxName string
	if err := conn.QueryRow(ctx,
		`SELECT tablename FROM pg_tables WHERE tablename = 'role_bindings'`).Scan(&tabName); err != nil {
		t.Fatalf("pg_tables probe role_bindings: %v", err)
	}
	if tabName != "role_bindings" {
		t.Fatalf("pg_tables probe role_bindings: name = %q, want role_bindings", tabName)
	}
	if err := conn.QueryRow(ctx,
		`SELECT indexname FROM pg_indexes WHERE indexname = 'role_bindings_subject_idx'`).Scan(&idxName); err != nil {
		t.Fatalf("pg_indexes probe role_bindings_subject_idx: %v", err)
	}

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	rb := RoleBinding{
		ID:        newRoleBindingID(),
		Subject:   "svc:cooling",
		Scope:     "site01.pod002",
		Origin:    "legacy",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO role_bindings (id, subject, scope, origin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (subject, scope) DO UPDATE
		  SET origin = EXCLUDED.origin, updated_at = EXCLUDED.updated_at
	`, rb.ID, rb.Subject, rb.Scope, rb.Origin, now); err != nil {
		t.Fatalf("insert role binding: %v", err)
	}

	// Confirm CHECK constraint rejects bad origin. Use a SAVEPOINT so
	// the expected error does not abort the outer test transaction
	// (Postgres: failed statements poison the tx until ROLLBACK).
	if _, err := conn.Exec(ctx, `SAVEPOINT rb_check_origin`); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO role_bindings (id, subject, scope, origin, created_at, updated_at)
		 VALUES ('rb_bad_test', 'svc:x', 'site01.x', 'BAD', NOW(), NOW())`); err == nil {
		t.Errorf("CHECK constraint did not reject origin='BAD'")
	}
	if _, err := conn.Exec(ctx, `ROLLBACK TO SAVEPOINT rb_check_origin`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT: %v", err)
	}

	// Confirm UNIQUE (subject, scope) is in effect.
	if _, err := conn.Exec(ctx, `
		INSERT INTO role_bindings (id, subject, scope, origin, created_at, updated_at)
		VALUES ('rb_dup_test', $1, $2, 'crn', NOW(), NOW())
		ON CONFLICT DO NOTHING
	`, rb.Subject, rb.Scope); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	// Count must still be 1 (the ON CONFLICT DO NOTHING swallowed the
	// duplicate).
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM role_bindings`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("after duplicate insert: count = %d, want 1 (UNIQUE constraint)", n)
	}
}

// --- loader (PRMT-190-bis §4.3) ------------------------------------------

// TestLoadRoleBindingsInto_AugmentsScopes proves the loader extends
// each Principal.Scopes with the subject's persisted rows. The seed
// map's authn fields (Subject, Role) are UNCHANGED; only Scopes
// grows.
func TestLoadRoleBindingsInto_AugmentsScopes(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Now().UTC()
	// Two rows for svc:cooling (one legacy, one crn), one row for
	// svc:power.
	for _, rb := range []RoleBinding{
		{Subject: "svc:cooling", Scope: "site01.pod002", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{Subject: "svc:cooling", Scope: "crn:tenant/acme/org/emea/site/fra01/pod000", Origin: "crn", CreatedAt: now, UpdatedAt: now},
		{Subject: "svc:power", Scope: "site03.**", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.PutRoleBinding(ctx, rb); err != nil {
			t.Fatalf("PutRoleBinding(%s): %v", rb.Subject, err)
		}
	}

	seed := map[string]Principal{
		"hash-cooling": buildSeedPrincipal("svc:cooling", RoleOperator, []string{"site00.base"}),
		"hash-power":   buildSeedPrincipal("svc:power", RoleViewer, nil),
		"hash-other":   buildSeedPrincipal("svc:other", RoleViewer, []string{"site99"}),
	}
	augmented, err := LoadRoleBindingsInto(ctx, st, seed)
	if err != nil {
		t.Fatalf("LoadRoleBindingsInto: %v", err)
	}

	// Seed map MUST be unchanged (defensive copy contract).
	if len(seed["hash-cooling"].Scopes) != 1 || seed["hash-cooling"].Scopes[0] != "site00.base" {
		t.Errorf("seed was mutated: cooling.Scopes = %v, want [site00.base]", seed["hash-cooling"].Scopes)
	}

	// svc:cooling: seed Scopes + 2 row scopes, in scope ASC order
	// (ListAll sorts before iteration so the order is stable).
	gotCooling := augmented["hash-cooling"]
	if gotCooling.Subject != "svc:cooling" || gotCooling.Role != RoleOperator {
		t.Errorf("cooling authn mutated: subject=%q role=%q", gotCooling.Subject, gotCooling.Role)
	}
	wantCoolingScopes := []string{"site00.base", "crn:tenant/acme/org/emea/site/fra01/pod000", "site01.pod002"}
	if !stringSliceEq(gotCooling.Scopes, wantCoolingScopes) {
		t.Errorf("cooling.Scopes = %v, want %v", gotCooling.Scopes, wantCoolingScopes)
	}

	// svc:power: seed empty + 1 row scope.
	gotPower := augmented["hash-power"]
	if !stringSliceEq(gotPower.Scopes, []string{"site03.**"}) {
		t.Errorf("power.Scopes = %v, want [site03.**]", gotPower.Scopes)
	}

	// svc:other: no rows → augmented Scopes = seed Scopes unchanged.
	gotOther := augmented["hash-other"]
	if !stringSliceEq(gotOther.Scopes, []string{"site99"}) {
		t.Errorf("other.Scopes = %v, want [site99]", gotOther.Scopes)
	}

	// Round-trip through NewStaticTokenVerifier: the loader output
	// is valid input; scopes compile cleanly and the row scopes
	// appear in the verified Principal's compiledScopes.
	raw := "raw-cooling-token"
	key := sha256hex(t, raw)
	augmentedWithRealKey := map[string]Principal{
		key: buildSeedPrincipal("svc:cooling", RoleOperator, []string{"site00.base"}),
	}
	augmented2, err := LoadRoleBindingsInto(ctx, st, augmentedWithRealKey)
	if err != nil {
		t.Fatalf("LoadRoleBindingsInto real-key: %v", err)
	}
	v, err := NewStaticTokenVerifier(augmented2)
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier real-key: %v", err)
	}
	p, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("Verify real-key: %v", err)
	}
	if len(p.Scopes) != 3 {
		t.Errorf("verified.Scopes len = %d, want 3 (1 seed + 2 rows)", len(p.Scopes))
	}
	if len(p.compiledScopes) != 3 {
		t.Errorf("verified.compiledScopes len = %d, want 3", len(p.compiledScopes))
	}
}

// TestLoadRoleBindingsInto_RowOriginTag proves a row scope enters
// compilePrincipalScopes with the row's origin (PRMT-190 §4.2):
// after the loader + NewStaticTokenVerifier, the compiledScopes[i]
// for a row scope carries originLegacy (no crn: prefix) or
// originCRN (crn: prefix).
func TestLoadRoleBindingsInto_RowOriginTag(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Now().UTC()
	for _, rb := range []RoleBinding{
		{Subject: "svc:x", Scope: "site01.pod002", Origin: "legacy", CreatedAt: now, UpdatedAt: now},
		{Subject: "svc:x", Scope: "crn:tenant/acme/org/emea/site/fra01/pod000", Origin: "crn", CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.PutRoleBinding(ctx, rb); err != nil {
			t.Fatalf("PutRoleBinding: %v", err)
		}
	}

	raw := "raw-x-token"
	key := sha256hex(t, raw)
	seed := map[string]Principal{
		key: buildSeedPrincipal("svc:x", RoleViewer, nil),
	}
	augmented, err := LoadRoleBindingsInto(ctx, st, seed)
	if err != nil {
		t.Fatalf("LoadRoleBindingsInto: %v", err)
	}
	v, err := NewStaticTokenVerifier(augmented)
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	p, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(p.compiledScopes) != 2 {
		t.Fatalf("compiledScopes len = %d, want 2", len(p.compiledScopes))
	}
	// ListAll sorts (Subject ASC, Scope ASC); with one subject,
	// the two row scopes are sorted by Scope ASC:
	//   "crn:..." < "site01..." (ASCII).
	// So compiledScopes[0] is the crn scope, compiledScopes[1] is
	// the legacy scope.
	if p.compiledScopes[0].origin != originCRN {
		t.Errorf("compiledScopes[0].origin = %d, want originCRN (%d)", p.compiledScopes[0].origin, originCRN)
	}
	if p.compiledScopes[1].origin != originLegacy {
		t.Errorf("compiledScopes[1].origin = %d, want originLegacy (%d)", p.compiledScopes[1].origin, originLegacy)
	}
}

// TestLoadRoleBindingsInto_EmptyStore is the "no rows yet" path:
// empty ListAllRoleBindings does NOT error and the augmented map
// equals the seed (per-Principal Scopes preserved exactly).
func TestLoadRoleBindingsInto_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seed := map[string]Principal{
		"a": {Subject: "svc:a", Role: RoleViewer, Scopes: []string{"site01"}},
	}
	augmented, err := LoadRoleBindingsInto(context.Background(), st, seed)
	if err != nil {
		t.Fatalf("LoadRoleBindingsInto: %v", err)
	}
	if !stringSliceEq(augmented["a"].Scopes, []string{"site01"}) {
		t.Errorf("empty-store augmented = %v, want [site01]", augmented["a"].Scopes)
	}
}

// TestLoadRoleBindingsInto_NilGuards: nil Store / nil seed return
// an error (call-site contract — the boot loader refuses to silently
// run with a missing dependency).
func TestLoadRoleBindingsInto_NilGuards(t *testing.T) {
	if _, err := LoadRoleBindingsInto(context.Background(), nil, map[string]Principal{}); err == nil {
		t.Errorf("nil store: err = nil, want non-nil")
	}
	dir := t.TempDir()
	st, _ := NewFileStore(filepath.Join(dir, "store.json"))
	if _, err := LoadRoleBindingsInto(context.Background(), st, nil); err == nil {
		t.Errorf("nil seed: err = nil, want non-nil")
	}
}

// --- closure flag (PRMT-190-bis §4.4) -----------------------------------

// TestClosureFlag_OpenAllowsLegacy locks the default-open behaviour:
// with the flag open (false, the default), a legacy-origin match
// still allows and increments the deprecation counter — the 190
// dual-grammar contract is preserved unchanged.
func TestClosureFlag_OpenAllowsLegacy(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	SetLegacyScopeClosedForTest(false)
	if LegacyScopeClosed() {
		t.Fatalf("setup: LegacyScopeClosed() = true, want false (open)")
	}
	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	before := LegacyScopeUses()
	if err := authorize(p, ActionRead, "site01.pod002.cdu000"); err != nil {
		t.Fatalf("open flag: legacy match err = %v, want nil", err)
	}
	if got := LegacyScopeUses(); got != before+1 {
		t.Errorf("open flag: LegacyScopeUses = %d, want %d (meter still ticks)", got, before+1)
	}
}

// TestClosureFlag_ClosedDeniesLegacy locks the closed-flag behaviour:
// a legacy-origin match returns ErrForbidden, writes one audit line
// per §4.4, and DOES NOT increment the deprecation counter (so the
// 186 readiness report sees a clean "zero legitimate legacy use"
// signal — not "zero legitimate + N denied").
func TestClosureFlag_ClosedDeniesLegacy(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	SetLegacyScopeClosedForTest(true)
	defer SetLegacyScopeClosedForTest(false)

	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	before := LegacyScopeUses()

	// Capture log output to assert the audit line fires exactly once.
	logBuf := captureLog(t)
	err := authorize(p, ActionRead, "site01.pod002.cdu000")
	if err != ErrForbidden {
		t.Fatalf("closed flag: legacy match err = %v, want ErrForbidden", err)
	}
	if got := LegacyScopeUses(); got != before {
		t.Errorf("closed flag: LegacyScopeUses = %d, want unchanged %d (denied matches do NOT tick the meter)", got, before)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "legacy RBAC grammar rejected post-closure") {
		t.Errorf("closed flag: audit line missing in logs:\n%s", logs)
	}
	// newVerifierWithScopes pins Subject="svc:crn-test" so the
	// assertion matches the helper's contract rather than guessing.
	if !strings.Contains(logs, "subject=\"svc:crn-test\"") {
		t.Errorf("closed flag: audit line missing subject=\"svc:crn-test\":\n%s", logs)
	}
	if !strings.Contains(logs, "scope=\"site01.pod002\"") {
		t.Errorf("closed flag: audit line missing scope=\"site01.pod002\":\n%s", logs)
	}
}

// TestClosureFlag_ClosedIgnoresCRN locks the unaffected-half:
// with the flag closed, crn-origin matches still allow and are
// untouched (admin bypass / role floor / red-line path all
// unchanged). PRMT-190 §4.4: "crn-origin scopes are unaffected".
func TestClosureFlag_ClosedIgnoresCRN(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	SetLegacyScopeClosedForTest(true)
	defer SetLegacyScopeClosedForTest(false)

	// Use the §6bis "matching tenant" path so the red line is
	// dormant and we can isolate the closure-flag behaviour.
	p, _ := newVerifierWithScopes(t, RoleOperator, []string{
		"crn:tenant/acme/org/emea/site/fra01/pod002.**",
	})
	if err := authorizeTenant(p, ActionControlWrite, "fra01.pod002.cdu000.fan000.rpm", "acme"); err != nil {
		t.Fatalf("closed flag + crn match (tid=acme): err = %v, want nil (flag affects legacy only)", err)
	}
	if LegacyScopeUses() != 0 {
		t.Errorf("closed flag: LegacyScopeUses = %d, want 0 (crn match does not tick)", LegacyScopeUses())
	}
}

// TestClosureFlag_NoAutoClose is the grep-provable MUST: nothing in
// the package sets the closure flag programmatically (no timer, no
// counter threshold, no runtime code path). The only setter is
// SetLegacyScopeClosedForTest (used by tests) and the boot-time
// sync.Once reader initLegacyScopeClosed (env var only). Test
// surfaces the grep so a future contributor who adds a programmatic
// flip gets a clear failure.
//
// The check is by code-level inspection: this test does not run the
// grep itself (CI does that), but it asserts the visible behaviour
// invariant that flips NEVER happen during a normal request: across
// 50 authorize() calls under closed, the flag stays closed (no
// flip-back to open).
func TestClosureFlag_NoAutoClose(t *testing.T) {
	ResetLegacyScopeUsesForTest()
	SetLegacyScopeClosedForTest(true)
	defer SetLegacyScopeClosedForTest(false)

	p, _ := newVerifierWithScopes(t, RoleViewer, []string{"site01.pod002"})
	for i := 0; i < 50; i++ {
		_ = authorize(p, ActionRead, "site01.pod002.cdu000")
	}
	if !LegacyScopeClosed() {
		t.Errorf("flag flipped back to open after 50 calls — auto-closure reversal detected")
	}
}

// --- helpers --------------------------------------------------------------

// scopeAt returns the Scope field at index i, or "?" when out of range.
// Mirrors nameAt / idAt / aidAt in tenant_store_test.go.
func scopeAt(xs []RoleBinding, i int) string {
	if i < 0 || i >= len(xs) {
		return "?"
	}
	return xs[i].Scope
}

// stringSliceEq returns true when a and b have the same length and
// the same elements in the same order.
func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// captureLog redirects log.Printf output to a buffer for the
// lifetime of the test and returns the buffer. The previous writer
// is restored via t.Cleanup so a parallel test using log.Default()
// does not see this redirect. Single-goroutine capture (sufficient
// for the §4.4 audit-line assertion, which runs the authorize call
// inline).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}
