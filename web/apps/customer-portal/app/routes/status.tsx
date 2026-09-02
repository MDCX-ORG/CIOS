/**
 * Tenant status page (PRMT-207).
 *
 * Live path: GET {CUSTOMER_API_BASE}/api/customer/status (PRMT-208).
 * Explicit error state when fetch fails (PRMT-229; no mock rows).
 *
 * Health rules (v0, mirrored from gateway docs):
 *   critical open alarm → red; any open alarm → yellow; else green.
 */
import { useLoaderData } from "react-router";

import type { Route } from "./+types/status";
import { CustomerShell } from "~/components/customer-shell";
import { customerApiBase, requireSession } from "~/lib/auth.server";

type SiteHealth = "green" | "yellow" | "red";

type StatusPayload = {
  tenant_id: string;
  sites: { id: string; health: SiteHealth; open_alarms: number }[];
  as_of: string;
};

/** Status quartet (MDCX §3.4) — machine truth only. */
function healthClass(h: SiteHealth): string {
  switch (h) {
    case "green":
      return "bg-success/15 text-success";
    case "yellow":
      return "bg-warning/15 text-warning";
    case "red":
      return "bg-destructive/15 text-destructive";
  }
}

export async function loader({ request }: Route.LoaderArgs) {
  const session = await requireSession(request);
  const base = customerApiBase().replace(/\/$/, "");
  let payload: StatusPayload | null = null;
  let fetchError: string | null = null;

  try {
    const res = await fetch(`${base}/api/customer/status`, {
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${session.bearer}`,
        "X-CIOS-Tenant": session.tenantId,
      },
      signal: AbortSignal.timeout(4000),
    });
    if (res.ok) {
      payload = (await res.json()) as StatusPayload;
    } else {
      fetchError = `API ${res.status}`;
    }
  } catch (e) {
    fetchError = e instanceof Error ? e.message : "fetch failed";
  }

  if (!payload) {
    payload = {
      tenant_id: session.tenantId,
      sites: [],
      as_of: new Date().toISOString(),
    };
  }

  return {
    session,
    payload,
    fetchError,
    apiBase: base,
  };
}

export default function StatusPage() {
  const data = useLoaderData<typeof loader>();
  if (data.fetchError) {
    return (
      <CustomerShell
        title="Status"
        tenantId={data.session.tenantId}
        mainProps={{ "data-page": "status" }}
      >
        <div
          className="rounded border border-destructive/40 bg-destructive/10 p-4 text-sm text-destructive"
          role="alert"
          data-status-error
        >
          <p className="font-semibold">Status data unavailable</p>
          <p className="mt-1">
            {data.fetchError} · gateway{" "}
            <code className="text-xs">{data.apiBase}</code>. No cached or
            sample sites are shown.
          </p>
        </div>
      </CustomerShell>
    );
  }
  const overall: SiteHealth = data.payload.sites.reduce<SiteHealth>(
    (acc, s) => {
      if (s.health === "red" || acc === "red") return "red";
      if (s.health === "yellow" || acc === "yellow") return "yellow";
      return "green";
    },
    "green",
  );

  return (
    <CustomerShell
      title="Status"
      tenantId={data.session.tenantId}
      mainProps={{ "data-page": "status" }}
    >
      <section className="flex items-center gap-3">
        <span className="text-sm text-muted-foreground">Overall</span>
        <span
          className={`rounded px-2 py-1 text-sm font-semibold uppercase ${healthClass(overall)}`}
          data-overall-health={overall}
        >
          {overall}
        </span>
        <span className="text-xs text-muted-foreground">
          as of {data.payload.as_of}
        </span>
      </section>

      <table className="w-full border-collapse text-sm" data-status-table>
        <thead>
          <tr className="border-b border-border text-left text-muted-foreground">
            <th className="py-2 pr-4 font-medium">Site</th>
            <th className="py-2 pr-4 font-medium">Health</th>
            <th className="py-2 font-medium">Open alarms</th>
          </tr>
        </thead>
        <tbody>
          {data.payload.sites.length === 0 ? (
            <tr>
              <td colSpan={3} className="py-4 text-muted-foreground">
                No sites.
              </td>
            </tr>
          ) : (
            data.payload.sites.map((s) => (
              <tr key={s.id} className="border-b border-border/60">
                <td className="py-2 pr-4 font-mono text-xs">{s.id}</td>
                <td className="py-2 pr-4">
                  <span
                    className={`rounded px-2 py-0.5 text-xs font-semibold uppercase ${healthClass(s.health)}`}
                  >
                    {s.health}
                  </span>
                </td>
                <td className="py-2">{s.open_alarms}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>

      <p className="text-xs text-muted-foreground">
        Health rule (v0): critical open alarm → red; any open alarm → yellow;
        else green.
      </p>
    </CustomerShell>
  );
}
