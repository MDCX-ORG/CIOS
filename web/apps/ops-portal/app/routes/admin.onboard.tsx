/**
 * /admin/onboard — task-oriented new-site wizard (PRMT-220).
 * Steps: tenant → org → site attach → optional role-binding → confirm.
 * No hand-copied IDs: tenant/org via dropdowns; site slug client-validated.
 */
import { useMemo, useState } from "react";
import {
  Form,
  useActionData,
  useLoaderData,
  useNavigation,
} from "react-router";

import type { Route } from "./+types/admin.onboard";
import { AdminShell } from "~/components/admin-shell";
import { requireAdminSession } from "~/lib/auth.server";
import { adminUserError } from "~/lib/admin-errors";
import { loadApiAll, postApi } from "~/lib/fetch";

type Org = {
  id: string;
  tenant_id: string;
  name: string;
};

type TenantItem = {
  id: string;
  display_name: string;
  orgs?: Org[];
  default_org?: Org | null;
};

/** Site slug grammar: [a-z]{2,8}[0-9]{2}, not ending in 00. */
function validSiteSlug(s: string): boolean {
  if (s.length < 4 || s.length > 10) return false;
  if (!/^[a-z]{2,8}[0-9]{2}$/.test(s)) return false;
  if (s.endsWith("00")) return false;
  return true;
}

function validTenantSlug(s: string): boolean {
  return /^[a-z][a-z0-9-]{1,30}$/.test(s);
}

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  let tenants: TenantItem[] = [];
  let loadError: string | null = null;
  try {
    // Bounded load for picker; operators with >20k tenants should search
    // via Tenants page. Wizard is for typical onboard flows.
    const page = await loadApiAll<TenantItem>("/api/tenants", s);
    tenants = page.items;
    if (page.truncated) {
      loadError =
        "Tenant list is truncated; create a new tenant in the wizard or search on Tenants first.";
    }
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  return { user: s.user, tenants, loadError };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireAdminSession(request);
  const fd = await request.formData();

  const tenantMode = String(fd.get("tenant_mode") ?? "existing");
  const existingTenantId = String(fd.get("existing_tenant_id") ?? "").trim();
  const newTenantId = String(fd.get("new_tenant_id") ?? "").trim();
  const newTenantName = String(fd.get("new_tenant_name") ?? "").trim();

  const orgMode = String(fd.get("org_mode") ?? "existing");
  const existingOrgId = String(fd.get("existing_org_id") ?? "").trim();
  const newOrgName = String(fd.get("new_org_name") ?? "").trim();

  const site = String(fd.get("site") ?? "").trim().toLowerCase();
  const skipBinding = String(fd.get("skip_binding") ?? "") === "1";
  const subject = String(fd.get("subject") ?? "").trim();
  const scope = String(fd.get("scope") ?? "").trim();
  const origin = String(fd.get("origin") ?? "legacy").trim() || "legacy";

  if (!validSiteSlug(site)) {
    return {
      ok: false as const,
      error:
        "Invalid site slug. Use 2–8 lowercase letters + 2 digits (not 00), e.g. sgp01.",
    };
  }

  try {
    let tenantId = existingTenantId;
    let orgId = existingOrgId;
    let defaultOrgId = "";
    const created: string[] = [];

    if (tenantMode === "new") {
      if (!validTenantSlug(newTenantId) || !newTenantName) {
        return {
          ok: false as const,
          error: "New tenant needs a valid id slug and display name.",
        };
      }
      const t = await postApi<{
        id: string;
        default_org?: Org;
      }>("/api/tenants", s, {
        id: newTenantId,
        display_name: newTenantName,
      });
      tenantId = t.id;
      defaultOrgId = t.default_org?.id ?? "";
      created.push(`tenant ${tenantId}`);
    } else if (!tenantId) {
      return { ok: false as const, error: "Select a tenant." };
    }

    if (orgMode === "new") {
      if (!newOrgName || !/^[a-z][a-z0-9-]{1,30}$/.test(newOrgName)) {
        return {
          ok: false as const,
          error: "Org name must be a slug like engineering.",
        };
      }
      const o = await postApi<Org>("/api/orgs", s, {
        tenant_id: tenantId,
        name: newOrgName,
      });
      orgId = o.id;
      created.push(`org ${o.name}`);
    } else if (!orgId && defaultOrgId) {
      // New tenant auto-default when operator did not pick/create another org.
      orgId = defaultOrgId;
    } else if (!orgId) {
      return { ok: false as const, error: "Select an org." };
    }

    await postApi("/api/site-orgs", s, { site, org_id: orgId });
    created.push(`site ${site}`);

    if (!skipBinding) {
      if (!subject || !scope) {
        return {
          ok: false as const,
          error: "Role binding needs subject and scope, or skip the step.",
        };
      }
      await postApi("/api/role-bindings", s, { subject, scope, origin });
      created.push(`binding ${subject} → ${scope}`);
    }

    return {
      ok: true as const,
      error: null,
      summary: {
        tenantId,
        orgId,
        site,
        created,
      },
    };
  } catch (e) {
    return { ok: false as const, error: adminUserError(e) };
  }
}

const STEPS = ["Tenant", "Org", "Site", "Access", "Confirm"] as const;

export default function AdminOnboard() {
  const { user, tenants, loadError } = useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";

  const [step, setStep] = useState(0);
  const [tenantMode, setTenantMode] = useState<"existing" | "new">("existing");
  const [existingTenantId, setExistingTenantId] = useState("");
  const [newTenantId, setNewTenantId] = useState("");
  const [newTenantName, setNewTenantName] = useState("");

  const [orgMode, setOrgMode] = useState<"existing" | "new">("existing");
  const [existingOrgId, setExistingOrgId] = useState("");
  const [newOrgName, setNewOrgName] = useState("");

  const [site, setSite] = useState("");
  const [skipBinding, setSkipBinding] = useState(false);
  const [subject, setSubject] = useState("");
  const [scope, setScope] = useState("");
  const [origin, setOrigin] = useState("legacy");

  const selectedTenant = useMemo(
    () => tenants.find((t) => t.id === existingTenantId),
    [tenants, existingTenantId],
  );
  const orgsForTenant = selectedTenant?.orgs ?? [];

  const siteOk = site === "" || validSiteSlug(site);

  function canNext(): boolean {
    if (step === 0) {
      if (tenantMode === "new") {
        return validTenantSlug(newTenantId) && newTenantName.trim().length > 0;
      }
      return existingTenantId !== "";
    }
    if (step === 1) {
      if (tenantMode === "new" && orgMode === "existing") {
        // New tenant only has default until create; allow "use default".
        return true;
      }
      if (orgMode === "new") return /^[a-z][a-z0-9-]{1,30}$/.test(newOrgName);
      return existingOrgId !== "" || tenantMode === "new";
    }
    if (step === 2) return validSiteSlug(site);
    if (step === 3) {
      if (skipBinding) return true;
      return subject.trim() !== "" && scope.trim() !== "";
    }
    return true;
  }

  const orgLabel = (() => {
    if (orgMode === "new") return newOrgName || "(new org)";
    if (tenantMode === "new" && !existingOrgId) return "default (auto)";
    const o = orgsForTenant.find((x) => x.id === existingOrgId);
    return o ? `${o.name}` : existingOrgId || "—";
  })();

  const tenantLabel =
    tenantMode === "new"
      ? `${newTenantId} (${newTenantName})`
      : existingTenantId || "—";

  return (
    <AdminShell title="Onboard site" active="onboard">
      <section className="rounded-md border bg-card p-5" data-admin-onboard>
        <h2 className="font-semibold">New site wizard</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          Tenant → org → site → optional access grant. No hand-copied IDs.
          admin={user.sub}
        </p>

        {loadError ? (
          <p className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm">
            {loadError}
          </p>
        ) : null}

        {actionData?.ok === false ? (
          <p
            className="mt-3 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            data-admin-onboard-error
          >
            {actionData.error}
          </p>
        ) : null}
        {actionData?.ok === true ? (
          <div
            className="mt-3 rounded border border-border bg-muted px-3 py-2 text-sm"
            data-admin-onboard-ok
          >
            <p className="font-medium">Site onboarded.</p>
            <ul className="mt-1 list-inside list-disc font-mono text-xs">
              {actionData.summary.created.map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
            <p className="mt-2">
              <a
                className="underline"
                href={`/admin/sites?q=${encodeURIComponent(actionData.summary.site)}`}
              >
                View in Sites
              </a>
            </p>
          </div>
        ) : null}

        <ol
          className="mt-4 flex flex-wrap gap-3 font-mono text-sm"
          data-admin-onboard-steps
        >
          {STEPS.map((label, i) => (
            <li
              key={label}
              className={
                i === step
                  ? "border-l-2 border-foreground pl-2 font-bold text-foreground"
                  : i < step
                    ? "border-l-2 border-border pl-2 text-foreground"
                    : "border-l-2 border-transparent pl-2 text-muted-foreground"
              }
              data-admin-onboard-step={i}
              aria-current={i === step ? "step" : undefined}
            >
              {i + 1}. {label}
            </li>
          ))}
        </ol>

        <Form method="post" className="mt-6 space-y-4" data-admin-onboard-form>
          {/* Always submit full state; steps only gate visibility. */}
          <input type="hidden" name="tenant_mode" value={tenantMode} />
          <input
            type="hidden"
            name="existing_tenant_id"
            value={existingTenantId}
          />
          <input type="hidden" name="new_tenant_id" value={newTenantId} />
          <input type="hidden" name="new_tenant_name" value={newTenantName} />
          <input type="hidden" name="org_mode" value={orgMode} />
          <input type="hidden" name="existing_org_id" value={existingOrgId} />
          <input type="hidden" name="new_org_name" value={newOrgName} />
          <input type="hidden" name="site" value={site} />
          <input
            type="hidden"
            name="skip_binding"
            value={skipBinding ? "1" : "0"}
          />
          <input type="hidden" name="subject" value={subject} />
          <input type="hidden" name="scope" value={scope} />
          <input type="hidden" name="origin" value={origin} />

          {step === 0 ? (
            <div className="space-y-3" data-admin-onboard-panel="tenant">
              <div className="flex gap-3 text-sm">
                <label className="flex items-center gap-1">
                  <input
                    type="radio"
                    checked={tenantMode === "existing"}
                    onChange={() => setTenantMode("existing")}
                  />
                  Existing tenant
                </label>
                <label className="flex items-center gap-1">
                  <input
                    type="radio"
                    checked={tenantMode === "new"}
                    onChange={() => setTenantMode("new")}
                  />
                  Create tenant
                </label>
              </div>
              {tenantMode === "existing" ? (
                <label className="block text-sm">
                  <span className="text-muted-foreground">Tenant</span>
                  <select
                    className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
                    value={existingTenantId}
                    onChange={(e) => {
                      setExistingTenantId(e.target.value);
                      setExistingOrgId("");
                    }}
                    data-admin-onboard-tenant-select
                  >
                    <option value="">Select…</option>
                    {tenants.map((t) => (
                      <option key={t.id} value={t.id}>
                        {t.display_name} ({t.id})
                      </option>
                    ))}
                  </select>
                </label>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2">
                  <label className="block text-sm">
                    <span className="text-muted-foreground">Tenant id</span>
                    <input
                      value={newTenantId}
                      onChange={(e) => setNewTenantId(e.target.value.trim())}
                      placeholder="acme"
                      className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
                      data-admin-onboard-tenant-id
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="text-muted-foreground">Display name</span>
                    <input
                      value={newTenantName}
                      onChange={(e) => setNewTenantName(e.target.value)}
                      placeholder="ACME Inc"
                      className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
                      data-admin-onboard-tenant-name
                    />
                  </label>
                </div>
              )}
            </div>
          ) : null}

          {step === 1 ? (
            <div className="space-y-3" data-admin-onboard-panel="org">
              <p className="text-sm text-muted-foreground">
                Tenant: <strong>{tenantLabel}</strong>
              </p>
              <div className="flex gap-3 text-sm">
                <label className="flex items-center gap-1">
                  <input
                    type="radio"
                    checked={orgMode === "existing"}
                    onChange={() => setOrgMode("existing")}
                    disabled={tenantMode === "new"}
                  />
                  Existing org
                </label>
                <label className="flex items-center gap-1">
                  <input
                    type="radio"
                    checked={orgMode === "new"}
                    onChange={() => setOrgMode("new")}
                  />
                  Create org
                </label>
              </div>
              {tenantMode === "new" && orgMode === "existing" ? (
                <p className="text-sm text-muted-foreground">
                  New tenants get a <code className="text-xs">default</code> org
                  automatically — it will be used if you do not create another.
                </p>
              ) : null}
              {orgMode === "existing" && tenantMode === "existing" ? (
                <label className="block text-sm">
                  <span className="text-muted-foreground">Org</span>
                  <select
                    className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
                    value={existingOrgId}
                    onChange={(e) => setExistingOrgId(e.target.value)}
                    data-admin-onboard-org-select
                  >
                    <option value="">Select…</option>
                    {orgsForTenant.map((o) => (
                      <option key={o.id} value={o.id}>
                        {o.name}
                        {o.name === "default" ? " (default)" : ""}
                      </option>
                    ))}
                  </select>
                </label>
              ) : null}
              {orgMode === "new" ? (
                <label className="block text-sm">
                  <span className="text-muted-foreground">Org name (slug)</span>
                  <input
                    value={newOrgName}
                    onChange={(e) => setNewOrgName(e.target.value.trim())}
                    placeholder="engineering"
                    className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
                    data-admin-onboard-org-name
                  />
                </label>
              ) : null}
            </div>
          ) : null}

          {step === 2 ? (
            <div className="space-y-3" data-admin-onboard-panel="site">
              <p className="text-sm text-muted-foreground">
                Org: <strong>{orgLabel}</strong>
              </p>
              <label className="block text-sm">
                <span className="text-muted-foreground">Site slug</span>
                <input
                  value={site}
                  onChange={(e) => setSite(e.target.value.trim().toLowerCase())}
                  placeholder="sgp01"
                  className="mt-1 w-full max-w-xs rounded border bg-background px-2 py-1.5 font-mono text-sm"
                  data-admin-onboard-site
                />
              </label>
              {!siteOk ? (
                <p className="text-sm text-destructive" data-admin-onboard-site-error>
                  Must match [a-z]{"{2,8}"}[0-9]{"{2}"} and not end in 00.
                </p>
              ) : (
                <p className="text-xs text-muted-foreground">
                  Example: sgp01, fra02. Not site00.
                </p>
              )}
            </div>
          ) : null}

          {step === 3 ? (
            <div className="space-y-3" data-admin-onboard-panel="access">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={skipBinding}
                  onChange={(e) => setSkipBinding(e.target.checked)}
                  data-admin-onboard-skip-binding
                />
                Skip role binding for now
              </label>
              {!skipBinding ? (
                <div className="grid gap-3 sm:grid-cols-3">
                  <label className="block text-sm">
                    <span className="text-muted-foreground">Subject</span>
                    <input
                      value={subject}
                      onChange={(e) => setSubject(e.target.value)}
                      placeholder="svc:lab-operator"
                      className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
                      data-admin-onboard-subject
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="text-muted-foreground">Scope</span>
                    <input
                      value={scope}
                      onChange={(e) => setScope(e.target.value)}
                      placeholder={site ? `${site}.**` : "sgp01.**"}
                      className="mt-1 w-full rounded border bg-background px-2 py-1.5 font-mono text-sm"
                      data-admin-onboard-scope
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="text-muted-foreground">Origin</span>
                    <select
                      value={origin}
                      onChange={(e) => setOrigin(e.target.value)}
                      className="mt-1 w-full rounded border bg-background px-2 py-1.5 text-sm"
                    >
                      <option value="legacy">legacy</option>
                      <option value="crn">crn</option>
                    </select>
                  </label>
                </div>
              ) : null}
            </div>
          ) : null}

          {step === 4 ? (
            <div
              className="space-y-2 rounded border bg-muted/30 p-4 text-sm"
              data-admin-onboard-panel="confirm"
            >
              <h3 className="font-medium">Review</h3>
              <dl className="grid gap-1 sm:grid-cols-[8rem_1fr]">
                <dt className="text-muted-foreground">Tenant</dt>
                <dd className="font-mono text-xs">{tenantLabel}</dd>
                <dt className="text-muted-foreground">Org</dt>
                <dd className="font-mono text-xs">{orgLabel}</dd>
                <dt className="text-muted-foreground">Site</dt>
                <dd className="font-mono text-xs">{site}</dd>
                <dt className="text-muted-foreground">Access</dt>
                <dd className="font-mono text-xs">
                  {skipBinding
                    ? "(skipped)"
                    : `${subject} → ${scope} (${origin})`}
                </dd>
              </dl>
              <p className="mt-2 text-muted-foreground">
                Submitting creates any new resources and binds the site in one
                go.
              </p>
            </div>
          ) : null}

          <div className="flex flex-wrap gap-2 pt-2">
            <button
              type="button"
              className="rounded border px-3 py-1.5 text-sm disabled:opacity-50"
              disabled={step === 0 || busy}
              onClick={() => setStep((s) => Math.max(0, s - 1))}
              data-admin-onboard-back
            >
              Back
            </button>
            {step < STEPS.length - 1 ? (
              <button
                type="button"
                className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
                disabled={!canNext() || busy}
                onClick={() => setStep((s) => Math.min(STEPS.length - 1, s + 1))}
                data-admin-onboard-next
              >
                Next
              </button>
            ) : (
              <button
                type="submit"
                className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
                disabled={!canNext() || busy}
                data-admin-onboard-submit
              >
                {busy ? "Submitting…" : "Confirm & create"}
              </button>
            )}
          </div>
        </Form>
      </section>
    </AdminShell>
  );
}
