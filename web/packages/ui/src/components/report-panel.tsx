/**
 * Presentational ops report panel (E3.5 — spec-009 §5 reports entry
 * point).
 *
 * Read-only — Phase A per spec-009 §8. No write/operate, no fetch,
 * no router data calls. Fed by the `reports.tsx` route loader, which
 * already passed through the mock seam or live `/api/reports/ops`
 * (spec-004 wire shape mirroring core.opsReportResponse —
 * core/reports.go L31-38: mttr_seconds? / mean_response_seconds? /
 * mtbf_seconds? / ticket_counts / alarm_top[] / window).
 *
 * `OpsReport` is re-declared locally (same pattern as
 * `tickets-table.tsx` / `alarms-table.tsx` / `capacity-panel.tsx`)
 * so the UI package has zero runtime deps on other workspace
 * packages (L88 UI package boundary). The route loader, which DOES
 * depend on the shared api-client package, projects into this
 * local shape.
 *
 * Each metric row carries `data-report-metric="<name>"` so the
 * smoke test + future filter UI can target individual rows without
 * parsing the table.
 */

import type { JSX } from "react";

// Pinned to core.opsReportResponse JSON (core/reports.go L31-38).
// Optional float fields are nullable (null when empty per the
// "0 / null" rule); window is omitempty in core (may be absent).
// Do NOT add fields not in core.
interface OpsReport {
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

export interface ReportPanelProps {
  report: OpsReport;
}

function fmtSeconds(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  // Render seconds in the most natural unit; the wire is seconds.
  if (Math.abs(v) < 60) return `${v.toFixed(2)}s`;
  const m = v / 60;
  if (Math.abs(m) < 60) return `${m.toFixed(1)}m`;
  const h = m / 60;
  if (Math.abs(h) < 48) return `${h.toFixed(1)}h`;
  return `${(h / 24).toFixed(1)}d`;
}

export function ReportPanel(props: ReportPanelProps): JSX.Element {
  const { report } = props;
  const { mttr_seconds, mean_response_seconds, mtbf_seconds } = report;
  const topAlarms = report.alarm_top ?? [];
  return (
    <div
      className="flex flex-col gap-4"
      data-reports-ready
      data-report-mttr={mttr_seconds ?? ""}
      data-report-mean-response={mean_response_seconds ?? ""}
      data-report-mtbf={mtbf_seconds ?? ""}
      data-report-window-since={report.window?.since ?? ""}
    >
      <table
        data-report-metrics
        className="w-full border-collapse text-sm"
      >
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">Metric</th>
            <th className="px-2 py-1">Value</th>
          </tr>
        </thead>
        <tbody>
          <tr data-report-metric="mttr">
            <td className="px-2 py-1 font-mono">MTTR</td>
            <td className="px-2 py-1 font-mono" data-report-cell="mttr">
              {fmtSeconds(mttr_seconds)}
            </td>
          </tr>
          <tr data-report-metric="mean_response">
            <td className="px-2 py-1 font-mono">Mean response</td>
            <td
              className="px-2 py-1 font-mono"
              data-report-cell="mean_response"
            >
              {fmtSeconds(mean_response_seconds)}
            </td>
          </tr>
          <tr data-report-metric="mtbf">
            <td className="px-2 py-1 font-mono">MTBF</td>
            <td className="px-2 py-1 font-mono" data-report-cell="mtbf">
              {fmtSeconds(mtbf_seconds)}
            </td>
          </tr>
        </tbody>
      </table>

      <section data-report-top-alarms>
        <h2 className="mb-2 text-sm font-semibold">Top alarms</h2>
        {topAlarms.length === 0 ? (
          <p
            className="text-sm text-muted-foreground"
            data-report-top-empty
          >
            No alarm recurrences in this window.
          </p>
        ) : (
          <table
            data-report-top-table
            className="w-full border-collapse text-sm"
          >
            <thead>
              <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
                <th className="px-2 py-1">Asset</th>
                <th className="px-2 py-1">Count</th>
              </tr>
            </thead>
            <tbody>
              {topAlarms.map((a) => (
                <tr
                  key={a.path}
                  data-report-top-row
                  data-report-top-path={a.path}
                >
                  <td
                    className="px-2 py-1 font-mono"
                    data-report-top-cell="path"
                  >
                    {a.path}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-report-top-cell="count"
                  >
                    {a.count}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}