/**
 * /tickets — ticket queue (E3.5 / P642; PRMT-199 write path).
 *
 *   - loader: GET /api/tickets (filters + cursor)
 *   - action: POST create → /api/tickets; POST transition → /api/tickets/{id}:transition
 *     body {"to": acknowledged|resolved|closed}
 */

import type { Route } from "./+types/tickets";
import { TicketsTable } from "@cios/ui";
import type { Ticket, Paged } from "@cios/api-client";
import { Form, redirect } from "react-router";

import { requireSession } from "~/lib/auth.server";
import { loadApi, postApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";
import { MOCK_ENABLED, mockTicketTransition } from "~/lib/mock.server";

const TICKET_SEVERITIES = ["critical", "major", "minor", "info"] as const;

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  for (const k of ["severity", "state", "cursor"] as const) {
    const v = url.searchParams.get(k);
    if (!v) continue;
    qs.set(k === "cursor" ? "page_token" : k, v);
  }
  const path = "/api/tickets" + (qs.toString() ? "?" + qs.toString() : "");

  const data: Paged<Ticket> = await loadApi<Paged<Ticket>>(path, s);

  return {
    tickets: data.items,
    nextCursor: data.next_page_token || undefined,
    filters: {
      severity: url.searchParams.get("severity") ?? undefined,
      state: url.searchParams.get("state") ?? undefined,
    },
    flash: url.searchParams.get("flash") ?? undefined,
    error: url.searchParams.get("error") ?? undefined,
    prefill: {
      open: url.searchParams.get("create") === "1",
      alarmId: url.searchParams.get("alarm_id") ?? "",
      assetPath: url.searchParams.get("asset_path") ?? "",
      title: url.searchParams.get("title") ?? "",
      severity: url.searchParams.get("severity") ?? "",
    },
  };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireSession(request);
  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");
  if (intent === "create") {
    const assetPath = String(form.get("asset_path") ?? "").trim();
    const title = String(form.get("title") ?? "").trim();
    const severity = String(form.get("severity") ?? "").trim();
    const alarmId = String(form.get("alarm_id") ?? "").trim();
    if (!assetPath || !title || !severity) {
      return redirect("/tickets?error=missing-fields");
    }
    if (MOCK_ENABLED) {
      // Mock seam has no ticket create (registered in 明确不做;
      // mirrors PRMT-230's mock-no-ack).
      return redirect("/tickets?error=mock-no-create");
    }
    try {
      const t = await postApi<Ticket>("/api/tickets", s, {
        asset_path: assetPath,
        title,
        severity,
        alarm_id: alarmId,
      });
      return redirect(
        `/tickets?flash=${encodeURIComponent("created " + (t?.id ?? ""))}`,
      );
    } catch (e) {
      const msg = e instanceof Error ? e.message : "create-failed";
      return redirect(`/tickets?error=${encodeURIComponent(msg)}`);
    }
  }
  if (intent !== "transition") {
    return redirect("/tickets?error=bad-intent");
  }
  const id = String(form.get("id") ?? "").trim();
  const to = String(form.get("to") ?? "").trim();
  if (!id || !to) {
    return redirect("/tickets?error=missing-fields");
  }

  if (MOCK_ENABLED) {
    const r = mockTicketTransition(id, to);
    if (!r.ok) {
      return redirect(`/tickets?error=${encodeURIComponent(r.error)}`);
    }
    return redirect(`/tickets?flash=${encodeURIComponent(id + "→" + to)}`);
  }

  try {
    await postApi(`/api/tickets/${encodeURIComponent(id)}:transition`, s, {
      to,
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : "transition-failed";
    return redirect(`/tickets?error=${encodeURIComponent(msg)}`);
  }
  return redirect(`/tickets?flash=${encodeURIComponent(id + "→" + to)}`);
}

export default function TicketsRoute({ loaderData }: Route.ComponentProps) {
  const { tickets, nextCursor, filters, flash, error, prefill } = loaderData;
  const hasFilter =
    Boolean(filters.severity) ||
    Boolean(filters.state) ||
    tickets.length > 0;
  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">Tickets</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-tickets-header-count
          >
            {tickets.length}
          </span>
        </>
      }
      mainProps={{
        "data-tickets-page": true,
        "data-tickets-ready": true,
        "data-tickets-filter-severity": filters.severity ?? "",
        "data-tickets-filter-state": filters.state ?? "",
        className: "max-w-5xl",
      }}
    >
      {flash ? (
        <p className="mb-2 text-sm text-success" data-tickets-flash>
          Transitioned: {flash}
        </p>
      ) : null}
      {error ? (
        <p className="mb-2 text-sm text-destructive" data-tickets-error>
          {error}
        </p>
      ) : null}
      {hasFilter && (filters.severity || filters.state) ? (
        <p
          className="text-xs uppercase tracking-wide text-muted-foreground"
          data-tickets-active-filters
        >
          {filters.severity ? `severity=${filters.severity}` : ""}
          {filters.severity && filters.state ? " · " : ""}
          {filters.state ? `state=${filters.state}` : ""}
        </p>
      ) : null}
      <details
        data-ticket-create
        open={prefill.open || undefined}
        className="mb-4 rounded border p-3"
      >
        <summary className="cursor-pointer text-sm font-medium">
          New ticket
        </summary>
        <Form method="post" className="mt-2 flex flex-wrap items-end gap-2">
          <input type="hidden" name="intent" value="create" />
          <input type="hidden" name="alarm_id" value={prefill.alarmId} />
          <label className="flex flex-col gap-1 text-xs">
            Asset path
            <input
              name="asset_path"
              defaultValue={prefill.assetPath}
              required
              data-ticket-create-asset
              className="rounded border px-2 py-1 font-mono text-sm"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs">
            Title
            <input
              name="title"
              defaultValue={prefill.title}
              required
              data-ticket-create-title
              className="rounded border px-2 py-1 text-sm"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs">
            Severity
            <select
              name="severity"
              defaultValue={
                (TICKET_SEVERITIES as readonly string[]).includes(
                  prefill.severity,
                )
                  ? prefill.severity
                  : "major"
              }
              data-ticket-create-severity
              className="rounded border px-2 py-1 text-sm"
            >
              {TICKET_SEVERITIES.map((sv) => (
                <option key={sv} value={sv}>
                  {sv}
                </option>
              ))}
            </select>
          </label>
          {prefill.alarmId ? (
            <span
              className="text-xs text-muted-foreground"
              data-ticket-create-alarm
            >
              from alarm {prefill.alarmId}
            </span>
          ) : null}
          <button
            type="submit"
            data-ticket-create-submit
            className="rounded border px-3 py-1 text-sm hover:bg-accent"
          >
            Create
          </button>
        </Form>
      </details>
      <TicketsTable tickets={tickets} nextCursor={nextCursor} canOperate />
    </OpsShell>
  );
}
