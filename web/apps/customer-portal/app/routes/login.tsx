import { Form, redirect, useLoaderData } from "react-router";

import type { Route } from "./+types/login";
import {
  devBypassEnabled,
  gatewayPublicBase,
  getSession,
  mockLoginSession,
  safeNextPath,
  sessionCookieHeader,
} from "~/lib/auth.server";

export async function loader({ request }: Route.LoaderArgs) {
  if (await getSession(request)) {
    return redirect("/status");
  }
  const url = new URL(request.url);
  const next = safeNextPath(url.searchParams.get("next"), "/status");
  // PRMT-228: bypass off → 302 to gateway OIDC (mirrors ops-portal
  // login.tsx). Fall through to the disabled mock form only when no
  // GATEWAY_PUBLIC_BASE is configured (avoids a dead relative redirect).
  if (!devBypassEnabled()) {
    const base = gatewayPublicBase();
    if (base) {
      return redirect(`${base}/auth/customer/login?next=${encodeURIComponent(next)}`);
    }
  }
  return {
    next,
    bypass: devBypassEnabled(),
    error: url.searchParams.get("error"),
  };
}

export async function action({ request }: Route.ActionArgs) {
  const form = await request.formData();
  const tenantId = String(form.get("tenant_id") ?? "");
  const label = String(form.get("label") ?? "");
  const next = safeNextPath(String(form.get("next") ?? "/status"), "/status");

  const session = mockLoginSession(tenantId, label);
  if (!session) {
    return redirect(
      `/login?error=${encodeURIComponent(
        devBypassEnabled()
          ? "tenant_id is required"
          : "Mock login disabled. Set CUSTOMER_DEV_BYPASS=1 for local dev.",
      )}&next=${encodeURIComponent(next)}`,
    );
  }

  return redirect(next, {
    headers: {
      "Set-Cookie": sessionCookieHeader(session),
    },
  });
}

export default function LoginPage() {
  const data = useLoaderData<typeof loader>();
  return (
    <main className="mx-auto flex min-h-screen max-w-md flex-col justify-center gap-6 p-6">
      <div>
        <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          MDCX · CIOS
        </p>
        <h1 className="mt-1 text-2xl font-semibold">Sign in</h1>
        <p className="mt-2 text-sm text-muted-foreground">
          Tenant status, SLA, and usage. Mock login for local development.
        </p>
      </div>

      {!data.bypass ? (
        <div
          className="rounded border border-warning/40 bg-warning/10 p-3 text-sm text-warning"
          role="status"
        >
          Mock login is off. Set{" "}
          <code className="text-xs">CUSTOMER_DEV_BYPASS=1</code> (or{" "}
          <code className="text-xs">CIOS_CUSTOMER_DEV_BYPASS=1</code>) to enable
          local tenant_id login.
        </div>
      ) : null}

      {data.error ? (
        <div
          className="rounded border border-destructive/40 bg-destructive/10 p-3 text-sm"
          role="alert"
        >
          {data.error}
        </div>
      ) : null}

      <Form method="post" className="flex flex-col gap-4">
        <input type="hidden" name="next" value={data.next} />
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium">Tenant ID</span>
          <input
            name="tenant_id"
            type="text"
            required
            autoComplete="organization"
            placeholder="acme"
            className="rounded border border-border bg-background px-3 py-2"
            disabled={!data.bypass}
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium">Label (optional)</span>
          <input
            name="label"
            type="text"
            placeholder="Demo user"
            className="rounded border border-border bg-background px-3 py-2"
            disabled={!data.bypass}
          />
        </label>
        <button
          type="submit"
          disabled={!data.bypass}
          className="rounded bg-foreground px-4 py-2 text-sm font-medium text-background disabled:opacity-50"
        >
          Continue
        </button>
      </Form>
    </main>
  );
}
