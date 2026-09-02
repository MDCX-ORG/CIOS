/**
 * Site switcher + Org dropdown (R6 — spec-009 §5.5).
 *
 * Top-left transparent dropdown listing sites grouped by Org (L84) when
 * present, or flat (alphabetical) when Org is not yet exposed by the
 * authority. Sites with a firing alarm are highlighted (`data-site-anomaly`).
 *
 * Presentational only — no fetch, no router. The route loader derives the
 * `SiteListProjection` (sites + per-site worst firing severity + active);
 * selection flows out via `onSelect(siteId)` and the caller navigates
 * client-side to `?site=<siteId>`.
 *
 * `SiteListProjection` is locally re-declared (mirroring the SiteChart /
 * ObjectInspector pattern) so `@cios/api-client` does not need to be added
 * as a workspace dep to `packages/ui` (its package.json is OUT of the §3
 * whitelist). The route loader (which DOES depend on @cios/api-client)
 * projects into this local shape.
 */

import type { JSX } from "react";

/** spec-003 severity vocabulary — must match @cios/api-client's union. */
export type AlarmSeverity = "info" | "warning" | "critical";

export interface SiteOption {
  /** Site id (crn root segment), e.g. "site01". */
  site: string;
  /** Optional display name from assets metadata. */
  name?: string;
  /** L84 Org; undefined when core/RBAC does not yet expose it. */
  org?: string;
  /** Worst severity among firing alarms for the site; undefined = none. */
  worstSeverity?: AlarmSeverity;
}

export interface SiteListProjection {
  sites: SiteOption[];
  active?: string;
}

export interface SiteSwitcherProps {
  projection: SiteListProjection;
  onSelect?: (site: string) => void;
}

function severityClass(sev: AlarmSeverity | undefined): string {
  if (!sev) return "";
  if (sev === "critical") {
    return "bg-red-100 text-red-900 ring-1 ring-red-300";
  }
  if (sev === "warning") {
    return "bg-amber-100 text-amber-900 ring-1 ring-amber-300";
  }
  return "bg-sky-100 text-sky-900 ring-1 ring-sky-300";
}

function compareSites(a: SiteOption, b: SiteOption): number {
  return a.site.localeCompare(b.site);
}

function groupByOrg(
  sites: readonly SiteOption[],
): { org?: string; items: SiteOption[] }[] {
  const groups = new Map<string, SiteOption[]>();
  const order: string[] = [];
  let anyOrg = false;
  for (const s of sites) {
    if (s.org !== undefined) {
      anyOrg = true;
      const key = s.org;
      const bucket = groups.get(key);
      if (bucket) {
        bucket.push(s);
      } else {
        groups.set(key, [s]);
        order.push(key);
      }
    }
  }
  if (!anyOrg) {
    return [{ items: [...sites].sort(compareSites) }];
  }
  return order
    .sort((a, b) => a.localeCompare(b))
    .map((org) => ({ org, items: (groups.get(org) ?? []).sort(compareSites) }));
}

export function SiteSwitcher(props: SiteSwitcherProps): JSX.Element {
  const { projection, onSelect } = props;
  const { sites, active } = projection;
  const groups = groupByOrg(sites);

  return (
    <div
      data-site-switcher
      data-org-grouped={groups.some((g) => g.org !== undefined) ? "true" : "false"}
      className="inline-flex flex-col gap-1 rounded-md border bg-background/60 p-2 text-sm backdrop-blur"
    >
      {groups.map((g, gi) => (
        <div
          key={g.org ?? `__flat_${gi}`}
          data-org-group={g.org ?? ""}
          className="flex flex-col gap-1"
        >
          {g.org !== undefined ? (
            <div
              data-org={g.org}
              className="px-2 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground"
            >
              {g.org}
            </div>
          ) : null}
          {g.items.map((s) => {
            const isAnomaly = s.worstSeverity !== undefined;
            const isActive = s.site === active;
            return (
              <button
                key={s.site}
                type="button"
                data-site={s.site}
                data-site-name={s.name ?? ""}
                data-site-anomaly={isAnomaly ? "true" : "false"}
                data-site-severity={s.worstSeverity ?? ""}
                aria-current={isActive ? "true" : undefined}
                onClick={() => onSelect?.(s.site)}
                className={[
                  "inline-flex items-center justify-between gap-2 rounded px-2 py-1 text-left",
                  "hover:bg-accent hover:text-accent-foreground",
                  isActive ? "font-semibold ring-1 ring-ring" : "",
                  severityClass(s.worstSeverity),
                ]
                  .filter(Boolean)
                  .join(" ")}
              >
                <span className="font-mono">{s.site}</span>
                {s.name ? (
                  <span className="text-xs text-muted-foreground">{s.name}</span>
                ) : null}
                {isAnomaly ? (
                  <span
                    data-site-anomaly-badge
                    aria-hidden="true"
                    className="ml-1 rounded px-1 text-[10px] font-semibold uppercase"
                  >
                    {s.worstSeverity}
                  </span>
                ) : null}
              </button>
            );
          })}
        </div>
      ))}
    </div>
  );
}
