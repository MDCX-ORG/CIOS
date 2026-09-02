/**
 * /runbooks — KB runbook + case viewer (E3.5).
 * Live: /api/cases returns closed tickets (core.Ticket); project to Case.
 * Missing runbook key → list-only (404 is soft).
 */
import type { Route } from "./+types/runbooks";
import { RunbookViewer } from "@cios/ui";
import type { Case, Runbook } from "@cios/api-client";
import { ApiError } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

const RUNBOOK_KEY_RE = /^[A-Za-z0-9_\-]+(?:\/[A-Za-z0-9_\-]+)*$/;

/** Live wire = Ticket fields; UI wants Case projection. */
function projectCase(raw: unknown): Case | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Record<string, unknown>;
  const id = typeof r.id === "string" ? r.id : "";
  if (!id) return null;
  const title = typeof r.title === "string" ? r.title : id;
  const crn =
    typeof r.crn === "string"
      ? r.crn
      : typeof r.asset_path === "string"
        ? r.asset_path
        : undefined;
  const closed =
    typeof r.closed_at === "string"
      ? r.closed_at
      : undefined;
  const summary =
    typeof r.summary === "string"
      ? r.summary
      : typeof r.severity === "string"
        ? `${r.severity}${r.state ? ` · ${r.state}` : ""}`
        : undefined;
  return { id, title, summary, crn, closed_at: closed };
}

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const rawKey = url.searchParams.get("key") ?? "";
  const key = rawKey && RUNBOOK_KEY_RE.test(rawKey) ? rawKey : "";

  let cases: Case[] = [];
  try {
    const casesRes = await loadApi<{ items?: unknown[] }>("/api/cases", s);
    cases = (casesRes.items ?? [])
      .map(projectCase)
      .filter((c): c is Case => c != null);
  } catch {
    cases = [];
  }

  let runbook: Runbook | undefined;
  if (key) {
    try {
      // Live returns markdown text or JSON depending on apigw; handle both.
      const raw = await loadApi<unknown>("/api/runbooks/" + key, s);
      if (typeof raw === "string") {
        const body = raw;
        const h1 = body.match(/^#\s+(.+)$/m);
        runbook = {
          key,
          title: h1?.[1]?.trim() || key,
          body,
        };
      } else if (raw && typeof raw === "object") {
        const o = raw as Record<string, unknown>;
        runbook = {
          key,
          title: typeof o.title === "string" ? o.title : key,
          body: typeof o.body === "string" ? o.body : JSON.stringify(o),
        };
      }
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) {
        // leave runbook undefined for soft list-only
      }
    }
  }

  return {
    cases,
    runbook,
    requestedKey: key || undefined,
  };
}

export default function RunbooksRoute({ loaderData }: Route.ComponentProps) {
  const { cases, runbook, requestedKey } = loaderData;
  return (
    <OpsShell
      title={
        <>
          <span className="text-xl font-semibold">Runbooks / Cases</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-runbooks-header-count
          >
            {cases.length}
          </span>
        </>
      }
      mainProps={{
        "data-runbooks-page": true,
        "data-runbooks-ready": true,
        "data-runbooks-requested-key": requestedKey ?? "",
        className: "max-w-5xl",
      }}
    >
      {cases.length === 0 && !runbook ? (
        <section
          className="rounded-md border border-dashed bg-card p-4 text-sm text-muted-foreground"
          data-runbooks-live-empty
        >
          <p className="font-semibold text-foreground">
            No closed cases in the live store
          </p>
          <p className="mt-1">
            Cases = closed tickets. Seed open tickets only until closed
            postmortems are added. Related:{" "}
            <a href="/tickets" className="text-primary underline">
              Tickets
            </a>
            {" · "}
            <a href="/alarms" className="text-primary underline">
              Alarms
            </a>
          </p>
        </section>
      ) : null}
      <RunbookViewer cases={cases} runbook={runbook} />
    </OpsShell>
  );
}
