/**
 * /admin/tenants — Tenants + Orgs admin (L109 P804 + PRMT-220).
 * Search, hard delete, org rename, isolation tier.
 */
import { useState } from "react";
import {
  Form,
  useActionData,
  useLoaderData,
  useNavigation,
  useSearchParams,
  useSubmit,
} from "react-router";

import type { Route } from "./+types/admin.tenants";
import { AdminShell } from "~/components/admin-shell";
import { ConfirmDialog } from "~/components/confirm-dialog";
import { requireAdminSession } from "~/lib/auth.server";
import { adminUserError } from "~/lib/admin-errors";
import { deleteApi, loadApiAll, postApi } from "~/lib/fetch";

type Org = {
  id: string;
  tenant_id: string;
  name: string;
  created_at?: string;
};

type TenantItem = {
  id: string;
  display_name: string;
  isolation_tier?: string;
  status?: string;
  created_at?: string;
  orgs?: Org[];
  default_org?: Org | null;
};

type PendingDelete =
  | { kind: "tenant"; id: string }
  | { kind: "org"; id: string; name: string }
  | null;

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  const url = new URL(request.url);
  const q = (url.searchParams.get("q") ?? "").trim();
  let items: TenantItem[] = [];
  let truncated = false;
  let loadError: string | null = null;
  try {
    const path = q
      ? `/api/tenants?q=${encodeURIComponent(q)}`
      : "/api/tenants";
    const page = await loadApiAll<TenantItem>(path, s);
    items = page.items;
    truncated = page.truncated;
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  return { user: s.user, items, truncated, loadError, q };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireAdminSession(request);
  const fd = await request.formData();
  const intent = String(fd.get("intent") ?? "create-tenant");

  try {
    if (intent === "delete-tenant") {
      const id = String(fd.get("id") ?? "").trim();
      if (!id) return { ok: false as const, error: "Tenant id required" };
      await deleteApi(`/api/tenants/${encodeURIComponent(id)}`, s);
      return { ok: true as const, error: null, intent };
    }
    if (intent === "delete-org") {
      const id = String(fd.get("id") ?? "").trim();
      if (!id) return { ok: false as const, error: "Org id required" };
      await deleteApi(`/api/orgs/${encodeURIComponent(id)}`, s);
      return { ok: true as const, error: null, intent };
    }
    if (intent === "rename-org") {
      const id = String(fd.get("id") ?? "").trim();
      const name = String(fd.get("name") ?? "").trim();
      if (!id || !name) {
        return { ok: false as const, error: "Org id and new name required" };
      }
      await postApi(`/api/orgs/${encodeURIComponent(id)}:rename`, s, { name });
      return { ok: true as const, error: null, intent };
    }
    if (intent === "set-tier") {
      const id = String(fd.get("id") ?? "").trim();
      const isolation_tier = String(fd.get("isolation_tier") ?? "").trim();
      if (!id || !isolation_tier) {
        return { ok: false as const, error: "Tenant id and tier required" };
      }
      await postApi(`/api/tenants/${encodeURIComponent(id)}:tier`, s, {
        isolation_tier,
      });
      return { ok: true as const, error: null, intent };
    }
    if (intent === "create-org") {
      const tenant_id = String(fd.get("tenant_id") ?? "").trim();
      const name = String(fd.get("name") ?? "").trim();
      if (!tenant_id || !name) {
        return { ok: false as const, error: "Tenant and org name required" };
      }
      await postApi<Org>("/api/orgs", s, { tenant_id, name });
      return { ok: true as const, error: null, intent };
    }
    const id = String(fd.get("id") ?? "").trim();
    const display_name = String(fd.get("display_name") ?? "").trim();
    if (!id || !display_name) {
      return { ok: false as const, error: "Id and display name required" };
    }
    await postApi("/api/tenants", s, { id, display_name });
    return { ok: true as const, error: null, intent: "create-tenant" };
  } catch (e) {
    return { ok: false as const, error: adminUserError(e) };
  }
}

export default function AdminTenants() {
  const { user, items, truncated, loadError, q } =
    useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";
  const [searchParams] = useSearchParams();
  const submit = useSubmit();
  const [pending, setPending] = useState<PendingDelete>(null);
  const [renameOrg, setRenameOrg] = useState<{ id: string; name: string } | null>(
    null,
  );

  return (
    <AdminShell title="Tenants & orgs" active="tenants">
      <section className="rounded-md border bg-card p-5" data-admin-tenants>
        <h2 className="font-semibold">Tenants</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          List, search, create, rename orgs, raise isolation tier, hard-delete.
          Create auto-provisions the reserved{" "}
          <code className="text-xs">default</code> org. admin={user.sub}
        </p>

        <Form
          method="get"
          className="mt-4 flex flex-wrap items-end gap-2"
          data-admin-tenants-search
        >
          <label className="block text-sm">
            <span className="text-muted-foreground">Search id / name</span>
            <input
              name="q"
              defaultValue={q || searchParams.get("q") || ""}
              placeholder="acme"
              className="mt-1 w-56 rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-tenants-search-input
            />
          </label>
          <button
            type="submit"
            className="rounded border px-3 py-1.5 text-sm"
            data-admin-tenants-search-submit
          >
            Search
          </button>
        </Form>

        {truncated ? (
          <p
            className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm text-muted-foreground"
            data-admin-truncated
          >
            Showing first {items.length} matches. Narrow the search to see more.
          </p>
        ) : null}

        {loadError ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-tenants-load-error
          >
            Load failed: {loadError}
          </p>
        ) : null}
        {actionData?.ok === false ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-tenants-action-error
          >
            {actionData.error}
          </p>
        ) : null}
        {actionData?.ok === true ? (
          <p
            className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm"
            data-admin-tenants-action-ok
          >
            Saved.
          </p>
        ) : null}

        <Form
          method="post"
          className="mt-4 grid gap-3 sm:grid-cols-3"
          data-admin-tenants-create-tenant
        >
          <input type="hidden" name="intent" value="create-tenant" />
          <label className="block text-sm">
            <span className="text-muted-foreground">Tenant id</span>
            <input
              name="id"
              required
              placeholder="acme"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-tenants-input-id
            />
          </label>
          <label className="block text-sm sm:col-span-2">
            <span className="text-muted-foreground">Display name</span>
            <input
              name="display_name"
              required
              placeholder="ACME Inc"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
              data-admin-tenants-input-name
            />
          </label>
          <div className="sm:col-span-3">
            <button
              type="submit"
              disabled={busy}
              className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
              data-admin-tenants-submit-tenant
            >
              {busy ? "Saving…" : "Create tenant (+ default org)"}
            </button>
          </div>
        </Form>

        <div className="mt-6 overflow-x-auto rounded border">
          <table className="w-full text-left text-sm" data-admin-tenants-table>
            <thead className="border-b bg-muted/40 text-xs uppercase text-muted-foreground">
              <tr>
                <th className="px-3 py-2">Id</th>
                <th className="px-3 py-2">Name</th>
                <th className="px-3 py-2">Tier</th>
                <th className="px-3 py-2">Status</th>
                <th className="px-3 py-2">Orgs</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td
                    className="px-3 py-4 text-muted-foreground"
                    colSpan={6}
                    data-admin-tenants-empty
                  >
                    No tenants.
                  </td>
                </tr>
              ) : (
                items.map((row) => (
                  <tr
                    key={row.id}
                    className="border-t align-top"
                    data-admin-tenant-row={row.id}
                  >
                    <td className="px-3 py-2 font-mono">{row.id}</td>
                    <td className="px-3 py-2">{row.display_name}</td>
                    <td className="px-3 py-2">
                      <Form
                        method="post"
                        className="flex flex-wrap items-center gap-1"
                        data-admin-tenant-tier-form={row.id}
                      >
                        <input type="hidden" name="intent" value="set-tier" />
                        <input type="hidden" name="id" value={row.id} />
                        <select
                          name="isolation_tier"
                          defaultValue={row.isolation_tier ?? "label"}
                          className="rounded border bg-background px-1 py-0.5 font-mono text-xs"
                          data-admin-tenant-tier-select
                        >
                          <option value="label">label</option>
                          <option value="row">row</option>
                          <option value="db">db</option>
                        </select>
                        <button
                          type="submit"
                          disabled={busy}
                          className="text-xs text-primary hover:underline disabled:opacity-50"
                          data-admin-tenant-tier-submit
                        >
                          Set
                        </button>
                      </Form>
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">
                      {row.status ?? "—"}
                    </td>
                    <td className="px-3 py-2 text-xs">
                      <ul className="space-y-1">
                        {(row.orgs ?? []).map((o) => (
                          <li
                            key={o.id}
                            className="flex flex-wrap items-center gap-2"
                            data-admin-org-row={o.id}
                          >
                            <span className="font-mono">
                              {o.name}
                              {o.name === "default" ? " ★" : ""}
                            </span>
                            <button
                              type="button"
                              className="text-xs text-primary hover:underline"
                              data-admin-org-rename
                              onClick={() =>
                                setRenameOrg({ id: o.id, name: o.name })
                              }
                            >
                              Rename
                            </button>
                            <button
                              type="button"
                              className="text-xs text-destructive hover:underline"
                              data-admin-org-delete
                              onClick={() =>
                                setPending({
                                  kind: "org",
                                  id: o.id,
                                  name: o.name,
                                })
                              }
                            >
                              Delete
                            </button>
                          </li>
                        ))}
                      </ul>
                    </td>
                    <td className="px-3 py-2 text-right">
                      <button
                        type="button"
                        className="text-xs text-destructive hover:underline disabled:opacity-50"
                        disabled={busy}
                        data-admin-tenant-delete
                        onClick={() =>
                          setPending({ kind: "tenant", id: row.id })
                        }
                      >
                        Delete tenant
                      </button>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>

      <section
        className="mt-4 rounded-md border bg-card p-5"
        data-admin-orgs
      >
        <h2 className="font-semibold">Create org under tenant</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Advanced entry. Prefer{" "}
          <a href="/admin/onboard" className="underline">
            Onboard
          </a>{" "}
          for full site setup.
        </p>
        <Form
          method="post"
          className="mt-4 grid gap-3 sm:grid-cols-3"
          data-admin-orgs-form
        >
          <input type="hidden" name="intent" value="create-org" />
          <label className="block text-sm">
            <span className="text-muted-foreground">Tenant id</span>
            <input
              name="tenant_id"
              required
              list="admin-tenant-ids"
              placeholder="acme"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-orgs-input-tenant
            />
            <datalist id="admin-tenant-ids">
              {items.map((t) => (
                <option key={t.id} value={t.id} />
              ))}
            </datalist>
          </label>
          <label className="block text-sm sm:col-span-2">
            <span className="text-muted-foreground">Org name (slug)</span>
            <input
              name="name"
              required
              placeholder="engineering"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-orgs-input-name
            />
          </label>
          <div className="sm:col-span-3">
            <button
              type="submit"
              disabled={busy}
              className="rounded border px-3 py-1.5 text-sm disabled:opacity-50"
              data-admin-orgs-submit
            >
              {busy ? "Saving…" : "Create org"}
            </button>
          </div>
        </Form>
      </section>

      {renameOrg ? (
        <div
          className="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4"
          role="presentation"
          onClick={(e) => {
            if (e.target === e.currentTarget) setRenameOrg(null);
          }}
        >
          <Form
            method="post"
            className="w-full max-w-sm rounded-md border bg-card p-5 shadow-lg"
            data-admin-org-rename-form
            onSubmit={() => setRenameOrg(null)}
          >
            <input type="hidden" name="intent" value="rename-org" />
            <input type="hidden" name="id" value={renameOrg.id} />
            <h3 className="font-semibold">Rename org</h3>
            <p className="mt-1 font-mono text-xs text-muted-foreground">
              {renameOrg.id}
            </p>
            <label className="mt-3 block text-sm">
              <span className="text-muted-foreground">New name (slug)</span>
              <input
                name="name"
                required
                defaultValue={renameOrg.name}
                className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
                data-admin-org-rename-input
              />
            </label>
            <div className="mt-4 flex justify-end gap-2">
              <button
                type="button"
                className="rounded border px-3 py-1.5 text-sm"
                onClick={() => setRenameOrg(null)}
              >
                Cancel
              </button>
              <button
                type="submit"
                disabled={busy}
                className="rounded bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
                data-admin-org-rename-submit
              >
                Rename
              </button>
            </div>
          </Form>
        </div>
      ) : null}

      <ConfirmDialog
        open={pending != null}
        title={
          pending?.kind === "tenant"
            ? "Delete tenant"
            : pending?.kind === "org"
              ? "Delete org"
              : "Confirm"
        }
        description={
          pending?.kind === "tenant" ? (
            <span>
              Permanently deletes tenant{" "}
              <code className="font-mono text-xs">{pending.id}</code>. All
              orgs under it must already be removed.
            </span>
          ) : pending?.kind === "org" ? (
            <span>
              Permanently deletes org{" "}
              <code className="font-mono text-xs">{pending.name}</code> (
              <code className="font-mono text-xs">{pending.id}</code>). Detach
              all sites first.
            </span>
          ) : null
        }
        confirmValue={
          pending?.kind === "tenant"
            ? pending.id
            : pending?.kind === "org"
              ? pending.id
              : ""
        }
        busy={busy}
        onCancel={() => setPending(null)}
        onConfirm={() => {
          if (!pending) return;
          const fd = new FormData();
          if (pending.kind === "tenant") {
            fd.set("intent", "delete-tenant");
            fd.set("id", pending.id);
          } else {
            fd.set("intent", "delete-org");
            fd.set("id", pending.id);
          }
          submit(fd, { method: "post" });
          setPending(null);
        }}
      />
    </AdminShell>
  );
}
