/**
 * Telemetry SSE parser + client hook (spec-009 §6, PRMT-149).
 *
 * The gateway streams `TelemetryBatch` JSON (see `pkg/natspub/types.go`) over
 * SSE at `GET /api/sites/{site}/stream`. Each batch's `lines[]` carries
 * Prometheus text-exposition lines tagged `crn="..."`. This module:
 *
 *   - `parsePromLine`   : one promtext line -> {crn, quantity, value} or null.
 *   - `parseBatch`      : one TelemetryBatch -> Sample[].
 *   - `useTelemetryStream(site)` : client hook that opens an EventSource to
 *     the same-origin proxy route `/api/stream/:site` and accumulates the
 *     latest per-(crn,quantity) value, with a `stale` flag for drop/retry.
 *
 * Quantity keys come from spec-002 §3 and are aligned with PRMT-144's
 * `LabelSet` shape (power/utilization/uptime/state). We DO NOT invent
 * quantity names; metrics that don't map to a known quantity return null.
 */

import { useEffect, useRef, useState } from "react";

export interface TelemetryBatch {
  site: string;
  top_asset: string;
  timestamp: string;
  encoding: string;
  lines: string[];
}

export interface Sample {
  crn: string;
  quantity: string;
  value: number;
}

/**
 * Spec-002 §3 quantity name → Prometheus metric suffix map. Only the four
 * quantities PRMT-144's `LabelSet` cares about are listed; anything else
 * is dropped (parsePromLine returns null). Units are NOT included here —
 * `ObjectLabels` already attaches units from its seed (PRMT-144 contract).
 */
const METRIC_TO_QUANTITY: Record<string, string> = {
  cios_power_w: "power",
  cios_utilization_ratio: "utilization",
  cios_uptime_s: "uptime",
  cios_state: "state",
};

/**
 * Parse one Prometheus exposition line, e.g.
 *   `cios_power_w{crn="site01.pod000.cdu000"} 4200`
 *   `cios_utilization_ratio{crn="site01.pod000.cdu000",pod="p0"} 0.62`
 *
 * Returns null for: empty lines, comment lines (`#`), HELP/TYPE lines
 * (also `#` prefix), unknown metric names, or lines where the value is
 * not a finite number. We do NOT invent quantities.
 */
export function parsePromLine(line: string): Sample | null {
  const trimmed = line.trim();
  if (!trimmed || trimmed.startsWith("#")) return null;

  // Split on first whitespace into "<metric>{labels} value".
  const sp = trimmed.search(/\s/);
  if (sp < 0) return null;
  const head = trimmed.slice(0, sp);
  const rest = trimmed.slice(sp + 1).trim();

  // head is `<name>` or `<name>{<labels>}`. Pull the metric name (up to `{`).
  const brace = head.indexOf("{");
  const metric = brace < 0 ? head : head.slice(0, brace);
  const quantity = METRIC_TO_QUANTITY[metric];
  if (!quantity) return null;

  // Extract `crn="..."` from the labels (if any).
  let crn: string | null = null;
  if (brace >= 0) {
    const labelStr = head.slice(brace);
    const m = /crn="((?:[^"\\]|\\.)*)"/.exec(labelStr);
    if (m && m[1] !== undefined) crn = m[1];
  }
  if (!crn) return null;

  // The value is the first whitespace-delimited token of `rest`. A
  // timestamp (millis-since-epoch) MAY follow — we ignore it.
  const valStr = rest.split(/\s+/)[0];
  if (!valStr) return null;
  const value = Number(valStr);
  if (!Number.isFinite(value)) return null;

  return { crn, quantity, value };
}

export function parseBatch(batch: TelemetryBatch): Sample[] {
  const out: Sample[] = [];
  if (!batch || batch.encoding !== "promtext") return out;
  for (const line of batch.lines ?? []) {
    const s = parsePromLine(line);
    if (s) out.push(s);
  }
  return out;
}

export interface TelemetryStreamState {
  /** crn -> quantity -> latest numeric value. */
  samples: Record<string, Record<string, number>>;
  /** true after an error/close until the next message arrives. */
  stale: boolean;
}

/**
 * Client-only hook. Opens `EventSource("/api/stream/" + site)` and
 * accumulates the latest per-(crn,quantity) value. On error/close it
 * sets `stale=true` (keeps last samples) and retries with backoff.
 *
 * No `localStorage` / no `sessionStorage` (PRMT-149 §6).
 *
 * SSR-safe: when `typeof EventSource === "undefined"` (server) it returns
 * an empty, non-stale state and does nothing.
 */
export function useTelemetryStream(site?: string): TelemetryStreamState {
  const [state, setState] = useState<TelemetryStreamState>({
    samples: {},
    stale: false,
  });
  const esRef = useRef<EventSource | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const attemptRef = useRef(0);

  useEffect(() => {
    if (!site) return;
    if (typeof EventSource === "undefined") return;

    let cancelled = false;

    const merge = (batch: TelemetryBatch) => {
      const samples = parseBatch(batch);
      if (samples.length === 0) return;
      setState((prev) => {
        const next: Record<string, Record<string, number>> = {
          ...prev.samples,
        };
        for (const s of samples) {
          const bucket = next[s.crn] ? { ...next[s.crn] } : {};
          bucket[s.quantity] = s.value;
          next[s.crn] = bucket;
        }
        return { samples: next, stale: false };
      });
    };

    const open = () => {
      if (cancelled) return;
      const es = new EventSource(`/api/stream/${encodeURIComponent(site)}`);
      esRef.current = es;
      es.onmessage = (ev: MessageEvent<string>) => {
        attemptRef.current = 0;
        try {
          const batch = JSON.parse(ev.data) as TelemetryBatch;
          merge(batch);
        } catch {
          // Drop malformed frames — do NOT crash the stream.
        }
      };
      es.onerror = () => {
        // Mark stale, close (so the browser stops retrying), then backoff.
        setState((prev) => ({ samples: prev.samples, stale: true }));
        es.close();
        esRef.current = null;
        if (cancelled) return;
        const delay = Math.min(15_000, 500 * 2 ** attemptRef.current);
        attemptRef.current += 1;
        timerRef.current = setTimeout(open, delay);
      };
    };

    open();

    return () => {
      cancelled = true;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = null;
      if (esRef.current) {
        esRef.current.close();
        esRef.current = null;
      }
    };
  }, [site]);

  return state;
}