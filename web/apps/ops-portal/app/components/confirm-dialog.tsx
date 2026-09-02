/**
 * Destructive-action confirm (PRMT-220 / report U-4).
 * Shows the object id and requires the operator to re-type it.
 */
import { useEffect, useId, useState, type ReactNode } from "react";

export function ConfirmDialog(props: {
  open: boolean;
  title: string;
  description?: ReactNode;
  /** Exact string the user must type to enable Confirm. */
  confirmValue: string;
  confirmLabel?: string;
  busy?: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const {
    open,
    title,
    description,
    confirmValue,
    confirmLabel = "Delete",
    busy = false,
    onCancel,
    onConfirm,
  } = props;
  const [typed, setTyped] = useState("");
  const titleId = useId();
  const inputId = useId();

  useEffect(() => {
    if (open) setTyped("");
  }, [open, confirmValue]);

  if (!open) return null;

  const match = typed.trim() === confirmValue;
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      role="presentation"
      data-admin-confirm-dialog
      onClick={(e) => {
        if (e.target === e.currentTarget && !busy) onCancel();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="w-full max-w-md rounded-md border bg-card p-5 shadow-lg"
        data-admin-confirm-panel
      >
        <h2 id={titleId} className="text-base font-semibold">
          {title}
        </h2>
        {description ? (
          <div className="mt-2 text-sm text-muted-foreground">{description}</div>
        ) : null}
        <p className="mt-3 text-sm">
          Type{" "}
          <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
            {confirmValue}
          </code>{" "}
          to confirm.
        </p>
        <label className="mt-2 block text-sm" htmlFor={inputId}>
          <span className="sr-only">Confirmation id</span>
          <input
            id={inputId}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
            autoFocus
            className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
            data-admin-confirm-input
            disabled={busy}
          />
        </label>
        <div className="mt-4 flex justify-end gap-2">
          <button
            type="button"
            className="rounded border px-3 py-1.5 text-sm disabled:opacity-50"
            onClick={onCancel}
            disabled={busy}
            data-admin-confirm-cancel
          >
            Cancel
          </button>
          <button
            type="button"
            className="rounded bg-destructive px-3 py-1.5 text-sm font-medium text-destructive-foreground disabled:opacity-50"
            disabled={!match || busy}
            onClick={onConfirm}
            data-admin-confirm-submit
          >
            {busy ? "Working…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
