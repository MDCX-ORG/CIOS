/**
 * /admin/users — role bindings admin (L109 P803 + PRMT-220 search).
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

import type { Route } from "./+types/admin.users";
import { AdminShell } from "~/components/admin-shell";
import { ConfirmDialog } from "~/components/confirm-dialog";
import { requireAdminSession } from "~/lib/auth.server";
import { adminUserError } from "~/lib/admin-errors";
import { deleteApi, loadApiAll, postApi } from "~/lib/fetch";

type RoleBinding = {
  id: string;
  subject: string;
  scope: string;
  origin: string;
  created_at?: string;
  updated_at?: string;
};

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  const url = new URL(request.url);
  const q = (url.searchParams.get("q") ?? "").trim();
  let items: RoleBinding[] = [];
  let truncated = false;
  let loadError: string | null = null;
  try {
    // Reuse backend subject / q filters (PRMT-220).
    const path = q
      ? `/api/role-bindings?q=${encodeURIComponent(q)}`
      : "/api/role-bindings";
    const page = await loadApiAll<RoleBinding>(path, s);
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
  const intent = String(fd.get("intent") ?? "create");

  if (intent === "delete") {
    const subject = String(fd.get("subject") ?? "").trim();
    const scope = String(fd.get("scope") ?? "").trim();
    if (!subject || !scope) {
      return {
        ok: false as const,
        error: "Subject and scope required for delete",
      };
    }
    const qs = new URLSearchParams({ subject, scope });
    try {
      await deleteApi(`/api/role-bindings?${qs.toString()}`, s);
      return { ok: true as const, error: null };
    } catch (e) {
      return { ok: false as const, error: adminUserError(e) };
    }
  }

  const subject = String(fd.get("subject") ?? "").trim();
  const scope = String(fd.get("scope") ?? "").trim();
  const origin = String(fd.get("origin") ?? "legacy").trim() || "legacy";
  if (!subject || !scope) {
    return { ok: false as const, error: "Subject and scope are required" };
  }
  try {
    await postApi("/api/role-bindings", s, { subject, scope, origin });
    return { ok: true as const, error: null };
  } catch (e) {
    return { ok: false as const, error: adminUserError(e) };
  }
}

export default function AdminUsers() {
  const { user, items, truncated, loadError, q } =
    useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";
  const [searchParams] = useSearchParams();
  const submit = useSubmit();
  const [pending, setPending] = useState<{
    subject: string;
    scope: string;
  } | null>(null);

  return (
    <AdminShell title="Users & bindings" active="users">
      <section className="rounded-md border bg-card p-5" data-admin-users>
        <h2 className="font-semibold">Role bindings</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          Grant path-glob or crn scopes to subjects. Search filters by subject
          substring on the server.
        </p>
        <p className="mt-1 font-mono text-xs text-muted-foreground">
          admin={user.sub}
        </p>

        <Form
          method="get"
          className="mt-4 flex flex-wrap items-end gap-2"
          data-admin-users-search
        >
          <label className="block text-sm">
            <span className="text-muted-foreground">Search subject</span>
            <input
              name="q"
              defaultValue={q || searchParams.get("q") || ""}
              placeholder="svc:lab"
              className="mt-1 w-56 rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-users-search-input
            />
          </label>
          <button
            type="submit"
            className="rounded border px-3 py-1.5 text-sm"
            data-admin-users-search-submit
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
            data-admin-users-load-error
          >
            Load failed: {loadError}
          </p>
        ) : null}
        {actionData?.ok === false ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-users-action-error
          >
            {actionData.error}
          </p>
        ) : null}
        {actionData?.ok === true ? (
          <p
            className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm"
            data-admin-users-action-ok
          >
            Saved.
          </p>
        ) : null}

        <Form
          method="post"
          className="mt-4 grid gap-3 sm:grid-cols-4"
          data-admin-users-form
        >
          <input type="hidden" name="intent" value="create" />
          <label className="block text-sm sm:col-span-1">
            <span className="text-muted-foreground">Subject</span>
            <input
              name="subject"
              required
              placeholder="svc:lab-operator"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-users-input-subject
            />
          </label>
          <label className="block text-sm sm:col-span-2">
            <span className="text-muted-foreground">Scope</span>
            <input
              name="scope"
              required
              placeholder="sgp01.**"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
              data-admin-users-input-scope
            />
          </label>
          <label className="block text-sm">
            <span className="text-muted-foreground">Origin</span>
            <select
              name="origin"
              defaultValue="legacy"
              className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
              data-admin-users-input-origin
            >
              <option value="legacy">legacy (dot-glob)</option>
              <option value="crn">crn</option>
            </select>
          </label>
          <div className="sm:col-span-4">
            <button
              type="submit"
              disabled={busy}
              className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
              data-admin-users-submit
            >
              {busy ? "Saving…" : "Add / update binding"}
            </button>
          </div>
        </Form>

        <div className="mt-6 overflow-x-auto rounded border">
          <table className="w-full text-left text-sm" data-admin-users-table>
            <thead className="border-b bg-muted/40 text-xs uppercase text-muted-foreground">
              <tr>
                <th className="px-3 py-2">Subject</th>
                <th className="px-3 py-2">Scope</th>
                <th className="px-3 py-2">Origin</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td
                    className="px-3 py-4 text-muted-foreground"
                    colSpan={4}
                    data-admin-users-empty
                  >
                    No role bindings.
                  </td>
                </tr>
              ) : (
                items.map((row) => (
                  <tr
                    key={`${row.subject}:${row.scope}`}
                    className="border-t"
                    data-admin-binding-row={`${row.subject}|${row.scope}`}
                  >
                    <td className="px-3 py-2 font-mono text-xs">
                      {row.subject}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">{row.scope}</td>
                    <td className="px-3 py-2 text-xs">{row.origin}</td>
                    <td className="px-3 py-2 text-right">
                      <button
                        type="button"
                        disabled={busy}
                        className="text-xs text-destructive hover:underline disabled:opacity-50"
                        data-admin-binding-delete
                        onClick={() =>
                          setPending({
                            subject: row.subject,
                            scope: row.scope,
                          })
                        }
                      >
                        Delete
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
        open={pending != null}
        title="Delete role binding"
        description={
          pending ? (
            <span>
              Remove grant{" "}
              <code className="font-mono text-xs">{pending.subject}</code> →{" "}
              <code className="font-mono text-xs">{pending.scope}</code>.
            </span>
          ) : null
        }
        confirmValue={pending ? pending.subject : ""}
        busy={busy}
        onCancel={() => setPending(null)}
        onConfirm={() => {
          if (!pending) return;
          const fd = new FormData();
          fd.set("intent", "delete");
          fd.set("subject", pending.subject);
          fd.set("scope", pending.scope);
          submit(fd, { method: "post" });
          setPending(null);
        }}
      />
    </AdminShell>
  );
}
