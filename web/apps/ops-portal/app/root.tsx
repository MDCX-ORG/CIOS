import {
  Links,
  Meta,
  Outlet,
  Scripts,
  ScrollRestoration,
  isRouteErrorResponse,
  useRouteError,
} from "react-router";

import type { Route } from "./+types/root";
import stylesheetUrl from "./app.css?url";

export const links: Route.LinksFunction = () => [
  // Inter (MDCX Design Language §4 / PRMT-221).
  { rel: "preconnect", href: "https://fonts.googleapis.com" },
  {
    rel: "preconnect",
    href: "https://fonts.gstatic.com",
    crossOrigin: "anonymous",
  },
  {
    rel: "stylesheet",
    href: "https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap",
  },
  { rel: "stylesheet", href: stylesheetUrl },
  // Empty data-URI avoids browser → /favicon.ico 404 noise in dev (RR has no route).
  // Mono ink (not X Blue) — favicon is chrome, not the accent moment.
  {
    rel: "icon",
    href: "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='4' fill='%230a0a0a'/%3E%3Ctext x='16' y='22' text-anchor='middle' font-size='14' fill='white' font-family='system-ui'%3EM%3C/text%3E%3C/svg%3E",
  },
];

export function Layout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="h-full">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        {/* PRMT-221: sync .dark with OS preference before paint (class strategy). */}
        <script
          dangerouslySetInnerHTML={{
            __html: `(function(){try{if(window.matchMedia('(prefers-color-scheme: dark)').matches)document.documentElement.classList.add('dark');}catch(e){}})();`,
          }}
        />
        <Meta />
        <Links />
      </head>
      <body className="h-full bg-background font-sans text-foreground">
        {children}
        <ScrollRestoration />
        <Scripts />
      </body>
    </html>
  );
}

export default function App() {
  return <Outlet />;
}

export function ErrorBoundary() {
  const error = useRouteError();
  if (isRouteErrorResponse(error)) {
    return (
      <main className="p-6">
        <h1 className="text-xl font-semibold">
          {error.status} {error.statusText}
        </h1>
        <p className="mt-2 text-sm text-muted-foreground">
          {typeof error.data === "string" ? error.data : "Request failed."}
        </p>
      </main>
    );
  }
  const message = error instanceof Error ? error.message : "Unknown error";
  return (
    <main className="p-6">
      <h1 className="text-xl font-semibold">Application error</h1>
      <p className="mt-2 text-sm text-muted-foreground">{message}</p>
    </main>
  );
}
