/**
 * /alarms — alarms queue (live + mock). Projects path/severity from core.
 *
 *   - loader: GET /api/alarms (filters + cursor)
 *   - action: POST ack → /api/alarms/{id}:ack (PRMT-230; spec-003 §4
 *     firing→acked). Mock seam has no alarm-ack — see PRMT-230 §3.7.
 */

import type { Route } from "./+types/alarms";
import { AlarmsTable } from "@cios/ui";
import { Form, Link, redirect } from "react-router";

import { requireSession } from "~/lib/auth.server";
import { loadApi, postApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";
import { projectAlarm } from "~/lib/project-assets";
import { MOCK_ENABLED } from "~/lib/mock.server";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  for (const k of ["severity", "state", "cursor"] as const) {
    const v = url.searchParams.get(k);
    if (!v) continue;
    qs.set(k === "cursor" ? "page_token" : k, v);
  }
  const path = "/api/alarms" + (qs.toString() ? "?" + qs.toString() : "");

  const data = await loadApi<{
    items?: unknown[];
    next_page_token?: string;
  }>(path, s);

  let alarms = (data.items ?? [])
    .map(projectAlarm)
    .filter((a): a is NonNullable<typeof a> => a != null);

  // Client-side filter when live apigw ignores severity/state query (seeded full list).
  const sevF = url.searchParams.get("severity") ?? undefined;
  const stateF = url.searchParams.get("state") ?? undefined;
  if (sevF) {
    alarms = alarms.filter((a) => a.severity === sevF);
  }
  if (stateF) {
    alarms = alarms.filter((a) => a.state === stateF);
  }

  return {
    alarms,
    nextCursor: data.next_page_token || undefined,
    filters: {
      severity: sevF,
      state: stateF,
    },
    flash: url.searchParams.get("flash") ?? undefined,
    error: url.searchParams.get("error") ?? undefined,
  };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireSession(request);
  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");
  if (intent !== "ack") {
    return redirect("/alarms?error=bad-intent");
  }
  const id = String(form.get("id") ?? "").trim();
  if (!id) {
    return redirect("/alarms?error=missing-fields");
  }

  if (MOCK_ENABLED) {
    return redirect("/alarms?error=mock-no-ack");
  }

  try {
    await postApi(`/api/alarms/${encodeURIComponent(id)}:ack`, s, {});
  } catch (e) {
    const msg = e instanceof Error ? e.message : "ack-failed";
    return redirect(`/alarms?error=${encodeURIComponent(msg)}`);
  }
  return redirect(`/alarms?flash=${encodeURIComponent("acked " + id)}`);
}

export default function AlarmsRoute({ loaderData }: Route.ComponentProps) {
  const { alarms, nextCursor, filters, flash, error } = loaderData;
  const firing = alarms.filter((a) => a.state === "firing").length;
  const critical = alarms.filter((a) => a.severity === "critical").length;
  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">Alarms</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-alarms-header-count
          >
            {alarms.length}
          </span>
        </>
      }
      mainProps={{
        "data-alarms-page": true,
        "data-alarms-filter-severity": filters.severity ?? "",
        "data-alarms-filter-state": filters.state ?? "",
        className: "max-w-4xl",
      }}
    >
      {flash ? (
        <p className="mb-2 text-sm text-success" data-alarms-flash>
          {flash}
        </p>
      ) : null}
      {error ? (
        <p className="mb-2 text-sm text-destructive" data-alarms-error>
          {error}
        </p>
      ) : null}
      <section
        className="flex flex-wrap gap-3 text-sm"
        data-alarms-stats
        aria-label="Alarm summary"
      >
        <div className="rounded border bg-card px-3 py-2">
          <span className="text-muted-foreground">Firing </span>
          <span className="font-mono font-semibold" data-alarms-stat-firing>
            {firing}
          </span>
        </div>
        <div className="rounded border bg-card px-3 py-2">
          <span className="text-muted-foreground">Critical </span>
          <span className="font-mono font-semibold" data-alarms-stat-critical>
            {critical}
          </span>
        </div>
        <a
          href="/alarms?state=firing"
          className="self-center text-sm text-primary underline"
        >
          firing only
        </a>
        <a
          href="/alarms?severity=critical"
          className="self-center text-sm text-primary underline"
        >
          critical
        </a>
        <a
          href="/alarms"
          className="self-center text-sm text-muted-foreground underline"
        >
          clear filters
        </a>
      </section>

      {(filters.severity || filters.state) ? (
        <p
          className="text-xs uppercase tracking-wide text-muted-foreground"
          data-alarms-active-filters
        >
          {filters.severity ? `severity=${filters.severity}` : ""}
          {filters.severity && filters.state ? " · " : ""}
          {filters.state ? `state=${filters.state}` : ""}
        </p>
      ) : null}
      <AlarmsTable
        alarms={alarms}
        nextCursor={nextCursor}
        renderRowAction={(a) => {
          if (!a.id) return null;
          const sev =
            a.severity === "critical"
              ? "critical"
              : a.severity === "warning"
                ? "major"
                : "minor";
          const qs = new URLSearchParams({
            create: "1",
            alarm_id: a.id,
            asset_path: a.crn,
            severity: sev,
            title: a.summary ?? "",
          });
          return (
            <span className="flex items-center gap-2">
              {a.rawState === "firing" ? (
                <Form method="post">
                  <input type="hidden" name="intent" value="ack" />
                  <input type="hidden" name="id" value={a.id} />
                  <button
                    type="submit"
                    data-alarm-ack
                    className="rounded border px-2 py-0.5 text-xs hover:bg-accent"
                  >
                    Ack
                  </button>
                </Form>
              ) : null}
              {a.rawState !== "resolved" ? (
                <Link
                  to={`/tickets?${qs.toString()}`}
                  data-alarm-open-ticket
                  className="rounded border px-2 py-0.5 text-xs hover:bg-accent"
                >
                  Open ticket
                </Link>
              ) : null}
            </span>
          );
        }}
      />
    </OpsShell>
  );
}
