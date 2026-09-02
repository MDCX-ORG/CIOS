/**
 * /noc/cause/:crn — anomaly drilldown (R3, spec-009 §5.2).
 *
 *   - Fetches /api/topology + /api/alarms via the mock seam (or live,
 *     gated on PRMT-151 / feature/m3-auth — see §8).
 *   - Computes a deterministic root-cause (nearest upstream firing node
 *     by BFS into the target, lex tie-break) and impact (BFS out of the
 *     target, depth ≤4, cycle-safe). No inference engine.
 *   - Renders `<AnomalyDrilldown>` with a disabled "Operate (read-only)"
 *     placeholder (spec-009 §8 — Phase A has no write path).
 *
 * Empty data → graceful empty state (no crash). Read-only: no
 * POST/PUT/DELETE/PATCH is issued.
 */

import { Link } from "react-router";

import type { Route } from "./+types/noc.cause.$crn";
import { AnomalyDrilldown } from "@cios/ui";
import type {
  Alarm,
  CauseAnalysis,
  EdgeKind,
  Paged,
  Topology,
} from "@cios/api-client";

import { requireSession } from "~/lib/auth.server";
import { loadApi } from "~/lib/fetch";

const IMPACT_MAX_DEPTH = 4;

/**
 * Impact = all crns reachable from `target` by following edges OUT
 * (from === current), breadth-first, recording the first edge `via`;
 * cap depth at 4 (deterministic, cycle-safe via a visited set).
 */
function computeImpact(
  edges: { from: string; to: string; kind: EdgeKind }[],
  target: string,
): { crn: string; via: EdgeKind }[] {
  const out: { crn: string; via: EdgeKind }[] = [];
  const visited = new Set<string>([target]);
  let frontier: { crn: string; via: EdgeKind }[] = edges
    .filter((e) => e.from === target)
    .map((e) => ({ crn: e.to, via: e.kind }));
  let depth = 0;
  while (frontier.length > 0 && depth < IMPACT_MAX_DEPTH) {
    const next: { crn: string; via: EdgeKind }[] = [];
    for (const node of frontier) {
      if (visited.has(node.crn)) continue;
      visited.add(node.crn);
      out.push(node);
      for (const e of edges) {
        if (e.from === node.crn && !visited.has(e.to)) {
          next.push({ crn: e.to, via: e.kind });
        }
      }
    }
    frontier = next;
    depth += 1;
  }
  return out;
}

/**
 * Root cause = nearest upstream firing node. Walk edges INTO target
 * (to === current) one hop at a time; on each ring collect upstream
 * nodes that themselves carry a firing alarm; the first such ring
 * (lex tie-break for determinism) yields the root cause. If none,
 * undefined ("indeterminate"). The `via` is the first-hop edge kind.
 */
function computeRootCause(
  edges: { from: string; to: string; kind: EdgeKind }[],
  target: string,
  firing: Set<string>,
): { crn: string; via: EdgeKind } | undefined {
  // BFS backwards from target, level by level, capped at IMPACT_MAX_DEPTH.
  const visited = new Set<string>([target]);
  let frontier: { crn: string; via: EdgeKind }[] = edges
    .filter((e) => e.to === target)
    .map((e) => ({ crn: e.from, via: e.kind }));
  let depth = 0;
  while (frontier.length > 0 && depth < IMPACT_MAX_DEPTH) {
    const candidates: { crn: string; via: EdgeKind }[] = [];
    const next: { crn: string; via: EdgeKind }[] = [];
    for (const node of frontier) {
      if (visited.has(node.crn)) continue;
      visited.add(node.crn);
      if (firing.has(node.crn)) candidates.push(node);
      for (const e of edges) {
        if (e.to === node.crn && !visited.has(e.from)) {
          next.push({ crn: e.from, via: e.kind });
        }
      }
    }
    if (candidates.length > 0) {
      candidates.sort((a, b) => a.crn.localeCompare(b.crn));
      const first = candidates[0];
      return first ? { crn: first.crn, via: first.via } : undefined;
    }
    frontier = next;
    depth += 1;
  }
  return undefined;
}

export async function loader({ request, params }: Route.LoaderArgs) {
  const s = await requireSession(request);
  const target = params.crn ?? "";

  const topology: Topology = await loadApi<Topology>("/api/topology", s);

  const alarms: Paged<Alarm> = await loadApi<Paged<Alarm>>("/api/alarms", s);

  const firing = new Set<string>(
    alarms.items
      .filter((a) => a.state === "firing")
      .map((a) => a.crn),
  );

  const analysis: CauseAnalysis = {
    target,
    rootCause: computeRootCause(topology.edges, target, firing),
    impact: computeImpact(topology.edges, target),
  };

  const alarm = alarms.items.find((a) => a.crn === target && a.state === "firing");

  return { analysis, alarm };
}

export default function CauseDrilldown({ loaderData }: Route.ComponentProps) {
  const { analysis, alarm } = loaderData;
  const empty = !analysis.impact.length && !analysis.rootCause && !alarm;
  return (
    <main
      data-cause-ready
      data-cause-target={analysis.target}
      data-cause-empty={empty ? "true" : "false"}
      className="mx-auto flex min-h-screen max-w-3xl flex-col gap-4 p-6"
    >
      <header className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-4">
          <h1 className="text-xl font-semibold">Anomaly drilldown</h1>
          <span className="font-mono text-sm" data-cause-crn>
            {analysis.target}
          </span>
        </div>
        <Link
          to={`/noc?focus=${encodeURIComponent(analysis.target)}`}
          className="text-sm text-muted-foreground hover:underline"
          data-cause-back
        >
          Back to NOC
        </Link>
      </header>

      {empty ? (
        <section
          data-cause-empty-state
          className="rounded-md border bg-card p-4 text-sm text-muted-foreground"
        >
          No topology or firing alarm recorded for this crn.
        </section>
      ) : (
        <AnomalyDrilldown analysis={analysis} alarm={alarm} />
      )}
    </main>
  );
}
