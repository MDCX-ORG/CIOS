/**
 * Presentational object data labels (R4 — spec-009 §5.3).
 *
 * Renders the four R4 keys (uptime / power / state / utilization) for the
 * focused crn as a grid of label rows. Fed by a `LabelSet` projected in the
 * route loader from `/api/metrics/query`. No fetch, no router.
 *
 * The component is **controlled** (optional `labels` prop) so PRMT-149 can
 * later push live SSE-updated values without restructuring. Field names
 * mirror spec-002 quantities (uptime/runhours, power, status/state,
 * util/utilization). Units come from spec-002 units.yaml — no invented
 * vocabulary.
 *
 * `LabelSet` / `LabelEntry` are declared locally and shape-mirror
 * `@cios/api-client`'s same-named types. The UI package has zero workspace
 * runtime deps (UI package boundary); the route loader (which DOES depend on
 * @cios/api-client) projects into this local shape. See object-inspector.tsx
 * for the established pattern.
 */

import type { JSX } from "react";

interface LabelEntry {
  value: string;
  unit?: string;
}
export interface LabelSet {
  crn: string;
  uptime?: LabelEntry;
  power?: LabelEntry;
  state?: LabelEntry;
  utilization?: LabelEntry;
}

export interface ObjectLabelsProps {
  labels?: LabelSet;
}

function fmt(entry: LabelEntry | undefined): string {
  if (!entry) return "—";
  return entry.unit ? `${entry.value} ${entry.unit}` : entry.value;
}

export function ObjectLabels(props: ObjectLabelsProps): JSX.Element {
  const { labels } = props;
  if (!labels) {
    return (
      <div
        data-labels-empty
        className="text-sm text-muted-foreground"
      >
        Select an object
      </div>
    );
  }
  return (
    <div
      className="flex flex-col gap-2 text-sm"
      data-labels-crn={labels.crn}
    >
      <div
        className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm"
        data-label="uptime"
      >
        <span className="text-muted-foreground">uptime</span>
        <span className="font-mono">{fmt(labels.uptime)}</span>
      </div>
      <div
        className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm"
        data-label="power"
      >
        <span className="text-muted-foreground">power</span>
        <span className="font-mono">{fmt(labels.power)}</span>
      </div>
      <div
        className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm"
        data-label="state"
      >
        <span className="text-muted-foreground">state</span>
        <span className="font-mono">{fmt(labels.state)}</span>
      </div>
      <div
        className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm"
        data-label="utilization"
      >
        <span className="text-muted-foreground">utilization</span>
        <span className="font-mono">{fmt(labels.utilization)}</span>
      </div>
    </div>
  );
}
