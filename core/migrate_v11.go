// core/migrate_v11.go — PRMT-186 v1.1 one-shot migration + report tool
// (L101 D4, spec-001 v1.1 §5bis.2, spec-004 v1.1 §6bis).
//
// This file CALLS existing primitives (PRMT-184/185/189/190/190-bis).
// It does not add tables, store methods, or routes. It writes
// RoleBinding-rewrite diffs to a dedicated, append-only migration
// audit sink — NOT to tenant_audit (per §0.8 of PRMT-186, no audit
// op token exists for a scope rewrite; 184's CHECK is tenant-record-
// only).
//
// The migration is idempotent: re-running on an already-migrated
// store creates no org, attaches no site again, re-rewrites no
// already-crn row, and appends no duplicate diff.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// --- Constants (mirrors rbac.go vocabulary) ------------------------------

// RoleBinding origin vocabulary. These are the string forms stored
// in the role_bindings.origin column; the typed enum scopeOrigin
// (auth.go) is the in-process evaluation form. We use the raw string
// here because PutRoleBinding's signature is RoleBinding.Origin string.
const (
	rbOriginLegacy = "legacy"
	rbOriginCRN    = "crn"
)

// DefaultOrgName is the per-tenant `default` org that PRMT-186
// materialises (spec-001 v1.1 §5bis.2).
const DefaultOrgName = "default"

// --- Report types ---------------------------------------------------------

// MigrationAuditSink is the append-only destination for the
// RoleBinding-rewrite pre/post diff. Deliberately NOT tenant_audit
// (PRMT-186 §0.8 / §4.3). Default impl writes timestamped JSONL.
type MigrationAuditSink interface {
	RecordRewrite(subject, oldScope, newScope string) error
}

// RewriteDiff is one pre/post diff record the migration writes to
// the sink. TS is wall-clock at record time (not txn time, because
// the sink is append-only and lives outside the role_bindings txn).
type RewriteDiff struct {
	TS       time.Time `json:"ts"`
	Subject  string    `json:"subject"`
	OldScope string    `json:"old_scope"`
	NewScope string    `json:"new_scope"`
}

// MigrateReport is the per-run summary MigrateV11 returns to the
// caller / CLI.
type MigrateReport struct {
	TenantsSeen     int           `json:"tenants_seen"`
	OrgsEnsured     int           `json:"orgs_ensured"`       // new `default` orgs created
	OrgsAlready     int           `json:"orgs_already"`       // ErrOrgNameConflict swallows
	SitesAttached   int           `json:"sites_attached"`     // new site→org links
	SitesAlready    int           `json:"sites_already"`      // already-linked sites
	RBTotal         int           `json:"rb_total"`           // total rows seen
	RBRewritten     int           `json:"rb_rewritten"`       // legacy→crn writes
	RBSkippedCRN    int           `json:"rb_skipped_crn"`     // already-crn rows untouched
	RBSkippedNoSite int           `json:"rb_skipped_no_site"` // legacy rows with no resolvable site
	Diffs           []RewriteDiff `json:"diffs"`              // what was appended to the sink
}

// ClosureReport is the report mode output (PRMT-186 §4.2). It
// reports the in-process legacyScopeUses counter (PRMT-190's
// legacyScopeUses, exposed via core.LegacyScopeUses()) and states
// honestly that the counter is in-process and cannot prove
// historical zero-days without scrape history.
type ClosureReport struct {
	DaysRequested      int    `json:"days_requested"`
	LegacyScopeUsesNow int64  `json:"legacy_scope_uses_now"`
	ClosureFlagOpen    bool   `json:"closure_flag_open"`
	ClosureReady       bool   `json:"closure_ready"`
	HistoricalEvidence bool   `json:"historical_evidence_available"`
	Note               string `json:"note"`
}

// --- Default sink ---------------------------------------------------------

// JSONLMigrationAuditSink writes one JSONL line per rewrite to the
// supplied io.Writer. Safe for concurrent use (the migration loops
// are serial today but a future parallel rewrite must not tear
// lines).
type JSONLMigrationAuditSink struct {
	mu sync.Mutex
	w  io.Writer
	ts func() time.Time // injectable for tests
}

// NewJSONLMigrationAuditSink returns a JSONL sink writing to w.
// Pass os.Stdout or any *os.File for production.
func NewJSONLMigrationAuditSink(w io.Writer) *JSONLMigrationAuditSink {
	return &JSONLMigrationAuditSink{w: w, ts: time.Now}
}

// RecordRewrite appends one diff line. Time comes from the
// sink's clock; pass time.Now for production.
func (s *JSONLMigrationAuditSink) RecordRewrite(subject, oldScope, newScope string) error {
	d := RewriteDiff{
		TS:       s.ts(),
		Subject:  subject,
		OldScope: oldScope,
		NewScope: newScope,
	}
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("core: migrate v11: encode diff: %w", err)
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("core: migrate v11: write diff: %w", err)
	}
	return nil
}

// --- Site→tenant attribution (PRMT-186 §4.1) ------------------------------

// siteTenantMap is the attribution result for a single MigrateV11
// run. It is built from a) the existing site_orgs rows (PRMT-189)
// and b) the single-tenant case (every site → the sole tenant).
// Multi-tenant with no site_orgs signal is the STOP case (see
// buildSiteTenantMap).
type siteTenantMap struct {
	// site → tenantID
	sites map[string]string
	// tenants that have at least one site attributed; set membership
	// is what the rewrite loop uses to decide which RB rows to touch.
	tenantsWithSites map[string]bool
}

func newSiteTenantMap() *siteTenantMap {
	return &siteTenantMap{
		sites:            map[string]string{},
		tenantsWithSites: map[string]bool{},
	}
}

func (m *siteTenantMap) set(site, tenantID string) {
	m.sites[site] = tenantID
	m.tenantsWithSites[tenantID] = true
}

// buildSiteTenantMap constructs the site→tenant attribution per
// PRMT-186 §4.1:
//
//   - Existing site_orgs rows (PRMT-189) give a tenant via the
//     org's TenantID. That is the primary signal.
//   - For sites NOT in site_orgs: if the system has exactly one
//     tenant, attribution is unambiguous (every site → sole tenant's
//     `default`). Otherwise, STOP — do not invent a rule.
func buildSiteTenantMap(
	ctx context.Context,
	st Store,
	tenants []Tenant,
	allAssets []Asset,
	allSiteOrgs []SiteOrg,
	allOrgsByTenant map[string][]Org,
) (*siteTenantMap, error) {
	m := newSiteTenantMap()

	// (1) site_orgs → org → tenant
	for _, so := range allSiteOrgs {
		var tenantID string
		for tid, orgs := range allOrgsByTenant {
			for _, o := range orgs {
				if o.ID == so.OrgID {
					tenantID = tid
					break
				}
			}
			if tenantID != "" {
				break
			}
		}
		if tenantID == "" {
			// site_orgs row references an org we did not see; skip
			// rather than fail. The migration is best-effort on
			// pre-existing linkage and the next attachSiteToOrg
			// will repair it.
			continue
		}
		m.set(so.Site, tenantID)
	}

	// (2) assets → site (first dot-segment of path; cpath's
	// AssetPath.Site is the first segment and the seed pipeline
	// only writes paths that pass cpath validation, so a plain
	// dot-split on already-validated asset paths is safe).
	seenSites := map[string]bool{}
	for _, a := range allAssets {
		s := firstPathSegment(a.Path)
		if s == "" || seenSites[s] {
			continue
		}
		seenSites[s] = true
		if _, ok := m.sites[s]; ok {
			continue
		}
		if len(tenants) == 1 {
			// single-tenant: unambiguous
			m.set(s, tenants[0].ID)
			continue
		}
		// Multi-tenant + no signal → STOP per §4.1.
		return nil, fmt.Errorf(
			"core: migrate v11: site %q is not in site_orgs and there are %d tenants; cannot invent an attribution rule (PRMT-186 §4.1)",
			s, len(tenants),
		)
	}
	return m, nil
}

// firstPathSegment returns the first dot-separated segment of an
// asset path (== cpath.AssetPath.Site). Empty input → "". We do
// not import cpath here because ParseAssetPath requires a *Dict
// (the seed uses a closure-bound Dict the migration does not
// share); the seed pipeline guarantees paths we read from
// ListAssets are already validated.
func firstPathSegment(path string) string {
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			return path[:i]
		}
	}
	return path
}

// --- Migration entry (PRMT-186 §4.1) --------------------------------------

// MigrateV11 runs the one-shot v1.1 transition (L101 D4). Idempotent.
// See PRMT-186 §4.1 for the full contract.
func MigrateV11(ctx context.Context, st Store, principal string, sink MigrationAuditSink) (MigrateReport, error) {
	if sink == nil {
		return MigrateReport{}, fmt.Errorf("core: migrate v11: sink must not be nil")
	}
	if principal == "" {
		principal = "system:migrate-v11"
	}
	var rep MigrateReport

	// Snapshot: read everything before any write so the
	// site→tenant attribution reflects a single consistent
	// pre-migration world.
	tenants, err := st.ListTenants(ctx)
	if err != nil {
		return rep, fmt.Errorf("core: migrate v11: list tenants: %w", err)
	}
	rep.TenantsSeen = len(tenants)

	// Pre-index orgs by tenant for the attribution pass.
	allOrgsByTenant := map[string][]Org{}
	for _, t := range tenants {
		orgs, err := st.ListOrgs(ctx, t.ID)
		if err != nil {
			return rep, fmt.Errorf("core: migrate v11: list orgs for %s: %w", t.ID, err)
		}
		allOrgsByTenant[t.ID] = orgs
	}

	assets, err := st.ListAssets(ctx)
	if err != nil {
		return rep, fmt.Errorf("core: migrate v11: list assets: %w", err)
	}
	allSiteOrgs, err := st.ListSiteOrgs(ctx)
	if err != nil {
		return rep, fmt.Errorf("core: migrate v11: list site orgs: %w", err)
	}

	stm, err := buildSiteTenantMap(ctx, st, tenants, assets, allSiteOrgs, allOrgsByTenant)
	if err != nil {
		return rep, err
	}

	// Pass 1: ensure `default` org per tenant (185), backfill sites
	// (189). Idempotent.
	defaultOrgID := map[string]string{} // tenantID → default org ID
	for _, t := range tenants {
		// ensure default
		org, err := st.CreateOrg(ctx, t.ID, DefaultOrgName, principal)
		if err != nil {
			if errors.Is(err, ErrOrgNameConflict) {
				// already exists; look it up
				orgs := allOrgsByTenant[t.ID]
				rep.OrgsAlready++
				var found bool
				for _, o := range orgs {
					if o.Name == DefaultOrgName {
						org = o
						found = true
						break
					}
				}
				if !found {
					return rep, fmt.Errorf(
						"core: migrate v11: tenant %s: %w returned but org not visible in list",
						t.ID, ErrOrgNameConflict,
					)
				}
			} else {
				return rep, fmt.Errorf("core: migrate v11: create default org for %s: %w", t.ID, err)
			}
		} else {
			rep.OrgsEnsured++
		}
		defaultOrgID[t.ID] = org.ID
	}

	// Pass 2: attach every attributed site to its tenant's
	// `default` org. AttachSiteToOrg is idempotent (189).
	for site, tenantID := range stm.sites {
		orgID, ok := defaultOrgID[tenantID]
		if !ok {
			// tenant with no `default` — cannot happen because pass 1
			// is exhaustive.
			return rep, fmt.Errorf(
				"core: migrate v11: site %s attributed to tenant %s but no default org ID",
				site, tenantID,
			)
		}
		// Skip if already linked to this exact org (189 keeps it
		// idempotent; we still call it for a fresh re-home case
		// where site moved orgs in the substrate).
		var already bool
		for _, so := range allSiteOrgs {
			if so.Site == site && so.OrgID == orgID {
				already = true
				break
			}
		}
		if already {
			rep.SitesAlready++
			continue
		}
		if err := st.AttachSiteToOrg(ctx, site, orgID, principal); err != nil {
			return rep, fmt.Errorf("core: migrate v11: attach site %s to org %s: %w", site, orgID, err)
		}
		rep.SitesAttached++
	}

	// Pass 3: rewrite legacy-origin RoleBinding rows to crn-form
	// under org/default (190 + 190-bis). crn-origin rows are
	// skipped. For each legacy row we (a) write the new crn row
	// via PutRoleBinding (subject stays the same; scope differs →
	// append succeeds on a different (subject, scope) tuple);
	// THEN (b) DeleteRoleBinding(subject, oldScope) to retire the
	// legacy row. The diff goes to the dedicated sink, never to
	// tenant_audit. Idempotent: a re-run sees no legacy row to
	// delete and the crn row already present → PutRoleBinding
	// upserts in place, DeleteRoleBinding no-ops, no diff written.
	bindings, err := st.ListAllRoleBindings(ctx)
	if err != nil {
		return rep, fmt.Errorf("core: migrate v11: list role bindings: %w", err)
	}
	rep.RBTotal = len(bindings)

	for _, rb := range bindings {
		if rb.Origin == rbOriginCRN {
			rep.RBSkippedCRN++
			continue
		}
		// legacy-origin: derive the site (first path segment of
		// the dot-glob scope) and look up its tenant.
		site, ok := legacyScopeSite(rb.Scope)
		if !ok {
			// No parseable site — we cannot reattribute this row
			// to a tenant's `default`. Skip (the audit-sink diff
			// is conditional on a successful rewrite, so no diff
			// is appended for a skipped row).
			rep.RBSkippedNoSite++
			continue
		}
		tenantID, ok := stm.sites[site]
		if !ok {
			rep.RBSkippedNoSite++
			continue
		}
		// Note: we use tenantID (not the default org ID) when
		// calling legacyToCRN — the crn's tid field is the tenant
		// token; the org is hard-coded to "default" by legacyToCRN
		// (PRMT-190 §4.3 contract).

		// Build the new scope: legacyToCRN re-routes the dot-glob
		// under tenant/org/default, with the new tenantID.
		newScope := legacyToCRN(rb.Scope, tenantID).String()
		oldScope := rb.Scope

		// idempotency: if the row's current scope already
		// matches the would-be rewrite target, do not re-write
		// (no diff).
		if oldScope == newScope {
			continue
		}

		// (a) Write the crn row FIRST. Mint a fresh ID — the legacy
		// row's id is its PRIMARY KEY in pgStore, and the putRoleBinding
		// ON CONFLICT clause only covers (subject, scope), NOT id. A
		// fresh id avoids the duplicate-PK error that an id-reuse
		// would hit on the first rewrite in production. fileStore has
		// no PK so id-reuse would not surface there; pgStore (where the
		// schema's PRIMARY KEY is `id`) does. Audit-sink diff is keyed
		// on (subject, oldScope, newScope), not on row id — id reuse
		// was not load-bearing for traceability.
		crnRow := RoleBinding{
			ID:        newRoleBindingID(),
			Subject:   rb.Subject,
			Scope:     newScope,
			Origin:    rbOriginCRN,
			CreatedAt: rb.CreatedAt,
			UpdatedAt: time.Now().UTC(),
		}
		if err := st.PutRoleBinding(ctx, crnRow); err != nil {
			return rep, fmt.Errorf(
				"core: migrate v11: put role binding subject=%s: %w", rb.Subject, err,
			)
		}
		// (b) THEN remove the legacy (subject, oldScope) row.
		// The crn row is already in place; an interrupted
		// migration that crashes between (a) and (b) leaves an
		// extra crn row but the next pass sees its scope/form
		// and the (b) below in this loop retires the orphan.
		if err := st.DeleteRoleBinding(ctx, rb.Subject, oldScope); err != nil {
			return rep, fmt.Errorf(
				"core: migrate v11: delete legacy role binding subject=%s scope=%s: %w",
				rb.Subject, oldScope, err,
			)
		}

		if err := sink.RecordRewrite(rb.Subject, oldScope, newScope); err != nil {
			return rep, fmt.Errorf("core: migrate v11: audit sink: %w", err)
		}
		rep.RBRewritten++
		rep.Diffs = append(rep.Diffs, RewriteDiff{
			TS:       time.Now(),
			Subject:  rb.Subject,
			OldScope: oldScope,
			NewScope: newScope,
		})
	}
	return rep, nil
}

// legacyScopeSite returns the first path segment of a dot-glob
// scope (the site slug), or "",false if the scope is unparseable.
// This is the site signal we need for reattribute-to-tenant; the
// crn-form scopes are skipped before this is called.
func legacyScopeSite(scope string) (string, bool) {
	// dot-glob: e.g. "site01.pod002.chiller*" → first segment
	for i := 0; i < len(scope); i++ {
		if scope[i] == '.' {
			s := scope[:i]
			if s == "" {
				return "", false
			}
			return s, true
		}
	}
	// bare token (no dot): treat the whole thing as a site (and
	// rely on tenant attribution to fail-closed in the multi-tenant
	// case).
	if scope == "" {
		return "", false
	}
	return scope, true
}

// --- Report entry (PRMT-186 §4.2) -----------------------------------------

// ReportLegacyUse reports the §6bis closure criterion from the
// in-process metric only. It NEVER flips the closure flag (R6 —
// human-only; see PRMT-190-bis config). days is currently advisory
// (the counter is in-process and cannot prove historical zero-days
// without scrape history; we surface that honestly).
func ReportLegacyUse(_ context.Context, days int) (ClosureReport, error) {
	if days <= 0 {
		days = 30
	}
	uses := LegacyScopeUses()
	r := ClosureReport{
		DaysRequested:      days,
		LegacyScopeUsesNow: uses,
		ClosureFlagOpen:    !LegacyScopeClosed(),
		ClosureReady:       uses == 0,
		// PRMT-190 exposes the metric as an in-process counter;
		// we cannot prove historical zero-days without a scrape
		// history the substrate does not ship. We are honest about
		// this in the Note — a human verifies before flipping.
		HistoricalEvidence: false,
		Note: fmt.Sprintf(
			"in-process counter only; cannot prove %d consecutive zero-days without scrape history. "+
				"Closure flag is NOT flipped by this command (R6 — human-only via 190-bis config).",
			days,
		),
	}
	return r, nil
}

// --- File-based sink helper (CLI default) --------------------------------

// OpenMigrationAuditFile opens path for append, creating it if
// absent. The CLI calls this once at startup and passes the
// resulting *os.File to NewJSONLMigrationAuditSink. Returns a
// Closer-wrapped sink the CLI must Close() at the end.
func OpenMigrationAuditFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("core: migrate v11: empty audit sink path")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("core: migrate v11: open audit sink %q: %w", path, err)
	}
	return f, nil
}
