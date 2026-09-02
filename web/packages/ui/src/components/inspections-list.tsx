/**
 * Presentational inspection template list (E3.5 — spec-009 §5
 * inspection entry point).
 *
 * Read-only — Phase A per spec-009 §8. No create/edit/delete, no
 * checklist submission, no fetch, no router data calls. Fed by the
 * `inspections.tsx` route loader, which already passed through the
 * mock seam or live `/api/inspections` and projected into the local
 * `Inspection` shape below.
 *
 * `Inspection` is re-declared locally (same pattern as
 * `tickets-table.tsx` / `alarms-table.tsx` / `maintenance-list.tsx`)
 * so the UI package has zero runtime deps on other workspace
 * packages (L88 UI package boundary). The route loader, which DOES depend
 * on the shared types package, projects into this local shape. Do NOT
 * add fields not in core.InspectionTemplate (core/store.go L222-230).
 *
 * The mobile checklist form (`/v1/inspections/form/{id}`, M2 P561)
 * is out of scope — desktop list view only (PRMT-160 §2).
 */

import type { JSX } from "react";

// Pinned to core.InspectionTemplate (core/store.go L222-230).
// Do NOT add fields not in core. PRMT-160 §4 hint listing
// `template/crn/due_at/status/ticket_ref` is a stale draft; core is
// the source of truth per the §4 "Do NOT invent" rule (same
// precedent as PRMT-158's maintenance-list.tsx L22-25 note).
interface Inspection {
  id: string;
  asset_path: string;
  title: string;
  items: string[];
  interval: number; // nanoseconds (time.Duration JSON encoding)
  next_due: string; // RFC3339
  enabled: boolean;
}

export interface InspectionsListProps {
  inspections: Inspection[];
  /** Optional cursor — present only if the upstream envelope carries
   *  a `next_page_token`. Core's listInspectionsResponse has none
   *  (M2 inspection scale is small — inspection.go L80), so this is
   *  typically undefined and the Next control is not rendered. */
  nextCursor?: string;
}

export function InspectionsList(props: InspectionsListProps): JSX.Element {
  const { inspections, nextCursor } = props;
  return (
    <div
      className="flex flex-col gap-3"
      data-inspections-ready
      data-inspections-count={inspections.length}
    >
      <table
        data-inspections-table
        className="w-full border-collapse text-sm"
      >
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">ID</th>
            <th className="px-2 py-1">Asset</th>
            <th className="px-2 py-1">Title</th>
            <th className="px-2 py-1">Next due</th>
            <th className="px-2 py-1">Items</th>
            <th className="px-2 py-1">Enabled</th>
          </tr>
        </thead>
        <tbody>
          {inspections.length === 0 ? (
            <tr data-inspections-empty>
              <td
                colSpan={6}
                className="px-2 py-3 text-center text-sm text-muted-foreground"
              >
                No inspection templates registered.
              </td>
            </tr>
          ) : (
            inspections.map((ins) => (
              <tr
                key={ins.id}
                data-inspection-row
                data-inspection-id={ins.id}
                data-inspection-enabled={ins.enabled ? "true" : "false"}
              >
                <td
                  className="px-2 py-1 font-mono"
                  data-inspection-cell="id"
                >
                  {ins.id}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-inspection-cell="asset"
                >
                  {ins.asset_path}
                </td>
                <td
                  className="px-2 py-1"
                  data-inspection-cell="title"
                >
                  {ins.title}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-inspection-cell="next_due"
                >
                  {ins.next_due}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-inspection-cell="items"
                >
                  {ins.items.length}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-inspection-cell="enabled"
                >
                  {ins.enabled ? "yes" : "no"}
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
      {/* Operate placeholder (M4 — disabled; PRMT-160 §6 MUST NOT). */}
      <div>
        <button
          type="button"
          disabled
          data-inspections-operate-placeholder
          className="rounded border px-3 py-1 text-sm opacity-50"
        >
          New inspection template (M4)
        </button>
      </div>
      {nextCursor ? (
        <div>
          <a
            data-next-page
            data-next-page-cursor={nextCursor}
            href={`?cursor=${encodeURIComponent(nextCursor)}`}
            className="text-sm text-muted-foreground hover:underline"
          >
            Next
          </a>
        </div>
      ) : null}
    </div>
  );
}