/**
 * @cios/api-client — typed read-only `fetch` wrapper over the gateway `/api/*` surface.
 *
 * Phase A scope: GET only. No retries, no caching, no interceptors. The single
 * POST that exists (token exchange at /auth/ops/token) lives in
 * `apps/ops-portal/app/lib/auth.server.ts`, not here, so the auth contract
 * stays out of this generic wrapper.
 */

export * from "./types";
export interface ApiClientOptions {
  /** Gateway ROOT, no trailing slash (e.g. "http://cios-apigw:8080"). */
  baseUrl: string;
  /** STS access_token; sent as `Authorization: Bearer <bearer>`. */
  bearer?: string;
  /** Extra headers (e.g. `Cookie`) for SSR requests. */
  headers?: HeadersInit;
  signal?: AbortSignal;
}

export class ApiError extends Error {
  readonly status: number;
  readonly problem?: unknown;

  constructor(status: number, message: string, problem?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

function joinUrl(baseUrl: string, path: string): string {
  if (!path.startsWith("/")) {
    throw new Error(`apiGet: path must start with "/", got ${path}`);
  }
  const trimmed = baseUrl.endsWith("/") ? baseUrl.slice(0, -1) : baseUrl;
  return `${trimmed}${path}`;
}

function tryParseProblem(headers: Headers, text: string): unknown | undefined {
  const ct = headers.get("content-type") ?? "";
  if (!ct.toLowerCase().includes("application/problem+json")) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/**
 * Read-only GET. `path` is the full gateway path starting with "/". Appended
 * to `opts.baseUrl`. When `opts.bearer` is set it is attached as the
 * `Authorization` header (gateway `/api/*` is bearer-protected, NOT
 * cookie-protected). On non-2xx: throws `ApiError` (parses RFC 7807 body if
 * present). On 2xx: parses JSON as `T`.
 */
export async function apiGet<T = unknown>(
  path: string,
  opts: ApiClientOptions,
): Promise<T> {
  const url = joinUrl(opts.baseUrl, path);
  const headers = new Headers(opts.headers);
  if (opts.bearer) {
    headers.set("Authorization", `Bearer ${opts.bearer}`);
  }
  if (!headers.has("Accept")) {
    headers.set("Accept", "application/json");
  }

  const res = await fetch(url, {
    method: "GET",
    headers,
    signal: opts.signal,
  });

  const text = await res.text();
  if (!res.ok) {
    const problem = tryParseProblem(res.headers, text);
    throw new ApiError(
      res.status,
      `apiGet ${path} failed: ${res.status} ${res.statusText}`,
      problem,
    );
  }
  if (text.length === 0) return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch (err) {
    throw new ApiError(
      res.status,
      `apiGet ${path}: invalid JSON body (${(err as Error).message})`,
    );
  }
}
