/**
 * /reports — ops report (E3.5 / P642; spec-009 §5 reports entry
 * point, M2 P551 MTTR/MTBF + top-alarms stats).
 *
 *   - Fetches /api/reports/ops via the mock seam (MOCK_GATEWAY=1)
 *     or live `apiGet` (spec-004 wire shape mirroring
 *     core.opsReportResponse — core/reports.go L31-38:
 *     mttr_seconds? / mean_response_seconds? / mtbf_seconds? /
 *     ticket_counts / alarm_top[] / window).
 *   - Honors optional `?since=` range forwarded as-is (RFC3339 per
 *     spec-004 / core.serveOpsReport). No invented query params.
 *   - SSR: no client-side recompute. Missing values render as
 *     em-dash (not NaN).
 *
 * Read-only (Phase A per spec-009 §8). No POST/PUT/DELETE/PATCH.
 */

import type { Route } from "./+types/reports";
import { ReportPanel } from "@cios/ui";
import type { OpsReport } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  for (const k of ["since"] as const) {
    const v = url.searchParams.get(k);
    if (!v) continue;
    qs.set(k, v);
  }
  const path =
    "/api/reports/ops" + (qs.toString() ? "?" + qs.toString() : "");

  const report: OpsReport = await loadApi<OpsReport>(path, s);

  return {
    report,
    since: url.searchParams.get("since") ?? undefined,
  };
}

export default function ReportsRoute({ loaderData }: Route.ComponentProps) {
  const { report, since } = loaderData;
    return (
    <OpsShell
      title={<>
          <span className="text-xl font-semibold">Ops report</span>
          {since ? (
            <span
              className="font-mono text-sm text-muted-foreground"
              data-reports-header-since
            >
              since {since}
            </span>
          ) : null}
        </>}
      mainProps={{
        "data-reports-page": true,
        "data-reports-since": since ?? "",
        className: "max-w-4xl",
      }}
    >
      <ReportPanel report={report} />
    </OpsShell>
  );
}