/**
 * Presentational ticket queue (E3.5 — spec-009 §5 ticket entry point).
 *
 * PRMT-199: optional per-row transition forms when `canOperate` is true.
 * Forms POST to the route action (method=post, name=transition).
 * Read-only mode still supported for pure presentational use.
 */

import type { JSX } from "react";

// spec-003 ticket severity vocabulary (PRMT-033 §4.2).
type TicketSeverity = "critical" | "major" | "minor" | "info";
// spec-008 ticket state machine (PRMT-033 §4.1).
type TicketState = "open" | "acknowledged" | "resolved" | "closed";

interface Ticket {
  id: string;
  alarm_id?: string;
  asset_path: string;
  title: string;
  severity: TicketSeverity;
  state: TicketState;
  assignee?: string;
  opened_at: string;
  acked_at?: string;
  resolved_at?: string;
  closed_at?: string;
  escalated_at?: string;
  resource_version: number;
  runbook?: string;
}

/** Legal next states per PRMT-033 §4.1 state machine. */
export function nextTicketStates(state: TicketState): TicketState[] {
  switch (state) {
    case "open":
      return ["acknowledged", "closed"];
    case "acknowledged":
      return ["resolved", "closed"];
    case "resolved":
      return ["closed"];
    default:
      return [];
  }
}

export interface TicketsTableProps {
  tickets: Ticket[];
  /** = `next_page_token` when non-empty; renders a Next control. */
  nextCursor?: string;
  /**
   * When true, render transition forms (PRMT-199). Forms POST to
   * the current route with intent=transition.
   */
  canOperate?: boolean;
}

export function TicketsTable(props: TicketsTableProps): JSX.Element {
  const { tickets, nextCursor, canOperate = false } = props;
  return (
    <div
      className="flex flex-col gap-3"
      data-tickets-table-wrap
      data-tickets-count={tickets.length}
      data-tickets-can-operate={canOperate ? "1" : "0"}
    >
      <table data-tickets-table className="w-full border-collapse text-sm">
        <thead>
          <tr className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="px-2 py-1">ID</th>
            <th className="px-2 py-1">Asset</th>
            <th className="px-2 py-1">Severity</th>
            <th className="px-2 py-1">State</th>
            <th className="px-2 py-1">Title</th>
            <th className="px-2 py-1">Assignee</th>
            {canOperate ? <th className="px-2 py-1">Actions</th> : null}
          </tr>
        </thead>
        <tbody>
          {tickets.length === 0 ? (
            <tr data-tickets-empty>
              <td
                colSpan={canOperate ? 7 : 6}
                className="px-2 py-3 text-center text-sm text-muted-foreground"
              >
                No tickets match the current filters.
              </td>
            </tr>
          ) : (
            tickets.map((t) => {
              const next = nextTicketStates(t.state);
              return (
                <tr
                  key={t.id}
                  data-ticket-row
                  data-ticket-id={t.id}
                  data-severity={t.severity}
                  data-state={t.state}
                >
                  <td className="px-2 py-1 font-mono" data-ticket-cell="id">
                    {t.id}
                  </td>
                  <td className="px-2 py-1 font-mono" data-ticket-cell="asset">
                    {t.asset_path}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-ticket-cell="severity"
                  >
                    {t.severity}
                  </td>
                  <td className="px-2 py-1 font-mono" data-ticket-cell="state">
                    {t.state}
                  </td>
                  <td className="px-2 py-1" data-ticket-cell="title">
                    {t.title}
                  </td>
                  <td
                    className="px-2 py-1 font-mono"
                    data-ticket-cell="assignee"
                  >
                    {t.assignee ?? ""}
                  </td>
                  {canOperate ? (
                    <td className="px-2 py-1" data-ticket-cell="actions">
                      <div
                        className="flex flex-wrap gap-1"
                        data-ticket-actions
                      >
                        {next.map((to) => (
                          <form
                            key={to}
                            method="post"
                            data-ticket-transition-form
                            data-ticket-transition-to={to}
                          >
                            <input type="hidden" name="intent" value="transition" />
                            <input type="hidden" name="id" value={t.id} />
                            <input type="hidden" name="to" value={to} />
                            <button
                              type="submit"
                              data-ticket-action={to}
                              className="rounded border px-2 py-0.5 text-xs hover:bg-muted"
                            >
                              {to}
                            </button>
                          </form>
                        ))}
                        {next.length === 0 ? (
                          <span className="text-xs text-muted-foreground">—</span>
                        ) : null}
                      </div>
                    </td>
                  ) : null}
                </tr>
              );
            })
          )}
        </tbody>
      </table>
      {!canOperate ? (
        <div>
          <button
            type="button"
            disabled
            data-operate-placeholder
            className="rounded border px-3 py-1 text-sm opacity-50"
          >
            Acknowledge / Resolve (read-only)
          </button>
        </div>
      ) : (
        <p
          className="text-xs text-muted-foreground"
          data-tickets-operate-ready
        >
          Ticket transitions enabled (open → acknowledged/closed → resolved →
          closed).
        </p>
      )}
      {nextCursor ? (
        <div>
          <a
            data-next-page
            data-next-page-cursor={nextCursor}
            href={`?cursor=${encodeURIComponent(nextCursor)}`}
            className="text-sm text-muted-foreground hover:underline"
          >
            Next
          </a>
        </div>
      ) : null}
    </div>
  );
}
