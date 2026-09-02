/**
 * Phase-A SSR mock-seam helper (PRMT-150).
 *
 * Encapsulates the `MOCK_ENABLED ? mockGet : apiGet(..., apiOptions(bearer))`
 * pattern that PRMT-142..149 duplicated across every protected loader. Loaders
 * call `loadApi<T>(path, session)` and stop branching locally.
 *
 * SSR-only. `path` starts with "/".
 *
 * Note: PRMT-150 §4.1 wrote `type Session` as the example signature. The
 * existing export in `auth.server.ts` is `ApiSession` (the same shape
 * `{user, bearer}`); per §4.1 ("If the exported session type is named
 * differently, import that exact name; do NOT invent or re-declare it"),
 * this file imports the actual `ApiSession` name.
 */
import { apiGet, ApiError } from "@cios/api-client";

import { apiOptions } from "./api.server";
import {
  MOCK_ENABLED,
  mockDelete,
  mockGet,
  mockPost,
  mockPut,
} from "./mock.server";
import type { ApiSession } from "./auth.server";

export async function loadApi<T>(path: string, session: ApiSession): Promise<T> {
  return MOCK_ENABLED
    ? mockGet<T>(path)
    : apiGet<T>(path, apiOptions(session.bearer));
}

/** Max pages loadApiAll will follow. 20 × MaxPageSize(1000) = 20k rows,
 *  which bounds SSR memory. Beyond that the caller must filter/search
 *  (PRMT-220). PRMT-219. */
export const MAX_PAGES = 20;

export type PagedResp<T> = { items?: T[]; next_page_token?: string };

/** Follows next_page_token up to MAX_PAGES. Returns whether the result
 *  was cut short so the caller can tell the user (the bug PRMT-219 fixes
 *  is silent truncation, not truncation itself). PRMT-218 made these
 *  endpoints paginated with a default page size of 1000. */
export async function loadApiAll<T>(
  path: string,
  session: ApiSession,
): Promise<{ items: T[]; truncated: boolean }> {
  const out: T[] = [];
  const seen = new Set<string>();
  let token = "";
  for (let i = 0; i < MAX_PAGES; i++) {
    const sep = path.includes("?") ? "&" : "?";
    const url = token
      ? `${path}${sep}page_token=${encodeURIComponent(token)}`
      : path;
    const data = await loadApi<PagedResp<T>>(url, session);
    if (Array.isArray(data.items)) out.push(...data.items);
    const next = data.next_page_token ?? "";
    if (!next) return { items: out, truncated: false };
    // Defensive: a repeated token would loop forever.
    if (seen.has(next)) return { items: out, truncated: true };
    seen.add(next);
    token = next;
  }
  return { items: out, truncated: true };
}

/**
 * POST helper (PRMT-199 + L109 admin writes). Supports MOCK_GATEWAY=1 via mockPost.
 */
export async function postApi<T = unknown>(
  path: string,
  session: ApiSession,
  body: unknown,
): Promise<T> {
  if (MOCK_ENABLED) {
    return mockPost<T>(path, body);
  }
  const opts = apiOptions(session.bearer);
  const base = opts.baseUrl.replace(/\/$/, "");
  if (!path.startsWith("/")) {
    throw new Error(`postApi: path must start with "/", got ${path}`);
  }
  const res = await fetch(`${base}${path}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${opts.bearer ?? ""}`,
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
    signal: opts.signal,
  });
  const text = await res.text();
  if (!res.ok) {
    throw new ApiError(res.status, text || res.statusText);
  }
  if (!text) {
    return undefined as T;
  }
  return JSON.parse(text) as T;
}

/** PUT helper (L109 model bindings). */
export async function putApi<T = unknown>(
  path: string,
  session: ApiSession,
  body: unknown,
): Promise<T> {
  if (MOCK_ENABLED) {
    return mockPut<T>(path, body);
  }
  const opts = apiOptions(session.bearer);
  const base = opts.baseUrl.replace(/\/$/, "");
  if (!path.startsWith("/")) {
    throw new Error(`putApi: path must start with "/", got ${path}`);
  }
  const res = await fetch(`${base}${path}`, {
    method: "PUT",
    headers: {
      Authorization: `Bearer ${opts.bearer ?? ""}`,
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
    signal: opts.signal,
  });
  const text = await res.text();
  if (!res.ok) {
    throw new ApiError(res.status, text || res.statusText);
  }
  if (!text) {
    return undefined as T;
  }
  return JSON.parse(text) as T;
}

/** DELETE helper (L109 P803). Supports MOCK_GATEWAY=1 via mockDelete. */
export async function deleteApi(
  path: string,
  session: ApiSession,
): Promise<void> {
  if (MOCK_ENABLED) {
    mockDelete(path);
    return;
  }
  const opts = apiOptions(session.bearer);
  const base = opts.baseUrl.replace(/\/$/, "");
  if (!path.startsWith("/")) {
    throw new Error(`deleteApi: path must start with "/", got ${path}`);
  }
  const res = await fetch(`${base}${path}`, {
    method: "DELETE",
    headers: {
      Authorization: `Bearer ${opts.bearer ?? ""}`,
      Accept: "application/json",
    },
    signal: opts.signal,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new ApiError(res.status, text || res.statusText);
  }
}

// twinsScenePath builds the live gateway scene route for a site. Pure + testable.
// Live route = PRMT-170 GET /api/twins/scene?site=<site>. site is the caller-supplied
// value (from noc.3d loader's existing ?site= param); encodeURIComponent guards it.
export function twinsScenePath(site: string): string {
  return `/api/twins/scene?site=${encodeURIComponent(site)}`;
}

/**
 * Client URL for a geometry blob.
 *
 * Prefer same-origin portal proxy (`/api/twins/geometry/<file>`) so the
 * browser is not blocked by missing CORS on apigw. Falls back to absolute
 * gateway URL only when explicitly requested (tests / direct debug).
 */
export function twinsGeometryUrl(
  gatewayBase: string,
  file: string,
  opts?: { sameOrigin?: boolean },
): string {
  const name = file.replace(/^.*\//, "");
  if (opts?.sameOrigin !== false) {
    // Default: same-origin proxy (fixes localhost:3210 → :8089 CORS).
    return `/api/twins/geometry/${encodeURIComponent(name)}`;
  }
  const base = gatewayBase.replace(/\/$/, "");
  return `${base}/api/twins/geometry/${encodeURIComponent(name)}`;
}