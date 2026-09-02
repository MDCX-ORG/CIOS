/**
 * Shared domain types for the ops portal surface.
 *
 * Mirrors spec-004 envelope (`{items, next_page_token}`) + spec-001 §2 asset
 * fields. If the live /v1/assets shape drifts from this, STOP and record in
 * §8 — do NOT invent fields here.
 */

export interface Asset {
  /** Cascade path, e.g. "site01.pod000.cdu000". */
  crn: string;
  /** spec-001 asset_type (e.g. "site", "pod", "cdu"). */
  type: string;
  name?: string;
  /** crn of parent; "" / undefined for site root. */
  parent?: string;
}

export interface Paged<T> {
  items: T[];
  next_page_token: string;
}

export type AssetsResponse = Paged<Asset>;

// spec-003 severity vocabulary — do NOT invent levels.
export type AlarmSeverity = "info" | "warning" | "critical";
export interface Alarm {
  crn: string; // affected asset crn
  severity: AlarmSeverity;
  state: "firing" | "resolved";
  summary?: string;
}
// R8 projection (spec-009 §4.2/§5.6). Temps carry value+press+flow; absent => undefined.
export interface TempPort {
  temp_c?: number;
  press_kpa?: number;
  flow_lpm?: number;
}
export interface InspectorFields {
  id: string; // = crn
  name?: string;
  status: string; // derived: "alarm" if firing else "ok" (see §4.3)
  inlet?: TempPort;
  outlet?: TempPort;
  alarm?: Alarm;
}

// Prometheus instant-query shape (spec-004 /v1/metrics/query → VM). If the live
// shape differs, STOP and record in §8.
export interface MetricSample { metric: Record<string, string>; value: [number, string]; }
export interface MetricsQueryResponse {
  status: "success" | "error";
  data: { resultType: "vector" | "matrix" | "scalar" | "string"; result: MetricSample[] };
}
// R4 projected label set. Keys mirror spec-002 quantities; values pre-formatted
// with unit by the loader. unit MUST come from spec-002 units.yaml.
export interface LabelEntry { value: string; unit?: string; }
export interface LabelSet {
  crn: string;
  uptime?: LabelEntry;
  power?: LabelEntry;
  state?: LabelEntry;
  utilization?: LabelEntry;
}

// Prometheus range-query shape (spec-004 /v1/metrics/query_range → VM). If the
// live shape differs, STOP and record in §8.
export interface MetricMatrix { metric: Record<string, string>; values: [number, string][]; }
export interface MetricsRangeResponse {
  status: "success" | "error";
  data: { resultType: "matrix"; result: MetricMatrix[] };
}
// R5 (spec-009 §5.4) projected site-level series. PUE is site-level only (L48).
export interface SeriesPoint { t: number; v: number; }
export interface SiteSeries {
  site: string;
  facility_power: SeriesPoint[];   // watts (spec-002)
  it_power: SeriesPoint[];
  pue: SeriesPoint[];              // dimensionless; site-level only (L48)
}

// R6 (spec-009 §5.5) projected site list for the top-left switcher.
// `worstSeverity` aggregates firing /api/alarms by crn prefix; `org` is
// populated from core/RBAC authority (L84) when exposed, undefined otherwise
// (frontend degrades to a flat list per §8).
export interface SiteOption {
  site: string;
  name?: string;
  org?: string;
  worstSeverity?: AlarmSeverity;
}
export interface SiteListProjection { sites: SiteOption[]; active?: string; }

// spec-001 §7 relationship graph. Edge kinds are the locked vocabulary — do NOT add kinds.
export type EdgeKind = "feeds" | "cools" | "connects";
export interface TopologyEdge { from: string; to: string; kind: EdgeKind; }
export interface Topology { edges: TopologyEdge[]; }
export interface CauseAnalysis {
  target: string;                 // the alarmed crn drilled into
  rootCause?: { crn: string; via: EdgeKind };   // deterministic per PRMT-147 §4.3
  impact: { crn: string; via: EdgeKind }[];     // downstream-affected crns
}

// spec-003 ticket severity vocabulary (mirror Alarm enum; PRMT-033 §4.2).
export type TicketSeverity = "critical" | "major" | "minor" | "info";
// spec-008 ticket state machine (PRMT-033 §4.1).
export type TicketState = "open" | "acknowledged" | "resolved" | "closed";

// /v1/tickets GET response item shape — pinned to core.Ticket (core/store.go).
// PRMT-156 §4: do NOT invent fields not in core. Timestamps are ISO-8601 strings
// (emitted by core.encodeJSON); omitempty fields may be absent.
export interface Ticket {
  id: string;
  alarm_id?: string;
  asset_path: string;
  title: string;
  severity: TicketSeverity;
  state: TicketState;
  assignee?: string;
  opened_at: string;
  acked_at?: string;
  resolved_at?: string;
  closed_at?: string;
  escalated_at?: string;
  resource_version: number;
  runbook?: string;
}

// /v1/capacity GET response — pinned to core.capacityResponse
// (core/capacity.go). PRMT-157 §4: do NOT invent fields not in core.
// Optional fields are nullable per json:"…" / json:"…,omitempty"
// semantics; `rated` / `measured_p95` / `remaining` are *float64 and
// may be null on VM failure (fail-soft, not 502).
export interface CapacityRow {
  path: string;
  lifecycle?: string;
  rated?: number | null;
  measured_p95?: number | null;
  remaining?: number | null;
  missing_rated?: boolean;
  degraded?: boolean;
}
// Pinned to core.capacityDimResult (core/capacity.go L67-78).
export interface CapacityDimension {
  dimension: "power" | "cooling" | "rack" | "gpu";
  status: "ok" | "not_implemented";
  unit: string; // "W" | "kw" | "watt"
  rated?: number | null;
  measured_p95?: number | null;
  remaining?: number | null;
  degraded: boolean;
  by_asset: CapacityRow[];
  missing_rated: number;
}
// Pinned to core.capacityResponse envelope (core/capacity.go L93-99).
export interface CapacityResponse {
  dimensions: CapacityDimension[];
  window: string;
}

// /v1/capacity/forecast — P741 (core/capacity_forecast.go).
export interface CapacityForecastDim {
  dimension: "power" | "cooling" | "rack" | "gpu" | string;
  status: "ok" | "degraded" | "not_implemented" | string;
  unit?: string;
  rated?: number | null;
  measured_baseline?: number | null;
  forecast_measured?: number | null;
  forecast_remaining?: number | null;
  degraded?: boolean;
  note?: string;
}
export interface CapacityForecastHorizon {
  horizon: string;
  days: number;
  dimensions: CapacityForecastDim[];
}
export interface CapacityForecastResponse {
  method: string;
  growth_pct_per_year: number;
  baseline_window: string;
  horizons: CapacityForecastHorizon[];
  notes?: string[];
}

// /v1/spares GET response — pinned to core.SparePart
// (core/store.go L143-150). PRMT-159 §4: do NOT invent fields not
// in core. `low_stock` is derived server-side per-id only (the list
// endpoint omits it; the UI derives `qty < min_qty` locally for the
// `data-low-stock` marker, matching core.sparePartWithDerived.LowStock
// semantics, PRMT-048 §2). `location` is omitempty in core.
export interface Spare {
  id: string;
  sku: string;
  name: string;
  qty: number;
  min_qty: number;
  location?: string;
}
// Pinned to core.listSparesResponse envelope (core/spares.go L77-80).
export type SparesResponse = Paged<Spare>;

// /v1/maintenance/upcoming GET response — pinned to
// core.maintenanceUpcomingItem + core.maintenanceUpcomingResponse
// (core/maintenance.go L34-48). PRMT-158 §4: do NOT invent fields
// not in core. `next_due` is RFC3339 (time.Time); `kind` is "pm"
// or "inspection" (L33); `overdue` is computed against request time.
export interface MaintenanceItem {
  kind: "pm" | "inspection";
  id: string;
  asset_path: string;
  title: string;
  next_due: string; // RFC3339
  overdue: boolean;
}
// Pinned to core.maintenanceUpcomingResponse envelope
// (core/maintenance.go L46-48). Items is always [] (never null).
export interface MaintenanceUpcomingResponse {
  items: MaintenanceItem[];
}

// /v1/inspections GET response — pinned to core.InspectionTemplate
// (core/store.go L222-230) + core.listInspectionsResponse envelope
// (core/inspection.go L55-57). PRMT-160 §4: do NOT invent fields
// not in core. The draft §4 hint listing `template/crn/due_at/status/
// ticket_ref` is a stale strawman; core is the source of truth per
// the §4 "Do NOT invent" rule (same precedent as PRMT-158's
// maintenance-list.tsx L22-25 note). `interval` is nanoseconds
// (time.Duration JSON encoding). `items` is the checklist array
// carried verbatim from core — no shape translation at the API layer.
export interface Inspection {
  id: string;
  asset_path: string;
  title: string;
  items: string[];
  interval: number; // nanoseconds (time.Duration JSON encoding)
  next_due: string; // RFC3339
  enabled: boolean;
}
// Pinned to core.listInspectionsResponse envelope
// (core/inspection.go L55-57). listInspectionsResponse has no
// `next_page_token` (M2 inspection scale is operator-set, small —
// inspection.go L80). Items is always [] (never null).
export interface InspectionsResponse {
  items: Inspection[];
}

// /v1/cases GET response item — KB seed projection (PRMT-161 §4;
// M2 P572). core.serveCases (core/runbooks.go L104-238) returns
// {items: Ticket[]} where Ticket is core.Ticket (core/store.go L72-91).
// The UI projection for the runbook/cases viewer drops the ticket-
// queue noise (state machine, assignee, opened_at, resource_version)
// and keeps only the KB seed surface (id / title / summary / crn /
// closed_at). `crn` and `closed_at` are derived from core.Ticket
// (asset_path → crn, *time.Time ClosedAt → ISO-8601 string) by the
// route loader before projection into this shape; do NOT invent
// fields here.
export interface Case {
  id: string;
  title: string;
  summary?: string;
  crn?: string;
  closed_at?: string;
}
// Pinned to core.listCasesResponse envelope (core/runbooks.go
// L104-106). NO `next_page_token` — /v1/cases is unpaged (uses
// ?limit= ceiling, not cursor pagination; PRMT-053 §2).
export interface CasesResponse {
  items: Case[];
}

// /v1/runbooks/{key} GET response — KB runbook lookup (PRMT-161 §4;
// M2 P571). core.serveRunbook (core/runbooks.go L40-99) returns the
// raw markdown body with Content-Type: text/markdown; there is no
// JSON envelope at the wire. The route loader wraps the body into
// {key, title, body} for the UI; `title` is derived server-side from
// the first H1 in the markdown (or falls back to the key); `body` is
// the markdown text verbatim. Do NOT invent fields here.
export interface Runbook {
  key: string;
  title: string;
  body: string;
}

// /v1/reports/ops GET response — ops report (PRMT-162 §4; M2 P551 /
// E2.6). core.serveOpsReport (core/reports.go L69) returns the JSON
// body produced by `opsReportResponse` (core/reports.go L31-38).
// `mttr_seconds` / `mean_response_seconds` / `mtbf_seconds` are
// *float64 and render as `null` when empty (so a client can tell "no
// resolved tickets" from "MTTR=0", matching the §2-bis "0 / null"
// rule); `ticket_counts.by_state` keys mirror Ticket.State
// (open/acknowledged/resolved/closed) and `by_severity` keys mirror
// Ticket.Severity (critical/major/minor/info); `alarm_top[].path` is
// the asset crn; `window.since` echoes the optional ?since filter
// (RFC3339) so the client can confirm the window. Do NOT invent
// fields here.
export interface OpsReport {
  mttr_seconds?: number | null;
  mean_response_seconds?: number | null;
  mtbf_seconds?: number | null;
  ticket_counts: {
    by_state: Record<string, number>;
    by_severity: Record<string, number>;
  };
  alarm_top: { path: string; count: number }[];
  window?: { since?: string };
}

// /api/usage GET response item — usage facts for 对量 (PRMT-194 / L102 /
// E3.2 portal surface). Energy (kWh) + rack_hour only; no money /
// invoice / currency fields. Live core envelope may land later; portal
// mock seeds the shape below. Do NOT invent billing fields here.
export type UsageKind = "energy" | "rack_hour";
export interface UsageRecord {
  id: string;
  kind: UsageKind;
  tenant_id: string;
  org_id?: string;
  site_id: string;
  asset_path: string;
  period_start: string; // RFC3339
  period_end: string; // RFC3339
  granularity: string; // e.g. "monthly"
  quantity: number;
  unit: string; // "kWh" | "h"
}
// Paged envelope for /api/usage (items + next_page_token).
export type UsageResponse = Paged<UsageRecord>;

// Mirror of twins-v0 (PRMT-169 §4.1). Single authority — do NOT add/rename
// fields. Reused by PRMT-152 (Phase B render base) for the Scene Engine
// scene descriptor served by the /api/twins gateway route (PRMT-170).
// - `access` wire enum is L97: `full` | `ghost` | `hidden` (aligned with
//   pkg/sceneprune). Live v0 Scene Engine emits `full` only; `hidden` is
//   represented by node absence; `ghost` is reserved and used by the
//   MOCK_GATEWAY=1 fixture to exercise the R2 outline path (CODE-SCAN-2026-07-16 §2.3).
// - `geometry.file` is a file name, NOT a URL (resolved relative to
//   /api/twins/geometry/). The descriptor carries NO colors/thresholds —
//   visual_state is owned by spec-009 §4.1 (L92).
export type SceneModel = "ac45" | "dc45" | "placeholder";
export type SceneAccess = "full" | "ghost" | "hidden";
export type SceneEdgeRel = "feeds" | "cools";

export interface SceneNode {
  /** cpath (authority = seed assets.yaml; renderer does not re-validate cpath). */
  path: string;
  /** Asset type, e.g. "cdu". */
  type: string;
  /** == path literal (isomorphic mapping). */
  gltf_node: string;
  model: SceneModel;
  /** L97 access; access !== "full" ⇒ R2 outline/ghost render. */
  access: SceneAccess;
}

export interface SceneEdge {
  from: string;
  to: string;
  rel: SceneEdgeRel;
  /** R7 flow denominator (live vs rated_kw). */
  rated_kw: number;
}

export interface SceneBinding {
  path: string;
  /** Relative point names from protocol/ point tables (names only; no values/thresholds/colors). */
  points: string[];
}

export interface SceneDescriptor {
  /** String literal; loader MUST reject any other value (STOP, do not adapt). */
  contract: "twins-v0";
  site: string;
  geometry: { format: "gltf-binary"; file: string };
  nodes: SceneNode[];
  edges: SceneEdge[];
  bindings: SceneBinding[];
  /** authority = "spec-009 §4.1"; descriptor carries NO colors/thresholds. */
  visual: { authority: string; note: string };
}