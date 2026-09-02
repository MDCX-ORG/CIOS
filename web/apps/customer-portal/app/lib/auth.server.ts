/**
 * Customer-portal auth seam (PRMT-207 scaffold + PRMT-213 STS live path).
 *
 * Live path mirrors ops-portal:
 *   cookie `cios_customer_session` → POST {GATEWAY_BASE_URL}/auth/customer/token
 *   → access_token bearer for /api/* calls.
 *
 * Dev bypass: CUSTOMER_DEV_BYPASS=1 or CIOS_CUSTOMER_DEV_BYPASS=1 AND
 * NODE_ENV !== "production". Production always rejects bypass.
 */

export const REALM = "customer" as const;
export const LOGIN_PATH = "/login";
export const LOGOUT_PATH = "/logout";
export const SESSION_COOKIE = "cios_customer_session";
/** Gateway OIDC wire cookie — set by /auth/customer/callback and read
 * back by /auth/customer/token (pkg/apigw sessionCookieWire). The mock
 * SESSION_COOKIE above stays dev-bypass-only. PRMT-228. */
export const GATEWAY_SESSION_COOKIE = "cios_session";

export interface CustomerSession {
  tenantId: string;
  label: string;
  /** Present after live STS exchange; optional for mock bypass. */
  bearer?: string;
  sub?: string;
}

export interface ApiSession {
  tenantId: string;
  label: string;
  bearer: string;
  sub: string;
}

interface TokenResponse {
  access_token: string;
  token_type: "Bearer";
  expires_in: number;
}

interface JwtPayload {
  sub?: string;
  tenant?: string;
  realm?: string;
  [k: string]: unknown;
}

/** Same-origin open-redirect guard for `?next=`. */
export function safeNextPath(raw: string | null, fallback = "/status"): string {
  if (!raw) return fallback;
  if (!raw.startsWith("/")) return fallback;
  if (raw.startsWith("//")) return fallback;
  if (/[\r\n]/.test(raw)) return fallback;
  return raw;
}

export function devBypassEnabled(): boolean {
  if (process.env.NODE_ENV === "production") return false;
  return (
    process.env.CUSTOMER_DEV_BYPASS === "1" ||
    process.env.CIOS_CUSTOMER_DEV_BYPASS === "1"
  );
}

/** Gateway base for live fetches (PRMT-208 endpoints). */
export function customerApiBase(): string {
  return process.env.CUSTOMER_API_BASE ?? process.env.GATEWAY_BASE_URL ?? "http://127.0.0.1:8081";
}

/** Public gateway origin for browser-facing 302s (OIDC login). */
export function gatewayPublicBase(): string {
  return process.env.GATEWAY_PUBLIC_BASE ?? "";
}

function joinUrl(base: string, path: string): string {
  if (!path.startsWith("/")) {
    throw new Error(`joinUrl: path must start with "/", got ${path}`);
  }
  const trimmed = base.endsWith("/") ? base.slice(0, -1) : base;
  return `${trimmed}${path}`;
}

function readCookie(request: Request, name: string): string | null {
  const cookieHeader = request.headers.get("cookie");
  if (!cookieHeader) return null;
  for (const part of cookieHeader.split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === name) {
      return decodeURIComponent(rest.join("="));
    }
  }
  return null;
}

function encodeSession(s: CustomerSession): string {
  const payload = JSON.stringify({
    tenant_id: s.tenantId,
    label: s.label,
  });
  return Buffer.from(payload, "utf8").toString("base64url");
}

function decodeSession(raw: string): CustomerSession | null {
  try {
    const json = Buffer.from(raw, "base64url").toString("utf8");
    const obj = JSON.parse(json) as { tenant_id?: string; label?: string };
    if (!obj.tenant_id || typeof obj.tenant_id !== "string") return null;
    const tenantId = obj.tenant_id.trim();
    if (!tenantId) return null;
    return {
      tenantId,
      label: typeof obj.label === "string" ? obj.label : "",
    };
  } catch {
    return null;
  }
}

export function sessionCookieHeader(session: CustomerSession): string {
  const value = encodeURIComponent(encodeSession(session));
  return `${SESSION_COOKIE}=${value}; Path=/; HttpOnly; SameSite=Lax`;
}

export function clearSessionCookieHeader(): string {
  return `${SESSION_COOKIE}=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0`;
}

function decodeJwtPayload(token: string): JwtPayload | null {
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
    res = await fetch(joinUrl(base, "/auth/customer/token"), {
      method: "POST",
      headers: {
        Cookie: `${GATEWAY_SESSION_COOKIE}=${cookie}`,
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

/**
 * Resolve session: live STS exchange when GATEWAY_BASE_URL set,
 * else mock cookie identity under dev bypass.
 */
export async function getSession(request: Request): Promise<ApiSession | null> {
  // Live STS path (PRMT-213 + PRMT-228): the gateway OIDC callback sets
  // the wire cookie `cios_session`; exchange THAT, not the mock cookie.
  if (process.env.GATEWAY_BASE_URL) {
    const wire = readCookie(request, GATEWAY_SESSION_COOKIE);
    if (wire) {
      const tok = await exchangeToken(wire);
      if (tok) {
        const payload = decodeJwtPayload(tok.access_token);
        const tenantId =
          (typeof payload?.tenant === "string" && payload.tenant) || "unknown";
        return {
          tenantId,
          label: "",
          bearer: tok.access_token,
          sub: payload?.sub ?? `tenant:${tenantId}`,
        };
      }
    }
    // Live exchange unavailable: mock cookie only under dev bypass (lab).
  }

  // Mock cookie path — dev bypass only (production hard-rejects in
  // devBypassEnabled()).
  if (!devBypassEnabled()) return null;
  const raw = readCookie(request, SESSION_COOKIE);
  if (!raw) return null;
  const mock = decodeSession(raw);
  if (!mock) return null;
  return {
    tenantId: mock.tenantId,
    label: mock.label,
    bearer: "dev-customer-bypass",
    sub: `tenant:${mock.tenantId}`,
  };
}

/** Sync helper kept for loaders that still call getSession(request) sync-style.
 * Prefer await getSession. This peeks cookie only (no STS). */
export function getSessionSync(request: Request): CustomerSession | null {
  const raw = readCookie(request, SESSION_COOKIE);
  if (!raw) return null;
  return decodeSession(raw);
}

/** Throws a 302 redirect to `/login` when no session. */
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

/** Build mock session from login form (dev bypass only). */
export function mockLoginSession(
  tenantId: string,
  label: string,
): CustomerSession | null {
  if (!devBypassEnabled()) return null;
  const tid = tenantId.trim();
  if (!tid) return null;
  return { tenantId: tid, label: label.trim() };
}
