/**
 * /assets — CMDB table (live + mock).
 * Shows path/type/model/serial/power so AC45/DC45 pods are visible.
 */
import type { Route } from "./+types/assets";
import { AssetsTable } from "@cios/ui";

import { OpsShell } from "~/components/ops-shell";
import { requireSession } from "~/lib/auth.server";
import { loadAllPortalAssets } from "~/lib/project-assets";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const url = new URL(request.url);
  const typeF = url.searchParams.get("type") ?? undefined;
  const modelF = (url.searchParams.get("model") ?? "").trim();
  const q = (url.searchParams.get("q") ?? "").trim().toLowerCase();

  let assets = await loadAllPortalAssets(s);
  if (typeF) {
    assets = assets.filter((a) => a.type === typeF);
  }
  if (modelF) {
    const m = modelF.toLowerCase();
    assets = assets.filter((a) => (a.model ?? "").toLowerCase().includes(m));
  }
  if (q) {
    assets = assets.filter((a) => {
      const hay =
        `${a.crn} ${a.name ?? ""} ${a.type} ${a.model ?? ""} ${a.serial ?? ""}`.toLowerCase();
      return hay.includes(q);
    });
  }

  const pods = assets.filter((a) => a.type === "pod");
  const modelSummary = pods.reduce<Record<string, number>>((acc, p) => {
    const m = p.model ?? "unknown";
    acc[m] = (acc[m] ?? 0) + 1;
    return acc;
  }, {});

  return {
    assets,
    filters: { type: typeF, model: modelF || undefined, q: q || undefined },
    stats: {
      total: assets.length,
      pods: pods.length,
      modelSummary,
    },
  };
}

export default function AssetsRoute({ loaderData }: Route.ComponentProps) {
  const { assets, filters, stats } = loaderData;
  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">Assets (CMDB)</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-assets-header-count
          >
            {stats.total}
          </span>
        </>
      }
      mainProps={{
        "data-assets-page": true,
        "data-assets-ready": true,
        "data-assets-filter-type": filters.type ?? "",
        "data-assets-filter-model": filters.model ?? "",
        "data-assets-filter-q": filters.q ?? "",
        className: "max-w-6xl",
      }}
    >
      <section
        className="flex flex-wrap gap-3 text-sm"
        data-assets-stats
        aria-label="Asset summary"
      >
        <div className="rounded border bg-card px-3 py-2" data-assets-stat-pods>
          <span className="text-muted-foreground">Pods </span>
          <span className="font-mono font-semibold">{stats.pods}</span>
        </div>
        {Object.entries(stats.modelSummary).map(([model, n]) => (
          <div
            key={model}
            className="rounded border bg-card px-3 py-2"
            data-assets-stat-model={model}
          >
            <span className="font-semibold text-primary">{model}</span>
            <span className="text-muted-foreground"> × </span>
            <span className="font-mono">{n}</span>
          </div>
        ))}
      </section>

      <form
        method="get"
        className="flex flex-wrap items-end gap-3"
        data-assets-filter-form
      >
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Type
          <input
            name="type"
            defaultValue={filters.type ?? ""}
            placeholder="pod, rack, cdu…"
            className="rounded border bg-background px-2 py-1 font-mono text-sm text-foreground"
            data-assets-filter-type-input
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Model
          <input
            name="model"
            defaultValue={filters.model ?? ""}
            placeholder="DC45, AC45…"
            className="rounded border bg-background px-2 py-1 font-mono text-sm text-foreground"
            data-assets-filter-model-input
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Search
          <input
            name="q"
            defaultValue={filters.q ?? ""}
            placeholder="path / serial"
            className="rounded border bg-background px-2 py-1 font-mono text-sm text-foreground"
            data-assets-filter-q-input
          />
        </label>
        <button
          type="submit"
          className="rounded bg-primary px-3 py-1.5 text-sm text-primary-foreground"
          data-assets-filter-submit
        >
          Apply
        </button>
        <a
          href="/assets?model=DC45"
          className="text-sm text-primary underline"
          data-assets-quick-dc45
        >
          DC45
        </a>
        <a
          href="/assets?model=AC45"
          className="text-sm text-primary underline"
          data-assets-quick-ac45
        >
          AC45
        </a>
        <a
          href="/assets?type=pod"
          className="text-sm text-muted-foreground underline"
        >
          pods only
        </a>
      </form>

      <AssetsTable assets={assets} />
    </OpsShell>
  );
}
