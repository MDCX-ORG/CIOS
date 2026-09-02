/**
 * /admin/models/:type/:model — detail, soft lint, S-layer bindings (P812/P813).
 */
import {
  Form,
  useActionData,
  useLoaderData,
  useNavigation,
  Link,
} from "react-router";

import type { Route } from "./+types/admin.models.$type.$model";
import { AdminShell } from "~/components/admin-shell";
import { requireAdminSession } from "~/lib/auth.server";
import { loadApi, postApi, putApi } from "~/lib/fetch";
import { ApiError } from "@cios/api-client";

type Binding = {
  prim: string;
  cios_type?: string;
  cios_model?: string;
  cios_relpath?: string;
  note?: string;
};

type Detail = {
  pack: {
    type: string;
    model: string;
    path: string;
    size_bytes: number;
    status: string;
    lint_result?: string;
  };
  bindings: {
    notes?: string;
    bindings: Binding[];
    updated_at?: string;
    updated_by?: string;
  };
  lint?: {
    result: string;
    soft_status: string;
    ran_at?: string;
    exit_code?: number;
    summary?: { pass?: number; fail?: number; warn?: number };
    raw_stderr?: string;
  } | null;
};

export async function loader({ request, params }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  const type = params.type ?? "";
  const model = params.model ?? "";
  let detail: Detail | null = null;
  let loadError: string | null = null;
  try {
    detail = await loadApi<Detail>(
      `/api/model-packs/${encodeURIComponent(type)}/${encodeURIComponent(model)}`,
      s,
    );
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  return { user: s.user, type, model, detail, loadError };
}

export async function action({ request, params }: Route.ActionArgs) {
  const s = await requireAdminSession(request);
  const type = params.type ?? "";
  const model = params.model ?? "";
  const fd = await request.formData();
  const intent = String(fd.get("intent") ?? "");

  if (intent === "lint") {
    try {
      await postApi(
        `/api/model-packs/${encodeURIComponent(type)}/${encodeURIComponent(model)}:lint`,
        s,
        {},
      );
      return { ok: true as const, error: null, intent: "lint", report: null };
    } catch (e) {
      return {
        ok: false as const,
        intent: "lint",
        error:
          e instanceof ApiError
            ? `${e.status}: ${e.message}`
            : e instanceof Error
              ? e.message
              : String(e),
        report: null,
      };
    }
  }

  if (intent === "conform_dry" || intent === "conform_apply") {
    const dry = intent === "conform_dry";
    try {
      const report = await postApi<Record<string, unknown>>(
        `/api/model-packs/${encodeURIComponent(type)}/${encodeURIComponent(model)}:conform`,
        s,
        { dry_run: dry, mode: "g8" },
      );
      return {
        ok: true as const,
        error: null,
        intent,
        report,
      };
    } catch (e) {
      return {
        ok: false as const,
        intent,
        error:
          e instanceof ApiError
            ? `${e.status}: ${e.message}`
            : e instanceof Error
              ? e.message
              : String(e),
        report: null,
      };
    }
  }

  if (intent === "save_bindings") {
    const notes = String(fd.get("notes") ?? "");
    const prims = fd.getAll("prim").map(String);
    const types = fd.getAll("cios_type").map(String);
    const models = fd.getAll("cios_model").map(String);
    const rels = fd.getAll("cios_relpath").map(String);
    const notesRow = fd.getAll("row_note").map(String);
    const bindings: Binding[] = [];
    for (let i = 0; i < prims.length; i++) {
      const prim = prims[i]?.trim() ?? "";
      if (!prim) continue;
      bindings.push({
        prim,
        cios_type: types[i]?.trim() || undefined,
        cios_model: models[i]?.trim() || undefined,
        cios_relpath: rels[i]?.trim() || undefined,
        note: notesRow[i]?.trim() || undefined,
      });
    }
    try {
      await putApi(
        `/api/model-packs/${encodeURIComponent(type)}/${encodeURIComponent(model)}/bindings`,
        s,
        { notes, bindings },
      );
      return { ok: true as const, error: null, intent: "save_bindings" };
    } catch (e) {
      return {
        ok: false as const,
        intent: "save_bindings",
        error:
          e instanceof ApiError
            ? `${e.status}: ${e.message}`
            : e instanceof Error
              ? e.message
              : String(e),
      };
    }
  }

  return { ok: false as const, error: "unknown intent", intent: "" };
}

export default function AdminModelDetail() {
  const { user, type, model, detail, loadError } = useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";

  const rows: Binding[] =
    detail?.bindings?.bindings?.length
      ? detail.bindings.bindings
      : [{ prim: "", cios_type: "", cios_relpath: "" }];

  return (
    <AdminShell title={`${type}/${model}`} active="models">
      <p className="text-sm">
        <Link to="/admin/models" className="text-primary hover:underline">
          ← Catalog
        </Link>
      </p>
      {loadError || !detail ? (
        <p className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          {loadError ?? "not found"}
        </p>
      ) : (
        <>
          <section
            className="mt-3 rounded-lg border bg-card p-5"
            data-admin-model-detail
          >
            <h2 className="font-semibold">
              {detail.pack.model}{" "}
              <span className="text-sm font-normal text-muted-foreground">
                ({detail.pack.type})
              </span>
            </h2>
            <p className="mt-1 font-mono text-xs text-muted-foreground">
              {detail.pack.path} · {detail.pack.size_bytes} B · status=
              {detail.pack.status}
            </p>
            <p className="mt-1 font-mono text-xs text-muted-foreground">
              admin={user.sub}
            </p>

            <div className="mt-4 flex flex-wrap items-center gap-3">
              <Form method="post">
                <input type="hidden" name="intent" value="lint" />
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
                  data-admin-model-lint
                >
                  {busy ? "…" : "Run soft lint"}
                </button>
              </Form>
              <Form method="post">
                <input type="hidden" name="intent" value="conform_dry" />
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded border px-3 py-1.5 text-sm disabled:opacity-50"
                  data-admin-model-conform-dry
                >
                  Preview G8 conform
                </button>
              </Form>
              <Form
                method="post"
                onSubmit={(e) => {
                  if (
                    !window.confirm(
                      `Apply G8 conform to S-layer for ${type}/${model}? This writes bindings JSON (vendor .usdc is not modified).`,
                    )
                  ) {
                    e.preventDefault();
                  }
                }}
              >
                <input type="hidden" name="intent" value="conform_apply" />
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded border border-warning/60 px-3 py-1.5 text-sm text-warning disabled:opacity-50"
                  data-admin-model-conform-apply
                >
                  Apply G8 → S-layer
                </button>
              </Form>
              {detail.lint ? (
                <span className="text-sm" data-admin-model-lint-status>
                  Last: <strong>{detail.lint.result}</strong> →{" "}
                  {detail.lint.soft_status}
                  {detail.lint.summary
                    ? ` (pass ${detail.lint.summary.pass ?? "?"} / fail ${detail.lint.summary.fail ?? "?"})`
                    : ""}
                </span>
              ) : (
                <span className="text-sm text-muted-foreground">No lint yet</span>
              )}
            </div>
            <p className="mt-2 text-xs text-muted-foreground">
              P814: G8 vocabulary only · S-first (bindings JSON) · never overwrites
              vendor .usdc
            </p>
            {detail.lint?.raw_stderr ? (
              <pre className="mt-2 max-h-24 overflow-auto rounded border bg-muted/30 p-2 text-xs text-muted-foreground">
                {detail.lint.raw_stderr}
              </pre>
            ) : null}
            {actionData?.ok === false ? (
              <p className="mt-2 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {actionData.error}
              </p>
            ) : null}
            {actionData?.ok === true ? (
              <p className="mt-2 rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">
                {actionData.intent === "lint"
                  ? "Lint stored."
                  : actionData.intent === "conform_dry"
                    ? "Dry-run below — nothing written."
                    : actionData.intent === "conform_apply"
                      ? "G8 conform applied to S-layer."
                      : "Bindings saved."}
              </p>
            ) : null}
            {(() => {
              const proposals = (
                actionData?.report as
                  | {
                      proposals?: {
                        prim: string;
                        from_type: string;
                        to_type: string;
                        action: string;
                        source: string;
                        skip_reason?: string;
                      }[];
                    }
                  | null
                  | undefined
              )?.proposals;
              if (!Array.isArray(proposals) || proposals.length === 0) return null;
              return (
                <div
                  className="mt-3 overflow-x-auto rounded border"
                  data-admin-model-conform-report
                >
                  <table className="w-full text-left text-xs">
                    <thead className="border-b bg-muted/40 uppercase text-muted-foreground">
                      <tr>
                        <th className="px-2 py-1">Prim</th>
                        <th className="px-2 py-1">From</th>
                        <th className="px-2 py-1">To</th>
                        <th className="px-2 py-1">Action</th>
                        <th className="px-2 py-1">Source</th>
                      </tr>
                    </thead>
                    <tbody>
                      {proposals.map((p) => (
                        <tr key={p.prim + p.from_type} className="border-t">
                          <td className="px-2 py-1 font-mono">{p.prim}</td>
                          <td className="px-2 py-1 font-mono">
                            {p.from_type || "—"}
                          </td>
                          <td className="px-2 py-1 font-mono">
                            {p.to_type || "—"}
                          </td>
                          <td className="px-2 py-1">
                            {p.action}
                            {p.skip_reason ? ` (${p.skip_reason})` : ""}
                          </td>
                          <td className="px-2 py-1">{p.source}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              );
            })()}
          </section>

          <section
            className="mt-4 rounded-lg border bg-card p-5"
            data-admin-model-bindings
          >
            <h3 className="font-semibold">S-layer bindings</h3>
            <p className="mt-1 text-sm text-muted-foreground">
              Edit prim → cios:type / cios:relpath without re-exporting USD.
              Stored under artifacts/model-studio/bindings/.
            </p>
            <Form method="post" className="mt-4 space-y-3">
              <input type="hidden" name="intent" value="save_bindings" />
              <label className="block text-sm">
                <span className="text-muted-foreground">Notes</span>
                <input
                  name="notes"
                  defaultValue={detail.bindings.notes ?? ""}
                  className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
                />
              </label>
              <div className="overflow-x-auto rounded border">
                <table className="w-full text-left text-sm">
                  <thead className="border-b bg-muted/40 text-xs uppercase text-muted-foreground">
                    <tr>
                      <th className="px-2 py-1">prim</th>
                      <th className="px-2 py-1">cios:type</th>
                      <th className="px-2 py-1">cios:model</th>
                      <th className="px-2 py-1">cios:relpath</th>
                      <th className="px-2 py-1">note</th>
                    </tr>
                  </thead>
                  <tbody>
                    {[...rows, { prim: "" }].map((row, i) => (
                      <tr key={i} className="border-t">
                        <td className="px-1 py-1">
                          <input
                            name="prim"
                            defaultValue={row.prim}
                            placeholder="geo/pump000"
                            className="w-full rounded border bg-background px-1 py-1 font-mono text-xs"
                          />
                        </td>
                        <td className="px-1 py-1">
                          <input
                            name="cios_type"
                            defaultValue={row.cios_type ?? ""}
                            placeholder="pump"
                            className="w-full rounded border bg-background px-1 py-1 font-mono text-xs"
                          />
                        </td>
                        <td className="px-1 py-1">
                          <input
                            name="cios_model"
                            defaultValue={row.cios_model ?? ""}
                            className="w-full rounded border bg-background px-1 py-1 font-mono text-xs"
                          />
                        </td>
                        <td className="px-1 py-1">
                          <input
                            name="cios_relpath"
                            defaultValue={row.cios_relpath ?? ""}
                            placeholder="fws.supply.flow"
                            className="w-full rounded border bg-background px-1 py-1 font-mono text-xs"
                          />
                        </td>
                        <td className="px-1 py-1">
                          <input
                            name="row_note"
                            defaultValue={row.note ?? ""}
                            className="w-full rounded border bg-background px-1 py-1 text-xs"
                          />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <button
                type="submit"
                disabled={busy}
                className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
                data-admin-model-save-bindings
              >
                {busy ? "Saving…" : "Save bindings"}
              </button>
            </Form>
          </section>
        </>
      )}
    </AdminShell>
  );
}
