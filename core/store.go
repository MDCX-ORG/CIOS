// Package core is the cios-core M0 site API: HTTP endpoints for asset
// registration, metric/point reads, and alarm listing. Storage is a
// single JSON file written atomically (tmp + rename) behind an
// in-memory index protected by a sync.RWMutex. The Store interface
// is the seam M1 will swap for PostgreSQL.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─── Sections (navigation only — no behavior) ───────────────────────────────
//   1. Entity types              — Asset / Alarm / Ticket / PM / Spare / ...
//   2. Store interface           — the contract pgStore + fileStore satisfy
//   3. fileStore struct + ctor   — in-memory index, atomic JSON persist
//   4. fileStore: assets/alarms  — Put/Get/List/Delete + SeedAlarms
//   5. fileStore: tickets/PM     — ticket CRUD + PM schedule
//   6. fileStore: audit/spare    — AssetAudit + SparePart / SpareTxn
//   7. fileStore: inspect/notes  — Inspection template, notes, audit
//   8. fileStore: mwindow        — PRMT-096 (maintenance windows)
// ─────────────────────────────────────────────────────────────────────────────

// Asset is one registered physical asset. Spec is a free-form
// pass-through for M0 (only spec.type is validated against the path
// leaf node type by the HTTP layer). ResourceVersion increments on
// every PutAsset overwrite; 0 means "create-or-force-overwrite",
// >0 means "optimistic lock against this value".
type Asset struct {
	Path            string         `json:"path"`
	ResourceVersion int64          `json:"resource_version"`
	Spec            map[string]any `json:"spec"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Alarm mirrors the M0 subset of spec-003 §2/§4. The state machine
// is out of scope for M0 (no rule engine), so we only list what is
// already in the seed file.
type Alarm struct {
	ID       string     `json:"id"`
	Path     string     `json:"path"`
	Severity string     `json:"severity"` // critical|major|minor|info
	State    string     `json:"state"`    // firing|acked|resolved
	Summary  string     `json:"summary"`
	Since    time.Time  `json:"since"`
	AckedBy  string     `json:"acked_by,omitempty"` // PRMT-230: principal subject that acked
	AckedAt  *time.Time `json:"acked_at,omitempty"` // PRMT-230: nil while firing
}

// ErrAlarmNotAckable is returned by AckAlarm when the alarm exists
// but is not in state "firing" (spec-003 §4: firing→acked is the
// only ack transition; re-ack and ack-of-resolved are conflicts).
var ErrAlarmNotAckable = errors.New("core: alarm not in firing state")

// Ticket is one operations work item (M2 E2.3). State machine and
// HTTP/CLI live in PRMT-033; this file is data-layer only.
// Nullable timestamps are *time.Time (nil = not yet reached).
// EscalatedAt is the SLA-breach marker (PRMT-036, spec-008 §3 + §5);
// nil = never breached. Once set, the SLA scanner never re-fires for
// that ticket (idempotent escalation, see core/sla.go).
//
// ResourceVersion is the optimistic-lock counter (PRMT-082); it
// increments on every successful PutTicket overwrite. Read by the
// HTTP transition/assign handlers before mutating, passed back as
// expectVersion so a racing writer loses with ErrVersionConflict
// (mapped to 409 RFC 7807 by the handler). Mirrors Asset's
// resource_version (PRMT-016b); the migration adds the column with
// DEFAULT 0 so existing rows are addressable (create path uses
// expectVersion=0).
type Ticket struct {
	ID              string     `json:"id"`
	AlarmID         string     `json:"alarm_id,omitempty"` // "" for manually-opened tickets
	AssetPath       string     `json:"asset_path"`
	Title           string     `json:"title"`
	Severity        string     `json:"severity"` // critical|major|minor|info (mirror Alarm)
	State           string     `json:"state"`    // open|acknowledged|resolved|closed
	Assignee        string     `json:"assignee,omitempty"`
	OpenedAt        time.Time  `json:"opened_at"` // NOT NULL
	AckedAt         *time.Time `json:"acked_at,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	EscalatedAt     *time.Time `json:"escalated_at,omitempty"`
	ResourceVersion int64      `json:"resource_version"`
	// Runbook is the knowledge-base key from the originating
	// AlarmRule (e.g. "rb/cdu-deltat-low"). Auto-opened tickets
	// carry the rule's runbook; manual tickets and PM tickets
	// have an empty Runbook. PRMT-044.
	Runbook string `json:"runbook,omitempty"`
}

// PMSchedule is one preventive-maintenance plan (M2 E2.4 P531 /
// PRMT-043). The scanner (core/pm.go) opens a ticket when
// `now >= NextDue && Enabled`. M2 ships calendar triggers only;
// meter (runhours) is a stub for v0.4 per spec-008 v0.3 Q12.
//
// ID is "pm_" + 16 base32 chars (mirror newTicketID's shape).
// LastRun is nil until the scanner has fired once. NextDue is
// always populated (initial = now + IntervalDays at create time;
// the scanner advances it after each successful open).
type PMSchedule struct {
	ID           string     `json:"id"`
	AssetPath    string     `json:"asset_path"`
	Kind         string     `json:"kind"` // calendar only in M2
	IntervalDays int        `json:"interval_days"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	NextDue      time.Time  `json:"next_due"`
	Title        string     `json:"title"`
	Severity     string     `json:"severity"` // critical|major|minor|info
	Enabled      bool       `json:"enabled"`
}

// AssetAudit is one immutable change record for an asset
// (M2 E2.1 P512 / PRMT-045). Append-only: there is no
// update/delete path. ID is "au_" + 16 base32 chars
// (mirror newTicketID's shape, distinct prefix).
//
// Op is one of:
//
//	"put"      — PUT /v1/assets/{path} (create or update)
//	"lifecycle"— POST /v1/assets/{path}:lifecycle
//	"delete"   — DELETE /v1/assets/{path}
//
// Detail is a free-form string the writer fills in (e.g.
// "1→2", "active→retired", "cascade=true,n=3").
type AssetAudit struct {
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	Principal string    `json:"principal"`
	Path      string    `json:"path"`
	Op        string    `json:"op"`
	Detail    string    `json:"detail"`
}

// Tenant is the tenant record defined by spec-001 v1.1 §5bis.1.
// ID is a slug in [a-z][a-z0-9-]{1,30} (validated at the store
// boundary by validTenantSlug). IsolationTier is one of
// "label" / "row" / "db"; Status is one of "active" / "suspended".
// Both are CHECK-constrained at the SQL layer and matched at the
// application layer by downstream write PRMTs (PRMT-182).
//
// This PRMT ships the read-side Store methods only — mutators
// (Create / Update / TierChange / StatusChange) arrive with
// their consuming PRMTs (182 / 185 / 186).
type Tenant struct {
	ID            string    `json:"id"`
	DisplayName   string    `json:"display_name"`
	IsolationTier string    `json:"isolation_tier"` // label|row|db
	Status        string    `json:"status"`         // active|suspended
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Org is the organisation record defined by spec-001 v1.1 §5bis.2.
// ID is "og_" + 16 base32 chars (newOrgID). Name is unique within
// TenantID — enforced by the SQL UNIQUE (tenant_id, name) and the
// matching idx. This PRMT ships read-side Store methods only;
// mutators (Create / Rename / Re-attach / Delete) arrive with
// their consuming PRMTs (185 / 186).
type Org struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// SiteOrg — spec-001 v1.1 §5bis.2 site→Org mapping. Site is the
// slug (cpath.AssetPath.Site), unique (one owning org per site).
type SiteOrg struct {
	Site      string    `json:"site"`
	OrgID     string    `json:"org_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RoleBinding is one persisted RBAC scope grant
// (E3.1 / PRMT-190-bis §4.2; spec-004 §6bis, R3). The static token
// config stays the authn seed (token → subject/role); these rows
// are the scope grants the v1.1 migration (PRMT-186) mechanically
// rewrites (dot-glob → crn). Origin mirrors PRMT-190's scopeOrigin
// vocabulary: "legacy" = pre-crn v1.0 dot-glob (deprecated, metered
// + warn-once at authorize time), "crn" = native §6bis crn. The
// window-closure flag in core/rbac.go rejects origin==legacy
// matches when flipped closed; crn-origin scopes are unaffected.
//
// ID is "rb_" + 16 base32 chars (mirror newOrgID's shape, see
// core/tenant.go newRoleBindingID). Subject matches
// Principal.Subject from the authn seed. Scope is the raw pattern
// the same way it would appear in rbac.*.yaml (dot-glob OR crn).
type RoleBinding struct {
	ID        string    `json:"id"`
	Subject   string    `json:"subject"`
	Scope     string    `json:"scope"`
	Origin    string    `json:"origin"` // legacy|crn
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TenantAudit is one immutable change record for a tenant or org
// (spec-001 v1.1 §5bis "append-only 租户审计 (actor + 前后值)"; mirrors
// AssetAudit's shape with TenantID in place of Path). Append-only
// — there is no Update / Delete path by design (audit integrity).
//
// Op is one of the seven values fixed in migrations/015_tenant_org.sql
// CHECK constraint — 'tenant_create', 'tier_change', 'tenant_status',
// 'org_create', 'org_rename', 'org_reattach', 'org_delete'. Detail is
// a free-form string the writer fills in (e.g. "label→row",
// "name:eng→engineering"). ID is "ta_" + 16 base32 chars
// (newTenantAuditID, mirror of newAuditID's shape).
type TenantAudit struct {
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	Principal string    `json:"principal"`
	TenantID  string    `json:"tenant_id"`
	Op        string    `json:"op"`
	Detail    string    `json:"detail"`
}

// SparePart is one catalog entry plus its current stock level
// (M2 E2.5 P541 / PRMT-048). ID is "sp_" + 16 base32 chars
// (mirror newTicketID's shape, distinct prefix). Qty is the
// current on-hand count (≥0, enforced by SQL CHECK); MinQty
// is the safety-stock threshold — low_stock is a DERIVED flag
// (qty < min_qty) computed at read time, not persisted. No
// procurement / supplier / price fields by prompt MUST NOT.
type SparePart struct {
	ID       string `json:"id"`
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Qty      int    `json:"qty"`
	MinQty   int    `json:"min_qty"`
	Location string `json:"location,omitempty"`
}

// SpareTxn is one append-only stock movement (PRMT-048 §2). ID is
// "st_" + 16 base32 chars. Delta > 0 = inbound (restock); delta <
// 0 = outbound (consumed). TicketID is the optional consuming
// ticket ("tk_..." when the operator pulled the part to fix
// something). Stock mutations go exclusively through :adjust,
// which writes one txn and updates spare_parts.qty atomically.
type SpareTxn struct {
	ID       string    `json:"id"`
	SpareID  string    `json:"spare_id"`
	Delta    int       `json:"delta"`
	TicketID string    `json:"ticket_id,omitempty"`
	At       time.Time `json:"at"`
}

// TicketNote is one append-only record on a ticket
// (M2 E2.3 / PRMT-060). Author is the principal at the time
// of write, or "anonymous" when no Authorization header is
// present. Body is the operator's text. The store contract
// has no Update / Delete path on ticket notes — notes are
// evidence (M4 training corpus). ID is "tn_" + 16 base32
// chars (mirror newTicketID's shape, distinct prefix).
type TicketNote struct {
	ID       string    `json:"id"`
	TicketID string    `json:"ticket_id"`
	Author   string    `json:"author"`
	Body     string    `json:"body"`
	At       time.Time `json:"at"`
}

// TicketAudit is one immutable state-change record on a
// ticket (M2 E2.3 / PRMT-061). Append-only — there is no
// update/delete path; the audit log is evidence for ops
// forensics (对齐 PRMT-045 asset_audit 形态).
//
// Op is one of:
//
//	"created"      — POST /v1/tickets
//	"transitioned" — POST /v1/tickets/{id}:transition
//	"assigned"     — POST /v1/tickets/{id}:assign
//
// FromState / ToState are nullable: "created" has no prior
// state (FromState == ""), "assigned" is not a state-machine
// transition (FromState == ToState == ""; the assignee is
// captured separately on the ticket). Who is the principal
// at the time of write, or "anonymous" when no Authorization
// header is present. ID is "ta_" + 16 base32 chars (mirror
// newTicketID's shape, distinct prefix).
type TicketAudit struct {
	ID        string    `json:"id"`
	TicketID  string    `json:"ticket_id"`
	Op        string    `json:"op"`
	FromState string    `json:"from_state,omitempty"`
	ToState   string    `json:"to_state,omitempty"`
	Who       string    `json:"who"`
	At        time.Time `json:"at"`
}

// InspectionTemplate is one recurring inspection plan
// (M2 E2.7 P561 / PRMT-049). The scanner (core/inspection.go)
// opens a ticket when `now >= NextDue && Enabled`, then advances
// NextDue by Interval. M2 ships calendar triggers only; meter
// / runhours is out of scope per the prompt (mirrors PM's
// spec-008 v0.3 Q12 stub).
//
// ID is "ins_" + 16 base32 chars (mirror newTicketID's shape).
// Items is the checklist; it is JSON-encoded as a string array.
// No nullable timestamps by design — LastRun is intentionally
// absent to keep the schema and advance-then-fire idempotency
// minimal (PM's LastRun is unused for that purpose too — the
// advance of NextDue is the in-memory gate).
type InspectionTemplate struct {
	ID        string        `json:"id"`
	AssetPath string        `json:"asset_path"`
	Title     string        `json:"title"`
	Items     []string      `json:"items"`
	Interval  time.Duration `json:"interval"` // stored as nanoseconds; > 0
	NextDue   time.Time     `json:"next_due"`
	Enabled   bool          `json:"enabled"`
}

// MaintenanceWindow is one explicit maintenance period declared
// by an operator (M2 E2.4 P532 / PRMT-096). While `now` falls inside
// [StartsAt, EndsAt) AND the alarming asset_path matches the
// window's asset_path (== or startsWith(window.asset_path+"."))
// cios-alarm suppresses automatic ticket creation — the alarm
// itself is still persisted (VM + CloudEvents), only the ticket
// is skipped. Lifecycle-maintenance ("maintenance" lifecycle on
// the asset) is intentionally NOT implicit — it lives behind a
// separate spec follow-up so this PRMT ships the explicit table
// only.
//
// ID is "mw_" + 16 base32 chars (mirror newTicketID's shape).
// Reason is optional; the SQL default is ” so fileStore and PG
// stay symmetric on the empty-string case.
type MaintenanceWindow struct {
	ID        string    `json:"id"`
	AssetPath string    `json:"asset_path"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason,omitempty"`
}

// ErrInsufficientStock is returned by AdjustSpare when delta would
// drive qty below zero. HTTP layer maps it to 422 RFC 7807.
var ErrInsufficientStock = errors.New("core: insufficient stock")

// ErrSKUExists is returned by PutSpare when the SKU is already
// taken by another spare_part (PostgreSQL UNIQUE constraint
// violation 23505). HTTP layer maps it to 422 RFC 7807.
var ErrSKUExists = errors.New("core: sku already exists")

// ErrDuplicateActiveTicket is returned by PutTicket when the
// incoming ticket would violate the partial-unique dedup index
// `tickets_alarm_id_active_uniq`: a non-empty alarm_id with at
// least one non-closed ticket already on file. The application-
// layer dedup scanners (alarm.Store.OpenTicket, spares, reconcile)
// check-then-insert; this sentinel surfaces a racing insert that
// slipped between the SELECT and the INSERT. PRMT-081.
// Callers treat this as a no-op (idempotent skip) — the existing
// ticket is the authoritative one.
var ErrDuplicateActiveTicket = errors.New("core: duplicate active ticket for alarm_id")

// Store is the persistence seam. The HTTP layer depends on this
// interface, not on fileStore directly, so M1 can replace it with a
// PostgreSQL implementation without touching the handlers.
type Store interface {
	// PutAsset writes a. expectVersion: 0 → create-if-absent or
	// force-overwrite; >0 → optimistic lock against the current
	// version. Returns the persisted Asset (with updated version
	// and timestamps). On version mismatch, returns ErrVersionConflict
	// and the current Asset so the caller can build a 409 response.
	PutAsset(ctx context.Context, a Asset, expectVersion int64) (Asset, error)
	GetAsset(ctx context.Context, path string) (Asset, bool, error)
	ListAssets(ctx context.Context) ([]Asset, error) // sorted by path
	// DeleteAsset removes the asset. If it has descendants and
	// cascade is false, returns ErrHasChildren (and the list of
	// descendants for the response detail). cascade=true removes
	// the subtree. Returns the number of deleted entries.
	DeleteAsset(ctx context.Context, path string, cascade bool) (int, error)
	ListAlarms(ctx context.Context) ([]Alarm, error)  // M0 fileStore never fails; M1 PG might
	SeedAlarms(ctx context.Context, in []Alarm) error // idempotent upsert by ID
	// AckAlarm transitions alarm {id} firing → acked (spec-003 §4,
	// one-way), recording actor + timestamp. found=false → no such
	// alarm. found=true with ErrAlarmNotAckable → alarm exists but
	// is already acked/resolved; the current row is returned so the
	// HTTP layer can build a 409 (PRMT-230).
	AckAlarm(ctx context.Context, id, actor string) (a Alarm, found bool, err error)
	// PutTicket writes t. expectVersion: 0 → create-if-absent or
	// force-overwrite (auto-opener / scanner path; PRMT-082); >0 →
	// optimistic lock against the current version. Returns the
	// persisted Ticket (with updated version). On version mismatch
	// returns ErrVersionConflict and the current Ticket so the
	// caller can build a 409 response. Mirrors PutAsset semantics
	// (PRMT-016b).
	PutTicket(ctx context.Context, t Ticket, expectVersion int64) (Ticket, error)
	GetTicket(ctx context.Context, id string) (Ticket, bool, error)
	ListTickets(ctx context.Context) ([]Ticket, error) // sorted by OpenedAt desc; empty → []Ticket{} (never nil)
	// PMSchedule (PRMT-043). PutPMSchedule is upsert by ID (same
	// semantics as PutTicket / SeedAlarms). ListPMSchedules sorts
	// by NextDue asc so the scanner walks the soonest-due first.
	PutPMSchedule(ctx context.Context, p PMSchedule) error
	GetPMSchedule(ctx context.Context, id string) (PMSchedule, bool, error)
	ListPMSchedules(ctx context.Context) ([]PMSchedule, error)
	// AssetAudit (PRMT-045). Append-only — there is no
	// UpdateAssetAudit / DeleteAssetAudit by design (audit
	// integrity). ListAssetAudits returns entries for one path
	// in TS desc order (newest first); an unknown path yields
	// an empty (non-nil) slice.
	AppendAssetAudit(ctx context.Context, a AssetAudit) error
	ListAssetAudits(ctx context.Context, path string) ([]AssetAudit, error)
	// Spare (PRMT-048). PutSpare idempotently upserts by ID
	// (full-field overwrite on conflict). GetSpare / ListSpares
	// are straight map reads. AdjustSpare is the ONLY legal
	// mutation path for stock — it appends one SpareTxn and
	// updates spare_parts.qty atomically; qty<0 yields
	// ErrInsufficientStock (HTTP 422). ListSpareTxns returns
	// the txn log for one spare_id, newest first; an unknown
	// id yields an empty (non-nil) slice.
	PutSpare(ctx context.Context, s SparePart) error
	GetSpare(ctx context.Context, id string) (SparePart, bool, error)
	ListSpares(ctx context.Context) ([]SparePart, error)
	AdjustSpare(ctx context.Context, id string, delta int, ticketID string, at time.Time) (SparePart, SpareTxn, error)
	ListSpareTxns(ctx context.Context, spareID string) ([]SpareTxn, error)
	// InspectionTemplate (PRMT-049). PutInspectionTemplate is
	// upsert by ID (same semantics as PutTicket / SeedAlarms /
	// PutPMSchedule). GetInspectionTemplate is a straight map
	// read. ListInspectionTemplates sorts by NextDue asc so the
	// scanner walks soonest-due first; ties broken by ID for a
	// deterministic order across runs.
	PutInspectionTemplate(ctx context.Context, t InspectionTemplate) error
	GetInspectionTemplate(ctx context.Context, id string) (InspectionTemplate, bool, error)
	ListInspectionTemplates(ctx context.Context) ([]InspectionTemplate, error)
	// TicketNote (PRMT-060). Append-only by design: there is no
	// UpdateTicketNote / DeleteTicketNote. ListTicketNotes returns
	// notes for one ticket in At ASC order (oldest first), so the
	// GET /v1/tickets/{id} response can inline the timeline
	// without re-sorting; an unknown ticket yields a non-nil
	// empty slice. UpdateTicketAssignee mutates only the
	// assignee column on an existing ticket; missing ticket →
	// (Ticket{}, false, nil).
	AppendTicketNote(ctx context.Context, n TicketNote) error
	ListTicketNotes(ctx context.Context, ticketID string) ([]TicketNote, error)
	UpdateTicketAssignee(ctx context.Context, ticketID, assignee string) (Ticket, bool, error)
	// TicketAudit (PRMT-061). Append-only — there is no
	// UpdateTicketAudit / DeleteTicketAudit. ListTicketAudits
	// returns entries for one ticket in At ASC order (oldest
	// first) so the GET /v1/tickets/{id}:history response can
	// inline the audit timeline without re-sorting; an unknown
	// ticket yields a non-nil empty slice.
	AppendTicketAudit(ctx context.Context, a TicketAudit) error
	ListTicketAudits(ctx context.Context, ticketID string) ([]TicketAudit, error)
	// SetAudit (PRMT-234). Append-only control-write audit — there is
	// no UpdateSetAudit / DeleteSetAudit. ListSetAudits returns ALL
	// records newest-first (At DESC, ID DESC tiebreak); pagination is
	// the HTTP handler's job (pair cursor, mirrors maintenance windows).
	AppendSetAudit(ctx context.Context, a SetAudit) error
	ListSetAudits(ctx context.Context) ([]SetAudit, error)
	// MaintenanceWindow (PRMT-096). PutMaintenanceWindow is upsert by
	// ID (full-field overwrite on conflict; mirrors PutInspectionTemplate).
	// GetMaintenanceWindow is a straight map read; ListMaintenanceWindows
	// sorts by StartsAt ASC so the list endpoint returns the soonest-
	// starting windows first; ties broken by ID for a deterministic
	// order across runs. DeleteMaintenanceWindow removes by ID and
	// reports whether the row existed (false on miss, no error).
	// ActiveWindowFor returns the first window whose [StartsAt, EndsAt)
	// contains `now` AND whose asset_path is `==` or a "."-prefixed
	// ancestor of assetPath; "no active window" → ("", false, nil).
	// cios-alarm calls this on every firing transition before opening
	// a ticket (PRMT-096 §2 / §4). fileStore and pgStore share the
	// semantic; only the prefix-match SQL differs.
	PutMaintenanceWindow(ctx context.Context, m MaintenanceWindow) error
	GetMaintenanceWindow(ctx context.Context, id string) (MaintenanceWindow, bool, error)
	ListMaintenanceWindows(ctx context.Context) ([]MaintenanceWindow, error)
	DeleteMaintenanceWindow(ctx context.Context, id string) (bool, error)
	ActiveWindowFor(ctx context.Context, assetPath string, now time.Time) (MaintenanceWindow, bool, error)
	// TryScannerLock attempts to acquire a per-scanner advisory
	// lock for one tick. acquired=true means the caller is the
	// leader for this scanner on this tick and may proceed;
	// acquired=false means another instance already leads and the
	// caller should skip the tick. release must be deferred
	// immediately after a successful acquire so the lock is
	// freed when the tick ends (session-scoped, NOT process-
	// scoped — see PRMT-065 §2). err is non-nil only on a real
	// store failure; acquired=false with err==nil is the normal
	// "another instance leads" path. fileStore is single-instance
	// and always returns (true, no-op, nil).
	TryScannerLock(ctx context.Context, name string) (acquired bool, release func(), err error)
	// Tenant / Org read models (spec-001 v1.1 §5bis). PRMT-184 ships
	// the read-side Store methods + audit plumbing ONLY; mutators
	// arrive with their consuming PRMTs (182 tier-write, 185
	// /v1/orgs, 186 migration). GetTenant / GetOrg follow the same
	// (T, bool, error) convention as GetAsset / GetTicket — an absent
	// row returns ("", false, nil); err is non-nil only on a real
	// store failure. ListTenants sorts by ID ASC for a stable order
	// across runs; an unknown / empty store yields []Tenant{} (never
	// nil). ListOrgs filters by tenant_id and sorts by name ASC.
	GetTenant(ctx context.Context, id string) (Tenant, bool, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	// CreateTenant (L109 P804) inserts one tenant at isolation_tier=label,
	// status=active, and ensures the reserved `default` org (spec-001
	// §5bis.2 / PRMT-186) in the same write. id must pass validTenantSlug;
	// displayName is free text (trimmed, non-empty). Duplicate id →
	// ErrTenantExists. Writes tenant_audit op="tenant_create" plus the
	// org_create row from CreateOrg.
	CreateTenant(ctx context.Context, id, displayName, principal string) (Tenant, Org, error)
	GetOrg(ctx context.Context, id string) (Org, bool, error)
	ListOrgs(ctx context.Context, tenantID string) ([]Org, error)
	// ListOrgsAll returns every org across all tenants, grouped by
	// tenant_id. Each group is sorted by name ASC (same contract as
	// ListOrgs). Tenants with no orgs are absent from the map -- callers
	// must treat a missing key as an empty slice.
	// Added by PRMT-214 to remove the per-tenant N+1 in serveTenantsList
	// (fileStore: T full-map scans; pgStore: T SQL round-trips).
	ListOrgsAll(ctx context.Context) (map[string][]Org, error)
	// Org mutators (PRMT-185 §4.1, spec-001 v1.1 §5bis.2 / spec-004
	// §1). All three are admin-only at the HTTP layer; the Store
	// surface does NOT re-check the role — the handler is the single
	// gate (mirrors UpdateTenantTier / AttachSiteToOrg).
	//
	// CreateOrg inserts one org under tenantID. The handler must have
	// already verified name against validTenantSlug AND that the
	// caller is RoleAdmin (the slug check is duplicated here so a
	// future caller cannot bypass the boundary). Returns the persisted
	// Org (with server-generated ID + created_at) and writes exactly
	// one tenant_audit op="org_create" in the same tx / under the
	// same write lock. (tenant_id, name) UNIQUE violation →
	// ErrOrgNameConflict. FK on tenants(id) violation → a wrapped
	// not-found the handler maps to 404 ("tenant not found").
	//
	// RenameOrg changes orgs.name. The handler must have already
	// verified newName against validTenantSlug; the helper re-checks
	// defensively. Writes one tenant_audit op="org_rename" detail
	// "<old>→<new>" in the same tx / under the same write lock.
	// (tenant_id, name) UNIQUE violation → ErrOrgNameConflict. Absent
	// id → wrapped not-found.
	//
	// DeleteOrg removes the org IFF it owns zero sites (PRMT-189
	// CountSitesByOrg == 0 inside the tx). count > 0 → returns
	// ErrOrgOwnsResources with NO delete and NO audit row. count == 0
	// → delete + exactly one tenant_audit op="org_delete". Applies
	// uniformly to `default` and every other org (spec-001 §5bis.2).
	// The Cluster ownership leg is vacuous until fleet E3.7 (no
	// Cluster store) — sites are the only ownership axis checked;
	// documented, not invented (R5).
	CreateOrg(ctx context.Context, tenantID, name, principal string) (Org, error)
	RenameOrg(ctx context.Context, id, newName, principal string) error
	DeleteOrg(ctx context.Context, id, principal string) error
	// TenantAudit (PRMT-184 §5bis). Append-only — there is no
	// UpdateTenantAudit / DeleteTenantAudit by design (audit
	// integrity, mirrors AppendAssetAudit). ListTenantAudits returns
	// entries for one tenant_id in TS DESC order (newest first); an
	// unknown tenant_id yields a non-nil empty slice.
	AppendTenantAudit(ctx context.Context, a TenantAudit) error
	ListTenantAudits(ctx context.Context, tenantID string) ([]TenantAudit, error)

	// UpdateTenantTier (PRMT-182) raises a tenant's isolation_tier
	// one-way-up and records the outcome to tenant_audit atomically.
	//   - target validated against the LOCAL {label,row,db} rank map
	//     label=0<row=1<db=2 (do NOT import pkg/tenant; pinned locally).
	//   - Out-of-allowlist target → a validation error the handler maps to 400.
	//   - Downgrade (target rank < current rank): returns ErrTierDowngrade,
	//     writes ONE tenant_audit row op="tier_change" detail "<cur>→<target> REFUSED".
	//   - Upgrade: updates tenants.isolation_tier + updated_at, writes ONE
	//     tenant_audit row op="tier_change" detail "<cur>→<target>".
	//   - Equal: no-op, no audit row, returns nil.
	//   - Unknown id: returns a wrapped not-found error, mirroring
	//     AdjustSpare ("core: adjust spare: not found"); the handler's
	//     GetTenant ok-bool pre-check is the primary 404 path.
	// pgStore performs guard+update+audit in ONE transaction; fileStore
	// does it under the write lock.
	UpdateTenantTier(ctx context.Context, id, target, principal string) error
	// Site→Org mapping (spec-001 v1.1 §5bis.2). Not-found via the ok
	// bool, mirroring GetAsset/GetOrg (no ErrNotFound sentinel exists).
	GetSiteOrg(ctx context.Context, site string) (SiteOrg, bool, error) // ok=false when unmapped
	ListSiteOrgs(ctx context.Context) ([]SiteOrg, error)                // site ASC; never nil
	CountSitesByOrg(ctx context.Context, orgID string) (int, error)     // for the 185 R5 delete guard

	// AttachSiteToOrg upserts the site→org mapping (the "改挂" primitive).
	// - site MUST pass the site-slug grammar (^[a-z]{2,8}[0-9]{2}$, no "00" tail);
	//   else a bad-slug error.
	// - orgID MUST reference an existing org (FK); else not-found error.
	// - Idempotent: if the site is already mapped to orgID, it is a
	//   no-op — NO row write, NO audit row.
	// - On create OR actual re-home (org changed), writes exactly one
	//   tenant_audit op="org_reattach" in the SAME tx: detail = "<site>→<orgID>"
	//   on first attach, "<site>: <oldOrgID>→<newOrgID>" on re-home.
	//   tenant_id on the audit row = the org's TenantID (read from orgs).
	AttachSiteToOrg(ctx context.Context, site, orgID, principal string) error
	// DetachSiteFromOrg removes the site→org mapping (PRMT-220 hard delete).
	// - Unknown / unmapped site → wrapped not-found (handler → 404).
	// - On success: delete row + one tenant_audit op="org_reattach"
	//   detail="<site>: <orgID>→" (unbind; reuses 015 CHECK token, same as
	//   PRMT-189 attach — no CHECK expansion).
	DetachSiteFromOrg(ctx context.Context, site, principal string) error
	// DeleteTenant removes the tenant IFF it owns zero orgs (PRMT-220).
	// count > 0 → ErrTenantOwnsOrgs, NO delete, NO audit.
	// count == 0 → delete + one tenant_audit op="tenant_status" detail="deleted"
	// (reuses 015 CHECK token; no CHECK expansion). Platform-admin only at HTTP.
	DeleteTenant(ctx context.Context, id, principal string) error

	// RoleBinding (PRMT-190-bis §4.2; spec-004 §6bis, R3). The static
	// token config stays the authn seed (token → subject/role); these
	// rows are the scope grants the v1.1 migration (PRMT-186)
	// mechanically rewrites (dot-glob → crn). ListRoleBindings /
	// ListAllRoleBindings are the surface PRMT-186 walks; the loader
	// (LoadRoleBindingsInto in core/auth.go) reads ListAllRoleBindings
	// to augment Principal.Scopes at verifier construction.
	//
	// PutRoleBinding is upsert on (subject, scope) — a re-put with a
	// different origin or updated_at updates the existing row, never
	// creates a duplicate. Empty subject or empty scope is a validation
	// error at the boundary (mirrors the fileStore UNIQUE convention
	// enforced by the SQL schema).
	//
	// ListRoleBindings(subject) returns the rows for one subject in
	// Scope ASC order; an unknown subject yields a non-nil empty slice.
	//
	// ListAllRoleBindings returns every row in (Subject ASC, Scope ASC)
	// order — the stable order PRMT-186 rewrites against. Empty store
	// yields []RoleBinding{} (never nil).
	PutRoleBinding(ctx context.Context, rb RoleBinding) error
	ListRoleBindings(ctx context.Context, subject string) ([]RoleBinding, error)
	ListAllRoleBindings(ctx context.Context) ([]RoleBinding, error)
	// DeleteRoleBinding removes the (subject, scope) row if present
	// and is an idempotent no-op when the row is already absent
	// (PRMT-186 §3 widening; mirrors the existing schema's
	// `UNIQUE (subject, scope)` index — migrations/017_role_bindings.sql).
	// Migration-only primitive; never exposed to any HTTP/CLI surface.
	DeleteRoleBinding(ctx context.Context, subject, scope string) error

	// Usage (PRMT-192 / spec-010). UpsertUsage assigns ID if empty
	// ("us_" + 16 base32) and replaces an existing row with the same
	// id. ListUsage filters by tenant/site/kind/granularity and
	// period overlap (PeriodStart < filter.PeriodEnd && PeriodEnd >
	// filter.PeriodStart; zero filter bounds are open), then sorts
	// stably by (PeriodStart, AssetPath, Kind). Empty result is a
	// non-nil empty slice. fileStore persists via the JSON blob;
	// pgStore stubs until a usage migration lands.
	UpsertUsage(ctx context.Context, rec UsageRecord) (UsageRecord, error)
	ListUsage(ctx context.Context, f UsageListFilter) ([]UsageRecord, error)
}

// Sentinel errors. HTTP layer maps them to RFC 7807 problem types.
var (
	ErrVersionConflict = errors.New("core: resource version conflict")
	ErrHasChildren     = errors.New("core: asset has children")
	// ErrTierDowngrade is returned by UpdateTenantTier when the
	// requested target rank is below the current tier (one-way-up
	// contract, L98(b) / spec-001 v1.1 §5bis.1). The mutator still
	// writes one REFUSED tenant_audit row before returning this
	// sentinel; the handler maps it to a 409 RFC 7807 problem with
	// type tail "tier-downgrade".
	ErrTierDowngrade = errors.New("core: isolation_tier downgrade refused")
	// ErrOrgNameConflict is returned by CreateOrg / RenameOrg when
	// the (tenant_id, name) UNIQUE on orgs would be violated. The
	// HTTP handler maps this to a 409 RFC 7807 problem with type
	// tail "org-name-conflict". PRMT-185 §4.1.
	ErrOrgNameConflict = errors.New("core: org name already exists in tenant")
	// ErrOrgOwnsResources is returned by DeleteOrg when the org
	// still has ≥1 site mapping (PRMT-189 CountSitesByOrg > 0)
	// inside the same transaction. The handler maps this to a 409
	// RFC 7807 problem with type tail "org-owns-resources". PRMT-185
	// §4.1 (R5). Applies uniformly to `default` and every other org
	// (spec-001 §5bis.2 — "名下仍有资源时不可删除").
	ErrOrgOwnsResources = errors.New("core: org owns resources")
	// ErrTenantExists is returned by CreateTenant when tenants.id
	// already exists (L109 P804). Handler → 409 "tenant-exists".
	ErrTenantExists = errors.New("core: tenant already exists")
	// ErrTenantOwnsOrgs is returned by DeleteTenant when the tenant
	// still has ≥1 org (PRMT-220). Handler → 409 "tenant-owns-orgs".
	// Caller must DeleteOrg each org first (including default).
	ErrTenantOwnsOrgs = errors.New("core: tenant owns orgs")
)

// fileStore holds the in-memory index plus the path to the JSON
// file. All public methods acquire the RWMutex; the persistence
// is private so callers cannot bypass the lock.
type fileStore struct {
	mu           sync.RWMutex
	path         string
	assets       map[string]Asset
	alarms       map[string]Alarm
	tickets      map[string]Ticket
	pmSchedules  map[string]PMSchedule
	audits       []AssetAudit // append-only; no need for a map
	spares       map[string]SparePart
	spareTxns    []SpareTxn // append-only; no need for a map
	inspections  map[string]InspectionTemplate
	notes        []TicketNote  // append-only; no need for a map
	ticketAuds   []TicketAudit // append-only; no need for a map
	setAudits    []SetAudit    // append-only; no need for a map
	mwWindows    map[string]MaintenanceWindow
	tenants      map[string]Tenant
	orgs         map[string]Org
	tenantAuds   []TenantAudit // append-only; no need for a map
	siteOrgs     map[string]SiteOrg
	roleBindings []RoleBinding // (subject, scope) UNIQUE — append + update in place
	usages       map[string]UsageRecord
}

// NewFileStore loads (or initialises) a Store backed by a single
// JSON file. Missing file → empty store. The file is read once at
// construction; every write is atomic (tmp + rename).
func NewFileStore(path string) (Store, error) {
	s := &fileStore{
		path:         path,
		assets:       map[string]Asset{},
		alarms:       map[string]Alarm{},
		tickets:      map[string]Ticket{},
		pmSchedules:  map[string]PMSchedule{},
		audits:       []AssetAudit{},
		spares:       map[string]SparePart{},
		spareTxns:    []SpareTxn{},
		inspections:  map[string]InspectionTemplate{},
		notes:        []TicketNote{},
		ticketAuds:   []TicketAudit{},
		setAudits:    []SetAudit{},
		mwWindows:    map[string]MaintenanceWindow{},
		tenants:      map[string]Tenant{},
		orgs:         map[string]Org{},
		tenantAuds:   []TenantAudit{},
		siteOrgs:     map[string]SiteOrg{},
		roleBindings: []RoleBinding{},
		usages:       map[string]UsageRecord{},
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("core: load store %s: %w", path, err)
	}
	return s, nil
}

// load reads the JSON file into the in-memory index. Missing or
// empty file → empty store (not an error). A corrupt file is fatal:
// the store cannot serve inconsistent data, and silent recovery
// would mask real corruption bugs.
func (s *fileStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var disk diskShape
	if err := json.Unmarshal(raw, &disk); err != nil {
		return fmt.Errorf("corrupt store file: %w", err)
	}
	for _, a := range disk.Assets {
		s.assets[a.Path] = a
	}
	for _, al := range disk.Alarms {
		s.alarms[al.ID] = al
	}
	for _, t := range disk.Tickets {
		s.tickets[t.ID] = t
	}
	for _, p := range disk.PMSchedules {
		s.pmSchedules[p.ID] = p
	}
	if disk.Audits != nil {
		s.audits = append(s.audits, disk.Audits...)
	} else {
		s.audits = []AssetAudit{}
	}
	for _, sp := range disk.Spares {
		s.spares[sp.ID] = sp
	}
	if disk.SpareTxns != nil {
		s.spareTxns = append(s.spareTxns, disk.SpareTxns...)
	} else {
		s.spareTxns = []SpareTxn{}
	}
	for _, it := range disk.Inspections {
		s.inspections[it.ID] = it
	}
	for _, mw := range disk.MWWindows {
		s.mwWindows[mw.ID] = mw
	}
	if disk.Notes != nil {
		s.notes = append(s.notes, disk.Notes...)
	} else {
		s.notes = []TicketNote{}
	}
	if disk.TicketAudits != nil {
		s.ticketAuds = append(s.ticketAuds, disk.TicketAudits...)
	} else {
		s.ticketAuds = []TicketAudit{}
	}
	if disk.SetAudits != nil {
		s.setAudits = append(s.setAudits, disk.SetAudits...)
	}
	for _, tn := range disk.Tenants {
		s.tenants[tn.ID] = tn
	}
	for _, og := range disk.Orgs {
		s.orgs[og.ID] = og
	}
	if disk.TenantAudits != nil {
		s.tenantAuds = append(s.tenantAuds, disk.TenantAudits...)
	} else {
		s.tenantAuds = []TenantAudit{}
	}
	for _, so := range disk.SiteOrgs {
		s.siteOrgs[so.Site] = so
	}
	if disk.RoleBindings != nil {
		s.roleBindings = append(s.roleBindings, disk.RoleBindings...)
	} else {
		s.roleBindings = []RoleBinding{}
	}
	for _, u := range disk.Usages {
		s.usages[u.ID] = u
	}
	return nil
}

// diskShape is the on-disk JSON layout. Version is reserved for a
// future migration hook; M0 ships with version=1.
type diskShape struct {
	Version      int                  `json:"version"`
	Assets       []Asset              `json:"assets"`
	Alarms       []Alarm              `json:"alarms"`
	Tickets      []Ticket             `json:"tickets"`
	PMSchedules  []PMSchedule         `json:"pm_schedules"`
	Audits       []AssetAudit         `json:"asset_audits"`
	Spares       []SparePart          `json:"spares"`
	SpareTxns    []SpareTxn           `json:"spare_txns"`
	Inspections  []InspectionTemplate `json:"inspection_templates"`
	Notes        []TicketNote         `json:"ticket_notes"`
	TicketAudits []TicketAudit        `json:"ticket_audits"`
	SetAudits    []SetAudit           `json:"set_audits"`
	MWWindows    []MaintenanceWindow  `json:"maintenance_windows"`
	Tenants      []Tenant             `json:"tenants"`
	Orgs         []Org                `json:"orgs"`
	TenantAudits []TenantAudit        `json:"tenant_audits"`
	SiteOrgs     []SiteOrg            `json:"site_orgs"`
	RoleBindings []RoleBinding        `json:"role_bindings"`
	Usages       []UsageRecord        `json:"usages"`
}

// save writes the in-memory state to disk atomically. Callers MUST
// hold s.mu (write lock) so the snapshot is consistent.
func (s *fileStore) save() error {
	// Direct slice references, not copies: save() is called only with
	// s.mu held for writing (28 call sites, all inside locked mutators),
	// so no concurrent mutation can occur while json.Marshal walks these
	// slices. The previous append([]T(nil), ...) copies duplicated the
	// entire audit/note/binding history on every single write (PRMT-215).
	disk := diskShape{
		Version:      1,
		Assets:       make([]Asset, 0, len(s.assets)),
		Alarms:       make([]Alarm, 0, len(s.alarms)),
		Tickets:      make([]Ticket, 0, len(s.tickets)),
		PMSchedules:  make([]PMSchedule, 0, len(s.pmSchedules)),
		Audits:       s.audits,
		Spares:       make([]SparePart, 0, len(s.spares)),
		SpareTxns:    s.spareTxns,
		Inspections:  make([]InspectionTemplate, 0, len(s.inspections)),
		Notes:        s.notes,
		TicketAudits: s.ticketAuds,
		SetAudits:    s.setAudits,
		MWWindows:    make([]MaintenanceWindow, 0, len(s.mwWindows)),
		Tenants:      make([]Tenant, 0, len(s.tenants)),
		Orgs:         make([]Org, 0, len(s.orgs)),
		TenantAudits: s.tenantAuds,
		SiteOrgs:     make([]SiteOrg, 0, len(s.siteOrgs)),
		RoleBindings: s.roleBindings,
		Usages:       make([]UsageRecord, 0, len(s.usages)),
	}
	// Stable order on disk: path for assets, ID for alarms, ID for tickets.
	paths := make([]string, 0, len(s.assets))
	for p := range s.assets {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		disk.Assets = append(disk.Assets, s.assets[p])
	}
	ids := make([]string, 0, len(s.alarms))
	for id := range s.alarms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		disk.Alarms = append(disk.Alarms, s.alarms[id])
	}
	tids := make([]string, 0, len(s.tickets))
	for id := range s.tickets {
		tids = append(tids, id)
	}
	sort.Strings(tids)
	for _, id := range tids {
		disk.Tickets = append(disk.Tickets, s.tickets[id])
	}
	pmids := make([]string, 0, len(s.pmSchedules))
	for id := range s.pmSchedules {
		pmids = append(pmids, id)
	}
	sort.Strings(pmids)
	for _, id := range pmids {
		disk.PMSchedules = append(disk.PMSchedules, s.pmSchedules[id])
	}
	spids := make([]string, 0, len(s.spares))
	for id := range s.spares {
		spids = append(spids, id)
	}
	sort.Strings(spids)
	for _, id := range spids {
		disk.Spares = append(disk.Spares, s.spares[id])
	}
	inids := make([]string, 0, len(s.inspections))
	for id := range s.inspections {
		inids = append(inids, id)
	}
	sort.Strings(inids)
	for _, id := range inids {
		disk.Inspections = append(disk.Inspections, s.inspections[id])
	}
	mwids := make([]string, 0, len(s.mwWindows))
	for id := range s.mwWindows {
		mwids = append(mwids, id)
	}
	sort.Strings(mwids)
	for _, id := range mwids {
		disk.MWWindows = append(disk.MWWindows, s.mwWindows[id])
	}
	tnids := make([]string, 0, len(s.tenants))
	for id := range s.tenants {
		tnids = append(tnids, id)
	}
	sort.Strings(tnids)
	for _, id := range tnids {
		disk.Tenants = append(disk.Tenants, s.tenants[id])
	}
	ogids := make([]string, 0, len(s.orgs))
	for id := range s.orgs {
		ogids = append(ogids, id)
	}
	sort.Strings(ogids)
	for _, id := range ogids {
		disk.Orgs = append(disk.Orgs, s.orgs[id])
	}
	sosites := make([]string, 0, len(s.siteOrgs))
	for site := range s.siteOrgs {
		sosites = append(sosites, site)
	}
	sort.Strings(sosites)
	for _, site := range sosites {
		disk.SiteOrgs = append(disk.SiteOrgs, s.siteOrgs[site])
	}
	// Stable order on disk for role_bindings: (subject ASC, scope ASC)
	// mirrors listAllRoleBindings so the in-memory snapshot and the
	// on-disk JSON have the same order — easier for ops grep during
	// the 186 rewrite.
	// Copy before sort: disk.RoleBindings aliases s.roleBindings (PRMT-215
	// direct-ref); sorting in place would reorder the live in-memory slice.
	if n := len(disk.RoleBindings); n > 0 {
		rb := append([]RoleBinding(nil), disk.RoleBindings...)
		sort.Slice(rb, func(i, j int) bool {
			if rb[i].Subject != rb[j].Subject {
				return rb[i].Subject < rb[j].Subject
			}
			return rb[i].Scope < rb[j].Scope
		})
		disk.RoleBindings = rb
	}
	uids := make([]string, 0, len(s.usages))
	for id := range s.usages {
		uids = append(uids, id)
	}
	sort.Strings(uids)
	for _, id := range uids {
		disk.Usages = append(disk.Usages, s.usages[id])
	}
	// Compact JSON: MarshalIndent doubled full-store buffers (flat indent
	// ~26% of alloc in PRMT-214). Ops pretty-print with `jq . <store>`
	// instead of a pretty switch (PRMT-215).
	buf, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "cios-core-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// Durability: rename is atomic w.r.t. process crash, but only an
	// fsync of the parent directory makes the rename itself survive
	// power loss (PRMT-215; previously neither fsync existed).
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// PutAsset implements Store.
func (s *fileStore) PutAsset(_ context.Context, a Asset, expectVersion int64) (Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if cur, ok := s.assets[a.Path]; ok {
		// Update path. Snapshot pre-change so a save failure can
		// restore the in-memory state to match the on-disk truth
		// (which still has the old value).
		prev := cur
		if expectVersion > 0 && cur.ResourceVersion != expectVersion {
			return cur, ErrVersionConflict
		}
		cur.Spec = a.Spec
		cur.ResourceVersion++
		cur.UpdatedAt = now
		s.assets[a.Path] = cur
		if err := s.save(); err != nil {
			s.assets[a.Path] = prev // restore the pre-change snapshot
			return Asset{}, err
		}
		return cur, nil
	}
	// Create path.
	if expectVersion > 0 {
		// Caller asked to update something that doesn't exist.
		return Asset{}, ErrVersionConflict
	}
	a.ResourceVersion = 1
	a.CreatedAt = now
	a.UpdatedAt = now
	s.assets[a.Path] = a
	if err := s.save(); err != nil {
		delete(s.assets, a.Path) // restore: the asset never existed on disk
		return Asset{}, err
	}
	return a, nil
}

// GetAsset implements Store.
func (s *fileStore) GetAsset(_ context.Context, path string) (Asset, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.assets[path]
	return a, ok, nil
}

// ListAssets implements Store. Sorted by path (callers rely on it
// for cursor pagination).
func (s *fileStore) ListAssets(_ context.Context) ([]Asset, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Asset, 0, len(s.assets))
	for _, a := range s.assets {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// hasChildren reports whether any asset starts with prefix+"." (so
// "site01.pod000" is a child of "site01" but "site011.cdu000" is not).
func (s *fileStore) hasChildren(path string) []Asset {
	prefix := path + "."
	var out []Asset
	for p, a := range s.assets {
		if strings.HasPrefix(p, prefix) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// DeleteAsset implements Store.
func (s *fileStore) DeleteAsset(_ context.Context, path string, cascade bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.assets[path]
	kids := s.hasChildren(path)
	if !exists && len(kids) == 0 {
		return 0, nil // not-found is reported by the HTTP layer after the lookup
	}
	if len(kids) > 0 && !cascade {
		return len(kids), ErrHasChildren
	}
	// Snapshot the deleted entries so a save failure can put them
	// back — disk still has them, memory must not lie.
	prevAssets := make(map[string]Asset, 1+len(kids))
	if exists {
		prevAssets[path] = s.assets[path]
	}
	for _, k := range kids {
		prevAssets[k.Path] = k
	}
	deleted := 0
	if exists {
		delete(s.assets, path)
		deleted++
	}
	for _, k := range kids {
		delete(s.assets, k.Path)
		deleted++
	}
	if err := s.save(); err != nil {
		// Restore every snapshot back to the in-memory index.
		for p, a := range prevAssets {
			s.assets[p] = a
		}
		return 0, err
	}
	return deleted, nil
}

// ListAlarms implements Store. Stable order: severity rank → Since
// desc. The HTTP layer also sorts, but having a sane order from
// the store keeps the in-memory representation predictable.
func (s *fileStore) ListAlarms(_ context.Context) ([]Alarm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Alarm, 0, len(s.alarms))
	for _, a := range s.alarms {
		out = append(out, a)
	}
	sortAlarms(out)
	return out, nil
}

// SeedAlarms implements Store. Idempotent upsert by ID; a re-seed
// with the same IDs leaves the original timestamps intact. The
// save is best-effort but we still return its error so M1
// implementations (PostgreSQL) can surface a real persistence
// failure; the fileStore path is currently failure-free unless
// the disk is full / the dir is gone, in which case boot should
// fail loudly rather than silently drop the seed.
func (s *fileStore) SeedAlarms(_ context.Context, in []Alarm) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range in {
		if _, ok := s.alarms[a.ID]; ok {
			// Existing: refresh fields but keep the original Since.
			prev := s.alarms[a.ID]
			prev.Severity = a.Severity
			prev.State = a.State
			prev.Summary = a.Summary
			prev.Path = a.Path
			s.alarms[a.ID] = prev
			continue
		}
		s.alarms[a.ID] = a
	}
	return s.save()
}

// AckAlarm implements Store (PRMT-230). Mutate-then-save mirrors
// SeedAlarms' posture: the save error is returned so PG-parity
// semantics hold, and boot-level disk failures surface loudly.
func (s *fileStore) AckAlarm(_ context.Context, id, actor string) (Alarm, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alarms[id]
	if !ok {
		return Alarm{}, false, nil
	}
	if a.State != "firing" {
		return a, true, ErrAlarmNotAckable
	}
	now := time.Now().UTC()
	a.State = "acked"
	a.AckedBy = actor
	a.AckedAt = &now
	s.alarms[id] = a
	if err := s.save(); err != nil {
		return Alarm{}, true, err
	}
	return a, true, nil
}

// severityRank orders severities for alarm sort.
func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "major":
		return 1
	case "minor":
		return 2
	case "info":
		return 3
	}
	return 4
}

// sortAlarms orders by severity (critical first) then Since desc.
func sortAlarms(xs []Alarm) {
	sort.SliceStable(xs, func(i, j int) bool {
		ri, rj := severityRank(xs[i].Severity), severityRank(xs[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return xs[i].Since.After(xs[j].Since)
	})
}

// PutTicket implements Store. Idempotent upsert by ID; a re-put
// with the same ID overwrites every field. The save is best-effort
// but we still return its error (mirror SeedAlarms).
//
// PRMT-081: enforces "one active ticket per non-empty alarm_id"
// (mirror of the partial-unique PG index
// tickets_alarm_id_active_uniq). Manual tickets (alarm_id="") are
// not subject to the check. A re-put of the SAME id is always
// allowed (state-machine transition path): the existing ticket
// row under the same id IS the one being updated. A PUT for a
// NEW id that would collide with an existing non-closed ticket
// on the same alarm_id returns ErrDuplicateActiveTicket and
// does NOT mutate the map, mirroring the 23505 the PG layer
// surfaces on the index.
//
// PRMT-082: expectVersion is the optimistic-lock compare value.
// 0 → create-or-force-overwrite (auto-openers / scanners; mirrors
// PutAsset expectVersion=0). >0 → CAS: the in-memory row's
// ResourceVersion must equal expectVersion, otherwise
// ErrVersionConflict and the current row are returned for the
// HTTP handler to build a 409 response. The write lock is held
// for the entire read-compare-write so two concurrent CAS
// attempts cannot both succeed. Each successful overwrite
// increments ResourceVersion by 1.
func (s *fileStore) PutTicket(_ context.Context, t Ticket, expectVersion int64) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AlarmID != "" && t.State != "closed" {
		for id, other := range s.tickets {
			if id == t.ID {
				continue
			}
			if other.AlarmID == t.AlarmID && other.State != "closed" {
				return Ticket{}, ErrDuplicateActiveTicket
			}
		}
	}
	if cur, ok := s.tickets[t.ID]; ok {
		// Update path. Snapshot pre-change so a save failure can
		// restore the in-memory state to match the on-disk truth.
		prev := cur
		if expectVersion > 0 && cur.ResourceVersion != expectVersion {
			return cur, ErrVersionConflict
		}
		t.ResourceVersion = cur.ResourceVersion + 1
		s.tickets[t.ID] = t
		if err := s.save(); err != nil {
			s.tickets[t.ID] = prev // restore the pre-change snapshot
			return Ticket{}, err
		}
		return t, nil
	}
	// Create path. expectVersion>0 with no existing row is a
	// CAS miss (caller thought the ticket existed).
	if expectVersion > 0 {
		return Ticket{}, ErrVersionConflict
	}
	t.ResourceVersion = 1
	s.tickets[t.ID] = t
	if err := s.save(); err != nil {
		delete(s.tickets, t.ID) // restore: the ticket never existed on disk
		return Ticket{}, err
	}
	return t, nil
}

// GetTicket implements Store. Not found → (Ticket{}, false, nil).
func (s *fileStore) GetTicket(_ context.Context, id string) (Ticket, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tickets[id]
	return t, ok, nil
}

// ListTickets implements Store. Sorted by OpenedAt desc; an
// empty store yields a non-nil empty slice so the JSON encoding
// is `[]` (mirrors ListAlarms).
func (s *fileStore) ListTickets(_ context.Context) ([]Ticket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Ticket, 0, len(s.tickets))
	for _, t := range s.tickets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.After(out[j].OpenedAt) })
	return out, nil
}

// PutPMSchedule implements Store. Idempotent upsert by ID (same
// semantics as PutTicket / SeedAlarms): existing ID → overwrite.
// Persistence is best-effort atomic — on save failure we restore
// the pre-change map so the in-memory index never diverges from
// what's on disk.
func (s *fileStore) PutPMSchedule(_ context.Context, p PMSchedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.pmSchedules[p.ID]
	s.pmSchedules[p.ID] = p
	if err := s.save(); err != nil {
		if existed {
			s.pmSchedules[p.ID] = prev
		} else {
			delete(s.pmSchedules, p.ID)
		}
		return err
	}
	return nil
}

// GetPMSchedule implements Store.
func (s *fileStore) GetPMSchedule(_ context.Context, id string) (PMSchedule, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pmSchedules[id]
	return p, ok, nil
}

// ListPMSchedules implements Store. Sorted by NextDue asc so the
// scanner walks soonest-due first; ties broken by ID for a
// deterministic order across runs.
func (s *fileStore) ListPMSchedules(_ context.Context) ([]PMSchedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PMSchedule, 0, len(s.pmSchedules))
	for _, p := range s.pmSchedules {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextDue.Equal(out[j].NextDue) {
			return out[i].NextDue.Before(out[j].NextDue)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// AppendAssetAudit implements Store. Append-only: there is no
// update path by design. The audit list is persisted alongside
// the rest of the store on every call. On save failure we
// truncate the in-memory slice back to its pre-call length so
// the in-memory state never claims to have written what the
// disk did not accept.
func (s *fileStore) AppendAssetAudit(_ context.Context, a AssetAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevLen := len(s.audits)
	s.audits = append(s.audits, a)
	if err := s.save(); err != nil {
		s.audits = s.audits[:prevLen]
		return err
	}
	return nil
}

// ListAssetAudits implements Store. Filters by path and sorts
// newest-first (TS desc). Empty store / no matches → non-nil
// empty slice.
func (s *fileStore) ListAssetAudits(_ context.Context, path string) ([]AssetAudit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AssetAudit, 0)
	for _, a := range s.audits {
		if a.Path != path {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.After(out[j].TS)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// --- Spare (PRMT-048) ---------------------------------------------------

// PutSpare implements Store. Idempotent upsert by ID. SKU
// uniqueness is enforced by the SQL UNIQUE in pgStore and now
// mirrored here (PRMT-080): if any other spare_part (different
// id) already holds the same sku, return ErrSKUExists and do
// not write. Self-update (same id, possibly new sku) is allowed
// — the id-keyed upsert contract still holds. The check runs
// inside the write lock so it is atomic with the save. On save
// failure we restore the prior value (mirror PutPMSchedule's
// snapshot pattern) so the in-memory state never diverges from
// disk.
func (s *fileStore) PutSpare(_ context.Context, sp SparePart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, other := range s.spares {
		if id == sp.ID {
			continue
		}
		if other.SKU == sp.SKU {
			return ErrSKUExists
		}
	}
	prev, existed := s.spares[sp.ID]
	s.spares[sp.ID] = sp
	if err := s.save(); err != nil {
		if existed {
			s.spares[sp.ID] = prev
		} else {
			delete(s.spares, sp.ID)
		}
		return err
	}
	return nil
}

// GetSpare implements Store. Not found → (SparePart{}, false, nil).
func (s *fileStore) GetSpare(_ context.Context, id string) (SparePart, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sp, ok := s.spares[id]
	return sp, ok, nil
}

// ListSpares implements Store. Sorted by ID so cursor pagination
// has a deterministic order (same trick as ListAlarms / ListTickets).
func (s *fileStore) ListSpares(_ context.Context) ([]SparePart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SparePart, 0, len(s.spares))
	for _, sp := range s.spares {
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AdjustSpare implements Store. Holds the write lock for the full
// read-modify-write (pg analogue is a tx). Refuses delta that would
// drive qty below zero with ErrInsufficientStock. On any failure
// after we've already mutated in-memory state we restore the
// prior spare and txn count so the in-memory index never diverges
// from disk.
func (s *fileStore) AdjustSpare(_ context.Context, id string, delta int, ticketID string, at time.Time) (SparePart, SpareTxn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.spares[id]
	if !ok {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: adjust spare: not found")
	}
	if delta == 0 {
		return SparePart{}, SpareTxn{}, fmt.Errorf("core: adjust spare: delta must be non-zero")
	}
	prevSpare := sp
	prevTxnLen := len(s.spareTxns)
	newQty := sp.Qty + delta
	if newQty < 0 {
		return SparePart{}, SpareTxn{}, ErrInsufficientStock
	}
	sp.Qty = newQty
	s.spares[id] = sp
	txn := SpareTxn{
		ID:       newSpareTxnID(),
		SpareID:  id,
		Delta:    delta,
		TicketID: ticketID,
		At:       at,
	}
	s.spareTxns = append(s.spareTxns, txn)
	if err := s.save(); err != nil {
		s.spares[id] = prevSpare
		s.spareTxns = s.spareTxns[:prevTxnLen]
		return SparePart{}, SpareTxn{}, err
	}
	return sp, txn, nil
}

// ListSpareTxns implements Store. Filters by spare_id, sorts
// newest-first (AT desc). Empty store / no matches → non-nil
// empty slice.
func (s *fileStore) ListSpareTxns(_ context.Context, spareID string) ([]SpareTxn, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SpareTxn, 0)
	for _, t := range s.spareTxns {
		if t.SpareID != spareID {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// --- Inspection (PRMT-049) -----------------------------------------------

// PutInspectionTemplate implements Store. Idempotent upsert by ID
// (full-field overwrite on conflict) — mirrors PutPMSchedule /
// PutSpare. On save failure we restore the prior value (mirror
// PutPMSchedule's snapshot pattern) so the in-memory index never
// diverges from disk.
func (s *fileStore) PutInspectionTemplate(_ context.Context, it InspectionTemplate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.inspections[it.ID]
	s.inspections[it.ID] = it
	if err := s.save(); err != nil {
		if existed {
			s.inspections[it.ID] = prev
		} else {
			delete(s.inspections, it.ID)
		}
		return err
	}
	return nil
}

// GetInspectionTemplate implements Store. Not found →
// (InspectionTemplate{}, false, nil).
func (s *fileStore) GetInspectionTemplate(_ context.Context, id string) (InspectionTemplate, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.inspections[id]
	return it, ok, nil
}

// ListInspectionTemplates implements Store. Sorted by NextDue asc
// so the scanner walks soonest-due first; ties broken by ID for
// a deterministic order across runs.
func (s *fileStore) ListInspectionTemplates(_ context.Context) ([]InspectionTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]InspectionTemplate, 0, len(s.inspections))
	for _, it := range s.inspections {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextDue.Equal(out[j].NextDue) {
			return out[i].NextDue.Before(out[j].NextDue)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// --- Ticket notes (PRMT-060) ---------------------------------------------

// AppendTicketNote implements Store. Append-only by design — the
// in-memory slice is appended before save; on save failure we
// truncate back to the pre-call length so the in-memory state
// never claims to have written what the disk did not accept
// (mirror AppendAssetAudit's snapshot pattern).
func (s *fileStore) AppendTicketNote(_ context.Context, n TicketNote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevLen := len(s.notes)
	s.notes = append(s.notes, n)
	if err := s.save(); err != nil {
		s.notes = s.notes[:prevLen]
		return err
	}
	return nil
}

// ListTicketNotes implements Store. Filters by ticket_id, sorts
// At ASC (oldest first) so the GET /v1/tickets/{id} response
// inlines the timeline without re-sorting. Empty / unknown id →
// non-nil empty slice.
func (s *fileStore) ListTicketNotes(_ context.Context, ticketID string) ([]TicketNote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TicketNote, 0)
	for _, n := range s.notes {
		if n.TicketID != ticketID {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// UpdateTicketAssignee implements Store. Mutates only the
// assignee column on an existing ticket; missing ticket →
// (Ticket{}, false, nil). Snapshot the pre-change ticket so a
// save failure can restore the in-memory state to match the
// on-disk truth (mirror PutPMSchedule's pattern).
func (s *fileStore) UpdateTicketAssignee(_ context.Context, ticketID, assignee string) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tickets[ticketID]
	if !ok {
		return Ticket{}, false, nil
	}
	prev := cur
	cur.Assignee = assignee
	s.tickets[ticketID] = cur
	if err := s.save(); err != nil {
		s.tickets[ticketID] = prev
		return Ticket{}, false, err
	}
	return cur, true, nil
}

// --- Ticket audit (PRMT-061) --------------------------------------------

// AppendTicketAudit implements Store. Append-only by design —
// there is no Update / Delete helper. On save failure we
// truncate the in-memory slice back to its pre-call length so
// the in-memory state never claims to have written what the
// disk did not accept (mirror AppendAssetAudit / AppendTicketNote).
func (s *fileStore) AppendTicketAudit(_ context.Context, a TicketAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevLen := len(s.ticketAuds)
	s.ticketAuds = append(s.ticketAuds, a)
	if err := s.save(); err != nil {
		s.ticketAuds = s.ticketAuds[:prevLen]
		return err
	}
	return nil
}

// ListTicketAudits implements Store. Filters by ticket_id,
// sorts At ASC (oldest first) so the GET /v1/tickets/{id}:history
// response inlines the audit timeline without re-sorting.
// Empty / unknown ticket → non-nil empty slice.
func (s *fileStore) ListTicketAudits(_ context.Context, ticketID string) ([]TicketAudit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TicketAudit, 0)
	for _, a := range s.ticketAuds {
		if a.TicketID != ticketID {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// AppendSetAudit implements Store (PRMT-234). Mutate-then-save with
// rollback mirrors AppendTicketAudit.
func (s *fileStore) AppendSetAudit(_ context.Context, a SetAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevLen := len(s.setAudits)
	s.setAudits = append(s.setAudits, a)
	if err := s.save(); err != nil {
		s.setAudits = s.setAudits[:prevLen]
		return err
	}
	return nil
}

// ListSetAudits implements Store. Returns a sorted copy, newest
// first (At DESC, ID DESC tiebreak) — same order as the pgStore
// SQL so the HTTP pair cursor behaves identically on both backends.
func (s *fileStore) ListSetAudits(_ context.Context) ([]SetAudit, error) {
	s.mu.RLock()
	out := make([]SetAudit, len(s.setAudits))
	copy(out, s.setAudits)
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// TryScannerLock implements Store. The fileStore is single-
// instance by definition (a JSON file behind a mutex), so leader
// election is a no-op: every tick is the leader, release is a
// no-op closure (PRMT-065 §2 — "fileStore: single-instance
// semantics → always acquired=true, release=no-op").
func (s *fileStore) TryScannerLock(_ context.Context, name string) (bool, func(), error) {
	return true, func() {}, nil
}

// --- MaintenanceWindow (PRMT-096) ----------------------------------------

// PutMaintenanceWindow implements Store. Idempotent upsert by ID
// (full-field overwrite on conflict; mirror PutInspectionTemplate).
// On save failure we restore the prior value (mirror PutPMSchedule's
// snapshot pattern) so the in-memory index never diverges from disk.
func (s *fileStore) PutMaintenanceWindow(_ context.Context, m MaintenanceWindow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.mwWindows[m.ID]
	s.mwWindows[m.ID] = m
	if err := s.save(); err != nil {
		if existed {
			s.mwWindows[m.ID] = prev
		} else {
			delete(s.mwWindows, m.ID)
		}
		return err
	}
	return nil
}

// GetMaintenanceWindow implements Store. Not found →
// (MaintenanceWindow{}, false, nil).
func (s *fileStore) GetMaintenanceWindow(_ context.Context, id string) (MaintenanceWindow, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.mwWindows[id]
	return m, ok, nil
}

// ListMaintenanceWindows implements Store. Sorted by StartsAt asc
// (soonest-starting first); ties broken by ID for deterministic
// order across runs.
func (s *fileStore) ListMaintenanceWindows(_ context.Context) ([]MaintenanceWindow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]MaintenanceWindow, 0, len(s.mwWindows))
	for _, m := range s.mwWindows {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.Before(out[j].StartsAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteMaintenanceWindow implements Store. Snapshot the pre-call
// map so a save failure can restore (mirror PutPMSchedule's pattern).
func (s *fileStore) DeleteMaintenanceWindow(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.mwWindows[id]
	if !existed {
		return false, nil
	}
	delete(s.mwWindows, id)
	if err := s.save(); err != nil {
		s.mwWindows[id] = prev
		return false, err
	}
	return true, nil
}

// ActiveWindowFor implements Store. Mirrors pgStore.ActiveWindowFor
// semantically: returns the first window whose [StartsAt, EndsAt)
// contains now AND whose asset_path matches assetPath (== OR
// "."-prefixed ancestor). fileStore walks the (small) in-memory
// index under a read lock; pgStore uses a single SQL query. The
// caller (pkg/alarm) treats "no active window" as the normal path
// (open ticket); a hit is the suppression branch.
//
// PRMT-096 R2 F3: collect ALL matching candidates first, then
// sort by (StartsAt asc, ID asc), then return the first. This
// matches pgStore's `ORDER BY starts_at ASC, id ASC LIMIT 1` so
// overlapping windows on the same asset return the same mwID
// across both stores.
func (s *fileStore) ActiveWindowFor(_ context.Context, assetPath string, now time.Time) (MaintenanceWindow, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	candidates := make([]MaintenanceWindow, 0, len(s.mwWindows))
	for _, m := range s.mwWindows {
		if now.Before(m.StartsAt) || !now.Before(m.EndsAt) {
			continue
		}
		if m.AssetPath != assetPath && !strings.HasPrefix(assetPath, m.AssetPath+".") {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return MaintenanceWindow{}, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].StartsAt.Equal(candidates[j].StartsAt) {
			return candidates[i].StartsAt.Before(candidates[j].StartsAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true, nil
}

// --- Tenant / Org / TenantAudit (PRMT-184, spec-001 v1.1 §5bis) ---

// GetTenant implements Store. Returns ("", false, nil) on absent
// row — mirrors the GetAsset / GetTicket convention. Err is
// non-nil only on a real store failure (fileStore has none).
func (s *fileStore) GetTenant(_ context.Context, id string) (Tenant, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[id]
	return t, ok, nil
}

// ListTenants implements Store. Returns tenants sorted by ID ASC
// for a stable order across runs. Empty / unknown store yields
// []Tenant{} (never nil) so JSON encoding is `[]`, not `null`.
func (s *fileStore) ListTenants(_ context.Context) ([]Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Tenant, 0, len(s.tenants))
	ids := make([]string, 0, len(s.tenants))
	for id := range s.tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, s.tenants[id])
	}
	return out, nil
}

// CreateTenant implements Store (L109 P804). Creates tenant + default org.
func (s *fileStore) CreateTenant(_ context.Context, id, displayName, principal string) (Tenant, Org, error) {
	id = strings.TrimSpace(id)
	displayName = strings.TrimSpace(displayName)
	if !validTenantSlug(id) {
		return Tenant{}, Org{}, fmt.Errorf("core: create tenant: invalid slug")
	}
	if displayName == "" {
		return Tenant{}, Org{}, fmt.Errorf("core: create tenant: display_name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[id]; ok {
		return Tenant{}, Org{}, ErrTenantExists
	}
	now := time.Now().UTC()
	t := Tenant{
		ID:            id,
		DisplayName:   displayName,
		IsolationTier: "label",
		Status:        "active",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	o := Org{
		ID:        newOrgID(),
		TenantID:  id,
		Name:      DefaultOrgName,
		CreatedAt: now,
	}
	prevAudLen := len(s.tenantAuds)
	s.tenants[id] = t
	s.orgs[o.ID] = o
	s.tenantAuds = append(s.tenantAuds,
		TenantAudit{
			ID:        newTenantAuditID(),
			TS:        now,
			Principal: principal,
			TenantID:  id,
			Op:        "tenant_create",
			Detail:    displayName,
		},
		TenantAudit{
			ID:        newTenantAuditID(),
			TS:        now,
			Principal: principal,
			TenantID:  id,
			Op:        "org_create",
			Detail:    o.ID + ":" + o.Name,
		},
	)
	if err := s.save(); err != nil {
		delete(s.tenants, id)
		delete(s.orgs, o.ID)
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return Tenant{}, Org{}, fmt.Errorf("core: create tenant: save: %w", err)
	}
	return t, o, nil
}

// GetOrg implements Store. Returns (Org{}, false, nil) on absent
// row — mirrors GetAsset / GetTicket convention.
func (s *fileStore) GetOrg(_ context.Context, id string) (Org, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orgs[id]
	return o, ok, nil
}

// ListOrgs implements Store. Filters by tenant_id and sorts by
// name ASC for a stable order across runs. Empty / unknown tenant
// yields []Org{} (never nil).
func (s *fileStore) ListOrgs(_ context.Context, tenantID string) ([]Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Capacity must be the MATCHING subset, not the whole store.
	// The previous make([]Org, 0, len(s.orgs)) allocated one full-store
	// slice per call; combined with the per-tenant call in
	// serveTenantsList that reached T x len(s.orgs) x sizeof(Org) per
	// GET /v1/tenants (PRMT-211 WP-1: ListOrgs = 92.10% of alloc_space,
	// 99.64% of inuse_space). Count first so the result is exact-sized
	// AND non-nil (the empty case must marshal as [] not null).
	n := 0
	for _, o := range s.orgs {
		if o.TenantID == tenantID {
			n++
		}
	}
	out := make([]Org, 0, n)
	for _, o := range s.orgs {
		if o.TenantID == tenantID {
			out = append(out, o)
		}
	}
	// Contract: name ASC (PRMT-184 §4). Sort the matched subset directly.
	// The previous implementation sorted names then re-resolved each name
	// with a full scan of s.orgs -- O(k x len(s.orgs)), and its comment
	// mis-described that scan as a "map lookup".
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListOrgsAll implements Store. One RLock, one pass over s.orgs, then
// name ASC per tenant (same contract as ListOrgs).
func (s *fileStore) ListOrgsAll(_ context.Context) (map[string][]Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Org)
	for _, o := range s.orgs {
		out[o.TenantID] = append(out[o.TenantID], o)
	}
	// Contract: name ASC per tenant (PRMT-184 §4), same as ListOrgs.
	for tid := range out {
		g := out[tid]
		sort.Slice(g, func(i, j int) bool { return g[i].Name < g[j].Name })
	}
	return out, nil
}

// AppendTenantAudit implements Store. Append-only — there is no
// update path by design (audit integrity, mirrors AppendAssetAudit).
// On save failure we truncate the in-memory slice back to its
// pre-call length so the in-memory state never claims to have
// written what the disk did not accept.
func (s *fileStore) AppendTenantAudit(_ context.Context, a TenantAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevLen := len(s.tenantAuds)
	s.tenantAuds = append(s.tenantAuds, a)
	if err := s.save(); err != nil {
		s.tenantAuds = s.tenantAuds[:prevLen]
		return err
	}
	return nil
}

// ListTenantAudits implements Store. Filters by tenant_id and
// sorts newest-first (TS DESC). Empty store / no matches yields
// a non-nil empty slice.
func (s *fileStore) ListTenantAudits(_ context.Context, tenantID string) ([]TenantAudit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TenantAudit, 0)
	for _, a := range s.tenantAuds {
		if a.TenantID != tenantID {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.After(out[j].TS)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// CreateOrg implements Store (PRMT-185 §4.1). fileStore holds the
// write lock across the existence + uniqueness checks, the insert,
// and the audit-append so a crash between the row write and the
// audit insert cannot leave a torn record (mirrors UpdateTenantTier's
// pre-save rollback). (tenant_id, name) uniqueness is enforced at
// the app layer because fileStore has no SQL UNIQUE; the duplicate
// path returns ErrOrgNameConflict so the handler can map to 409.
// FK on tenants(id) is enforced at the app layer by membership in
// s.tenants.
func (s *fileStore) CreateOrg(_ context.Context, tenantID, name, principal string) (Org, error) {
	if !validTenantSlug(name) {
		return Org{}, fmt.Errorf("core: create org: invalid slug")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[tenantID]; !ok {
		return Org{}, fmt.Errorf("core: create org: tenant not found")
	}
	for _, o := range s.orgs {
		if o.TenantID == tenantID && o.Name == name {
			return Org{}, ErrOrgNameConflict
		}
	}
	now := time.Now().UTC()
	o := Org{
		ID:        newOrgID(),
		TenantID:  tenantID,
		Name:      name,
		CreatedAt: now,
	}
	prevAudLen := len(s.tenantAuds)
	s.orgs[o.ID] = o
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  tenantID,
		Op:        "org_create",
		Detail:    o.ID + ":" + name,
	})
	if err := s.save(); err != nil {
		// Roll back so disk and index agree (mirrors AppendTenantAudit /
		// UpdateTenantTier save-failure pattern).
		delete(s.orgs, o.ID)
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return Org{}, fmt.Errorf("core: create org: save: %w", err)
	}
	return o, nil
}

// RenameOrg implements Store (PRMT-185 §4.1). fileStore holds the
// write lock across the slug re-check, the uniqueness re-check, the
// UPDATE, and the audit-append so a crash between the row write and
// the audit insert cannot leave a torn record. Equal-name is a
// no-op with zero audit rows (idempotent rename; mirrors the
// equal-tier no-op contract). Unknown id → wrapped not-found.
func (s *fileStore) RenameOrg(_ context.Context, id, newName, principal string) error {
	if !validTenantSlug(newName) {
		return fmt.Errorf("core: rename org: invalid slug")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.orgs[id]
	if !ok {
		return fmt.Errorf("core: rename org: not found")
	}
	if prev.Name == newName {
		// Idempotent no-op (mirrors UpdateTenantTier equal-tier branch).
		return nil
	}
	for _, o := range s.orgs {
		if o.TenantID == prev.TenantID && o.Name == newName && o.ID != id {
			return ErrOrgNameConflict
		}
	}
	now := time.Now().UTC()
	updated := prev
	updated.Name = newName
	prevAudLen := len(s.tenantAuds)
	s.orgs[id] = updated
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  prev.TenantID,
		Op:        "org_rename",
		Detail:    prev.Name + "→" + newName,
	})
	if err := s.save(); err != nil {
		// Roll back so disk and index agree.
		s.orgs[id] = prev
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return fmt.Errorf("core: rename org: save: %w", err)
	}
	return nil
}

// DeleteOrg implements Store (PRMT-185 §4.1, R5). fileStore holds the
// write lock across the existence check, the CountSitesByOrg guard,
// the DELETE, and the audit-append so a crash between the row write
// and the audit insert cannot leave a torn record. CountSitesByOrg
// > 0 → ErrOrgOwnsResources, NO delete, NO audit. count == 0 →
// delete + exactly one tenant_audit op="org_delete" detail "<name>"
// (the org is gone after the delete; capturing the name in the
// detail keeps the audit row human-readable). Applies uniformly to
// `default` and every other org (spec-001 §5bis.2).
func (s *fileStore) DeleteOrg(_ context.Context, id, principal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.orgs[id]
	if !ok {
		return fmt.Errorf("core: delete org: not found")
	}
	// R5 guard: CountSitesByOrg inside the lock so a racing
	// AttachSiteToOrg cannot slip a site mapping between the count
	// and the delete.
	n := 0
	for _, so := range s.siteOrgs {
		if so.OrgID == id {
			n++
		}
	}
	if n > 0 {
		return ErrOrgOwnsResources
	}
	now := time.Now().UTC()
	prevAudLen := len(s.tenantAuds)
	delete(s.orgs, id)
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  prev.TenantID,
		Op:        "org_delete",
		Detail:    prev.Name,
	})
	if err := s.save(); err != nil {
		// Roll back so disk and index agree.
		s.orgs[id] = prev
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return fmt.Errorf("core: delete org: save: %w", err)
	}
	return nil
}

// tenantTierRank is the LOCAL one-way-up rank map for isolation_tier
// (PRMT-182 §4.1, L98(b) / spec-001 v1.1 §5bis.1). Pinned here so
// core does NOT import pkg/tenant for rank ordering. label=0<row=1<db=2.
// An unknown tier returns ok=false; the mutator surfaces this as a
// validation error the handler maps to 400.
func tenantTierRank(tier string) (int, bool) {
	switch tier {
	case "label":
		return 0, true
	case "row":
		return 1, true
	case "db":
		return 2, true
	default:
		return 0, false
	}
}

// tenantTierValidationError is returned by UpdateTenantTier when the
// caller-supplied target is not in {label,row,db}. The handler maps
// this to a 400 bad-request problem.
var tenantTierValidationError = errors.New("core: invalid isolation_tier")

// UpdateTenantTier implements Store (PRMT-182). fileStore holds the
// write lock across the guard + update + audit-append so a crash
// between the row update and the audit insert cannot leave a torn
// record (mirrors AdjustSpare's pre-save rollback). Equal target is
// a no-op with zero audit rows; downgrade writes one REFUSED audit
// row and returns ErrTierDowngrade without mutating tenants[id].
func (s *fileStore) UpdateTenantTier(_ context.Context, id, target, principal string) error {
	targetRank, ok := tenantTierRank(target)
	if !ok {
		return tenantTierValidationError
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, exists := s.tenants[id]
	if !exists {
		return fmt.Errorf("core: update tenant tier: not found")
	}
	curRank, ok := tenantTierRank(t.IsolationTier)
	if !ok {
		// A corrupt store row (tier outside the allowlist) is a
		// store-corruption bug, not a caller-facing 400. Surface
		// as not-found so the caller treats it as "no such tenant".
		return fmt.Errorf("core: update tenant tier: not found")
	}
	now := time.Now().UTC()
	if targetRank == curRank {
		// Idempotent no-op per PRMT-182 §Resolved #1 (pinned):
		// no audit row on identical tier.
		return nil
	}
	if targetRank < curRank {
		// Downgrade refused: write ONE REFUSED audit row, leave the
		// record unchanged, surface ErrTierDowngrade so the handler
		// emits 409.
		prevLen := len(s.tenantAuds)
		s.tenantAuds = append(s.tenantAuds, TenantAudit{
			ID:        newTenantAuditID(),
			TS:        now,
			Principal: principal,
			TenantID:  id,
			Op:        "tier_change",
			Detail:    t.IsolationTier + "→" + target + " REFUSED",
		})
		if err := s.save(); err != nil {
			s.tenantAuds = s.tenantAuds[:prevLen]
			return err
		}
		return ErrTierDowngrade
	}
	// Upgrade path: update the record AND write ONE audit row.
	prev := t
	prevAudLen := len(s.tenantAuds)
	t.IsolationTier = target
	t.UpdatedAt = now
	s.tenants[id] = t
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  id,
		Op:        "tier_change",
		Detail:    prev.IsolationTier + "→" + target,
	})
	if err := s.save(); err != nil {
		// Roll back the in-memory state so the disk and the index
		// agree (mirrors AppendTenantAudit's save-failure pattern).
		s.tenants[id] = prev
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return err
	}
	return nil
}

// --- SiteOrg (PRMT-189, spec-001 v1.1 §5bis.2 site→Org mapping) ---

// validSiteSlug reports whether s matches the site-slug grammar
// [a-z]{2,8}[0-9]{2} (spec-001 §2; mirrored from cpath.reSite which
// is unexported, so we inline the check rather than touching
// pkg/cpath). Sites ending in "00" are also rejected — mirrors
// cpath's suffix check that rejects "site00" derivatives. Returns
// false on empty / too short / too long / wrong charset.
func validSiteSlug(s string) bool {
	if len(s) < 4 || len(s) > 10 { // 2..8 letters + 2 digits
		return false
	}
	// Last two chars must be digits; none may be "00".
	for i := 0; i < 2; i++ {
		c := s[len(s)-2+i]
		if c < '0' || c > '9' {
			return false
		}
	}
	if s[len(s)-2] == '0' && s[len(s)-1] == '0' {
		return false
	}
	// Leading 2..8 chars must be lowercase letters.
	for i := 0; i < len(s)-2; i++ {
		c := s[i]
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// siteSlugError is returned by AttachSiteToOrg when the supplied
// site fails validSiteSlug. Sentinel so callers (and tests) can
// distinguish bad-slug from not-found without parsing the message.
var siteSlugError = errors.New("core: invalid site slug")

// siteOrgNotFoundError mirrors the "core: adjust spare: not found"
// wrapped-error idiom used by AdjustSpare / UpdateTenantTier. There
// is intentionally NO exported sentinel — the prompt pins the
// not-found idiom to (T, bool, error) (mirrors GetAsset / GetOrg).
var siteOrgNotFoundError = errors.New("core: attach site to org: not found")

// GetSiteOrg implements Store. Returns (SiteOrg{}, false, nil) on
// an unmapped site — mirrors GetAsset / GetOrg convention.
func (s *fileStore) GetSiteOrg(_ context.Context, site string) (SiteOrg, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	so, ok := s.siteOrgs[site]
	return so, ok, nil
}

// ListSiteOrgs implements Store. Returns mappings sorted by site
// ASC for a stable order across runs. Empty store yields
// []SiteOrg{} (never nil) so JSON encoding is `[]`, not `null`.
func (s *fileStore) ListSiteOrgs(_ context.Context) ([]SiteOrg, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SiteOrg, 0, len(s.siteOrgs))
	sites := make([]string, 0, len(s.siteOrgs))
	for site := range s.siteOrgs {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	for _, site := range sites {
		out = append(out, s.siteOrgs[site])
	}
	return out, nil
}

// CountSitesByOrg implements Store. Returns the exact number of
// sites mapped to orgID (0 when none). Drives the 185 R5 delete
// guard (an org can only be deleted when no sites own it).
func (s *fileStore) CountSitesByOrg(_ context.Context, orgID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, so := range s.siteOrgs {
		if so.OrgID == orgID {
			n++
		}
	}
	return n, nil
}

// AttachSiteToOrg implements Store (PRMT-189). Idempotent under
// the write lock: same site→same org is a no-op with no audit row;
// site→different org upserts the row and writes ONE tenant_audit
// op="org_reattach" detail "<site>: <old>→<new>"; first attach
// (no prior row) upserts and writes ONE audit row detail
// "<site>→<orgID>". All-or-nothing on save failure: the in-memory
// mutation is rolled back so disk and index agree (mirrors the
// AppendTenantAudit + UpdateTenantTier save-failure pattern).
func (s *fileStore) AttachSiteToOrg(_ context.Context, site, orgID, principal string) error {
	if !validSiteSlug(site) {
		return siteSlugError
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Org FK check (fileStore has no SQL FK; mirror it at the app layer).
	o, ok := s.orgs[orgID]
	if !ok {
		return siteOrgNotFoundError
	}
	now := time.Now().UTC()
	cur, exists := s.siteOrgs[site]
	if exists && cur.OrgID == orgID {
		// Idempotent no-op: same site→same org. No row, no audit.
		return nil
	}
	prevAudLen := len(s.tenantAuds)
	var detail string
	if exists {
		detail = site + ": " + cur.OrgID + "→" + orgID
	} else {
		detail = site + "→" + orgID
	}
	updated := SiteOrg{Site: site, OrgID: orgID, CreatedAt: now, UpdatedAt: now}
	if exists {
		// Preserve original CreatedAt on re-home; only UpdatedAt advances.
		updated.CreatedAt = cur.CreatedAt
	}
	s.siteOrgs[site] = updated
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  o.TenantID,
		Op:        "org_reattach",
		Detail:    detail,
	})
	if err := s.save(); err != nil {
		// Roll back BOTH the site_orgs row and the audit append so
		// the index never claims what the disk did not accept.
		if exists {
			s.siteOrgs[site] = cur
		} else {
			delete(s.siteOrgs, site)
		}
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return err
	}
	return nil
}

// DetachSiteFromOrg implements Store (PRMT-220). Removes site→org
// under the write lock; writes one org_reattach audit with detail
// "<site>: <orgID>→" (unbind). Unmapped site → wrapped not-found.
func (s *fileStore) DetachSiteFromOrg(_ context.Context, site, principal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.siteOrgs[site]
	if !ok {
		return fmt.Errorf("core: detach site from org: not found")
	}
	// Resolve tenant via the mapped org (org may still exist).
	tenantID := ""
	if o, ook := s.orgs[cur.OrgID]; ook {
		tenantID = o.TenantID
	}
	now := time.Now().UTC()
	prevAudLen := len(s.tenantAuds)
	delete(s.siteOrgs, site)
	if tenantID != "" {
		s.tenantAuds = append(s.tenantAuds, TenantAudit{
			ID:        newTenantAuditID(),
			TS:        now,
			Principal: principal,
			TenantID:  tenantID,
			Op:        "org_reattach",
			Detail:    site + ": " + cur.OrgID + "→",
		})
	}
	if err := s.save(); err != nil {
		s.siteOrgs[site] = cur
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return fmt.Errorf("core: detach site from org: save: %w", err)
	}
	return nil
}

// DeleteTenant implements Store (PRMT-220). Refuses when any org
// remains under the tenant (including default). Empty → delete +
// tenant_status audit detail "deleted".
func (s *fileStore) DeleteTenant(_ context.Context, id, principal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.tenants[id]
	if !ok {
		return fmt.Errorf("core: delete tenant: not found")
	}
	for _, o := range s.orgs {
		if o.TenantID == id {
			return ErrTenantOwnsOrgs
		}
	}
	now := time.Now().UTC()
	prevAudLen := len(s.tenantAuds)
	delete(s.tenants, id)
	s.tenantAuds = append(s.tenantAuds, TenantAudit{
		ID:        newTenantAuditID(),
		TS:        now,
		Principal: principal,
		TenantID:  id,
		Op:        "tenant_status",
		Detail:    "deleted",
	})
	if err := s.save(); err != nil {
		s.tenants[id] = prev
		s.tenantAuds = s.tenantAuds[:prevAudLen]
		return fmt.Errorf("core: delete tenant: save: %w", err)
	}
	return nil
}

// --- RoleBinding (PRMT-190-bis §4.2, spec-004 §6bis, R3) ---

// PutRoleBinding implements Store. Upsert on (subject, scope): a
// re-put updates the existing row in place (origin + updated_at
// advance), never creates a duplicate. fileStore has no SQL UNIQUE
// so the dedup runs at the application layer under the write lock;
// pgStore delegates the same semantics to the SQL UNIQUE
// constraint. Empty subject or scope is rejected at the boundary
// so neither store can carry a row that violates the schema.
// Snapshot the pre-change slice so a save failure can restore the
// in-memory state to match what the disk now holds (mirrors the
// AppendTicketAudit / UpdateTenantTier save-failure pattern).
func (s *fileStore) PutRoleBinding(_ context.Context, rb RoleBinding) error {
	if rb.Subject == "" || rb.Scope == "" {
		return fmt.Errorf("core: put role binding: subject and scope required")
	}
	if rb.Origin == "" {
		rb.Origin = "legacy" // matches the SQL DEFAULT
	}
	if rb.ID == "" {
		rb.ID = newRoleBindingID()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	prevLen := len(s.roleBindings)
	// Upsert by (subject, scope) — update origin + updated_at on hit.
	for i, existing := range s.roleBindings {
		if existing.Subject == rb.Subject && existing.Scope == rb.Scope {
			updated := existing
			updated.Origin = rb.Origin
			updated.UpdatedAt = now
			if updated.CreatedAt.IsZero() {
				updated.CreatedAt = now
			}
			s.roleBindings[i] = updated
			if err := s.save(); err != nil {
				// Restore the row we just mutated so the index matches disk.
				s.roleBindings[i] = existing
				return err
			}
			return nil
		}
	}
	// No existing row — append.
	if rb.CreatedAt.IsZero() {
		rb.CreatedAt = now
	}
	rb.UpdatedAt = now
	s.roleBindings = append(s.roleBindings, rb)
	if err := s.save(); err != nil {
		// Truncate the append so the index never claims what disk did not accept.
		s.roleBindings = s.roleBindings[:prevLen]
		return err
	}
	return nil
}

// ListRoleBindings implements Store. Returns rows for one subject
// in Scope ASC order. Unknown / empty subject yields a non-nil
// empty slice so the loader (LoadRoleBindingsInto in core/auth.go)
// sees an empty list, not nil, and skips the augmentation cleanly.
func (s *fileStore) ListRoleBindings(_ context.Context, subject string) ([]RoleBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RoleBinding, 0)
	for _, rb := range s.roleBindings {
		if rb.Subject != subject {
			continue
		}
		out = append(out, rb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, nil
}

// ListAllRoleBindings implements Store. Returns every row in
// (Subject ASC, Scope ASC) order — the stable order PRMT-186
// rewrites against. Empty store yields []RoleBinding{} (never nil).
func (s *fileStore) ListAllRoleBindings(_ context.Context) ([]RoleBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RoleBinding, len(s.roleBindings))
	copy(out, s.roleBindings)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// DeleteRoleBinding implements Store. Removes the (subject, scope)
// row if present and is a no-op when the row is already absent
// (migration-only primitive; PRMT-186 §3 widening). Empty subject
// or scope is rejected so the call cannot target the empty-key
// row the schema's UNIQUE would otherwise refuse. Same lock and
// save-rollback pattern as PutRoleBinding: the in-memory index
// is restored from a pre-change snapshot when the disk write
// fails, so the index never claims what disk did not accept.
func (s *fileStore) DeleteRoleBinding(_ context.Context, subject, scope string) error {
	if subject == "" || scope == "" {
		return fmt.Errorf("core: delete role binding: subject and scope required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.roleBindings {
		if existing.Subject != subject || existing.Scope != scope {
			continue
		}
		// Preserve slice ordering by splicing out index i in place.
		s.roleBindings = append(s.roleBindings[:i], s.roleBindings[i+1:]...)
		if err := s.save(); err != nil {
			// Restore the row we just removed so the index matches disk.
			s.roleBindings = append(s.roleBindings[:i], append([]RoleBinding{existing}, s.roleBindings[i:]...)...)
			return err
		}
		return nil
	}
	// Idempotent: no row matched → no-op, no save() churn.
	return nil
}

// UpsertUsage implements Store (PRMT-192/195). Natural-key idempotent:
// if ID empty, reuse id of matching (kind, asset, period, granularity).
// On save failure the pre-change map entry is restored.
func (s *fileStore) UpsertUsage(_ context.Context, rec UsageRecord) (UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		key := usageNaturalKey(rec)
		for id, existing := range s.usages {
			if usageNaturalKey(existing) == key {
				rec.ID = id
				break
			}
		}
		if rec.ID == "" {
			rec.ID = newUsageID()
		}
	}
	prev, existed := s.usages[rec.ID]
	s.usages[rec.ID] = rec
	if err := s.save(); err != nil {
		if existed {
			s.usages[rec.ID] = prev
		} else {
			delete(s.usages, rec.ID)
		}
		return UsageRecord{}, err
	}
	return rec, nil
}

// ListUsage implements Store (PRMT-192). Filters by tenant/site/
// kind/granularity and period overlap; sorts by PeriodStart,
// AssetPath, Kind. Empty result is a non-nil empty slice.
func (s *fileStore) ListUsage(_ context.Context, f UsageListFilter) ([]UsageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UsageRecord, 0)
	for _, rec := range s.usages {
		if f.TenantID != "" && rec.TenantID != f.TenantID {
			continue
		}
		if f.SiteID != "" && rec.SiteID != f.SiteID {
			continue
		}
		if f.Kind != "" && rec.Kind != f.Kind {
			continue
		}
		if f.Granularity != "" && rec.Granularity != f.Granularity {
			continue
		}
		// Period overlap: rec.PeriodStart < f.PeriodEnd && rec.PeriodEnd > f.PeriodStart.
		// Zero filter bounds are open.
		if !f.PeriodEnd.IsZero() && !rec.PeriodStart.Before(f.PeriodEnd) {
			continue
		}
		if !f.PeriodStart.IsZero() && !rec.PeriodEnd.After(f.PeriodStart) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PeriodStart.Equal(out[j].PeriodStart) {
			return out[i].PeriodStart.Before(out[j].PeriodStart)
		}
		if out[i].AssetPath != out[j].AssetPath {
			return out[i].AssetPath < out[j].AssetPath
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}
