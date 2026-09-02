/**
 * Pure Platform Admin gate helpers (L109 P801).
 * No request/env side effects — unit-testable.
 */

export type AdminSessionShape = {
  user: { sub: string; roles: string[] };
};

export function rolesFromJwtPayload(
  payload: Record<string, unknown> | null | undefined,
): string[] {
  if (!payload) return [];
  const out: string[] = [];
  const push = (v: unknown) => {
    if (typeof v === "string" && v.trim()) out.push(v.trim());
  };
  const scope = payload.scope;
  if (Array.isArray(scope)) {
    for (const x of scope) push(x);
  } else if (typeof scope === "string") {
    for (const part of scope.split(/[\s,]+/)) push(part);
  }
  const roles = payload.roles;
  if (Array.isArray(roles)) {
    for (const x of roles) push(x);
  }
  const seen = new Set<string>();
  return out.filter((r) => {
    const k = r.toLowerCase();
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

/** True when session may open Platform Admin (/admin). */
export function sessionIsAdmin(s: AdminSessionShape): boolean {
  const roles = s.user.roles.map((r) => r.toLowerCase());
  if (roles.includes("admin")) return true;
  if (s.user.sub === "svc:lab-admin") return true;
  if (s.user.sub === "dev-no-auth" || s.user.sub === "dev") return true;
  return false;
}
