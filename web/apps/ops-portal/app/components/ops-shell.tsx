/**
 * Shared E3.5 ops-portal chrome: MDCX brand, primary nav, sign-out.
 * PRMT-221: wordmark + active nav = sole X Blue moment (text + 2px left rule).
 */
import { Link, useLocation } from "react-router";
import type { ReactNode } from "react";

const NAV: { to: string; label: string; id: string; match: string }[] = [
  { to: "/", label: "Home", id: "home", match: "/" },
  { to: "/noc", label: "NOC", id: "noc", match: "/noc" },
  { to: "/assets", label: "Assets", id: "assets", match: "/assets" },
  { to: "/alarms", label: "Alarms", id: "alarms", match: "/alarms" },
  { to: "/tickets", label: "Tickets", id: "tickets", match: "/tickets" },
  {
    to: "/maintenance",
    label: "Maintenance",
    id: "maintenance",
    match: "/maintenance",
  },
  { to: "/spares", label: "Spares", id: "spares", match: "/spares" },
  {
    to: "/inspections",
    label: "Inspections",
    id: "inspections",
    match: "/inspections",
  },
  { to: "/runbooks", label: "Runbooks", id: "runbooks", match: "/runbooks" },
  { to: "/reports", label: "Reports", id: "reports", match: "/reports" },
  { to: "/usage", label: "Usage", id: "usage", match: "/usage" },
  { to: "/admin", label: "Admin", id: "admin", match: "/admin" },
];

function isNavActive(pathname: string, match: string): boolean {
  if (match === "/") return pathname === "/";
  if (match === "/noc") {
    // Exact NOC home; 3D and cause are separate.
    return pathname === "/noc";
  }
  return pathname === match || pathname.startsWith(match + "/");
}

export function OpsShell(props: {
  title: ReactNode;
  children: ReactNode;
  /** Extra controls in the title row (e.g. site switcher). */
  titleExtra?: ReactNode;
  /** Props merged onto the outer <main> (data-* page markers). */
  mainProps?: Record<string, unknown> & { className?: string };
}) {
  const { title, children, titleExtra, mainProps } = props;
  const { className: mainClass, ...restMain } = mainProps ?? {};
  const { pathname } = useLocation();

  return (
    <main
      {...(restMain as Record<string, string | number | boolean | undefined>)}
      className={[
        "mx-auto flex min-h-screen max-w-7xl flex-col gap-4 p-6 font-sans",
        mainClass ?? "",
      ]
        .join(" ")
        .trim()}
    >
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-border pb-3">
        <div className="flex flex-wrap items-center gap-3">
          <Link
            to="/"
            className="flex items-center gap-2 text-muted-foreground hover:text-foreground"
            data-nav-brand
            aria-label="MDCX · CIOS Ops home"
          >
            {/* Light theme: dark mark; dark theme: white mark (§1.1). */}
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
              · CIOS Ops
            </span>
          </Link>
          <div className="flex flex-wrap items-center gap-3">
            {typeof title === "string" ? (
              <h1 className="text-xl font-semibold">{title}</h1>
            ) : (
              title
            )}
            {titleExtra}
          </div>
        </div>
        <nav
          aria-label="E3.5 pages"
          className="flex flex-wrap items-center gap-x-1 gap-y-1 text-sm"
          data-nav-e35
        >
          {NAV.map((item) => {
            const active = isNavActive(pathname, item.match);
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
        </nav>
        <a
          href="/logout"
          className="text-sm text-muted-foreground hover:underline"
          data-nav-logout
        >
          Sign out
        </a>
      </header>
      {children}
    </main>
  );
}
