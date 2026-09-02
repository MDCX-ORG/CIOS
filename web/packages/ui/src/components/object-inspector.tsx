/**
 * Presentational object inspector (R8 — spec-009 §4.2/§5.6).
 *
 * Fed by `InspectorFields` projected in the route loader. No fetch, no router.
 * Phase A: the `action` control is a read-only placeholder (spec-009 §8).
 * Quantity field names mirror spec-002 units (celsius / kpa / lpm).
 *
 * `Alarm`/`TempPort`/`InspectorFields` are declared locally and shape-mirror
 * `@cios/api-client`'s same-named types. The UI package has zero workspace
 * runtime deps (UI package boundary); the route loader (which DOES depend on
 * @cios/api-client) projects into this local shape. See asset-tree.tsx
 * for the established pattern.
 */

import type { JSX } from "react";

type AlarmSeverity = "info" | "warning" | "critical";
interface Alarm {
  crn: string;
  severity: AlarmSeverity;
  state: "firing" | "resolved";
  summary?: string;
}
interface TempPort {
  temp_c?: number;
  press_kpa?: number;
  flow_lpm?: number;
}
export interface InspectorFields {
  id: string;
  name?: string;
  status: string;
  inlet?: TempPort;
  outlet?: TempPort;
  alarm?: Alarm;
}

export interface ObjectInspectorProps {
  fields?: InspectorFields;
}

function fmt(value: number | undefined, suffix: string): string {
  return value === undefined ? "—" : `${value} ${suffix}`;
}

function PortRow({ label, port }: { label: string; port?: TempPort }): JSX.Element {
  return (
    <div className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="font-mono">
        {fmt(port?.temp_c, "°C")} · {fmt(port?.press_kpa, "kPa")} ·{" "}
        {fmt(port?.flow_lpm, "L/min")}
      </span>
    </div>
  );
}

function AlarmRow({ alarm }: { alarm?: Alarm }): JSX.Element {
  if (!alarm) {
    return (
      <div className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm">
        <span className="text-muted-foreground">Alarm</span>
        <span className="font-mono">—</span>
      </div>
    );
  }
  return (
    <div
      className="grid grid-cols-[6rem_1fr] gap-x-2 text-sm"
      data-alarm-row
      data-alarm-severity={alarm.severity}
      data-alarm-state={alarm.state}
    >
      <span className="text-muted-foreground">Alarm</span>
      <span className="font-mono">
        {alarm.severity}
        {alarm.summary ? ` — ${alarm.summary}` : ""}
      </span>
    </div>
  );
}

export function ObjectInspector(props: ObjectInspectorProps): JSX.Element {
  const { fields } = props;
  if (!fields) {
    return (
      <div
        data-inspector-empty
        className="text-sm text-muted-foreground"
      >
        Select an object
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-2 text-sm" data-inspector-crn={fields.id}>
      <div className="grid grid-cols-[6rem_1fr] gap-x-2">
        <span className="text-muted-foreground">ID</span>
        <span className="font-mono" data-inspector-id>{fields.id}</span>
      </div>
      <div className="grid grid-cols-[6rem_1fr] gap-x-2">
        <span className="text-muted-foreground">Name</span>
        <span className="font-mono">{fields.name ?? "—"}</span>
      </div>
      <div className="grid grid-cols-[6rem_1fr] gap-x-2">
        <span className="text-muted-foreground">Status</span>
        <span className="font-mono" data-inspector-status>{fields.status}</span>
      </div>
      <PortRow label="Inlet" port={fields.inlet} />
      <PortRow label="Outlet" port={fields.outlet} />
      <AlarmRow alarm={fields.alarm} />
      <div className="pt-2">
        <button
          type="button"
          data-action-placeholder
          disabled
          className="rounded-md border bg-muted px-3 py-1 text-sm text-muted-foreground"
        >
          Action (read-only)
        </button>
      </div>
    </div>
  );
}