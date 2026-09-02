/**
 * Project core/apigw asset JSON → portal view model.
 * Live shape: { path, spec: { type, model, ... } }
 * Mock shape:  { crn, type, name, parent, ... }
 */
import type { ApiSession } from "./auth.server";
import { loadApi } from "./fetch";

export interface PortalAsset {
  crn: string;
  type: string;
  name?: string;
  parent?: string;
  model?: string;
  lifecycle?: string;
  serial?: string;
  series?: string;
  rated_power_kw?: number;
  rated_cooling_kw?: number;
}

export function projectAsset(raw: unknown): PortalAsset | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Record<string, unknown>;

  // Mock / already-projected shape
  if (typeof r.crn === "string" && typeof r.type === "string") {
    return {
      crn: r.crn,
      type: r.type,
      name: typeof r.name === "string" ? r.name : undefined,
      parent: typeof r.parent === "string" ? r.parent : undefined,
      model: typeof r.model === "string" ? r.model : undefined,
      lifecycle: typeof r.lifecycle === "string" ? r.lifecycle : undefined,
      serial: typeof r.serial === "string" ? r.serial : undefined,
      series: typeof r.series === "string" ? r.series : undefined,
      rated_power_kw:
        typeof r.rated_power_kw === "number" ? r.rated_power_kw : undefined,
      rated_cooling_kw:
        typeof r.rated_cooling_kw === "number" ? r.rated_cooling_kw : undefined,
    };
  }

  // Live core/apigw shape
  if (typeof r.path === "string" && r.spec && typeof r.spec === "object") {
    const path = r.path;
    const spec = r.spec as Record<string, unknown>;
    const parts = path.split(".");
    const parent = parts.length > 1 ? parts.slice(0, -1).join(".") : undefined;
    const leaf = parts[parts.length - 1] ?? path;
    return {
      crn: path,
      type: typeof spec.type === "string" ? spec.type : "",
      name: typeof spec.name === "string" ? spec.name : leaf,
      parent,
      model: typeof spec.model === "string" ? spec.model : undefined,
      lifecycle:
        typeof spec.lifecycle === "string" ? spec.lifecycle : undefined,
      serial: typeof spec.serial === "string" ? spec.serial : undefined,
      series: typeof spec.series === "string" ? spec.series : undefined,
      rated_power_kw:
        typeof spec.rated_power_kw === "number"
          ? spec.rated_power_kw
          : undefined,
      rated_cooling_kw:
        typeof spec.rated_cooling_kw === "number"
          ? spec.rated_cooling_kw
          : undefined,
    };
  }

  return null;
}

/** Follow page_token until exhausted (or mock single page). Dedupes by crn. */
export async function loadAllPortalAssets(
  session: ApiSession,
): Promise<PortalAsset[]> {
  const byCrn = new Map<string, PortalAsset>();
  let token = "";
  for (let page = 0; page < 50; page++) {
    const qs = new URLSearchParams();
    qs.set("page_size", "200");
    if (token) qs.set("page_token", token);
    const path = `/api/assets?${qs.toString()}`;
    const data = await loadApi<{
      items?: unknown[];
      next_page_token?: string;
    }>(path, session);
    const items = data.items ?? [];
    let added = 0;
    for (const raw of items) {
      const a = projectAsset(raw);
      if (!a || !a.crn || !a.type) continue;
      if (!byCrn.has(a.crn)) {
        byCrn.set(a.crn, a);
        added++;
      }
    }
    const next = data.next_page_token ?? "";
    // Stop on empty page, no progress (token loop / re-seed dups), or end.
    if (!next || next === token || items.length === 0 || added === 0) break;
    token = next;
  }
  const out = Array.from(byCrn.values());
  // Ensure site roots exist for NOC switcher when seed has no type=site rows.
  const sites = new Set<string>();
  for (const a of out) {
    const site = a.crn.split(".")[0];
    if (site) sites.add(site);
  }
  for (const site of sites) {
    if (!out.some((a) => a.crn === site && a.type === "site")) {
      out.unshift({
        crn: site,
        type: "site",
        name: site.toUpperCase(),
        lifecycle: "active",
      });
    }
  }
  return out;
}

export function fleetSummary(assets: PortalAsset[]) {
  const pods = assets.filter((a) => a.type === "pod");
  const modelSummary = pods.reduce<Record<string, number>>((acc, p) => {
    const m = p.model ?? "unknown";
    acc[m] = (acc[m] ?? 0) + 1;
    return acc;
  }, {});
  const typeSummary = assets.reduce<Record<string, number>>((acc, a) => {
    acc[a.type] = (acc[a.type] ?? 0) + 1;
    return acc;
  }, {});
  const sites = assets.filter((a) => a.type === "site");
  const powerKw = pods.reduce((s, p) => s + (p.rated_power_kw ?? 0), 0);
  const coolingKw = pods.reduce((s, p) => s + (p.rated_cooling_kw ?? 0), 0);
  return {
    total: assets.length,
    sites: sites.length,
    pods: pods.length,
    modelSummary,
    typeSummary,
    powerKw,
    coolingKw,
    siteIds: sites.map((s) => s.crn),
    podRows: pods.map((p) => ({
      crn: p.crn,
      model: p.model ?? "—",
      serial: p.serial ?? "—",
      power: p.rated_power_kw,
      cooling: p.rated_cooling_kw,
      series: p.series,
    })),
  };
}

/** Map live alarm severity (major/minor) → portal AlarmSeverity. */
export function projectAlarmSeverity(
  raw: string | undefined,
): "info" | "warning" | "critical" {
  switch (raw) {
    case "critical":
      return "critical";
    case "major":
    case "warning":
      return "warning";
    case "minor":
    case "info":
    default:
      return "info";
  }
}

export function projectAlarm(raw: unknown): {
  crn: string;
  severity: "info" | "warning" | "critical";
  state: "firing" | "resolved";
  summary?: string;
  /** PRMT-230: core alarm id; "" when the payload has no usable id (mock rows). */
  id: string;
  /** PRMT-230: un-collapsed spec-003 state: firing|acked|resolved. */
  rawState: string;
} | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Record<string, unknown>;
  // live: path; mock: crn
  const crn =
    typeof r.crn === "string"
      ? r.crn
      : typeof r.path === "string"
        ? r.path
        : "";
  if (!crn) return null;
  const stateRaw = typeof r.state === "string" ? r.state : "firing";
  // acked still "firing" for ops display (still open)
  const displayState: "firing" | "resolved" =
    stateRaw === "resolved" || stateRaw === "cleared" ? "resolved" : "firing";
  return {
    crn,
    severity: projectAlarmSeverity(
      typeof r.severity === "string" ? r.severity : undefined,
    ),
    state: displayState,
    summary: typeof r.summary === "string" ? r.summary : undefined,
    id: typeof r.id === "string" ? r.id : "",
    rawState: stateRaw,
  };
}
