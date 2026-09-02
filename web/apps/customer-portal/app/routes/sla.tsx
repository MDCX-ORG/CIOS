/**
 * Customer SLA read-only page (PRMT-207 / Q4).
 * Defaults: 99.9%, calendar month, display-only credit note.
 * Live path TODO: GET {CUSTOMER_API_BASE}/api/customer/sla (PRMT-208).
 */
import { useLoaderData } from "react-router";

import type { Route } from "./+types/sla";
import { CustomerShell } from "~/components/customer-shell";
import { customerApiBase, requireSession } from "~/lib/auth.server";

type SlaPayload = {
  target_pct: number;
  window: string;
  credit_note: string;
};

const DEFAULT_SLA: SlaPayload = {
  target_pct: 99.9,
  window: "calendar_month",
  credit_note: "display-only; no financial effect",
};

export async function loader({ request }: Route.LoaderArgs) {
  const session = await requireSession(request);
  const base = customerApiBase().replace(/\/$/, "");
  let payload: SlaPayload = DEFAULT_SLA;
  let live = false;

  try {
    const res = await fetch(`${base}/api/customer/sla`, {
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${session.bearer}`,
        "X-CIOS-Tenant": session.tenantId,
      },
      signal: AbortSignal.timeout(4000),
    });
    if (res.ok) {
      payload = (await res.json()) as SlaPayload;
      live = true;
    }
  } catch {
    // keep defaults
  }

  return { session, payload, live, apiBase: base };
}

export default function SlaPage() {
  const data = useLoaderData<typeof loader>();
  const { payload } = data;

  return (
    <CustomerShell
      title="SLA"
      tenantId={data.session.tenantId}
      mainProps={{ "data-page": "sla" }}
    >
      <p className="text-sm text-muted-foreground">
        Customer uptime SLA (not internal ticket-SLA).{" "}
        {data.live ? (
          <span data-sla-source="live">Live from gateway.</span>
        ) : (
          <span data-sla-source="default">
            Defaults (gateway{" "}
            <code className="text-xs">{data.apiBase}</code> not available).
          </span>
        )}
      </p>

      <dl className="grid gap-4 rounded border border-border p-4 sm:grid-cols-3">
        <div>
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">
            Target
          </dt>
          <dd className="mt-1 text-2xl font-semibold" data-sla-target>
            {payload.target_pct}%
          </dd>
        </div>
        <div>
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">
            Window
          </dt>
          <dd className="mt-1 font-mono text-sm" data-sla-window>
            {payload.window}
          </dd>
        </div>
        <div className="sm:col-span-1">
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">
            Credit
          </dt>
          <dd className="mt-1 text-sm" data-sla-credit>
            {payload.credit_note}
          </dd>
        </div>
      </dl>

      <p className="text-xs text-muted-foreground">
        Credit is display-only and has no financial effect (Q4). No ERP or
        billing integration in v0.
      </p>
    </CustomerShell>
  );
}
