/**
 * /spares — spare-part inventory (E3.5 / P643; spec-009 §5 spares
 * entry point).
 *
 *   - Fetches /api/spares via the mock seam (MOCK_GATEWAY=1) or live
 *     `apiGet` (spec-004 `{items, next_page_token}` envelope).
 *   - Honors cursor `?cursor=`; cursor → `page_token` per spec-004 §3
 *     (`?page_token=<opaque>` request, `{items, next_page_token}`
 *     response). The list endpoint has no spec-004 whitelist filter
 *     params (spares are not asset-path scoped, per
 *     core/spares.go L116-120), so the loader only forwards `cursor`.
 *   - SSR-filtered: no client-side filtering. Empty result renders an
 *     empty state row inside the table — no crash.
 *
 * Read-only (Phase A per spec-009 §8). No POST/PUT/DELETE/PATCH;
 * adjust/consume is M4.
 */

import type { Route } from "./+types/spares";
import { SparesTable } from "@cios/ui";
import type { Spare, Paged } from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";
import { OpsShell } from "~/components/ops-shell";

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireSession(request);

  const url = new URL(request.url);
  const qs = new URLSearchParams();
  // cursor (URL) → page_token (spec-004 request). No other filters:
  // the list endpoint has no spec-004 whitelist filter params
  // (core/spares.go L116-120), and PRMT-159 §4 forbids invented params.
  const cursor = url.searchParams.get("cursor");
  if (cursor) qs.set("page_token", cursor);

  const path = "/api/spares" + (qs.toString() ? "?" + qs.toString() : "");

  const data: Paged<Spare> = await loadApi<Paged<Spare>>(path, s);

  return {
    spares: data.items,
    nextCursor: data.next_page_token || undefined,
  };
}

export default function SparesRoute({ loaderData }: Route.ComponentProps) {
  const { spares, nextCursor } = loaderData;
    return (
    <OpsShell
      title={<>
          <span className="text-xl font-semibold">Spares</span>
          <span
            className="font-mono text-sm text-muted-foreground"
            data-spares-header-count
          >
            {spares.length}
          </span>
        </>}
      mainProps={{
        "data-spares-page": true,
        "data-spares-filter-cursor": nextCursor ?? "",
        className: "max-w-5xl",
      }}
    >
      <SparesTable spares={spares} nextCursor={nextCursor} />
    </OpsShell>
  );
}