// Package core — rbac.go: the authorize() decision function and
// scope-glob handling. Decision rules are spec-004 §6 + LOCKED L50
// (D14): read implies subtree, write does NOT. The role × action
// matrix is fixed here in code; the data side (Principal.Scopes)
// is the only thing that varies per token.
//
// PRMT-019 §4.3.
// PRMT-113: glob compile moved out of the per-request hot path —
// Principal now carries precompiled exact+subtree globs populated
// at NewStaticTokenVerifier time (see compilePrincipalScopes in
// auth.go). authorize() only does Match(), never Compile.
//
// PRMT-190: crn-aware evaluation. Each scope carries an origin
// (legacy|crn) and a tid; the red line (crn tid ≠ request tenant)
// is enforced via the authorizeTenant seam so this file does not
// need to know about request context. Legacy-origin use is metered
// + warned (sync/atomic counter, stdlib sync.Map for rate-limited
// logging).
package core

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yurimeng/cios/pkg/cpath"
)

// legacyScopeUses counts each authorizeTenant match on a legacy-
// origin (pre-crn) scope during the §6bis transition (PRMT-190
// §4.4). stdlib sync/atomic — no prometheus client. The counter
// is exported as a metric to whichever core metrics surface the
// project wires it into (the in-package accessor LegacyScopeUses
// exists for tests; the canonical exposition wiring belongs to
// PRMT-190-bis, per the §4.4 STOP-or-report rule).
var legacyScopeUses atomic.Int64

// legacyScopeWarned is a per-process set of raw scope patterns
// already emitted a deprecation warning for. PRMT-190 §4.4 pins
// "at most one line per legacy scope pattern per process, using a
// sync.Map of already-warned patterns — no third-party rate
// limiter". The map keys are the raw scope strings (e.g.
// "site01.chiller*"), values are empty structs.
var legacyScopeWarned sync.Map

// LegacyScopeUses returns the current value of the legacy-origin
// deprecation counter. Exposed for tests and for an exposition
// wiring (190-bis) to read; PRMT-190 deliberately does NOT edit
// any metrics handler file (§4.4 STOP rule), so callers in this
// PRMT are limited to in-package tests.
func LegacyScopeUses() int64 {
	return legacyScopeUses.Load()
}

// ResetLegacyScopeUsesForTest zeroes the deprecation counter and
// clears the warn-once set. Tests call this between subtests to
// keep the counter deterministic across cases.
func ResetLegacyScopeUsesForTest() {
	legacyScopeUses.Store(0)
	legacyScopeWarned = sync.Map{}
}

// legacyScopeClosed is the human-flipped window-closure flag
// (PRMT-190-bis §4.4; spec-004 §6bis, R6). When true, every
// authorizeTenant match on a legacy-origin (originLegacy) scope
// returns ErrForbidden + writes one audit line, instead of the
// open-state behaviour of allow + counter++ + warn-once.
//
// Lifecycle:
//   - Default: open (false). The flag is read from the boot env at
//     process start via initLegacyScopeClosed (sync.Once, one read
//     for the process lifetime).
//   - Flip: human-only. The flag has NO programmatic writer in the
//     package; the env var CIOS_LEGACY_CLOSED is the ONLY setter.
//     R6 forbids auto-closure (timers, counters, thresholds) and
//     this grep-provable MUST preserves that. PRMT-186's
//     observation-report tool measures readiness; it never flips.
//   - Tests: SetLegacyScopeClosedForTest toggles the value; tests
//     reset via that helper between cases.
//
// The env-name prefix "CIOS_" matches the project's existing boot
// convention (CIOS_PG_DSN at cmd/cios-core/main.go L140; see the
// envLookup wrapper for the indirection). Values accepted: "1",
// "true", "yes", "on" → closed (case-insensitive). Anything else
// → open. Empty / unset → open (default).
var legacyScopeClosed atomic.Bool

var legacyScopeClosedOnce sync.Once

// initLegacyScopeClosed reads CIOS_LEGACY_CLOSED exactly once per
// process. PRMT-190-bis §4.4 mandates "no code path that sets the
// flag to closed" — this is the single setter, gated on a sync.Once
// so a re-init cannot race.
func initLegacyScopeClosed() {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CIOS_LEGACY_CLOSED")))
	switch v {
	case "1", "true", "yes", "on":
		legacyScopeClosed.Store(true)
	default:
		legacyScopeClosed.Store(false)
	}
}

// LegacyScopeClosed returns the current value of the closure flag.
// Used by tests and by the audit line emission; the authorize path
// inlines the atomic load to avoid a function call on every match.
func LegacyScopeClosed() bool {
	legacyScopeClosedOnce.Do(initLegacyScopeClosed)
	return legacyScopeClosed.Load()
}

// SetLegacyScopeClosedForTest sets the closure flag from a test.
// Production code MUST NOT call this (R6); the lint pattern at the
// call sites makes this grep-provable.
func SetLegacyScopeClosedForTest(v bool) {
	legacyScopeClosedOnce.Do(initLegacyScopeClosed) // initialise the once before any Store
	legacyScopeClosed.Store(v)
}

// warnLegacyScopeOnce emits the §4.4 deprecation line for raw scope
// s at most once per process. Pattern matches across all callers
// (matching legacy raw scopes arrive via principal.Scopes) so the
// sync.Map key is the raw pattern itself, not the compiled glob.
func warnLegacyScopeOnce(raw string) {
	if _, loaded := legacyScopeWarned.LoadOrStore(raw, struct{}{}); loaded {
		return
	}
	log.Printf("core: DEPRECATED legacy dot-glob RBAC scope %q used; migrate to crn (spec-004 §6bis)", raw)
}

// compileScope is a thin wrapper around cpath.CompileGlob used
// only by the load-time validation in NewStaticTokenVerifier (via
// compilePrincipalScopes). It remains exported to the package
// because tests in this package also reach for it to build
// pre-compiled fixtures, and because removing it would change
// the call site of a function that lives in this file.
//
// PRMT-113: authorize no longer calls this on the request path.
func compileScope(pattern string) (cpath.Glob, error) {
	return cpath.CompileGlob(pattern)
}

// authorize decides whether p may perform action on path.
//
// Decision rules (spec-004 §6, LOCKED L50):
//
//   - admin role: allow everything.
//   - Role floor: viewer may only do read; operator may do read +
//     control:write; only admin may apply. Floor failure → ErrForbidden.
//   - Scope match for read: any scope pattern s such that
//     Glob(s).Match(path) OR Glob(s+".**").Match(path). The "+.**"
//     branch is what makes a scope of "site01.pod002" cover its full
//     subtree on Query (L50 "读隐含子树").
//   - Scope match for control:write / apply: any scope pattern s
//     such that Glob(s).Match(path) ONLY — no implicit subtree
//     (L50 "写显式"); to authorize a subtree write the operator must
//     hold a scope like "site01.pod002.**".
//
// All glob compilation happens in NewStaticTokenVerifier
// (PRMT-113). The hot path here does Match() only. For a
// Principal built by struct literal (test fixtures, ctx-attached
// Principals that bypass the verifier) compiledScopes is nil and
// authorize compiles on the fly from Scopes — this branch is not
// on the production request hot path because production requests
// always arrive via Verify(), which returns a Principal whose
// compiledScopes is already populated.
//
// PRMT-190: authorize is now a thin wrapper around authorizeTenant
// with an empty request tenant (the no-tenant posture — admin
// ops-realm per R1, or test code that does not thread ctx). The
// §6bis red line is therefore dormant on this code path. Production
// callers should call authorizeTenant directly with the request
// tenant from PRMT-188 TenantFromContext (PRMT-190-bis wires the
// middleware); keeping authorize() alive preserves the existing
// signature and all current callers in the package (and in the
// "compile-on-the-fly" lazy fallback in tests).
//
// Returns nil on allow, ErrForbidden on deny.
func authorize(p Principal, action Action, path string) error {
	return authorizeTenant(p, action, path, "")
}

// authorizeTenant is the package-private variant of authorize that
// takes a request tenant identity for the §6bis cross-tenant red
// line (PRMT-190 §4.3). The red line fires only when:
//
//   - the matching scope's origin is originCRN, AND
//   - requestTenant != "" (a tenant was attached to the request), AND
//   - sg.tid != requestTenant.
//
// Legacy-origin scopes (originLegacy) carry tid == "" (the
// transition-tenant sentinel, see auth.go) so the red line does not
// 403 on tid for them — the scope is not yet tenant-bound. The
// deprecation meter still increments on every legacy match.
//
// When requestTenant is "" the red line is dormant (ops-realm
// posture, R1). Admin bypass and role floor are unchanged.
//
// Caller convention: pass TenantFromContext(ctx) when a request
// context is available; pass "" otherwise (tests, internal
// non-request callers). The middleware wiring that threads the real
// tenant lands in PRMT-190-bis, which edits authmw.go (out of this
// PRMT's whitelist per §3).
func authorizeTenant(p Principal, action Action, path, requestTenant string) error {
	// Role floor first (short-circuit before any scope work).
	if err := roleAllows(p, action); err != nil {
		return err
	}
	// Admin bypasses scope entirely — the role itself is the ceiling.
	if p.Role == RoleAdmin {
		return nil
	}

	// Lazy fallback: a Principal built by struct literal (tests,
	// ctx-attached Principals) has no compiledScopes. Compile on
	// first use so those call sites keep working; in production,
	// every Principal arrives via Verify() with compiledScopes set
	// and this branch is dead.
	scopes := p.compiledScopes
	if scopes == nil {
		ready, err := compilePrincipalScopes(p)
		if err != nil {
			// Bad scope (shouldn't reach here in production — load
			// time validation already rejected). Fail closed.
			return ErrForbidden
		}
		scopes = ready.compiledScopes
	}

	// Scope match — at least one Principal scope must match.
	for i, sg := range scopes {
		if sg.exact.Match(path) {
			return sg.afterMatch(p, i, requestTenant)
		}
		// L50 read-implies-subtree: a scope that names an asset also
		// covers everything below it for reads. We do NOT do this for
		// writes — operator with scope "site01.pod002" cannot Set a
		// point under that pod; they need "site01.pod002.**".
		if action == ActionRead {
			if sg.subtree.Match(path) {
				return sg.afterMatch(p, i, requestTenant)
			}
		}
	}
	return ErrForbidden
}

// roleAllows is the "role floor" half of authorize: it checks
// whether p.Role permits action, WITHOUT touching scopes. The HTTP
// middleware uses it as a soft gate for list endpoints (where the
// per-item scope check is delegated to the handler, since only the
// handler holds the items). The full per-item decision still goes
// through authorize; roleAllows only exists so the middleware can
// avoid calling authorize against the synthetic path "**" on
// collection endpoints (no viewer's scope can ever match that
// literal — see PRMT-022 §1 R1 STOP-RESULT for the probe data).
//
// Decision rules (mirrors the role floor of authorize, split out
// purely for code sharing; admin is a full bypass, not a "floor"):
//
//   - admin:        any action
//   - operator:     read, control:write (not apply)
//   - viewer:       read only
//   - unknown role: ErrForbidden (fail closed)
//
// PRMT-022 R2 §4.0.
func roleAllows(p Principal, action Action) error {
	switch p.Role {
	case RoleAdmin:
		return nil
	case RoleOperator:
		if action != ActionRead && action != ActionControlWrite {
			return ErrForbidden
		}
		return nil
	case RoleViewer:
		if action != ActionRead {
			return ErrForbidden
		}
		return nil
	default:
		// Unknown role (typo in config, M3 tenant before it ships, etc.).
		// Fail closed.
		return ErrForbidden
	}
}

// afterMatch is the per-scope post-match hook that fires the §6bis
// red line and the legacy deprecation meter (PRMT-190 §4.3, §4.4).
// It is called from authorizeTenant after a scopeGlob has matched
// (exact OR subtree-for-read). Returning nil means allow;
// ErrForbidden means deny (cross-tenant). scopeIndex identifies the
// matched scope in p.Scopes so the deprecation warning can name
// the original raw pattern.
//
// i is the index of sg in p.compiledScopes (== p.Scopes after the
// copy in compilePrincipalScopes); we read the raw pattern from
// p.Scopes[i] for the warn-once key so the log message references
// the exact scope string the operator wrote in config.
func (sg scopeGlob) afterMatch(p Principal, i int, requestTenant string) error {
	// §6bis red line: a crn-origin scope whose tid does not match
	// the request tenant is denied. requestTenant == "" means
	// "no tenant attached" (ops-realm admin posture or test code);
	// the red line is dormant in that case. Legacy-origin scopes
	// carry tid == transitionTenantSentinel ("") so they take the
	// dormant branch as well — the deprecation meter below still
	// fires.
	if sg.origin == originCRN && requestTenant != "" && sg.tid != requestTenant {
		return ErrForbidden
	}
	// PRMT-190-bis §4.4 window-closure flag: when the human-flipped
	// flag is closed and the matched scope is originLegacy, reject
	// the match (403) and write one audit line per §4.4. The flag
	// is checked BEFORE the legacy meter increments so a denied
	// match does not pollute the readiness counter — the report
	// tool (PRMT-186) sees "zero legitimate legacy use" rather than
	// "zero legitimate legacy use + N denied post-closure".
	if sg.origin == originLegacy && legacyScopeClosed.Load() {
		if i >= 0 && i < len(p.Scopes) {
			log.Printf("core: legacy RBAC grammar rejected post-closure subject=%q scope=%q", p.Subject, p.Scopes[i])
		} else {
			log.Printf("core: legacy RBAC grammar rejected post-closure subject=%q scope=<unindexed>", p.Subject)
		}
		return ErrForbidden
	}
	// Deprecation meter for legacy-origin matches (PRMT-190 §4.4).
	// warnLegacyScopeOnce is rate-limited to one log line per raw
	// pattern per process. The counter increments regardless of the
	// warn-throttle so a metrics scrape always sees every legacy use.
	if sg.origin == originLegacy {
		legacyScopeUses.Add(1)
		if i >= 0 && i < len(p.Scopes) {
			warnLegacyScopeOnce(p.Scopes[i])
		}
	}
	return nil
}
