/**
 * Presentational Runbook + Case viewer (E3.5 — spec-009 §5 KB entry
 * point; M2 P571/P572 seed).
 *
 * Read-only — Phase A per spec-009 §8 / L81 / L88. No case authoring,
 * no write path, no router data calls. Fed by the `runbooks.tsx` route
 * loader, which already passed through the mock seam or live
 * `/api/cases` + `/api/runbooks/{key}` and projected to the local
 * shapes below.
 *
 * `Case` + `Runbook` re-declared locally (same pattern as
 * `alarms-table.tsx` / `object-inspector.tsx` /
 * `anomaly-drilldown.tsx`) so the UI package has zero runtime deps on
 * other workspace packages (L88 UI package boundary). The route loader,
 * which DOES depend on `@cios/api-client`, projects into this local
 * shape.
 */

import type { JSX } from "react";

// `Case` mirrors the canonical `Case` from `@cios/api-client/types.ts`.
// Fields: id, title, summary?, crn?, closed_at?. The cases envelope
// (`{items: Case[]}`) maps from core.listCasesResponse
// (core/runbooks.go L104-106), which re-uses the core.Ticket shape —
// the UI projection intentionally drops the noise (state machine,
// assignee, opened_at, runbook key) and keeps only the KB seed
// surface (M2 P572). Live core.Ticket fields (assignee, resource_version,
// etc.) are NOT mirrored here; that surface is the ticket queue page,
// not the KB viewer.
interface Case {
  id: string;
  title: string;
  summary?: string;
  crn?: string;
  closed_at?: string;
}

// `Runbook` mirrors the canonical `Runbook` from
// `@cios/api-client/types.ts`. The live `/v1/runbooks/{key}` endpoint
// returns text/markdown with no JSON envelope (core/runbooks.go L96-98);
// the route loader wraps the body in `{key, title, body}` for the UI.
// `body` is the raw markdown — rendered as a `<pre>` block (Phase A,
// no markdown pipeline; spec-009 §8 keeps renderers vanilla).
interface Runbook {
  key: string;
  title: string;
  body: string;
}

export interface RunbookViewerProps {
  cases: Case[];
  /** Present when the route loader resolved a `?key=` request. */
  runbook?: Runbook;
}

export function RunbookViewer(props: RunbookViewerProps): JSX.Element {
  const { cases, runbook } = props;
  return (
    <div className="flex flex-col gap-6" data-runbooks-viewer-wrap>
      {/* Case list — always rendered. data-case-row[] is the smoke hook. */}
      <section data-runbooks-cases-section>
        <h2 className="mb-2 text-sm font-semibold uppercase tracking-wide text-muted-foreground">
          Closed cases
        </h2>
        {cases.length === 0 ? (
          <p
            className="rounded-md border bg-card p-4 text-sm text-muted-foreground"
            data-runbooks-cases-empty
          >
            No closed cases in the KB seed yet.
          </p>
        ) : (
          <ul
            className="divide-y rounded-md border bg-card"
            data-runbooks-case-list
          >
            {cases.map((c) => (
              <li
                key={c.id}
                data-case-row
                data-case-id={c.id}
                className="flex flex-col gap-1 px-4 py-3"
              >
                <div className="flex items-center justify-between gap-3">
                  <span
                    className="font-mono text-sm"
                    data-case-cell="id"
                  >
                    {c.id}
                  </span>
                  <span
                    className="font-mono text-xs text-muted-foreground"
                    data-case-cell="closed-at"
                  >
                    {c.closed_at ?? ""}
                  </span>
                </div>
                <div
                  className="text-sm"
                  data-case-cell="title"
                >
                  {c.title}
                </div>
                {c.crn ? (
                  <div
                    className="font-mono text-xs text-muted-foreground"
                    data-case-cell="crn"
                  >
                    {c.crn}
                  </div>
                ) : null}
                {c.summary ? (
                  <div
                    className="text-xs text-muted-foreground"
                    data-case-cell="summary"
                  >
                    {c.summary}
                  </div>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Runbook detail — only when the loader supplied one. */}
      {runbook ? (
        <section data-runbook-detail data-runbook-key={runbook.key}>
          <h2 className="mb-2 text-sm font-semibold uppercase tracking-wide text-muted-foreground">
            Runbook
          </h2>
          <article
            className="flex flex-col gap-2 rounded-md border bg-card p-4"
            data-runbook-detail-body
          >
            <header className="flex items-center justify-between gap-3">
              <h3
                className="font-mono text-sm"
                data-runbook-cell="title"
              >
                {runbook.title}
              </h3>
              <span
                className="font-mono text-xs text-muted-foreground"
                data-runbook-cell="key"
              >
                {runbook.key}
              </span>
            </header>
            <pre
              className="overflow-x-auto whitespace-pre-wrap rounded bg-muted/40 p-3 font-mono text-xs"
              data-runbook-cell="body"
            >
              {runbook.body}
            </pre>
          </article>
        </section>
      ) : null}
    </div>
  );
}
