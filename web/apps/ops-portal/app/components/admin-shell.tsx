/**
 * Platform Admin chrome (L109 P801) — secondary nav under OpsShell.
 */
import { Link } from "react-router";
import type { ReactNode } from "react";

import { OpsShell } from "~/components/ops-shell";

const ADMIN_NAV: { to: string; label: string; id: string }[] = [
  { to: "/admin", label: "Overview", id: "overview" },
  { to: "/admin/onboard", label: "Onboard", id: "onboard" },
  { to: "/admin/sites", label: "Sites", id: "sites" },
  { to: "/admin/tenants", label: "Tenants", id: "tenants" },
  { to: "/admin/users", label: "Users", id: "users" },
  { to: "/admin/models", label: "Models", id: "models" },
  { to: "/admin/draw", label: "Site draw", id: "draw" },
];

export function AdminShell(props: {
  title: string;
  children: ReactNode;
  /** Active secondary nav id */
  active?: string;
}) {
  const { title, children, active } = props;
  return (
    <OpsShell
      title={
        <span className="flex flex-col gap-0.5 sm:flex-row sm:items-baseline sm:gap-2">
          <span>Platform Admin</span>
          <span className="text-sm font-normal text-muted-foreground">
            / {title}
          </span>
        </span>
      }
      mainProps={{
        "data-admin-portal": true,
        "data-admin-page": active ?? "overview",
        className: "max-w-5xl",
      }}
    >
      <nav
        aria-label="Platform Admin"
        className="flex flex-wrap gap-2 border-b border-border pb-3 text-sm"
        data-admin-nav
      >
        {ADMIN_NAV.map((item) => {
          const isActive = active === item.id;
          return (
            <Link
              key={item.id}
              to={item.to}
              data-admin-nav-link={item.id}
              aria-current={isActive ? "page" : undefined}
              className={
                isActive
                  ? // PRMT-221: sole X Blue moment in this region (text + 2px left rule)
                    "border-l-2 border-accent px-3 py-1.5 font-medium text-accent-text"
                  : "border-l-2 border-transparent px-3 py-1.5 text-muted-foreground hover:bg-muted hover:text-foreground"
              }
            >
              {item.label}
            </Link>
          );
        })}
      </nav>
      {children}
    </OpsShell>
  );
}
