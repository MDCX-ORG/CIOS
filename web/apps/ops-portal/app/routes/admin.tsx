/**
 * /admin — Platform Admin hub (L109 P801).
 * Gate: requireAdminSession. CRUD modules land in P802+.
 */
import { Link, useLoaderData } from "react-router";

import type { Route } from "./+types/admin";
import { AdminShell } from "~/components/admin-shell";
import { requireAdminSession } from "~/lib/auth.server";

const MODULES: {
  to: string;
  title: string;
  blurb: string;
  id: string;
  status: "ready" | "stub";
}[] = [
  {
    to: "/admin/sites",
    title: "Sites",
    blurb: "Site → org attach / re-home (P802)",
    id: "sites",
    status: "ready",
  },
  {
    to: "/admin/tenants",
    title: "Tenants & orgs",
    blurb: "List/create tenants + orgs; default org visible (P804)",
    id: "tenants",
    status: "ready",
  },
  {
    to: "/admin/users",
    title: "Users & bindings",
    blurb: "Subjects × path/crn scopes (P803)",
    id: "users",
    status: "ready",
  },
  {
    to: "/admin/models",
    title: "Model Studio",
    blurb: "USD pack catalog, soft lint, S-layer bindings (P811–P813)",
    id: "models",
    status: "ready",
  },
  {
    to: "/admin/draw",
    title: "Site draw",
    blurb: "2D place + connect → CMDB writeback (P821–P825)",
    id: "draw",
    status: "ready",
  },
];

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  return {
    user: s.user,
  };
}

export default function AdminHub() {
  const { user } = useLoaderData<typeof loader>();
  return (
    <AdminShell title="Overview" active="overview">
      <section
        className="rounded-lg border bg-card p-5 shadow-sm"
        data-admin-hub
      >
        <p className="text-sm text-muted-foreground">Signed in as</p>
        <p className="mt-1 font-mono text-base" data-admin-user-sub>
          {user.sub}
        </p>
        <p className="mt-1 font-mono text-xs text-muted-foreground" data-admin-user-roles>
          roles: {user.roles.length ? user.roles.join(", ") : "(none in token)"}
        </p>
        <p className="mt-3 text-sm text-muted-foreground">
          L109 Platform Admin — manage sites, identity bindings, model packs,
          and site layout. Ops day-to-day pages stay under the main nav.
        </p>
      </section>
      <ul className="mt-4 grid gap-3 sm:grid-cols-2" data-admin-modules>
        {MODULES.map((m) => (
          <li key={m.id}>
            <Link
              to={m.to}
              data-admin-module={m.id}
              className="block rounded-lg border bg-card p-4 shadow-sm transition hover:border-primary"
            >
              <div className="flex items-center justify-between gap-2">
                <h2 className="font-semibold">{m.title}</h2>
                <span
                  className={
                    m.status === "ready"
                      ? "text-xs uppercase tracking-wide text-success"
                      : "text-xs uppercase tracking-wide text-warning"
                  }
                >
                  {m.status}
                </span>
              </div>
              <p className="mt-1 text-sm text-muted-foreground">{m.blurb}</p>
            </Link>
          </li>
        ))}
      </ul>
    </AdminShell>
  );
}
