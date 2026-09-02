/**
 * Presentational site info chart (R5 — spec-009 §5.4).
 *
 * Renders three site-level series — Facility Power (W), IT Power (W), and
 * PUE (dimensionless, site-level only per L48) — as inline SVG polylines
 * inside the `data-slot="site-chart"` element. Dependency-free (no
 * charting library) per PRMT-140 §4.7 allowlist and PRMT-145 §6.
 *
 * `SiteSeries` is locally re-declared and shape-mirrors `@cios/api-client`'s
 * same-named type. The route loader (which DOES depend on @cios/api-client)
 * projects into this local shape. This mirrors the ObjectInspector /
 * ObjectLabels pattern and avoids adding `@cios/api-client` as a workspace
 * dep to `packages/ui` (see §8 — spec-145 §4.2 imports the type directly,
 * but `packages/ui/package.json` is OUT of the §3 whitelist, so the import
 * cannot be added in this PRMT).
 *
 * Component is presentational only (no fetch, no router).
 */

import type { JSX } from "react";

export interface SeriesPoint {
  /** Unix seconds (Prometheus matrix value[0]). */
  t: number;
  /** Numeric sample (Prometheus matrix value[1] parsed). */
  v: number;
}

export interface SiteSeries {
  site: string;
  /** Facility power, watts (spec-002). */
  facility_power: SeriesPoint[];
  /** IT power, watts (spec-002). */
  it_power: SeriesPoint[];
  /** PUE, dimensionless; site-level only (L48). */
  pue: SeriesPoint[];
}

export interface SiteChartProps {
  series?: SiteSeries;
}

interface CurveSpec {
  key: "facility_power" | "it_power" | "pue";
  label: string;
  /** Site-level flag drives the "(site)" suffix on the PUE legend only. */
  siteOnly?: boolean;
  /** Tailwind/CSS variable color from globals.css — no invented tokens. */
  stroke: string;
  /** Unit suffix for the data-end label. */
  unit: string;
}

const CURVES: readonly CurveSpec[] = [
  { key: "facility_power", label: "Facility Power", stroke: "var(--chart-1, #6366f1)", unit: "W" },
  { key: "it_power", label: "IT Power", stroke: "var(--chart-2, #10b981)", unit: "W" },
  { key: "pue", label: "PUE", siteOnly: true, stroke: "var(--chart-3, #f59e0b)", unit: "" },
];

const VIEW_W = 320;
const VIEW_H = 96;
const PAD_X = 4;
const PAD_Y = 8;

function valueExt(points: SeriesPoint[]): { vMin: number; vMax: number } | null {
  const [head, ...rest] = points;
  if (head === undefined) return null;
  let vMin = head.v;
  let vMax = head.v;
  for (const p of rest) {
    if (p.v < vMin) vMin = p.v;
    if (p.v > vMax) vMax = p.v;
  }
  return { vMin, vMax };
}

function buildPath(
  points: SeriesPoint[],
  tMin: number,
  tMax: number,
  ext: { vMin: number; vMax: number } | null,
): string {
  if (points.length === 0 || ext === null) return "";
  const span = (a: number, b: number) => (b - a === 0 ? 0.5 : b - a);
  const w = VIEW_W - PAD_X * 2;
  const h = VIEW_H - PAD_Y * 2;
  return points
    .map((p, i) => {
      const x = PAD_X + ((p.t - tMin) / span(tMin, tMax)) * w;
      const y = PAD_Y + (1 - (p.v - ext.vMin) / span(ext.vMin, ext.vMax)) * h;
      return `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");
}

/** Strip the "M"/"L" SVG path prefix and emit "x,y x,y …" for <polyline points>. */
function toPolyPoints(path: string): string {
  return path
    .split(" ")
    .filter(Boolean)
    .map((seg) => seg.slice(1))
    .join(" ");
}

function isEmpty(s: SiteSeries | undefined): boolean {
  if (!s) return true;
  return s.facility_power.length === 0 && s.it_power.length === 0 && s.pue.length === 0;
}

function dataExtent(points: SeriesPoint[]): { tMin: number; tMax: number; vMin: number; vMax: number } | null {
  const [head, ...rest] = points;
  if (head === undefined) return null;
  let tMin = head.t;
  let tMax = head.t;
  let vMin = head.v;
  let vMax = head.v;
  for (const p of rest) {
    if (p.t < tMin) tMin = p.t;
    if (p.t > tMax) tMax = p.t;
    if (p.v < vMin) vMin = p.v;
    if (p.v > vMax) vMax = p.v;
  }
  return { tMin, tMax, vMin, vMax };
}

export function SiteChart(props: SiteChartProps): JSX.Element {
  const { series } = props;
  const [CURVE_FP, CURVE_IP, CURVE_PUE] = CURVES as readonly [
    CurveSpec,
    CurveSpec,
    CurveSpec,
  ];

  if (!series) {
    return (
      <div data-chart-empty className="text-sm text-muted-foreground">
        No site telemetry
      </div>
    );
  }
  if (isEmpty(series)) {
    return (
      <div data-chart-empty className="text-sm text-muted-foreground">
        No site telemetry
      </div>
    );
  }

  // Compute a shared time extent across all three series so the polylines
  // align horizontally on the same axis.
  const all: SeriesPoint[] = [
    ...series.facility_power,
    ...series.it_power,
    ...series.pue,
  ];
  const ext = dataExtent(all);
  if (!ext) {
    return (
      <div data-chart-empty className="text-sm text-muted-foreground">
        No site telemetry
      </div>
    );
  }
  const tMin = ext.tMin;
  const tMax = ext.tMax === ext.tMin ? ext.tMin + 1 : ext.tMax;

  const pointsByKey: Record<CurveSpec["key"], SeriesPoint[]> = {
    facility_power: series.facility_power,
    it_power: series.it_power,
    pue: series.pue,
  };

  return (
    <div className="flex flex-col gap-2 text-sm" data-site-chart data-site={series.site}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
        {CURVES.map((c) => (
          <span key={c.key} className="inline-flex items-center gap-1">
            <span
              aria-hidden="true"
              className="inline-block h-2 w-3 rounded-sm"
              style={{ backgroundColor: c.stroke }}
            />
            <span>
              {c.label}
              {c.siteOnly ? " (site)" : ""}
            </span>
          </span>
        ))}
      </div>
      <svg
        role="img"
        aria-label={`Site info chart for ${series.site}`}
        viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
        className="block h-24 w-full"
        data-site-chart-svg
      >
        <polyline
          data-series="facility_power"
          data-site-only="false"
          fill="none"
          stroke={CURVE_FP.stroke}
          strokeWidth={1.5}
          strokeLinejoin="round"
          strokeLinecap="round"
          points={toPolyPoints(buildPath(pointsByKey.facility_power, tMin, tMax, valueExt(pointsByKey.facility_power)))}
        />
        <polyline
          data-series="it_power"
          data-site-only="false"
          fill="none"
          stroke={CURVE_IP.stroke}
          strokeWidth={1.5}
          strokeLinejoin="round"
          strokeLinecap="round"
          points={toPolyPoints(buildPath(pointsByKey.it_power, tMin, tMax, valueExt(pointsByKey.it_power)))}
        />
        <polyline
          data-series="pue"
          data-site-only="true"
          fill="none"
          stroke={CURVE_PUE.stroke}
          strokeWidth={1.5}
          strokeLinejoin="round"
          strokeLinecap="round"
          points={toPolyPoints(buildPath(pointsByKey.pue, tMin, tMax, valueExt(pointsByKey.pue)))}
        />
      </svg>
      <div className="grid grid-cols-3 gap-x-3 text-xs">
        {CURVES.map((c) => {
          const pts = pointsByKey[c.key];
          const last = pts.length > 0 ? pts[pts.length - 1] : undefined;
          return (
            <div key={c.key} className="flex items-baseline gap-1">
              <span className="text-muted-foreground">{c.label}</span>
              <span className="font-mono" data-series-end={c.key}>
                {last ? `${last.v}${c.unit ? ` ${c.unit}` : ""}` : "—"}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}