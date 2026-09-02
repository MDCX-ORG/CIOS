/**
 * User-facing error text for admin actions (PRMT-220 / report U-3).
 * Never surface raw HTTP status codes or page_token jargon.
 */
import { ApiError } from "@cios/api-client";

export function adminUserError(e: unknown): string {
  if (e instanceof ApiError) {
    const raw = e.message?.trim() || "";
    if (raw.startsWith("{")) {
      try {
        const j = JSON.parse(raw) as {
          detail?: string;
          title?: string;
          type?: string;
        };
        if (j.detail && typeof j.detail === "string") return j.detail;
        if (j.title && typeof j.title === "string") return j.title;
        if (j.type && typeof j.type === "string") {
          const tail = j.type.split("/").pop() ?? j.type;
          if (tail === "org-owns-resources") {
            return "This org still has site bindings. Detach sites first.";
          }
          if (tail === "tenant-owns-orgs") {
            return "This tenant still has orgs. Delete orgs first.";
          }
          if (tail === "tier-downgrade") {
            return "Isolation tier can only be raised, not lowered.";
          }
          if (tail === "tenant-exists") {
            return "A tenant with this id already exists.";
          }
          if (tail === "org-name-conflict") {
            return "An org with this name already exists under the tenant.";
          }
          return tail.replace(/-/g, " ");
        }
      } catch {
        /* fall through */
      }
    }
    if (raw) return raw;
    return "Request failed. Please try again.";
  }
  if (e instanceof Error) return e.message;
  return String(e);
}
