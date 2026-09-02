// Package core — auth.go: Principal/Role/Action types, sentinel
// errors, and the TokenVerifier seam (with one concrete static
// implementation for M1 scoped API tokens). RBAC decision logic
// lives in rbac.go; the HTTP middleware that wires them together
// lives in authmw.go.
//
// PRMT-019 §4.1–§4.2. OIDC/JWT is an explicit seam (TokenVerifier
// is satisfied by a future *oidcVerifier without touching callers);
// this prompt ships only the static API-token verifier.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/yurimeng/cios/pkg/cpath"
)

// scopeOrigin records how a Principal scope entered the system, per
// spec-004 §6bis (PRMT-190). originLegacy is a v1.0 dot-glob scope
// that was implicitly interpreted as a crn under
// tenant/org/default at load time; it is deprecated and its use is
// metered + warned (PRMT-190 §4.4). originCRN is a native §6bis
// crn-form scope, in which case its tid is the explicit tenant and
// the cross-tenant red line (§6bis) applies at authorize time.
type scopeOrigin uint8

const (
	originLegacy scopeOrigin = iota // v1.0 dot-glob, normalized to crn/org/default (deprecated)
	originCRN                       // native crn-form scope
)

// transitionTenantSentinel is the tid value a legacy-origin scope
// carries when no token-tenant has been bound to it yet (PRMT-190
// §4.2 fallback). It signals "not yet tenant-bound" — the red line
// does not 403 on tid for this case (the caller has not had a chance
// to thread the request tenant) but the deprecation meter still
// fires. PRMT-190-bis may later thread a real per-token tenant.
const transitionTenantSentinel = ""

// Action is the RBAC decision axis (spec-004 §6 角色表). The three
// values cover M1's HTTP surface; tenant-scoped reads (M3) and the
// double-approval gate for risk_class=C (spec-006 §5.4) are not
// modelled here.
type Action string

const (
	ActionRead         Action = "read"          // GET (Query)
	ActionControlWrite Action = "control:write" // PUT points:set (A/B 类)
	ActionApply        Action = "apply"         // PUT/DELETE assets (declarative)
)

// Role is the role enum from spec-004 §6. Role tenant is M3 and is
// not in the M1 activation set; we deliberately do NOT define a
// const for it so a config typo of "tenant" fails RBAC closed
// rather than silently granting M3-only semantics.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Principal is the subject resolved from a Bearer token. Scopes is
// a list of CIOS path glob patterns (spec-001 §2 grammar), already
// compiled by NewStaticTokenVerifier so the request path never sees
// CompileGlob errors. Subject is opaque (typically "svc:<name>" for
// machine tokens or "oidc:<sub>" for future OIDC).
//
// compiledScopes is the precompiled form of Scopes, populated at
// Principal construction (NewStaticTokenVerifier) so authorize()
// can Match() without per-request CompileGlob. It is nil for
// principals built by struct literal (test fixtures, ctx-attached
// Principals that bypass the verifier); authorize() then compiles
// on the fly, but that path is not on the production request hot
// path — Verify() always returns a Principal with compiledScopes
// already set.
type Principal struct {
	Subject        string
	Role           Role
	Scopes         []string // raw patterns, retained for audit logging
	compiledScopes []scopeGlob
}

// scopeGlob is one precompiled scope pattern: the exact pattern
// and its ".<anything-below>" subtree variant used for the L50
// read-implies-subtree branch. cpath.Glob is safe for concurrent
// reads and immutable after CompileGlob returns, so a Principal
// built once is safe to authorize against many times.
//
// PRMT-190 adds two fields:
//   - origin:    whether this scope entered as a v1.0 legacy dot-glob
//     (originLegacy, deprecated, metered) or as a native §6bis crn
//     (originCRN).
//   - tid:       the tenant id of the crn interpretation. For
//     originCRN it is the crn's explicit <tid>. For originLegacy it
//     is transitionTenantSentinel ("") unless the token config later
//     supplies a per-token tenant (out of scope here; PRMT-190-bis).
type scopeGlob struct {
	exact   cpath.Glob
	subtree cpath.Glob
	origin  scopeOrigin
	tid     string
}

// ErrUnauthorized signals 401: no/bad Bearer token, unknown token,
// or expired token (the verifier decides). ErrForbidden signals
// 403: the principal is known but the (action, path) is outside
// their scope or role floor.
var (
	ErrUnauthorized = errors.New("core: unauthorized")
	ErrForbidden    = errors.New("core: forbidden")
)

// TokenVerifier resolves a raw Bearer token string into a Principal.
// Implementations must return ErrUnauthorized (and NOT log token
// plaintext) for unknown/expired/malformed tokens. The seam exists
// so a future *oidcVerifier (JWT/JWKS) drops into the same middleware.
type TokenVerifier interface {
	Verify(token string) (Principal, error)
}

// staticTokenVerifier holds an in-memory map from sha256(token) hex
// to Principal. The verifier compares hashes, never plaintext, so
// the configuration file and any audit log never have to carry the
// raw token. Scopes are pre-compiled at construction time so a bad
// glob in config fails the boot, not the request.
type staticTokenVerifier struct {
	// byHash maps sha256 hex of the raw bearer token to its Principal.
	// The map is read-only after NewStaticTokenVerifier returns; the
	// verifier holds no lock because it is never mutated at runtime.
	byHash map[string]Principal
}

// NewStaticTokenVerifier builds a verifier from a token table. The
// caller supplies the hash → Principal map (the cmd/cios-core boot
// reads it from RBAC config and hashes nothing on its own — the
// config already stores hex digests, never plaintext).
//
// Each Principal.Scopes pattern is compiled via pkg/cpath.CompileGlob
// at construction time; if any scope fails to compile, the function
// returns an error and no verifier is built. This enforces the
// "装载期校验" requirement of §4.3 / §4.7 — the request path is
// guaranteed never to encounter a bad scope.
//
// Each compiled scope is stored on the Principal itself so the
// per-request authorize() path can Match() without re-compiling
// (PRMT-113: compile out of the hot path). The returned verifier
// holds a defensive copy of tokens (and of each Principal.Scopes
// slice) so the caller may reuse/mutate its own maps without
// poisoning the verifier.
func NewStaticTokenVerifier(tokens map[string]Principal) (*staticTokenVerifier, error) {
	byHash := make(map[string]Principal, len(tokens))
	for h, p := range tokens {
		// Validate every scope pattern at load time AND pre-compile
		// for the request hot path. compilePrincipalScopes returns
		// the same error as the old per-pattern compileScope loop,
		// so callers (and TestStaticTokenVerifier_RejectsBadScopeAtLoad)
		// see identical behaviour on bad input.
		ready, err := compilePrincipalScopes(p)
		if err != nil {
			return nil, err
		}
		byHash[h] = ready
	}
	return &staticTokenVerifier{byHash: byHash}, nil
}

// compilePrincipalScopes returns a copy of p with compiledScopes
// populated from p.Scopes. Bad scope patterns return an error
// equivalent to the per-pattern compileScope error, so the load-
// time rejection contract is unchanged. compilePrincipalScopes is
// the single entry point every Principal that ends up in
// authorize() should pass through; the request path itself
// never calls it (and the test-only direct struct literals that
// reach authorize() without it are handled by the lazy fallback in
// authorize — see the comment on the lazy branch).
//
// PRMT-190 §4.2 dual-grammar: each raw scope is detected as either
// a legacy v1.0 dot-glob (no "crn:" prefix) or a native §6bis crn.
// The site-tail (whatever follows /site/ in crn, or the entire dot
// path in legacy) is compiled once via cpath.CompileGlob so the
// existing L50 exact + subtree Match machinery in authorize() is
// reused unchanged. The scope's origin and tid are recorded on the
// scopeGlob so authorize() can apply the §6bis red line and meter
// legacy use without re-parsing per request.
//
// The transition tenant for legacy scopes is
// transitionTenantSentinel ("") at this stage because the static
// token config today has no per-token tenant field (PRMT-190 §4.2
// fallback). authorizeTenant() threads the request's tenant at
// evaluation; if/when the scope is originLegacy && tid=="", the
// red line does not 403 on tid (it cannot, the scope is not bound
// to a tenant yet) but the deprecation counter still increments on
// match.
func compilePrincipalScopes(p Principal) (Principal, error) {
	scopesCopy := make([]string, len(p.Scopes))
	copy(scopesCopy, p.Scopes)
	compiled := make([]scopeGlob, 0, len(scopesCopy))
	for _, s := range scopesCopy {
		// Determine origin + site-tail dot path.
		var (
			dotPath string
			origin  scopeOrigin
			tid     string
		)
		if strings.HasPrefix(s, "crn:") {
			c, err := ParseCRN(s)
			if err != nil {
				return Principal{}, err
			}
			origin = originCRN
			tid = c.tid
			dotPath = c.dotPath()
		} else {
			c := legacyToCRN(s, transitionTenantSentinel)
			origin = originLegacy
			tid = c.tid // "" by design (sentinel)
			dotPath = c.dotPath()
		}
		exact, err := cpath.CompileGlob(dotPath)
		if err != nil {
			return Principal{}, err
		}
		// Pre-compile the ".<anything-below>" subtree variant too:
		// L50 read-implies-subtree means a viewer with scope
		// "site01.pod002" also matches "site01.pod002.<anything>".
		// Pre-compiling avoids rebuilding it on every read authorize
		// call. Bad ".<anything>" patterns are not a thing in the
		// current grammar (any ".<segment>**" form is well-formed
		// by spec-001 §2 if the base was), so the error is forwarded
		// unchanged.
		subtree, err := cpath.CompileGlob(dotPath + ".**")
		if err != nil {
			return Principal{}, err
		}
		compiled = append(compiled, scopeGlob{
			exact:   exact,
			subtree: subtree,
			origin:  origin,
			tid:     tid,
		})
	}
	return Principal{
		Subject:        p.Subject,
		Role:           p.Role,
		Scopes:         scopesCopy,
		compiledScopes: compiled,
	}, nil
}

// Verify hashes raw with SHA-256 and looks it up in the configured
// table. Hit → a fresh Principal copy (so the caller cannot mutate
// the verifier's stored value). Miss → ErrUnauthorized. The raw
// token MUST NOT be returned, logged, or used for the key — only
// the hex digest goes anywhere.
func (v *staticTokenVerifier) Verify(raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	p, ok := v.byHash[key]
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	// Return a copy so the caller cannot mutate verifier state.
	// compiledScopes is shared by reference — cpath.Glob is
	// immutable and safe for concurrent reads (per pkg/cpath), and
	// a copy of the slice header would be shallow anyway.
	scopesCopy := make([]string, len(p.Scopes))
	copy(scopesCopy, p.Scopes)
	return Principal{
		Subject:        p.Subject,
		Role:           p.Role,
		Scopes:         scopesCopy,
		compiledScopes: p.compiledScopes,
	}, nil
}

// hashToken is a small helper for tests / cmd-side config tooling
// that wants to turn a plaintext token into the hex digest used as
// the NewStaticTokenVerifier map key. It is intentionally unexported
// from outside the package surface that documents its semantics:
// production code should hash once at config-generation time, not
// per-request.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// LoadRoleBindingsInto returns a copy of the seed token→Principal
// map with each Principal.Scopes extended by the subject's persisted
// role_bindings rows (PRMT-190-bis §4.3; spec-004 §6bis, R3). The
// authn seed (token → subject/role) is UNCHANGED — only the Scopes
// list grows. The caller hands the returned map to
// NewStaticTokenVerifier (unchanged signature) which compiles every
// scope (row-sourced OR token-config-sourced) via the existing
// compilePrincipalScopes path; row scopes therefore gain their origin
// tag from the row's origin column, exactly like any other scope
// (PRMT-190 §4.2 dual grammar applies unchanged).
//
// Seed is NOT mutated; the loader copies each Principal and its
// Scopes slice before appending row scopes so a future caller can
// reuse the original seed (mirrors NewStaticTokenVerifier's own
// "defensive copy" comment). A missing subject in the seed (no
// matching token) is silently skipped — the row is dormant until a
// token configures that subject, exactly like an unused rbac.*.yaml
// subject.
//
// Loader is construction-time only: it is called once at boot from
// cmd/cios-core/main.go loadRBAC (after the static token config is
// read and BEFORE NewStaticTokenVerifier). It is NOT on the request
// hot path. A failing Store.ListAllRoleBindings call surfaces as a
// wrapped error so the boot fails loudly rather than silently running
// without row scopes (an empty ListAllRoleBindings result is the
// legitimate "no rows yet" path and does NOT error).
func LoadRoleBindingsInto(ctx context.Context, st Store, seed map[string]Principal) (map[string]Principal, error) {
	if st == nil {
		return nil, errors.New("core: load role bindings: nil store")
	}
	if seed == nil {
		return nil, errors.New("core: load role bindings: nil seed")
	}
	rows, err := st.ListAllRoleBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("core: load role bindings: list: %w", err)
	}
	// Index rows by subject so the per-token augmentation is O(rows
	// for that subject). Rows are pre-sorted (subject, scope) by
	// ListAllRoleBindings, so the appended slice is also sorted —
	// the boot-time compile sees a deterministic order matching the
	// on-disk / in-DB order.
	bySubject := make(map[string][]string, len(seed))
	for _, rb := range rows {
		if rb.Subject == "" || rb.Scope == "" {
			// Defensive: a corrupt row cannot reach compilePrincipalScopes
			// without first breaking the (subject, scope) UNIQUE; skip
			// rather than fail-closed at boot — the row will surface
			// via SQL CHECK on next Put.
			continue
		}
		bySubject[rb.Subject] = append(bySubject[rb.Subject], rb.Scope)
	}
	out := make(map[string]Principal, len(seed))
	for h, p := range seed {
		// Defensive copy of Scopes so the loader never mutates the
		// caller's seed map.
		scopes := make([]string, len(p.Scopes), len(p.Scopes)+len(bySubject[p.Subject]))
		copy(scopes, p.Scopes)
		scopes = append(scopes, bySubject[p.Subject]...)
		out[h] = Principal{
			Subject: p.Subject,
			Role:    p.Role,
			Scopes:  scopes,
			// compiledScopes is intentionally left nil — the caller
			// hands the result to NewStaticTokenVerifier which calls
			// compilePrincipalScopes for every Principal (existing
			// path; auth.go L163). Row scopes therefore compile once
			// at boot, gaining their origin tag from compilePrincipalScopes
			// per the row's `rb.origin` column (PRMT-190 §4.2 —
			// legacy|prefix → originLegacy, "crn:" prefix → originCRN).
		}
	}
	return out, nil
}
