/**
 * /admin/models — Model Studio catalog (L109 P811).
 */
import { Link, useLoaderData } from "react-router";

import type { Route } from "./+types/admin.models";
import { AdminShell } from "~/components/admin-shell";
import { requireAdminSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";

type Pack = {
  type: string;
  model: string;
  path: string;
  size_bytes: number;
  status: string;
  lint_result?: string;
  lint_fail?: number;
  lint_pass?: number;
  has_bindings: boolean;
  binding_count: number;
};

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  let items: Pack[] = [];
  let loadError: string | null = null;
  try {
    const data = await loadApi<{ items: Pack[] }>("/api/model-packs", s);
    items = Array.isArray(data.items) ? data.items : [];
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  return { user: s.user, items, loadError };
}

function statusClass(st: string) {
  if (st === "ready") return "text-success";
  if (st === "pending_conform") return "text-warning";
  if (st === "lint_unavailable") return "text-muted-foreground";
  return "text-muted-foreground";
}

export default function AdminModels() {
  const { user, items, loadError } = useLoaderData<typeof loader>();
  return (
    <AdminShell title="Model Studio" active="models">
      <section className="rounded-lg border bg-card p-5" data-admin-models>
        <h2 className="font-semibold">Model packs</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          G-layer USD under <code className="text-xs">assets/usd/</code>. S-layer
          bindings + soft lint live under{" "}
          <code className="text-xs">artifacts/model-studio/</code> (L109 —
          platform revises semantics without vendor re-export).
        </p>
        <p className="mt-1 font-mono text-xs text-muted-foreground">
          admin={user.sub}
        </p>
        {loadError ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-models-error
          >
            {loadError}
          </p>
        ) : null}
        <div className="mt-4 overflow-x-auto rounded border">
          <table className="w-full text-left text-sm" data-admin-model-list>
            <thead className="border-b bg-muted/40 text-xs uppercase text-muted-foreground">
              <tr>
                <th className="px-3 py-2">Model</th>
                <th className="px-3 py-2">Type</th>
                <th className="px-3 py-2">Status</th>
                <th className="px-3 py-2">Lint</th>
                <th className="px-3 py-2">Bindings</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td className="px-3 py-4 text-muted-foreground" colSpan={6}>
                    No packs under assets/usd (or API offline).
                  </td>
                </tr>
              ) : (
                items.map((p) => (
                  <tr
                    key={`${p.type}/${p.model}`}
                    className="border-t"
                    data-admin-model={p.model}
                  >
                    <td className="px-3 py-2 font-mono font-medium">{p.model}</td>
                    <td className="px-3 py-2 font-mono text-xs">{p.type}</td>
                    <td className={`px-3 py-2 text-xs ${statusClass(p.status)}`}>
                      {p.status}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">
                      {p.lint_result ?? "—"}
                      {typeof p.lint_fail === "number"
                        ? ` (fail ${p.lint_fail})`
                        : ""}
                    </td>
                    <td className="px-3 py-2 text-xs">
                      {p.has_bindings ? p.binding_count : "—"}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <Link
                        to={`/admin/models/${encodeURIComponent(p.type)}/${encodeURIComponent(p.model)}`}
                        className="text-xs text-primary hover:underline"
                        data-admin-model-open
                      >
                        Open
                      </Link>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>
    </AdminShell>
  );
}
