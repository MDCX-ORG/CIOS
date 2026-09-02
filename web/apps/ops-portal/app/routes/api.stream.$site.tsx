/**
 * Same-origin SSE proxy for the gateway telemetry stream (PRMT-149, spec-009 §6).
 *
 * Browser `EventSource` cannot set `Authorization: Bearer`, and `/api/*` on
 * the gateway is bearer-protected (L81). So the portal re-exposes the
 * gateway stream as `GET /api/stream/:site`:
 *
 *   - Live mode: server-side `fetch(GATEWAY_BASE_URL + "/api/sites/" + site + "/stream")`
 *     with the session bearer, and the upstream body is piped through VERBATIM
 *     (spec-009 §6.1 / §7.1 — Gateway/portal carries identity, not data
 *     semantics). The bearer NEVER leaves the server.
 *   - Mock mode (`MOCK_GATEWAY=1`): emit one canned `data:` event whose JSON
 *     is a valid `TelemetryBatch` with ≥1 promtext line for `site`, then a
 *     SSE keep-alive comment.
 *
 * `site` is validated against `^[a-z]{2,8}[0-9]{2}$` — the same shape the
 * gateway SSE handler enforces (`pkg/apigw/sse.go::parseSiteFromStreamPath`).
 *
 * Loader-only resource route (no default export) — React Router does not
 * render a page for this URL.
 */

import type { Route } from "./+types/api.stream.$site";
import { requireSession } from "~/lib/auth.server";
import { MOCK_ENABLED } from "~/lib/mock.server";

const SITE_RE = /^[a-z]{2,8}[0-9]{2}$/;

function sseHeaders(): HeadersInit {
  return {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  };
}

function jsonEvent(data: unknown): Uint8Array {
  // One SSE `data:` frame terminated by a blank line.
  const payload = `data: ${JSON.stringify(data)}\n\n`;
  return new TextEncoder().encode(payload);
}

function keepAliveComment(): Uint8Array {
  return new TextEncoder().encode(`:keep-alive\n\n`);
}

export async function loader({ request, params }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const site = params.site;
  if (!site || !SITE_RE.test(site)) {
    return new Response("bad site", { status: 400 });
  }

  // Mock seam (PRMT-149 §4.1): emit one canned TelemetryBatch with ≥1 valid
  // promtext line for `site`, then keep-alive comments forever. Mirrors the
  // mock telemetry fixture already in use elsewhere (R4 labels).
  if (MOCK_ENABLED) {
    const encoder = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const batch = {
          site,
          top_asset: `${site}.pod000`,
          timestamp: new Date().toISOString(),
          encoding: "promtext",
          lines: [
            `cios_power_w{crn="${site}.pod000.cdu000"} 4300`,
            `cios_utilization_ratio{crn="${site}.pod000.cdu000"} 0.65`,
            `cios_uptime_s{crn="${site}.pod000.cdu000"} 90061`,
            `cios_state{crn="${site}.pod000.cdu000"} 0`,
          ],
        };
        controller.enqueue(encoder.encode(":ok\n\n"));
        controller.enqueue(jsonEvent(batch));
        // Heartbeats so middleware/proxies don't sever the connection.
        const t = setInterval(() => {
          try {
            controller.enqueue(keepAliveComment());
          } catch {
            clearInterval(t);
          }
        }, 15_000);
        // The interval is held by the closure; it is cleared on cancel().
        // (ReadableStreamDefaultController doesn't expose cancel() here,
        // but teardown happens when the request signal fires — the
        // interval will then throw on enqueue and clear itself.)
        request.signal.addEventListener("abort", () => {
          clearInterval(t);
          try {
            controller.close();
          } catch {
            // already closed
          }
        });
      },
    });
    return new Response(stream, { status: 200, headers: sseHeaders() });
  }

  // Live mode: pipe the gateway stream VERBATIM (spec-009 §6.1). The portal
  // does NOT parse promtext here — the client hook does.
  const base = process.env.GATEWAY_BASE_URL ?? "";
  if (!base) {
    return new Response("gateway base url not configured", { status: 502 });
  }
  const upstreamUrl = `${base.replace(/\/$/, "")}/api/sites/${site}/stream`;
  const upstream = await fetch(upstreamUrl, {
    headers: { Authorization: `Bearer ${s.bearer}` },
    signal: request.signal,
  });
  if (!upstream.ok || !upstream.body) {
    return new Response("upstream unavailable", { status: 502 });
  }
  // Tee upstream.body straight into our response. No buffering, no parsing.
  return new Response(upstream.body, {
    status: 200,
    headers: sseHeaders(),
  });
}