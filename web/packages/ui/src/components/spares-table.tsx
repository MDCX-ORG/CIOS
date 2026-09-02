/**
 * Presentational spare-part inventory (E3.5 / P643 — spec-009 §5
 * spares entry point).
 *
 * Read-only — Phase A per spec-009 §8. No adjust/consume (M4), no
 * fetch, no router data calls. Fed by the `spares.tsx` route loader,
 * which already passed through the mock seam or live `/api/spares`
 * (spec-004 `{items, next_page_token}` envelope) and projected to the
 * local `Spare` shape below.
 *
 * `Spare` re-declared locally (same pattern as `tickets-table.tsx` /
 * `alarms-table.tsx` / `maintenance-list.tsx`) so the UI package has
 * zero runtime deps on other workspace packages (L88 UI package boundary).
 * The route loader, which depends on the shared types package,
 * projects into this local shape. Do NOT add fields not in
 * core.SparePart (core/store.go L143-150).
 *
 * `low_stock` is NOT in the list envelope (PRMT-048 §2 — derived
 * server-side per-id only); the UI derives `qty < min_qty` locally so
 * the `data-low-stock` row marker matches core.sparePartWithDerived.
 */

import type { JSX } from "react";

// Pinned to core.SparePart (core/store.go L143-150). Do NOT add
// fields not in core. location is omitempty in core.
interface Spare {
  id: string;
  sku: string;
  name: string;
  qty: number;
  min_qty: number;
  location?: string;
}

export interface SparesTableProps {
  spares: Spare[];
  /** = `next_page_token` when non-empty; renders a Next control. */
  nextCursor?: string;
}

export function SparesTable(props: SparesTableProps): JSX.Element {
  const { spares, nextCursor } = props;
  return (
    <div
      className="flex flex-col gap-3"
      data-spares-ready
      data-spares-count={spares.length}
    >
      <table
        data-spares-table
        className="w-full border-collapse text-sm"
      >
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">ID</th>
            <th className="px-2 py-1">SKU</th>
            <th className="px-2 py-1">Name</th>
            <th className="px-2 py-1">Qty</th>
            <th className="px-2 py-1">Min</th>
            <th className="px-2 py-1">Location</th>
          </tr>
        </thead>
        <tbody>
          {spares.length === 0 ? (
            <tr data-spares-empty>
              <td
                colSpan={6}
                className="px-2 py-3 text-center text-sm text-muted-foreground"
              >
                No spares reported by the gateway.
              </td>
            </tr>
          ) : (
            spares.map((sp) => {
              // Local derivation of low_stock — matches core's per-id
              // low_stock derivation (PRMT-048 §2).
              const lowStock = sp.qty < sp.min_qty;
              return (
                <tr
                  key={sp.id}
                  data-spare-row
                  data-spare-id={sp.id}
                  data-low-stock={lowStock ? "true" : "false"}
                >
                  <td
                    className="px-2 py-1 font-mono"
                    data-spare-cell="id"
                  >
                    {sp.id}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-spare-cell="sku"
                  >
                    {sp.sku}
                  </td>
                  <td
                    className="px-2 py-1"
                    data-spare-cell="name"
                  >
                    {sp.name}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-spare-cell="qty"
                  >
                    {sp.qty}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-spare-cell="min_qty"
                  >
                    {sp.min_qty}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-spare-cell="location"
                  >
                    {sp.location ?? ""}
                  </td>
                </tr>
              );
            })
          )}
        </tbody>
      </table>
      {nextCursor ? (
        <div>
          <a
            data-next-page
            data-next-page-cursor={nextCursor}
            href={`?cursor=${encodeURIComponent(nextCursor)}`}
            className="text-sm text-muted-foreground hover:underline"
          >
            Next
          </a>
        </div>
      ) : null}
    </div>
  );
}