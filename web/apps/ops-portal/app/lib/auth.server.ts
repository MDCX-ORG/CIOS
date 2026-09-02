/**
 * Server-only auth seam.
 *
 * Reads the `cios_session` cookie, exchanges it for an access_token via
 * `POST {GATEWAY_BASE_URL}/auth/ops/token`, and exposes a `requireSession`
 * guard for protected loaders. The bearer (not the cookie) authenticates
 * `/api/*` calls. Identity decoded from the access_token JWT payload is
 * display-only and unverified — the gateway enforces authorization on
 * `/api/*`.
 *
 * `MOCK_GATEWAY=1` short-circuits the network call.
 */

import { MOCK_ENABLED, mockSession } from "./mock.server";
import { rolesFromJwtPayload, sessionIsAdmin } from "./auth-admin";

export { rolesFromJwtPayload, sessionIsAdmin } from "./auth-admin";

export const REALM = "ops" as const;
export const LOGIN_PATH = "/login";
export const LOGOUT_PATH = "/logout";

export interface SessionUser {
  sub: string;
  realm: "ops";
  /** Roles/scopes from STS token (display + coarse UI gate). Gateway still enforces /api/*. */
  roles: string[];
}

export interface ApiSession {
  user: SessionUser;
  bearer: string;
}

/**
 * requireSession + admin role. Non-admin → 403 (not redirect to login).
 * Unauthenticated → same 302 as requireSession.
 */
export async function requireAdminSession(
  request: Request,
  redirectTo: string = LOGIN_PATH,
): Promise<ApiSession> {
  const s = await requireSession(request, redirectTo);
  if (sessionIsAdmin(s)) return s;
  throw new Response("Forbidden: Platform Admin requires admin role", {
    status: 403,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
}

interface TokenResponse {
  access_token: string;
  token_type: "Bearer";
  expires_in: number;
}

interface JwtPayload {
  sub?: string;
  realm?: string;
  exp?: number;
  iat?: number;
  [k: string]: unknown;
}

/** Same-origin open-redirect guard for `?next=`. */
export function safeNextPath(raw: string | null, fallback = "/"): string {
  if (!raw) return fallback;
  // Must start with "/" but NOT "//" (which would be cross-origin).
  if (!raw.startsWith("/")) return fallback;
  if (raw.startsWith("//")) return fallback;
  // Reject CR/LF injection.
  if (/[\r\n]/.test(raw)) return fallback;
  return raw;
}

function joinUrl(base: string, path: string): string {
  if (!path.startsWith("/")) {
    throw new Error(`joinUrl: path must start with "/", got ${path}`);
  }
  const trimmed = base.endsWith("/") ? base.slice(0, -1) : base;
  return `${trimmed}${path}`;
}

function readSessionCookie(request: Request): string | null {
  const cookieHeader = request.headers.get("cookie");
  if (!cookieHeader) return null;
  for (const part of cookieHeader.split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === "cios_session") {
      return rest.join("=");
    }
  }
  return null;
}

function decodeJwtPayload(token: string): JwtPayload | null {
  // Display-only decode. Server-side `/api/*` is the source of truth for
  // authorization. Portal admin UI gate uses scope/roles as a coarse filter only.
  const segments = token.split(".");
  if (segments.length < 2) return null;
  const seg = segments[1];
  if (!seg) return null;
  try {
    const padded = seg.replace(/-/g, "+").replace(/_/g, "/");
    const json =
      typeof atob === "function"
        ? atob(padded)
        : Buffer.from(padded, "base64").toString("binary");
    return JSON.parse(json) as JwtPayload;
  } catch {
    return null;
  }
}

async function exchangeToken(
  cookie: string,
): Promise<{ access_token: string; expires_in: number } | null> {
  const base = process.env.GATEWAY_BASE_URL;
  if (!base) return null;
  let res: Response;
  try {
    res = await fetch(joinUrl(base, "/auth/ops/token"), {
      method: "POST",
      headers: {
        Cookie: `cios_session=${cookie}`,
        Accept: "application/json",
      },
    });
  } catch {
    return null;
  }
  if (!res.ok) return null;
  let body: TokenResponse;
  try {
    body = (await res.json()) as TokenResponse;
  } catch {
    return null;
  }
  if (!body.access_token) return null;
  return { access_token: body.access_token, expires_in: body.expires_in };
}

export async function getSession(request: Request): Promise<ApiSession | null> {
  if (MOCK_ENABLED) return mockSession();
  // Dev-only live-path bypass (PRMT-173 posture mirror). Default OFF. Impossible in prod.
  if (process.env.DEV_PORTAL_NO_AUTH === "1" && process.env.NODE_ENV !== "production") {
    return {
      user: { sub: "dev-no-auth", realm: "ops", roles: ["admin"] },
      bearer: "dev-no-auth",
    };
  }
  const cookie = readSessionCookie(request);
  if (!cookie) return null;
  const tok = await exchangeToken(cookie);
  if (!tok) return null;
  const payload = decodeJwtPayload(tok.access_token);
  const sub = payload?.sub ?? "unknown";
  const roles = rolesFromJwtPayload(payload);
  return {
    user: { sub, realm: "ops", roles },
    bearer: tok.access_token,
  };
}

export async function getUser(request: Request): Promise<SessionUser | null> {
  const s = await getSession(request);
  return s ? s.user : null;
}

/** Throws a 302 redirect to `/login` (no/invalid session) or the given path. */
export async function requireSession(
  request: Request,
  redirectTo: string = LOGIN_PATH,
): Promise<ApiSession> {
  const s = await getSession(request);
  if (s) return s;
  const url = new URL(request.url);
  const next = url.pathname + url.search;
  const target = `${redirectTo}?next=${encodeURIComponent(next)}`;
  throw new Response(null, {
    status: 302,
    headers: { Location: target },
  });
}

export function gatewayPublicBase(): string {
  return process.env.GATEWAY_PUBLIC_BASE ?? "";
}
