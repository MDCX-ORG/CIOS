/**
 * Ops portal home — E3.5 hub with live fleet snapshot (AC45/DC45 + alarms).
 */
import { Link, useLoaderData } from "react-router";

import type { Route } from "./+types/_index";
import { OpsShell } from "~/components/ops-shell";
import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import {
  fleetSummary,
  loadAllPortalAssets,
  projectAlarm,
} from "~/lib/project-assets";

const CARDS: { to: string; title: string; blurb: string; id: string }[] = [
  { to: "/noc", title: "NOC", blurb: "Asset tree, inspector, site chart", id: "noc" },
  { to: "/assets", title: "Assets", blurb: "CMDB table — filter by model/type", id: "assets" },
  { to: "/alarms", title: "Alarms", blurb: "Active and historical alarms", id: "alarms" },
  { to: "/tickets", title: "Tickets", blurb: "Ops ticket queue", id: "tickets" },
  { to: "/maintenance", title: "Maintenance", blurb: "Upcoming PM windows", id: "maintenance" },
  { to: "/spares", title: "Spares", blurb: "Spare parts inventory", id: "spares" },
  { to: "/inspections", title: "Inspections", blurb: "Inspection templates", id: "inspections" },
  { to: "/runbooks", title: "Runbooks", blurb: "Cases and runbook KB", id: "runbooks" },
  { to: "/reports", title: "Reports", blurb: "Ops report metrics", id: "reports" },
  {
    to: "/usage",
    title: "Usage",
    blurb: "Energy + rack-hour 对量 facts",
    id: "usage",
  },
  {
    to: "/admin",
    title: "Platform Admin",
    blurb: "Sites, users, models, site draw (L109)",
    id: "admin",
  },
];

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const signedOutDev =
    new URL(request.url).searchParams.get("signed_out") === "dev";

  let fleet = {
    total: 0,
    sites: 0,
    pods: 0,
    modelSummary: {} as Record<string, number>,
    typeSummary: {} as Record<string, number>,
    powerKw: 0,
    coolingKw: 0,
    siteIds: [] as string[],
    podRows: [] as {
      crn: string;
      model: string;
      serial: string;
      power?: number;
      cooling?: number;
      series?: string;
    }[],
  };
  let fleetError: string | null = null;
  try {
    const assets = await loadAllPortalAssets(s);
    fleet = fleetSummary(assets);
  } catch (e) {
    fleetError = e instanceof Error ? e.message : String(e);
  }

  let alarmFiring = 0;
  let alarmCritical = 0;
  let alarmTotal = 0;
  let alarmsError: string | null = null;
  try {
    const raw = await loadApi<{ items?: unknown[] }>("/api/alarms", s);
    const alarms = (raw.items ?? [])
      .map(projectAlarm)
      .filter((a): a is NonNullable<typeof a> => a != null);
    alarmTotal = alarms.length;
    alarmFiring = alarms.filter((a) => a.state === "firing").length;
    alarmCritical = alarms.filter((a) => a.severity === "critical").length;
  } catch (e) {
    alarmsError = e instanceof Error ? e.message : String(e);
  }

  return {
    user: s.user,
    fleet,
    alarms: { total: alarmTotal, firing: alarmFiring, critical: alarmCritical },
    fleetError,
    alarmsError,
    signedOutDev,
  };
}

export default function Index() {
  const { user, fleet, alarms, signedOutDev, fleetError, alarmsError } =
    useLoaderData<typeof loader>();
  const dc45 = fleet.modelSummary["DC45"] ?? 0;
  const ac45 = fleet.modelSummary["AC45"] ?? 0;

  return (
    <OpsShell
      title="CIOS Ops Portal"
      mainProps={{ "data-ops-portal-ready": true, className: "max-w-5xl" }}
    >
      {signedOutDev ? (
        <p
          className="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning"
          data-ops-dev-signed-out
        >
          DEV_PORTAL_NO_AUTH: no real session cookie to clear. Portal still
          open as <span className="font-mono">{user.sub}</span>.
        </p>
      ) : null}
      {fleetError || alarmsError ? (
        <div
          className="rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
          role="alert"
          data-ops-home-error
        >
          {fleetError ? <p>Fleet data unavailable ({fleetError}).</p> : null}
          {alarmsError ? <p>Alarm data unavailable ({alarmsError}).</p> : null}
        </div>
      ) : null}
      <section
        className="rounded-lg border bg-card p-6 text-card-foreground shadow-sm"
        data-ops-home-status
      >
        <p className="text-sm text-muted-foreground">Signed in as</p>
        <p className="mt-1 font-mono text-base" data-testid="user-sub">
          {user.sub}
        </p>
        <div
          className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4"
          data-ops-home-kpis
        >
          <div className="rounded border px-3 py-2" data-ops-home-site-count>
            <p className="text-xs text-muted-foreground">Sites</p>
            <p className="font-mono text-2xl font-semibold">{fleetError ? "—" : fleet.sites}</p>
          </div>
          <div className="rounded border px-3 py-2" data-ops-home-asset-count>
            <p className="text-xs text-muted-foreground">Assets</p>
            <p className="font-mono text-2xl font-semibold">{fleetError ? "—" : fleet.total}</p>
          </div>
          <div className="rounded border px-3 py-2" data-ops-home-pod-count>
            <p className="text-xs text-muted-foreground">Pods</p>
            <p className="font-mono text-2xl font-semibold">{fleetError ? "—" : fleet.pods}</p>
          </div>
          <div className="rounded border px-3 py-2" data-ops-home-alarm-firing>
            <p className="text-xs text-muted-foreground">Firing alarms</p>
            <p className="font-mono text-2xl font-semibold">{alarmsError ? "—" : alarms.firing}</p>
            <p className="text-xs text-muted-foreground">
              critical {alarmsError ? "—" : alarms.critical} · total {alarmsError ? "—" : alarms.total}
            </p>
          </div>
        </div>
        <div className="mt-3 flex flex-wrap gap-2 text-sm" data-ops-home-models>
          <Link
            to="/assets?model=DC45"
            className="rounded border border-primary/40 bg-primary/5 px-3 py-1.5 font-semibold text-primary hover:bg-primary/10"
            data-ops-home-model="DC45"
          >
            DC45 × {fleetError ? "—" : dc45}
          </Link>
          <Link
            to="/assets?model=AC45"
            className="rounded border border-primary/40 bg-primary/5 px-3 py-1.5 font-semibold text-primary hover:bg-primary/10"
            data-ops-home-model="AC45"
          >
            AC45 × {fleetError ? "—" : ac45}
          </Link>
          {Object.entries(fleet.modelSummary)
            .filter(([m]) => m !== "DC45" && m !== "AC45")
            .map(([m, n]) => (
              <Link
                key={m}
                to={`/assets?model=${encodeURIComponent(m)}`}
                className="rounded border px-3 py-1.5 text-muted-foreground hover:border-primary"
                data-ops-home-model={m}
              >
                {m} × {n}
              </Link>
            ))}
        </div>
        <p className="mt-3 text-sm text-muted-foreground" data-ops-home-capacity>
          Rated fleet capacity:{" "}
          <span className="font-mono text-foreground">
            {fleetError ? "—" : fleet.powerKw} kW IT
          </span>
          {" · "}
          <span className="font-mono text-foreground">
            {fleetError ? "—" : fleet.coolingKw} kW cooling
          </span>
        </p>
      </section>

      {fleet.podRows.length > 0 ? (
        <section
          className="rounded-lg border bg-card p-4 shadow-sm"
          data-ops-home-pods
          aria-label="Pod inventory"
        >
          <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-muted-foreground">
            Live pods
          </h2>
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs uppercase text-muted-foreground">
                <tr>
                  <th className="px-2 py-1">Path</th>
                  <th className="px-2 py-1">Model</th>
                  <th className="px-2 py-1">Series</th>
                  <th className="px-2 py-1">Serial</th>
                  <th className="px-2 py-1">Power</th>
                  <th className="px-2 py-1">Cooling</th>
                  <th className="px-2 py-1"></th>
                </tr>
              </thead>
              <tbody>
                {fleet.podRows.map((p) => (
                  <tr
                    key={p.crn}
                    className="border-t"
                    data-ops-home-pod
                    data-pod-model={p.model}
                  >
                    <td className="px-2 py-2 font-mono text-xs">{p.crn}</td>
                    <td className="px-2 py-2 font-semibold text-primary">
                      {p.model}
                    </td>
                    <td className="px-2 py-2 text-xs">{p.series ?? "—"}</td>
                    <td className="px-2 py-2 font-mono text-xs text-muted-foreground">
                      {p.serial}
                    </td>
                    <td className="px-2 py-2 font-mono text-xs">
                      {p.power != null ? `${p.power} kW` : "—"}
                    </td>
                    <td className="px-2 py-2 font-mono text-xs">
                      {p.cooling != null ? `${p.cooling} kW` : "—"}
                    </td>
                    <td className="px-2 py-2 text-xs">
                      <Link
                        to={`/noc?site=${encodeURIComponent(p.crn.split(".")[0] ?? "")}&focus=${encodeURIComponent(p.crn)}`}
                        className="text-primary underline"
                      >
                        NOC
                      </Link>
                      {" · "}
                      <Link
                        to={`/assets?q=${encodeURIComponent(p.crn)}`}
                        className="text-primary underline"
                      >
                        CMDB
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}

      <section
        className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3"
        data-ops-home-cards
        aria-label="E3.5 ops surfaces"
      >
        {CARDS.map((c) => (
          <Link
            key={c.id}
            to={c.to}
            data-ops-home-card={c.id}
            className="rounded-lg border bg-card p-4 shadow-sm transition hover:border-primary"
          >
            <h2 className="font-semibold">{c.title}</h2>
            <p className="mt-1 text-sm text-muted-foreground">{c.blurb}</p>
          </Link>
        ))}
      </section>
    </OpsShell>
  );
}
