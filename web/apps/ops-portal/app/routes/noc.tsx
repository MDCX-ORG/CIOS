/**
 * /noc — three-pane NOC shell (Phase A list/tree; spec-009 §1 R1 + §5.1).
 *
 * Live assets project path/spec → crn/type so AC45/DC45 pods appear in the tree.
 */

import { useCallback } from "react";
import { useLoaderData, useNavigate } from "react-router";

import type { Route } from "./+types/noc";
import {
  AssetTree,
  ObjectInspector,
  ObjectLabels,
  SiteChart,
  SiteSwitcher,
} from "@cios/ui";
import type {
  AlarmSeverity,
  InspectorFields,
  LabelSet,
  MetricsQueryResponse,
  MetricsRangeResponse,
  SeriesPoint,
  SiteListProjection,
  SiteOption,
  SiteSeries,
} from "@cios/api-client";

import { OpsShell } from "~/components/ops-shell";
import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import {
  loadAllPortalAssets,
  projectAlarm,
  type PortalAsset,
} from "~/lib/project-assets";
import { useTelemetryStream } from "~/lib/telemetry";

const SEVERITY_RANK: Record<AlarmSeverity, number> = {
  info: 1,
  warning: 2,
  critical: 3,
};

function worstSeverity(
  current: AlarmSeverity | undefined,
  next: AlarmSeverity | undefined,
): AlarmSeverity | undefined {
  if (!current) return next;
  if (!next) return current;
  return SEVERITY_RANK[next] > SEVERITY_RANK[current] ? next : current;
}

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const url = new URL(request.url);
  const focus = url.searchParams.get("focus") ?? undefined;
  const siteParam = url.searchParams.get("site") ?? undefined;

  const assets: PortalAsset[] = await loadAllPortalAssets(s);

  const alarmsRaw = await loadApi<{ items?: unknown[] }>("/api/alarms", s);
  const alarms = (alarmsRaw.items ?? [])
    .map(projectAlarm)
    .filter((a): a is NonNullable<typeof a> => a != null);

  const siteAssets = assets.filter((a) => a.type === "site");
  const firingBySite = new Map<string, AlarmSeverity | undefined>();
  for (const alarm of alarms) {
    if (alarm.state !== "firing") continue;
    const siteId = alarm.crn.split(".")[0];
    if (!siteId) continue;
    firingBySite.set(
      siteId,
      worstSeverity(firingBySite.get(siteId), alarm.severity),
    );
  }
  const sites: SiteOption[] = siteAssets
    .map((a): SiteOption => {
      const opt: SiteOption = { site: a.crn, name: a.name };
      const sev = firingBySite.get(a.crn);
      if (sev !== undefined) opt.worstSeverity = sev;
      return opt;
    })
    .sort((a, b) => a.site.localeCompare(b.site));
  const firstSite = sites[0]?.site;
  const active =
    siteParam && sites.some((s) => s.site === siteParam)
      ? siteParam
      : firstSite;
  const siteList: SiteListProjection = { sites, active };

  // Filter tree to active site when set (keeps tree usable at fleet scale).
  const treeAssets = active
    ? assets.filter(
        (a) => a.crn === active || a.crn.startsWith(active + "."),
      )
    : assets;

  let inspector: InspectorFields | undefined;
  if (focus) {
    const asset = assets.find((a) => a.crn === focus);
    const firing = alarms.find(
      (a) => a.crn === focus && a.state === "firing",
    );
    const nameParts = [
      asset?.name,
      asset?.model ? `(${asset.model})` : undefined,
    ].filter(Boolean);
    inspector = {
      id: focus,
      name: nameParts.length ? nameParts.join(" ") : asset?.name,
      status: firing ? "alarm" : "ok",
      alarm: firing ?? undefined,
    };
  }

  let labels: LabelSet | undefined;
  if (focus) {
    labels = { crn: focus };
    try {
      const metricsPath =
        "/api/metrics/query?query=" + encodeURIComponent(`crn="${focus}"`);
      const metrics: MetricsQueryResponse = await loadApi<MetricsQueryResponse>(
        metricsPath,
        s,
      );
      for (const sample of metrics.data?.result ?? []) {
        const name = sample.metric?.__name__ ?? "";
        const raw = sample.value?.[1];
        if (typeof raw !== "string") continue;
        if (name === "cios_power_w") {
          labels.power = { value: raw, unit: "W" };
        } else if (name === "cios_utilization_ratio") {
          labels.utilization = { value: raw, unit: "ratio" };
        } else if (name === "cios_uptime_s") {
          labels.uptime = { value: raw, unit: "s" };
        } else if (name === "cios_state") {
          labels.state = { value: raw };
        }
      }
    } catch {
      // Live apigw may not expose metrics yet — fall through to CMDB labels.
    }

    // Enrich labels with static CMDB fields when metrics are empty (live seed).
    const asset = assets.find((a) => a.crn === focus);
    if (asset?.model && !labels.state) {
      labels.state = { value: asset.model };
    }
    if (asset?.rated_power_kw != null && !labels.power) {
      labels.power = {
        value: String(asset.rated_power_kw * 1000),
        unit: "W",
      };
    }
  }

  let siteSeries: SiteSeries | undefined;
  const siteFromParam = active;
  const siteFromFocus = focus ? focus.split(".")[0] : undefined;
  const siteRoot =
    siteFromParam ??
    siteFromFocus ??
    assets.find((a) => a.type === "site")?.crn ??
    assets[0]?.crn;
  if (siteRoot) {
    try {
      const rangePath =
        "/api/metrics/query_range?query=" +
        encodeURIComponent(`site="${siteRoot}"`);
      const range: MetricsRangeResponse = await loadApi<MetricsRangeResponse>(
        rangePath,
        s,
      );
      const toPoints = (
        vals: [number, string][] | undefined,
      ): SeriesPoint[] => {
        if (!vals) return [];
        const out: SeriesPoint[] = [];
        for (const v of vals) {
          const n = Number(v[1]);
          if (Number.isFinite(n) && Number.isFinite(v[0]))
            out.push({ t: v[0], v: n });
        }
        return out;
      };
      const fp: SeriesPoint[] = [];
      const ip: SeriesPoint[] = [];
      const pue: SeriesPoint[] = [];
      for (const m of range.data?.result ?? []) {
        const name = m.metric?.__name__ ?? "";
        if (name === "cios_facility_power_w") {
          for (const p of toPoints(m.values)) fp.push(p);
        } else if (name === "cios_it_power_w") {
          for (const p of toPoints(m.values)) ip.push(p);
        } else if (name === "cios_pue") {
          for (const p of toPoints(m.values)) pue.push(p);
        }
      }
      siteSeries = { site: siteRoot, facility_power: fp, it_power: ip, pue };
    } catch {
      // No metrics backend on this stack — empty chart is fine.
      siteSeries = {
        site: siteRoot,
        facility_power: [],
        it_power: [],
        pue: [],
      };
    }
  }

  const podModels = treeAssets
    .filter((a) => a.type === "pod")
    .map((a) => ({ crn: a.crn, model: a.model ?? "—" }));

  return {
    assets: treeAssets,
    focus,
    inspector,
    labels,
    siteSeries,
    siteList,
    podModels,
    alarmCount: alarms.filter((a) => a.state === "firing").length,
  };
}

export default function Noc() {
  const {
    assets,
    focus,
    inspector,
    labels,
    siteSeries,
    siteList,
    podModels,
    alarmCount,
  } = useLoaderData<typeof loader>();
  const navigate = useNavigate();

  const stream = useTelemetryStream(siteList.active);
  const liveLabels = (() => {
    if (!focus || !labels) return labels;
    const liveQty = stream.samples[focus];
    if (!liveQty) return labels;
    let changed = false;
    const merged: LabelSet = { ...labels };
    for (const q of Object.keys(liveQty) as (keyof LabelSet)[]) {
      if (q === "crn") continue;
      const v = liveQty[q as string];
      if (typeof v !== "number" || !Number.isFinite(v)) continue;
      const existing = merged[q];
      merged[q] = {
        value: String(v),
        ...(existing?.unit ? { unit: existing.unit } : {}),
      };
      changed = true;
    }
    return changed ? merged : labels;
  })();

  const handleSelect = useCallback(
    (crn: string) => {
      const params = new URLSearchParams();
      if (siteList.active) params.set("site", siteList.active);
      if (crn !== focus) params.set("focus", crn);
      const qs = params.toString();
      navigate(qs ? `/noc?${qs}` : "/noc");
    },
    [focus, navigate, siteList.active],
  );

  const handleSiteSelect = useCallback(
    (site: string) => {
      const params = new URLSearchParams();
      params.set("site", site);
      if (focus) params.set("focus", focus);
      navigate(`/noc?${params.toString()}`);
    },
    [focus, navigate],
  );

  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">NOC — Asset Hierarchy</span>
          <SiteSwitcher projection={siteList} onSelect={handleSiteSelect} />
        </>
      }
      mainProps={{ "data-noc-ready": true }}
    >
      {podModels.length > 0 ? (
        <section
          className="flex flex-wrap gap-2 text-sm"
          data-noc-pod-models
          aria-label="Pods at site"
        >
          {podModels.map((p) => (
            <a
              key={p.crn}
              href={`/noc?site=${encodeURIComponent(siteList.active ?? "")}&focus=${encodeURIComponent(p.crn)}`}
              className="rounded border bg-card px-2 py-1 font-mono text-xs hover:border-primary"
              data-noc-pod-chip
              data-pod-model={p.model}
            >
              <span className="font-semibold text-primary">{p.model}</span>
              <span className="text-muted-foreground"> · {p.crn}</span>
            </a>
          ))}
          <span
            className="rounded border bg-card px-2 py-1 text-xs text-muted-foreground"
            data-noc-firing-count
          >
            firing alarms: {alarmCount}
          </span>
        </section>
      ) : null}

      <div
        className="grid gap-4 md:grid-cols-[20rem_1fr]"
        data-noc-grid
      >
        <AssetTree assets={assets} focus={focus} onSelect={handleSelect} />

        <section className="flex flex-col gap-4">
          <header
            className="rounded-md border bg-card p-4"
            data-focus-header
            data-focus-crn={focus ?? ""}
          >
            <p className="text-xs uppercase tracking-wide text-muted-foreground">
              Focus
            </p>
            <p className="mt-1 font-mono text-base" data-testid="focus-crn">
              {focus ?? "—"}
            </p>
          </header>

          <aside
            data-slot="inspector"
            aria-label="Object inspector"
            className="rounded-md border bg-card p-4 min-h-[8rem] text-sm text-muted-foreground"
          >
            <ObjectInspector fields={inspector} />
            {focus && inspector?.alarm ? (
              <p className="mt-2 text-sm">
                <a
                  href={`/noc/cause/${encodeURIComponent(focus)}`}
                  data-view-cause
                  data-view-cause-crn={focus}
                  className="text-primary hover:underline"
                >
                  View cause
                </a>
              </p>
            ) : null}
          </aside>

          <section
            data-slot="labels"
            aria-label="Object data labels"
            className="rounded-md border bg-card p-4 min-h-[6rem] text-sm text-muted-foreground"
          >
            <ObjectLabels labels={liveLabels} />
          </section>

          <section
            data-slot="site-chart"
            aria-label="Site info chart"
            className="rounded-md border bg-card p-4 min-h-[10rem] text-sm text-muted-foreground"
          >
            <SiteChart series={siteSeries} />
          </section>
        </section>
      </div>
    </OpsShell>
  );
}
