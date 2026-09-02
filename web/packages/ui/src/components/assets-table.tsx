/**
 * CMDB assets table — supports mock + live projected rows (E3.5).
 * Read-only; filters applied by the route loader.
 */
export interface AssetRow {
  crn: string;
  type: string;
  name?: string;
  parent?: string;
  model?: string;
  lifecycle?: string;
  serial?: string;
  series?: string;
  rated_power_kw?: number;
  rated_cooling_kw?: number;
}

export interface AssetsTableProps {
  assets: AssetRow[];
  nextCursor?: string;
}

export function AssetsTable({ assets, nextCursor }: AssetsTableProps) {
  return (
    <div className="overflow-x-auto rounded-md border" data-assets-table>
      <table className="w-full text-left text-sm">
        <thead className="border-b bg-muted/40 text-xs uppercase tracking-wide text-muted-foreground">
          <tr>
            <th className="px-3 py-2 font-medium">Path</th>
            <th className="px-3 py-2 font-medium">Type</th>
            <th className="px-3 py-2 font-medium">Model</th>
            <th className="px-3 py-2 font-medium">Serial</th>
            <th className="px-3 py-2 font-medium">Power kW</th>
            <th className="px-3 py-2 font-medium">Cooling kW</th>
            <th className="px-3 py-2 font-medium">Lifecycle</th>
          </tr>
        </thead>
        <tbody>
          {assets.length === 0 ? (
            <tr data-assets-empty>
              <td
                colSpan={7}
                className="px-3 py-6 text-center text-muted-foreground"
              >
                No assets match this filter.
              </td>
            </tr>
          ) : (
            assets.map((a, i) => (
              <tr
                key={`${a.crn}:${i}`}
                data-asset-row
                data-asset-crn={a.crn}
                data-asset-type={a.type}
                data-asset-model={a.model ?? ""}
                className="border-b last:border-0 hover:bg-muted/30"
              >
                <td className="px-3 py-2 font-mono text-xs">{a.crn}</td>
                <td className="px-3 py-2">{a.type}</td>
                <td
                  className="px-3 py-2 font-semibold"
                  data-asset-model-cell
                >
                  {a.model ?? "—"}
                </td>
                <td className="px-3 py-2 font-mono text-xs text-muted-foreground">
                  {a.serial ?? "—"}
                </td>
                <td className="px-3 py-2 font-mono text-xs">
                  {a.rated_power_kw != null ? a.rated_power_kw : "—"}
                </td>
                <td className="px-3 py-2 font-mono text-xs">
                  {a.rated_cooling_kw != null ? a.rated_cooling_kw : "—"}
                </td>
                <td className="px-3 py-2 text-xs">{a.lifecycle ?? "—"}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>
      {nextCursor ? (
        <p
          className="border-t px-3 py-2 text-xs text-muted-foreground"
          data-assets-next-cursor
        >
          next page token present (loaded all pages in route when possible)
        </p>
      ) : null}
    </div>
  );
}
