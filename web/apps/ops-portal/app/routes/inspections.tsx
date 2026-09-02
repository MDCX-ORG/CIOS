/**
 * /inspections — inspection template list (E3.5 / P643; spec-009 §5
 * inspection entry point).
 *
 *   - Fetches /api/inspections via the mock seam (MOCK_GATEWAY=1) or
 *     live `apiGet` (spec-004 `{items, …}` envelope — note: no
 *     `next_page_token`; M2 inspection scale is operator-set, small —
 *     core/inspection.go L80).
 *   - Read-only (Phase A per spec-009 §8). No checklist submission,
 *     no template create/edit/delete — those are M4 (mobile form
 *     route M2 P561 / desktop create M4).
 *   - Honors URL filter `?cursor=` (spec-004 cursor convention —
 *     `?page_token=<opaque>` request, even though core's current
 *     list endpoint ignores it; PRMT-160 §4 MUST).
 *
 * The mobile checklist form (M2 P561 SSR) is intentionally out of
 * scope here — desktop list view only.
 */

import type { Route } from "./+types/inspections";
import { InspectionsList } from "@cios/ui";
import type { InspectionsResponse } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  for (const k of ["cursor"] as const) {
    const v = url.searchParams.get(k);
    if (!v) continue;
    // cursor (URL) → page_token (spec-004 request).
    qs.set(k === "cursor" ? "page_token" : k, v);
  }
  const path = "/api/inspections" + (qs.toString() ? "?" + qs.toString() : "");

  const data: InspectionsResponse = await loadApi<InspectionsResponse>(
    path,
    s,
  );

  return {
    inspections: Array.isArray(data.items) ? data.items : [],
    // core's listInspectionsResponse has no `next_page_token`
    // (inspection.go L55-57 / L80). Leave undefined; the UI
    // component won't render the Next control.
    nextCursor: undefined,
  };
}

export default function InspectionsRoute({
  loaderData,
}: Route.ComponentProps) {
  const { inspections, nextCursor } = loaderData;
    return (
    <OpsShell
      title={<>
          <span className="text-xl font-semibold">Inspections</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-inspections-header-count
          >
            {inspections.length}
          </span>
        </>}
      mainProps={{
        "data-inspections-page": true,
        "data-inspections-ready": true,
        className: "max-w-5xl",
      }}
    >
      <InspectionsList inspections={inspections} nextCursor={nextCursor} />
    </OpsShell>
  );
}