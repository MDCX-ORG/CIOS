import { redirect } from "react-router";

import { gatewayPublicBase } from "~/lib/auth.server";

/**
 * Resource route: clear ops session.
 * - Live OIDC: 302 to gateway /auth/ops/logout
 * - DEV_PORTAL_NO_AUTH=1 (local portal-live): no real cookie — bounce home
 * - Empty GATEWAY_PUBLIC_BASE: avoid same-origin 404 on /auth/ops/logout
 */
export function loader() {
  const noAuth =
    process.env.DEV_PORTAL_NO_AUTH === "1" &&
    process.env.NODE_ENV !== "production";
  if (noAuth) {
    return redirect("/?signed_out=dev");
  }

  const base = gatewayPublicBase();
  if (!base) {
    // Misconfigured live auth — soft-land rather than 404.
    return redirect("/login?next=%2F");
  }
  const target = `${base}/auth/ops/logout?next=${encodeURIComponent("/")}`;
  return redirect(target);
}
