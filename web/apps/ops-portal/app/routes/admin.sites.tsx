/**
 * /admin/sites — site→org registry (L109 P802 + PRMT-220 search/delete).
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

import type { Route } from "./+types/admin.sites";
import { AdminShell } from "~/components/admin-shell";
import { ConfirmDialog } from "~/components/confirm-dialog";
import { requireAdminSession } from "~/lib/auth.server";
import { adminUserError } from "~/lib/admin-errors";
import { deleteApi, loadApiAll, postApi } from "~/lib/fetch";

type SiteOrg = {
  site: string;
  org_id: string;
  created_at?: string;
  updated_at?: string;
};

type Org = {
  id: string;
  tenant_id: string;
  name: string;
};

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  const url = new URL(request.url);
  const q = (url.searchParams.get("q") ?? "").trim();
  let items: SiteOrg[] = [];
  let orgs: Org[] = [];
  let truncated = false;
  let loadError: string | null = null;
  try {
    const path = q
      ? `/api/site-orgs?q=${encodeURIComponent(q)}`
      : "/api/site-orgs";
    const page = await loadApiAll<SiteOrg>(path, s);
    items = page.items;
    truncated = page.truncated;
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  // Org dropdown for attach (advanced form still available).
  try {
    // Orgs require tenant_id; skip bulk load — attach form keeps free text
    // only when no org list available. Onboard wizard is the primary path.
    void orgs;
  } catch {
    /* ignore */
  }
  return { user: s.user, items, truncated, loadError, q };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireAdminSession(request);
  const fd = await request.formData();
  const intent = String(fd.get("intent") ?? "attach");

  if (intent === "delete") {
    const site = String(fd.get("site") ?? "").trim();
    if (!site) return { ok: false as const, error: "Site is required" };
    try {
      await deleteApi(
        `/api/site-orgs?site=${encodeURIComponent(site)}`,
        s,
      );
      return { ok: true as const, error: null, intent };
    } catch (e) {
      return { ok: false as const, error: adminUserError(e) };
    }
  }

  const site = String(fd.get("site") ?? "").trim();
  const org_id = String(fd.get("org_id") ?? "").trim();
  if (!site || !org_id) {
    return { ok: false as const, error: "Site and org are required" };
  }
  try {
    await postApi<SiteOrg>("/api/site-orgs", s, { site, org_id });
    return { ok: true as const, error: null, intent: "attach" };
  } catch (e) {
    return { ok: false as const, error: adminUserError(e) };
  }
}

export default function AdminSites() {
  const { user, items, truncated, loadError, q } =
    useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";
  const [searchParams] = useSearchParams();
  const submit = useSubmit();
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  return (
    <AdminShell title="Sites" active="sites">
      <section className="rounded-md border bg-card p-5" data-admin-sites>
        <h2 className="font-semibold">Site registry (site → org)</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          Attach a site slug to an org. Prefer the{" "}
          <a href="/admin/onboard" className="underline">
            Onboard wizard
          </a>{" "}
          so you never hand-copy org ids. Grammar:{" "}
          <code className="text-xs">sgp01</code> (letters + 2 digits, not 00).
        </p>
        <p className="mt-1 font-mono text-xs text-muted-foreground">
          admin={user.sub}
        </p>

        <Form
          method="get"
          className="mt-4 flex flex-wrap items-end gap-2"
          data-admin-sites-search
        >
          <label className="block text-sm">
            <span className="text-muted-foreground">Search site</span>
            <input
              name="q"
              defaultValue={q || searchParams.get("q") || ""}
              placeholder="sgp"
              className="mt-1 w-56 rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-sites-search-input
            />
          </label>
          <button
            type="submit"
            className="rounded border px-3 py-1.5 text-sm"
            data-admin-sites-search-submit
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
            data-admin-sites-load-error
          >
            Load failed: {loadError}
          </p>
        ) : null}
        {actionData?.ok === false ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-sites-action-error
          >
            {actionData.error}
          </p>
        ) : null}
        {actionData?.ok === true ? (
          <p
            className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm"
            data-admin-sites-action-ok
          >
            Saved.
          </p>
        ) : null}

        <Form
          method="post"
          className="mt-4 grid gap-3 sm:grid-cols-3"
          data-admin-sites-form
        >
          <input type="hidden" name="intent" value="attach" />
          <label className="block text-sm">
            <span className="text-muted-foreground">Site slug</span>
            <input
              name="site"
              required
              placeholder="sgp01"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-sites-input-site
            />
          </label>
          <label className="block text-sm sm:col-span-2">
            <span className="text-muted-foreground">
              Org id (use Onboard wizard to avoid hand-copy)
            </span>
            <input
              name="org_id"
              required
              placeholder="og_…"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-sites-input-org
            />
          </label>
          <div className="sm:col-span-3">
            <button
              type="submit"
              disabled={busy}
              className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
              data-admin-sites-submit
            >
              {busy ? "Saving…" : "Attach / re-home"}
            </button>
          </div>
        </Form>

        <div className="mt-6 overflow-x-auto rounded border">
          <table className="w-full text-left text-sm" data-admin-sites-table>
            <thead className="border-b bg-muted/40 text-xs uppercase text-muted-foreground">
              <tr>
                <th className="px-3 py-2">Site</th>
                <th className="px-3 py-2">Org id</th>
                <th className="px-3 py-2">Updated</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td
                    className="px-3 py-4 text-muted-foreground"
                    colSpan={4}
                    data-admin-sites-empty
                  >
                    No site→org rows.
                  </td>
                </tr>
              ) : (
                items.map((row) => (
                  <tr
                    key={row.site}
                    className="border-t"
                    data-admin-site-row={row.site}
                  >
                    <td className="px-3 py-2 font-mono">{row.site}</td>
                    <td className="px-3 py-2 font-mono text-xs">{row.org_id}</td>
                    <td className="px-3 py-2 font-mono text-xs text-muted-foreground">
                      {row.updated_at ?? "—"}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <button
                        type="button"
                        className="text-xs text-destructive hover:underline disabled:opacity-50"
                        disabled={busy}
                        data-admin-site-delete
                        onClick={() => setPendingDelete(row.site)}
                      >
                        Detach
                      </button>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>

      <ConfirmDialog
        open={pendingDelete != null}
        title="Detach site"
        description={
          <span>
            This removes the site→org binding for{" "}
            <code className="font-mono text-xs">{pendingDelete}</code>. The
            site slug is not destroyed; you can re-attach later.
          </span>
        }
        confirmValue={pendingDelete ?? ""}
        confirmLabel="Detach"
        busy={busy}
        onCancel={() => setPendingDelete(null)}
        onConfirm={() => {
          if (!pendingDelete) return;
          const fd = new FormData();
          fd.set("intent", "delete");
          fd.set("site", pendingDelete);
          submit(fd, { method: "post" });
          setPendingDelete(null);
        }}
      />
    </AdminShell>
  );
}
