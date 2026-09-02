/**
 * Customer Portal chrome: MDCX brand, Status | SLA | Usage | Logout.
 * PRMT-222: wordmark + active nav X Blue (same console X principle as ops).
 * Deliberately separate from ops-portal (no tickets / Set / NOC).
 */
import { Link, useLocation } from "react-router";
import type { ReactNode } from "react";

const NAV: { to: string; label: string; id: string; match: string }[] = [
  { to: "/status", label: "Status", id: "status", match: "/status" },
  { to: "/sla", label: "SLA", id: "sla", match: "/sla" },
  { to: "/usage", label: "Usage", id: "usage", match: "/usage" },
];

export function CustomerShell(props: {
  title: ReactNode;
  children: ReactNode;
  tenantId?: string;
  mainProps?: Record<string, unknown> & { className?: string };
}) {
  const { title, children, tenantId, mainProps } = props;
  const { className: mainClass, ...restMain } = mainProps ?? {};
  const { pathname } = useLocation();

  return (
    <main
      {...(restMain as Record<string, string | number | boolean | undefined>)}
      className={[
        "mx-auto flex min-h-screen max-w-4xl flex-col gap-4 p-6 font-sans",
        mainClass ?? "",
      ]
        .join(" ")
        .trim()}
    >
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-border pb-3">
        <div className="flex flex-wrap items-center gap-3">
          <Link
            to="/status"
            className="flex items-center gap-2 text-muted-foreground hover:text-foreground"
            data-nav-brand
            aria-label="MDCX · CIOS home"
          >
            <img
              src="/brand/mdcx-wordmark-on-light.png"
              alt=""
              width={96}
              height={20}
              className="block h-5 w-auto dark:hidden"
              data-brand-wordmark="light"
            />
            <img
              src="/brand/mdcx-wordmark-on-dark.png"
              alt=""
              width={96}
              height={20}
              className="hidden h-5 w-auto dark:block"
              data-brand-wordmark="dark"
            />
            <span className="text-xs font-semibold uppercase tracking-wide">
              · CIOS
            </span>
          </Link>
          {typeof title === "string" ? (
            <h1 className="text-xl font-semibold">{title}</h1>
          ) : (
            title
          )}
          {tenantId ? (
            <span
              className="rounded bg-muted px-2 py-0.5 font-mono text-xs text-muted-foreground"
              data-tenant-id
            >
              {tenantId}
            </span>
          ) : null}
        </div>
        <nav
          aria-label="Customer portal"
          className="flex flex-wrap items-center gap-x-1 gap-y-1 text-sm"
          data-nav-customer
        >
          {NAV.map((item) => {
            const active =
              pathname === item.match || pathname.startsWith(item.match + "/");
            return (
              <Link
                key={item.id}
                to={item.to}
                data-nav-link={item.id}
                aria-current={active ? "page" : undefined}
                className={
                  active
                    ? "border-l-2 border-accent pl-2 font-medium text-accent-text"
                    : "border-l-2 border-transparent pl-2 text-muted-foreground hover:text-foreground"
                }
              >
                {item.label}
              </Link>
            );
          })}
          <a
            href="/logout"
            className="ml-2 text-sm text-muted-foreground hover:underline"
            data-nav-logout
          >
            Log out
          </a>
        </nav>
      </header>
      {children}
    </main>
  );
}
