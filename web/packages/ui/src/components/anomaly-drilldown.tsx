/**
 * Presentational anomaly drilldown (R3 — spec-009 §5.2).
 *
 * Fed by `CauseAnalysis` + the target's `Alarm` projected in the route
 * loader. No fetch, no router, no operate/write path (Phase A is
 * read-only per spec-009 §8).
 *
 * Re-declares `Alarm`/`CauseAnalysis`/`EdgeKind` locally (the same
 * pattern as `object-inspector.tsx`) to keep the UI package free of
 * workspace runtime deps. The route loader, which DOES depend on
 * @cios/api-client, projects into this local shape.
 */

import type { JSX } from "react";

type EdgeKind = "feeds" | "cools" | "connects";
type AlarmSeverity = "info" | "warning" | "critical";
interface Alarm {
  crn: string;
  severity: AlarmSeverity;
  state: "firing" | "resolved";
  summary?: string;
}
export interface CauseAnalysis {
  target: string;
  rootCause?: { crn: string; via: EdgeKind };
  impact: { crn: string; via: EdgeKind }[];
}

export interface AnomalyDrilldownProps {
  analysis: CauseAnalysis;
  alarm?: Alarm;
}

export function AnomalyDrilldown(props: AnomalyDrilldownProps): JSX.Element {
  const { analysis, alarm } = props;
  return (
    <div
      className="flex flex-col gap-4"
      data-anomaly-drilldown
      data-target-crn={analysis.target}
    >
      <section
        className="rounded-md border bg-card p-4"
        data-alarm-summary
        data-alarm-severity={alarm?.severity ?? "none"}
      >
        <p className="text-xs uppercase tracking-wide text-muted-foreground">
          Alarm
        </p>
        <p className="mt-1 font-mono text-base">
          {alarm
            ? `${alarm.severity}${alarm.summary ? ` — ${alarm.summary}` : ""}`
            : "no firing alarm"}
        </p>
      </section>

      <section className="rounded-md border bg-card p-4" data-root-cause-section>
        <p className="text-xs uppercase tracking-wide text-muted-foreground">
          Root cause
        </p>
        <p
          className="mt-1 font-mono text-base"
          data-root-cause={analysis.rootCause?.crn ?? "indeterminate"}
        >
          {analysis.rootCause
            ? `${analysis.rootCause.crn} (via ${analysis.rootCause.via})`
            : "indeterminate"}
        </p>
      </section>

      <section className="rounded-md border bg-card p-4" data-impact-section>
        <p className="text-xs uppercase tracking-wide text-muted-foreground">
          Impact
        </p>
        {analysis.impact.length === 0 ? (
          <p className="mt-1 font-mono text-base" data-impact-empty>
            none
          </p>
        ) : (
          <ul className="mt-1 flex flex-col gap-1 text-sm font-mono">
            {analysis.impact.map((i) => (
              <li
                key={`${i.crn}:${i.via}`}
                data-impact-item
                data-impact-crn={i.crn}
                data-impact-via={i.via}
              >
                {i.crn} (via {i.via})
              </li>
            ))}
          </ul>
        )}
      </section>

      <div>
        <button
          type="button"
          data-operate-placeholder
          disabled
          className="rounded-md border bg-muted px-3 py-1 text-sm text-muted-foreground"
        >
          Operate (read-only)
        </button>
      </div>
    </div>
  );
}
