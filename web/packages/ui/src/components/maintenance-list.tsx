/**
 * Presentational upcoming-maintenance list (E3.5 — spec-009 §5 PM
 * entry point).
 *
 * Read-only — Phase A per spec-009 §8. No edit/create, no fetch, no
 * router data calls. Fed by the `maintenance.tsx` route loader, which
 * already passed through the mock seam or live
 * `/api/maintenance/upcoming` and projected into the local
 * `MaintenanceItem` shape below.
 *
 * `MaintenanceItem` is re-declared locally (same pattern as
 * `tickets-table.tsx` / `alarms-table.tsx` / `capacity-panel.tsx`) so
 * the UI package has zero runtime deps on other workspace packages
 * (L88 UI package boundary). The route loader, which DOES depend on the
 * shared types package, projects into this local shape. Do NOT add
 * fields not in core.maintenanceUpcomingItem (core/maintenance.go
 * L34-41).
 */

import type { JSX } from "react";

// Pinned to core.maintenanceUpcomingItem (core/maintenance.go L34-41).
// Do NOT add fields not in core. ticket_ref does not exist on the
// wire shape — PRMT-158 §4's hint is a stale draft; core is the
// source of truth per the §4 "Do NOT invent" rule.
interface MaintenanceItem {
  kind: "pm" | "inspection";
  id: string;
  asset_path: string;
  title: string;
  next_due: string; // RFC3339
  overdue: boolean;
}

export interface MaintenanceListProps {
  items: MaintenanceItem[];
}

export function MaintenanceList(props: MaintenanceListProps): JSX.Element {
  const { items } = props;
  return (
    <div
      className="flex flex-col gap-3"
      data-maintenance-ready
      data-maintenance-count={items.length}
    >
      <table
        data-maintenance-table
        className="w-full border-collapse text-sm"
      >
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">Kind</th>
            <th className="px-2 py-1">Asset</th>
            <th className="px-2 py-1">Title</th>
            <th className="px-2 py-1">Next due</th>
            <th className="px-2 py-1">Status</th>
          </tr>
        </thead>
        <tbody>
          {items.length === 0 ? (
            <tr data-maintenance-empty>
              <td
                colSpan={5}
                className="px-2 py-3 text-center text-sm text-muted-foreground"
              >
                No upcoming maintenance reported by the gateway.
              </td>
            </tr>
          ) : (
            items.map((it) => (
              <tr
                key={`${it.kind}:${it.id}`}
                data-maintenance-row
                data-maintenance-kind={it.kind}
                data-maintenance-asset={it.asset_path}
                data-maintenance-overdue={it.overdue ? "true" : "false"}
              >
                <td
                  className="px-2 py-1 font-mono"
                  data-maintenance-cell="kind"
                >
                  {it.kind}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-maintenance-cell="asset"
                >
                  {it.asset_path}
                </td>
                <td
                  className="px-2 py-1"
                  data-maintenance-cell="title"
                >
                  {it.title}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-maintenance-cell="next_due"
                >
                  {it.next_due}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-maintenance-cell="status"
                >
                  {it.overdue ? "overdue" : "ok"}
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}