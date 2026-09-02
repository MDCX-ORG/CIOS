/**
 * /maintenance — upcoming maintenance page (E3.5 / P643; spec-009 §5
 * PM + inspection entry point).
 *
 *   - Fetches /api/maintenance/upcoming via the mock seam
 *     (MOCK_GATEWAY=1) or live `apiGet`. Merged PM + inspection view
 *     owned by M2 P558 (core/maintenance.go serveMaintenanceUpcoming).
 *   - Honors URL params `?before=<RFC3339>` and `?overdue=true|false`
 *     (spec-004 whitelist; core/maintenance.go L10-17). No filter UI
 *     in Phase A; URL param pass-through is the seam.
 *
 * Read-only (Phase A per spec-009 §8). No POST/PUT/DELETE/PATCH.
 * Schedule mutation is M4 (PRMT-158 §6 MUST NOT).
 */

import type { Route } from "./+types/maintenance";
import { MaintenanceList } from "@cios/ui";
import type { MaintenanceUpcomingResponse } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  // core/maintenance.go L10-17: only `before` (RFC3339) and `overdue`
  // (bool) are accepted. Do NOT invent other params (PRMT-158 §6).
  const before = url.searchParams.get("before");
  if (before) qs.set("before", before);
  const overdue = url.searchParams.get("overdue");
  if (overdue) qs.set("overdue", overdue);
  const path =
    "/api/maintenance/upcoming" +
    (qs.toString() ? "?" + qs.toString() : "");

  const data: MaintenanceUpcomingResponse =
    await loadApi<MaintenanceUpcomingResponse>(path, s);

  return {
    items: Array.isArray(data.items) ? data.items : [],
    filters: { before: before ?? undefined, overdue: overdue ?? undefined },
  };
}

export default function MaintenanceRoute({ loaderData }: Route.ComponentProps) {
  const { items, filters } = loaderData;
  const hasFilter = Boolean(filters.before) || Boolean(filters.overdue);
    return (
    <OpsShell
      title={<>
          <span className="text-xl font-semibold">Maintenance</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-maintenance-header-count
          >
            {items.length}
          </span>
        </>}
      mainProps={{
        "data-maintenance-page": true,
        "data-maintenance-filter-before": filters.before ?? "",
        "data-maintenance-filter-overdue": filters.overdue ?? "",
        className: "max-w-5xl",
      }}
    >
      {hasFilter ? (
        <p
          className="text-xs uppercase tracking-wide text-muted-foreground"
          data-maintenance-active-filters
        >
          {filters.before ? `before=${filters.before}` : ""}
          {filters.before && filters.overdue ? " · " : ""}
          {filters.overdue ? `overdue=${filters.overdue}` : ""}
        </p>
      ) : null}
      <MaintenanceList items={items} />
    </OpsShell>
  );
}