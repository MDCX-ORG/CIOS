/**
 * /usage — usage facts page (E3.2 / L102 · 对量).
 *
 *   - Fetches /api/usage via mock or live loadApi; forwards kind /
 *     granularity / period_* query params (PRMT-196).
 *   - Read-only. No money, no invoice UI.
 */

import type { Route } from "./+types/usage";
import type { UsageRecord } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const url = new URL(request.url);
  const qs = new URLSearchParams();
  for (const k of ["kind", "granularity", "period_start", "period_end"] as const) {
    const v = url.searchParams.get(k);
    if (v) qs.set(k, v);
  }
  const path = "/api/usage" + (qs.toString() ? `?${qs}` : "");
  const data = await loadApi<{ items: UsageRecord[] }>(path, s);
  const items = Array.isArray(data.items) ? data.items : [];
  return {
    items,
    kind: url.searchParams.get("kind") ?? "",
    granularity: url.searchParams.get("granularity") ?? "",
  };
}

function filterHref(kind: string, granularity: string): string {
  const p = new URLSearchParams();
  if (kind) p.set("kind", kind);
  if (granularity) p.set("granularity", granularity);
  const s = p.toString();
  return s ? `/usage?${s}` : "/usage";
}

export default function UsageRoute({ loaderData }: Route.ComponentProps) {
  const { items, kind, granularity } = loaderData;
  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">Usage</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-usage-header-count
          >
            {items.length}
          </span>
        </>
      }
      mainProps={{
        "data-usage-page": true,
        "data-usage-ready": true,
        className: "max-w-5xl",
      }}
    >
      <p className="text-sm text-muted-foreground" data-usage-blurb>
        Usage facts for 对量 — energy (kWh) and rack-hour. No billing.
      </p>
      <nav
        className="mb-3 flex flex-wrap gap-2 text-sm"
        data-usage-filters
        aria-label="Usage filters"
      >
        {(
          [
            ["", "all"],
            ["energy", "energy"],
            ["rack_hour", "rack_hour"],
          ] as const
        ).map(([k, label]) => (
          <a
            key={label}
            href={filterHref(k, granularity)}
            className="rounded border px-2 py-1 hover:border-primary"
            data-usage-filter-kind={label}
            data-usage-filter-active={kind === k ? "true" : "false"}
          >
            {label}
          </a>
        ))}
        <span className="mx-1 text-muted-foreground">|</span>
        {(
          [
            ["", "any"],
            ["monthly", "monthly"],
            ["daily", "daily"],
          ] as const
        ).map(([g, label]) => (
          <a
            key={label}
            href={filterHref(kind, g)}
            className="rounded border px-2 py-1 hover:border-primary"
            data-usage-filter-granularity={label}
          >
            {label}
          </a>
        ))}
      </nav>
      <div className="overflow-x-auto rounded-lg border bg-card shadow-sm">
        <table className="w-full text-left text-sm" data-usage-table>
          <thead className="text-xs uppercase text-muted-foreground">
            <tr>
              <th className="px-3 py-2">Kind</th>
              <th className="px-3 py-2">Asset</th>
              <th className="px-3 py-2">Period</th>
              <th className="px-3 py-2">Granularity</th>
              <th className="px-3 py-2">Quantity</th>
              <th className="px-3 py-2">Tenant</th>
              <th className="px-3 py-2">Org</th>
            </tr>
          </thead>
          <tbody>
            {items.length === 0 ? (
              <tr data-usage-empty>
                <td
                  colSpan={7}
                  className="px-3 py-6 text-center text-muted-foreground"
                >
                  No usage records
                </td>
              </tr>
            ) : (
              items.map((row) => (
                <tr
                  key={row.id}
                  className="border-t"
                  data-usage-row
                  data-usage-kind={row.kind}
                  data-usage-id={row.id}
                >
                  <td className="px-3 py-2 font-mono text-xs">{row.kind}</td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {row.asset_path}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-muted-foreground">
                    {row.period_start} → {row.period_end}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {row.granularity}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {row.quantity} {row.unit}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-muted-foreground">
                    {row.tenant_id}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-muted-foreground">
                    {row.org_id}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </OpsShell>
  );
}
