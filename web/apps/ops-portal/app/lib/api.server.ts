import type { ApiClientOptions } from "@cios/api-client";

/**
 * Server-only. Builds `ApiClientOptions` for an authenticated `/api/*` call.
 *
 *   - baseUrl = process.env.GATEWAY_BASE_URL  (required; throws if missing)
 *   - bearer  = session.bearer (from `requireSession` / `getSession`)
 *
 * In `MOCK_GATEWAY=1` mode, callers branch to `mock.server.ts` and never
 * reach this helper — the `apiGet` wrapper itself is the generic,
 * non-mockable HTTP surface per @cios/api-client §4.1.
 */
export function apiOptions(
  bearer: string,
  signal?: AbortSignal,
): ApiClientOptions {
  const baseUrl = process.env.GATEWAY_BASE_URL;
  if (!baseUrl || baseUrl.length === 0) {
    throw new Error(
      "GATEWAY_BASE_URL is not set. Set it to the gateway root " +
        "(e.g. http://cios-apigw:8080) or run with MOCK_GATEWAY=1.",
    );
  }
  return { baseUrl, bearer, signal };
}
