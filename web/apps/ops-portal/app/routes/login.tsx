import { redirect } from "react-router";

import { gatewayPublicBase, safeNextPath } from "~/lib/auth.server";

// Resource route: 302-redirect to gateway OIDC. No default export
// (intentionally — see PRMT-140 §8 coder notes re: RR7 resource routes).
export function loader({ request }: { request: Request }) {
  const url = new URL(request.url);
  const next = safeNextPath(url.searchParams.get("next"), "/");
  const base = gatewayPublicBase();
  const target = `${base}/auth/ops/login?next=${encodeURIComponent(next)}`;
  return redirect(target);
}
