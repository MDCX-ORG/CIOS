/**
 * Presentational alarms queue (R3 — spec-009 §5.2 entry point).
 *
 * Read-only — Phase A per spec-009 §8. No ack/clear/operate, no fetch,
 * no router data calls. Fed by the `alarms.tsx` route loader, which
 * already passed through the mock seam or live `/api/alarms` (spec-004
 * `{items, next_page_token}` envelope) and projected to the local
 * `Alarm` shape below.
 *
 * `Alarm` / `AlarmSeverity` re-declared locally (same pattern as
 * `object-inspector.tsx` / `anomaly-drilldown.tsx`) so the UI package
 * has zero runtime deps on other workspace packages (UI package boundary).
 * The route loader, which DOES depend on `@cios/api-client`, projects
 * into this local shape.
 */

import type { JSX } from "react";

// spec-003 severity vocabulary — do NOT add levels (PRMT-143 §4.1).
type AlarmSeverity = "info" | "warning" | "critical";
interface Alarm {
  crn: string;
  severity: AlarmSeverity;
  state: "firing" | "resolved";
  summary?: string;
  id?: string;
  rawState?: string;
}

export interface AlarmsTableProps {
  alarms: Alarm[];
  /** = `next_page_token` when non-empty; renders a Next control. */
  nextCursor?: string;
  /** PRMT-230: per-row action cell (route injects the ack form). */
  renderRowAction?: (a: Alarm) => JSX.Element | null;
}

export function AlarmsTable(props: AlarmsTableProps): JSX.Element {
  const { alarms, nextCursor, renderRowAction } = props;
  return (
    <div
      className="flex flex-col gap-3"
      data-alarms-table-wrap
      data-alarms-count={alarms.length}
    >
      <table
        data-alarms-table
        className="w-full border-collapse text-sm"
      >
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">CRN</th>
            <th className="px-2 py-1">Severity</th>
            <th className="px-2 py-1">State</th>
            <th className="px-2 py-1">Summary</th>
            {renderRowAction ? <th className="px-2 py-1">Actions</th> : null}
          </tr>
        </thead>
        <tbody>
          {alarms.length === 0 ? (
            <tr data-alarms-empty>
              <td
                colSpan={renderRowAction ? 5 : 4}
                className="px-2 py-3 text-center text-sm text-muted-foreground"
              >
                No alarms match the current filters.
              </td>
            </tr>
          ) : (
            alarms.map((a) => (
              <tr
                key={a.crn}
                data-alarm-row
                data-severity={a.severity}
                data-state={a.state}
              >
                <td className="px-2 py-1 font-mono">
                  <a
                    href={`/noc/cause/${a.crn}`}
                    data-alarm-crn
                    className="hover:underline"
                  >
                    {a.crn}
                  </a>
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-alarm-cell="severity"
                >
                  {a.severity}
                </td>
                <td
                  className="px-2 py-1 font-mono"
                  data-alarm-cell="state"
                >
                  {a.state}
                </td>
                <td
                  className="px-2 py-1"
                  data-alarm-cell="summary"
                >
                  {a.summary ?? ""}
                </td>
                {renderRowAction ? (
                  <td
                    className="px-2 py-1"
                    data-alarm-cell="action"
                  >
                    {renderRowAction(a)}
                  </td>
                ) : null}
              </tr>
            ))
          )}
        </tbody>
      </table>
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
