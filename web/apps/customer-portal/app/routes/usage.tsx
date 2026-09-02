/**
 * Customer Usage (对量) — read-only energy / rack-hour facts.
 *
 * Live: GET {CUSTOMER_API_BASE}/api/customer/usage (tenant forced by gateway).
 * Explicit error state when API unreachable (PRMT-229; no mock rows).
 * No money, invoice, or Pricing UI (L102 Commercial B parked).
 */
import { useLoaderData } from "react-router";

import type { Route } from "./+types/usage";
import { CustomerShell } from "~/components/customer-shell";
import { customerApiBase, requireSession } from "~/lib/auth.server";

type UsageItem = {
  id: string;
  kind?: string;
  tenant_id?: string;
  site_id?: string;
  asset_path?: string;
  period_start?: string;
  period_end?: string;
  granularity?: string;
  quantity?: number;
  unit?: string;
};

type UsagePayload = {
  items: UsageItem[];
  tenant_id?: string;
  note?: string;
};

export async function loader({ request }: Route.LoaderArgs) {
  const session = await requireSession(request);
  const base = customerApiBase().replace(/\/$/, "");
  const url = new URL(request.url);
  const kind = url.searchParams.get("kind") ?? "";
  const granularity = url.searchParams.get("granularity") ?? "";

  const qs = new URLSearchParams();
  if (kind) qs.set("kind", kind);
  if (granularity) qs.set("granularity", granularity);

  let payload: UsagePayload | null = null;
  let fetchError: string | null = null;

  try {
    const res = await fetch(
      `${base}/api/customer/usage` + (qs.toString() ? `?${qs}` : ""),
      {
        headers: {
          Accept: "application/json",
          Authorization: `Bearer ${session.bearer ?? ""}`,
          "X-CIOS-Tenant": session.tenantId,
        },
        signal: AbortSignal.timeout(4000),
      },
    );
    if (res.ok) {
      payload = (await res.json()) as UsagePayload;
      if (!Array.isArray(payload.items)) {
        payload.items = [];
      }
    } else {
      fetchError = `API ${res.status}`;
    }
  } catch (e) {
    fetchError = e instanceof Error ? e.message : "fetch failed";
  }

  if (!payload) {
    payload = { tenant_id: session.tenantId, items: [] };
  }

  return {
    session,
    payload,
    fetchError,
    apiBase: base,
    kind,
    granularity,
  };
}

function filterHref(kind: string, granularity: string): string {
  const p = new URLSearchParams();
  if (kind) p.set("kind", kind);
  if (granularity) p.set("granularity", granularity);
  const s = p.toString();
  return s ? `/usage?${s}` : "/usage";
}

export default function UsagePage() {
  const data = useLoaderData<typeof loader>();
  const items = data.payload.items;

  if (data.fetchError) {
    return (
      <CustomerShell
        title="Usage"
        tenantId={data.session.tenantId}
        mainProps={{ "data-page": "usage", "data-usage-page": true }}
      >
        <div
          className="rounded border border-destructive/40 bg-destructive/10 p-4 text-sm text-destructive"
          role="alert"
          data-usage-error
        >
          <p className="font-semibold">Usage data unavailable</p>
          <p className="mt-1">
            {data.fetchError} · gateway{" "}
            <code className="text-xs">{data.apiBase}</code>. No cached or
            sample rows are shown.
          </p>
        </div>
      </CustomerShell>
    );
  }

  return (
    <CustomerShell
      title="Usage"
      tenantId={data.session.tenantId}
      mainProps={{ "data-page": "usage", "data-usage-page": true }}
    >
      <p className="text-sm text-muted-foreground" data-usage-blurb>
        Usage facts for 对量 — energy (kWh) and rack-hour. No billing or
        invoice (Commercial B parked).
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
            href={filterHref(k, data.granularity)}
            className="rounded border px-2 py-1 hover:border-primary"
            data-usage-filter-kind={label}
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
            href={filterHref(data.kind, g)}
            className="rounded border px-2 py-1 hover:border-primary"
            data-usage-filter-granularity={label}
          >
            {label}
          </a>
        ))}
      </nav>

      <div className="overflow-x-auto rounded border">
        <table className="w-full text-left text-sm" data-usage-table>
          <thead className="border-b bg-muted/40 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th className="px-3 py-2">kind</th>
              <th className="px-3 py-2">site</th>
              <th className="px-3 py-2">asset</th>
              <th className="px-3 py-2">period</th>
              <th className="px-3 py-2">qty</th>
              <th className="px-3 py-2">unit</th>
            </tr>
          </thead>
          <tbody>
            {items.length === 0 ? (
              <tr data-usage-empty>
                <td colSpan={6} className="px-3 py-4 text-muted-foreground">
                  No usage records
                </td>
              </tr>
            ) : (
              items.map((row) => (
                <tr
                  key={row.id}
                  className="border-b last:border-0"
                  data-usage-row
                  data-usage-kind={row.kind}
                >
                  <td className="px-3 py-2 font-mono text-xs">{row.kind}</td>
                  <td className="px-3 py-2 font-mono text-xs">{row.site_id}</td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {row.asset_path}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {row.period_start?.slice(0, 10)}
                    {row.period_end ? ` → ${row.period_end.slice(0, 10)}` : ""}
                  </td>
                  <td className="px-3 py-2 font-mono">{row.quantity}</td>
                  <td className="px-3 py-2 font-mono text-xs">{row.unit}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
      <p className="text-xs text-muted-foreground" data-usage-count>
        {items.length} record(s)
        {data.payload.note ? ` · ${data.payload.note}` : ""}
      </p>
    </CustomerShell>
  );
}
