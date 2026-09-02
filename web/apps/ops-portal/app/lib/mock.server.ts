/**
 * Active only when process.env.MOCK_GATEWAY === "1".
 *
 * Provides canned responses so acceptance runs without a live gateway/IdP:
 *   - getSession  -> { user: { sub: "dev", realm: "ops" }, bearer: "mock-token" }
 *   - apiGet      -> { items: [], next_page_token: "" }   (per path)
 *
 * `auth.server.ts` / `_index.tsx` MUST branch to these when
 * `MOCK_GATEWAY === "1"`. `api.server.ts` does NOT short-circuit because
 * `apiGet` is the generic, non-mockable wrapper per @cios/api-client §4.1.
 */

export const MOCK_ENABLED: boolean = process.env.MOCK_GATEWAY === "1";

export function mockSession(): {
  user: { sub: string; realm: "ops"; roles: string[] };
  bearer: string;
} {
  // L109 admin shell: mock path is lab-only and treated as admin for UI work.
  return {
    user: { sub: "dev", realm: "ops", roles: ["admin"] },
    bearer: "mock-token",
  };
}

/** Mutable ticket fixture store for write-path tests (PRMT-199). */
type MockTicket = {
  id: string;
  alarm_id: string;
  asset_path: string;
  title: string;
  severity: string;
  state: string;
  assignee: string;
  opened_at: string;
  acked_at?: string;
  resolved_at?: string;
  closed_at?: string;
  resource_version: number;
};

const MOCK_TICKETS: MockTicket[] = [
  {
    id: "tk_AAAAAAAAAAAA0001",
    alarm_id: "",
    asset_path: "site01.pod000.cdu000",
    title: "Coolant supply temp high",
    severity: "major",
    state: "open",
    assignee: "",
    opened_at: "2026-06-28T08:00:00Z",
    resource_version: 1,
  },
  {
    id: "tk_AAAAAAAAAAAA0002",
    alarm_id: "",
    asset_path: "site02.pod001.cdu001",
    title: "Coolant leak detected",
    severity: "critical",
    state: "acknowledged",
    assignee: "ops-na/alice",
    opened_at: "2026-06-28T07:30:00Z",
    acked_at: "2026-06-28T07:45:00Z",
    resource_version: 2,
  },
  {
    id: "tk_AAAAAAAAAAAA0003",
    alarm_id: "",
    asset_path: "site01.pod000",
    title: "Pod 000 minor threshold drift",
    severity: "minor",
    state: "open",
    assignee: "",
    opened_at: "2026-06-28T06:15:00Z",
    resource_version: 1,
  },
  {
    id: "tk_AAAAAAAAAAAA0004",
    alarm_id: "",
    asset_path: "site02.pod001",
    title: "PM: CDU filter replacement",
    severity: "info",
    state: "resolved",
    assignee: "ops-na/bob",
    opened_at: "2026-06-27T12:00:00Z",
    resolved_at: "2026-06-27T14:00:00Z",
    resource_version: 3,
  },
  {
    // PRMT-233: predict-scanner fixture (core/predict.go openPredictTicket).
    id: "tk_AAAAAAAAAAAA0005",
    alarm_id: "predict:site01.pod000.cdu000",
    asset_path: "site01.pod000.cdu000",
    title: "predictive: vibration anomaly (z=3.42)",
    severity: "major",
    state: "open",
    assignee: "",
    opened_at: "2026-08-30T02:00:00Z",
    resource_version: 1,
  },
];

const LEGAL: Record<string, string[]> = {
  open: ["acknowledged", "closed"],
  acknowledged: ["resolved", "closed"],
  resolved: ["closed"],
  closed: [],
};

/** PRMT-199: mock ticket state-machine transition. */
export function mockTicketTransition(
  id: string,
  to: string,
): { ok: true } | { ok: false; error: string } {
  const t = MOCK_TICKETS.find((x) => x.id === id);
  if (!t) return { ok: false, error: "ticket-not-found" };
  const next = LEGAL[t.state] ?? [];
  if (!next.includes(to)) {
    return { ok: false, error: `illegal-transition:${t.state}→${to}` };
  }
  t.state = to;
  t.resource_version += 1;
  const now = new Date().toISOString();
  if (to === "acknowledged") t.acked_at = now;
  if (to === "resolved") t.resolved_at = now;
  if (to === "closed") t.closed_at = now;
  return { ok: true };
}

// --- L109 admin mock stores (in-process; reset per process) ---
type MockSiteOrg = {
  site: string;
  org_id: string;
  created_at: string;
  updated_at: string;
};
type MockRoleBinding = {
  id: string;
  subject: string;
  scope: string;
  origin: string;
  created_at: string;
  updated_at: string;
};
/** L109 P804 — mirrors core.Tenant + core.Org wire shapes. */
type MockOrg = {
  id: string;
  tenant_id: string;
  name: string;
  created_at: string;
};
type MockTenant = {
  id: string;
  display_name: string;
  isolation_tier: string;
  status: string;
  created_at: string;
  updated_at: string;
};

const MOCK_SITE_ORGS: MockSiteOrg[] = [
  {
    site: "sgp01",
    org_id: "og_LABDEFAULT00001",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
  },
];
const MOCK_ROLE_BINDINGS: MockRoleBinding[] = [
  {
    id: "rb_LABOPERATOR00001",
    subject: "svc:lab-operator",
    scope: "sgp01.**",
    origin: "legacy",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
  },
];
// Seed lab tenant so Sites can use a real org_id without a live core.
const MOCK_TENANTS: MockTenant[] = [
  {
    id: "lab",
    display_name: "Lab Default",
    isolation_tier: "label",
    status: "active",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
  },
];
const MOCK_ORGS: MockOrg[] = [
  {
    id: "og_LABDEFAULT00001",
    tenant_id: "lab",
    name: "default",
    created_at: "2026-07-01T00:00:00Z",
  },
];
let mockOrgSeq = 1;

function newMockOrgId(): string {
  // Shape loosely matches core: og_ + 16 alnum (mock is not base32-strict).
  const n = String(mockOrgSeq++).padStart(12, "0");
  return `og_MOCK${n}`;
}

function mockTenantListItems() {
  return MOCK_TENANTS.map((t) => {
    const orgs = MOCK_ORGS.filter((o) => o.tenant_id === t.id);
    const def = orgs.find((o) => o.name === "default") ?? null;
    return {
      ...t,
      orgs,
      default_org: def,
    };
  });
}

export function mockPost<T = unknown>(path: string, body: unknown): T {
  const now = new Date().toISOString();
  // PRMT-220: POST /api/tenants/{id}:tier
  {
    const m = path.match(/^\/api\/tenants\/([^/:?]+):tier$/);
    if (m) {
      const id = m[1]!;
      const b = body as { isolation_tier?: string };
      const tier = (b.isolation_tier ?? "").trim();
      const t = MOCK_TENANTS.find((x) => x.id === id);
      if (!t) throw new Error(`mockPost tier: tenant not found: ${id}`);
      if (!["label", "row", "db"].includes(tier)) {
        throw new Error(`mockPost tier: bad isolation_tier: ${tier}`);
      }
      const rank: Record<string, number> = { label: 0, row: 1, db: 2 };
      if (rank[tier]! < rank[t.isolation_tier]!) {
        throw new Error("isolation_tier downgrade refused");
      }
      t.isolation_tier = tier;
      t.updated_at = now;
      return { ...t } as T;
    }
  }
  // PRMT-220: POST /api/orgs/{id}:rename
  {
    const m = path.match(/^\/api\/orgs\/([^/:?]+):rename$/);
    if (m) {
      const id = m[1]!;
      const b = body as { name?: string };
      const name = (b.name ?? "").trim();
      const o = MOCK_ORGS.find((x) => x.id === id);
      if (!o) throw new Error(`mockPost rename: org not found: ${id}`);
      if (!name) throw new Error("mockPost rename: name required");
      if (
        MOCK_ORGS.some(
          (x) => x.tenant_id === o.tenant_id && x.name === name && x.id !== id,
        )
      ) {
        throw new Error(`mockPost rename: org name conflict: ${name}`);
      }
      o.name = name;
      return { ...o } as T;
    }
  }
  // L109 P804: create tenant (+ auto default org)
  if (path === "/api/tenants" || path.startsWith("/api/tenants?")) {
    const b = body as { id?: string; display_name?: string };
    const id = (b.id ?? "").trim();
    const display_name = (b.display_name ?? "").trim();
    if (!id || !display_name) {
      throw new Error("mockPost tenants: id and display_name required");
    }
    if (MOCK_TENANTS.some((t) => t.id === id)) {
      throw new Error(`mockPost tenants: tenant already exists: ${id}`);
    }
    const tenant: MockTenant = {
      id,
      display_name,
      isolation_tier: "label",
      status: "active",
      created_at: now,
      updated_at: now,
    };
    const defOrg: MockOrg = {
      id: newMockOrgId(),
      tenant_id: id,
      name: "default",
      created_at: now,
    };
    MOCK_TENANTS.push(tenant);
    MOCK_TENANTS.sort((a, b) => a.id.localeCompare(b.id));
    MOCK_ORGS.push(defOrg);
    return { ...tenant, default_org: defOrg } as T;
  }
  // L109 P804: create org under tenant
  if (path === "/api/orgs" || path.startsWith("/api/orgs?")) {
    const b = body as { tenant_id?: string; name?: string };
    const tenant_id = (b.tenant_id ?? "").trim();
    const name = (b.name ?? "").trim();
    if (!tenant_id || !name) {
      throw new Error("mockPost orgs: tenant_id and name required");
    }
    if (!MOCK_TENANTS.some((t) => t.id === tenant_id)) {
      throw new Error(`mockPost orgs: tenant not found: ${tenant_id}`);
    }
    if (MOCK_ORGS.some((o) => o.tenant_id === tenant_id && o.name === name)) {
      throw new Error(`mockPost orgs: org name already exists under tenant: ${name}`);
    }
    const org: MockOrg = {
      id: newMockOrgId(),
      tenant_id,
      name,
      created_at: now,
    };
    MOCK_ORGS.push(org);
    return org as T;
  }
  if (path === "/api/site-orgs" || path.startsWith("/api/site-orgs?")) {
    const b = body as { site?: string; org_id?: string };
    const site = (b.site ?? "").trim();
    const org_id = (b.org_id ?? "").trim();
    if (!site || !org_id) {
      throw new Error("mockPost site-orgs: site and org_id required");
    }
    // Soft validate org exists so bad paste is obvious in mock UI.
    if (!MOCK_ORGS.some((o) => o.id === org_id)) {
      throw new Error(
        `mockPost site-orgs: unknown org_id ${org_id} — create/list under /admin/tenants first`,
      );
    }
    const idx = MOCK_SITE_ORGS.findIndex((x) => x.site === site);
    const row: MockSiteOrg = {
      site,
      org_id,
      created_at: idx >= 0 ? MOCK_SITE_ORGS[idx]!.created_at : now,
      updated_at: now,
    };
    if (idx >= 0) MOCK_SITE_ORGS[idx] = row;
    else MOCK_SITE_ORGS.push(row);
    MOCK_SITE_ORGS.sort((a, b) => a.site.localeCompare(b.site));
    return row as T;
  }
  if (path === "/api/role-bindings" || path.startsWith("/api/role-bindings?")) {
    const b = body as { subject?: string; scope?: string; origin?: string };
    const subject = (b.subject ?? "").trim();
    const scope = (b.scope ?? "").trim();
    const origin = (b.origin ?? "legacy").trim() || "legacy";
    if (!subject || !scope) {
      throw new Error("mockPost role-bindings: subject and scope required");
    }
    const idx = MOCK_ROLE_BINDINGS.findIndex(
      (x) => x.subject === subject && x.scope === scope,
    );
    const row: MockRoleBinding = {
      id:
        idx >= 0
          ? MOCK_ROLE_BINDINGS[idx]!.id
          : `rb_MOCK${String(MOCK_ROLE_BINDINGS.length).padStart(10, "0")}`,
      subject,
      scope,
      origin,
      created_at: idx >= 0 ? MOCK_ROLE_BINDINGS[idx]!.created_at : now,
      updated_at: now,
    };
    if (idx >= 0) MOCK_ROLE_BINDINGS[idx] = row;
    else MOCK_ROLE_BINDINGS.push(row);
    return row as T;
  }
  {
    const m = path.match(/^\/api\/model-packs\/([^/]+)\/([^/:]+):lint$/);
    if (m) {
      const type = m[1]!;
      const model = m[2]!;
      // Soft fail demo: AC45 pending, DC45 pass
      const fail = model === "AC45";
      const soft = fail ? "pending_conform" : "ready";
      const result = fail ? "fail" : "pass";
      MOCK_LINT.set(packKey(type, model), { result, soft_status: soft });
      const pack = MOCK_PACKS.find((p) => p.type === type && p.model === model);
      if (pack) {
        pack.status = soft;
        pack.lint_result = result;
      }
      return {
        type,
        model,
        result,
        soft_status: soft,
        exit_code: fail ? 1 : 0,
        ran_at: now,
        summary: { pass: fail ? 10 : 14, fail: fail ? 4 : 0, warn: 0 },
      } as T;
    }
  }
  {
    // May include ?rebuild_scene=1 from admin.draw writeback intent.
    const m = path.match(/^\/api\/site-layouts\/([^/:?]+):writeback(?:\?.*)?$/);
    if (m) {
      const site = m[1]!;
      const b = (body ?? {}) as Record<string, unknown>;
      const doc = {
        site,
        instances: Array.isArray(b.instances) ? b.instances : [],
        edges: Array.isArray(b.edges) ? b.edges : [],
        updated_at: now,
        last_writeback: {
          at: now,
          actor: "mock",
          assets_created: Array.isArray(b.instances) ? b.instances.length : 0,
          assets_updated: 0,
          edges_kept: Array.isArray(b.edges) ? b.edges.length : 0,
        },
      };
      MOCK_LAYOUTS.set(site, doc);
      return { layout: doc, writeback: doc.last_writeback } as T;
    }
  }
  {
    // L109 P825: async scene rebuild kick (mock completes immediately).
    const m = path.match(
      /^\/api\/site-layouts\/([^/:?]+):rebuild-scene(?:\?.*)?$/,
    );
    if (m) {
      const site = m[1]!;
      const job = {
        site,
        job_id: `mock-job-${site}-${Date.now()}`,
        status: "ok",
        started_at: now,
        finished_at: now,
        exit_code: 0,
        out_dir: `artifacts/scene/${site}`,
        message: "mock rebuild complete (no real scene-engine)",
      };
      MOCK_SCENE_JOBS.set(site, job);
      return job as T;
    }
  }
  {
    const m = path.match(/^\/api\/model-packs\/([^/]+)\/([^/:]+):conform$/);
    if (m) {
      const type = m[1]!;
      const model = m[2]!;
      const b = (body ?? {}) as { dry_run?: boolean };
      const dry = !!b.dry_run;
      const proposals = [
        {
          prim: "/root/geo/picv000",
          from_type: "picv",
          to_type: "valve",
          source: "alias",
          action: "apply",
        },
      ];
      if (!dry) {
        const key = packKey(type, model);
        const cur = MOCK_BINDINGS.get(key) ?? { notes: "", bindings: [] as MockBinding[] };
        cur.bindings = [
          {
            prim: "/root/geo/picv000",
            cios_type: "valve",
            cios_model: model,
          },
        ];
        cur.notes = (cur.notes ? cur.notes + "\n" : "") + "P814 G8 mock apply";
        MOCK_BINDINGS.set(key, cur);
        const pack = MOCK_PACKS.find((p) => p.type === type && p.model === model);
        if (pack) {
          pack.has_bindings = true;
          pack.binding_count = 1;
        }
      }
      return {
        type,
        model,
        mode: "g8",
        strategy: "s_first",
        dry_run: dry,
        g_layer_action: "none",
        proposals,
        applied_count: dry ? 0 : 1,
        skipped_count: 0,
        platform_ready: !dry,
        note: "mock P814",
      } as T;
    }
  }
  throw new Error(`mockPost: no handler for path ${path}`);
}

// --- L109 Model Studio mock ---
type MockPack = {
  type: string;
  model: string;
  path: string;
  size_bytes: number;
  status: string;
  lint_result?: string;
  has_bindings: boolean;
  binding_count: number;
};
type MockBinding = {
  prim: string;
  cios_type?: string;
  cios_model?: string;
  cios_relpath?: string;
  note?: string;
};
const MOCK_PACKS: MockPack[] = [
  {
    type: "pod",
    model: "DC45",
    path: "assets/usd/pod/DC45.usdc",
    size_bytes: 1_000_000,
    status: "unknown",
    has_bindings: false,
    binding_count: 0,
  },
  {
    type: "pod",
    model: "AC45",
    path: "assets/usd/pod/AC45.usdc",
    size_bytes: 500_000,
    status: "unknown",
    has_bindings: false,
    binding_count: 0,
  },
];
const MOCK_BINDINGS = new Map<string, { notes: string; bindings: MockBinding[] }>();
const MOCK_LINT = new Map<string, { result: string; soft_status: string }>();

function packKey(type: string, model: string) {
  return `${type}/${model}`;
}

// Site layouts mock (P821+)
const MOCK_LAYOUTS = new Map<string, Record<string, unknown>>();
const MOCK_SCENE_JOBS = new Map<string, Record<string, unknown>>();

export function mockPut<T = unknown>(path: string, body: unknown): T {
  const lay = path.match(/^\/api\/site-layouts\/([^/:?]+)$/);
  if (lay) {
    const site = lay[1]!;
    const b = (body ?? {}) as Record<string, unknown>;
    const doc = {
      site,
      instances: Array.isArray(b.instances) ? b.instances : [],
      edges: Array.isArray(b.edges) ? b.edges : [],
      updated_at: new Date().toISOString(),
      updated_by: "mock",
    };
    MOCK_LAYOUTS.set(site, doc);
    return doc as T;
  }
  const m = path.match(/^\/api\/model-packs\/([^/]+)\/([^/]+)\/bindings$/);
  if (m) {
    const type = m[1]!;
    const model = m[2]!;
    const b = body as { notes?: string; bindings?: MockBinding[] };
    const bindings = Array.isArray(b.bindings) ? b.bindings : [];
    MOCK_BINDINGS.set(packKey(type, model), {
      notes: b.notes ?? "",
      bindings,
    });
    const pack = MOCK_PACKS.find((p) => p.type === type && p.model === model);
    if (pack) {
      pack.has_bindings = bindings.length > 0;
      pack.binding_count = bindings.length;
    }
    return {
      type,
      model,
      updated_at: new Date().toISOString(),
      notes: b.notes ?? "",
      bindings,
    } as T;
  }
  throw new Error(`mockPut: no handler for path ${path}`);
}

export function mockDelete(path: string): void {
  // PRMT-220: DELETE /api/site-orgs?site=
  if (path.startsWith("/api/site-orgs")) {
    const q = path.includes("?") ? path.slice(path.indexOf("?") + 1) : "";
    const site = (new URLSearchParams(q).get("site") ?? "").trim();
    const i = MOCK_SITE_ORGS.findIndex((x) => x.site === site);
    if (i < 0) throw new Error(`mockDelete site-orgs: not found: ${site}`);
    MOCK_SITE_ORGS.splice(i, 1);
    return;
  }
  // PRMT-220: DELETE /api/orgs/{id}
  {
    const m = path.match(/^\/api\/orgs\/([^/?]+)$/);
    if (m) {
      const id = m[1]!;
      if (MOCK_SITE_ORGS.some((s) => s.org_id === id)) {
        throw new Error("org owns resources");
      }
      const i = MOCK_ORGS.findIndex((o) => o.id === id);
      if (i < 0) throw new Error(`mockDelete org: not found: ${id}`);
      MOCK_ORGS.splice(i, 1);
      return;
    }
  }
  // PRMT-220: DELETE /api/tenants/{id}
  {
    const m = path.match(/^\/api\/tenants\/([^/?]+)$/);
    if (m) {
      const id = m[1]!;
      if (MOCK_ORGS.some((o) => o.tenant_id === id)) {
        throw new Error("tenant still has orgs; delete orgs first");
      }
      const i = MOCK_TENANTS.findIndex((t) => t.id === id);
      if (i < 0) throw new Error(`mockDelete tenant: not found: ${id}`);
      MOCK_TENANTS.splice(i, 1);
      return;
    }
  }
  if (!path.startsWith("/api/role-bindings")) {
    throw new Error(`mockDelete: no handler for path ${path}`);
  }
  const q = path.includes("?") ? path.slice(path.indexOf("?") + 1) : "";
  const qs = new URLSearchParams(q);
  const subject = (qs.get("subject") ?? "").trim();
  const scope = (qs.get("scope") ?? "").trim();
  const i = MOCK_ROLE_BINDINGS.findIndex(
    (x) => x.subject === subject && x.scope === scope,
  );
  if (i >= 0) MOCK_ROLE_BINDINGS.splice(i, 1);
}

export async function mockGet<T = unknown>(path: string): Promise<T> {
  // L109 P804 tenants/orgs (must be before generic 404)
  if (path === "/api/tenants" || path.startsWith("/api/tenants?")) {
    const qs = path.includes("?")
      ? new URLSearchParams(path.slice(path.indexOf("?") + 1))
      : new URLSearchParams();
    const q = (qs.get("q") ?? "").trim().toLowerCase();
    let items = mockTenantListItems();
    if (q) {
      items = items.filter(
        (t) =>
          t.id.toLowerCase().includes(q) ||
          t.display_name.toLowerCase().includes(q),
      );
    }
    return { items } as T;
  }
  if (path === "/api/orgs" || path.startsWith("/api/orgs?")) {
    const q = path.includes("?") ? path.slice(path.indexOf("?") + 1) : "";
    const tenantId = new URLSearchParams(q).get("tenant_id")?.trim();
    const items = tenantId
      ? MOCK_ORGS.filter((o) => o.tenant_id === tenantId)
      : [...MOCK_ORGS];
    return { items } as T;
  }
  if (path === "/api/site-layouts" || path.startsWith("/api/site-layouts?")) {
    // relations mirror protocol/types.yaml (PRMT-223); mock static list for UI.
    return {
      sites: [...MOCK_LAYOUTS.keys()],
      relations: ["cools", "connects", "feeds"],
    } as T;
  }
  {
    // GET /api/site-layouts/:site/scene-job
    const jobM = path.match(/^\/api\/site-layouts\/([^/:?]+)\/scene-job$/);
    if (jobM) {
      const site = jobM[1]!;
      const job = MOCK_SCENE_JOBS.get(site) ?? {
        site,
        status: "none",
        message: "no rebuild yet (mock)",
      };
      return job as T;
    }
  }
  {
    const m = path.match(/^\/api\/site-layouts\/([^/:?]+)$/);
    if (m) {
      const site = m[1]!;
      const doc = MOCK_LAYOUTS.get(site) ?? {
        site,
        instances: [],
        edges: [],
      };
      return doc as T;
    }
  }
  if (path === "/api/model-packs" || path.startsWith("/api/model-packs?")) {
    return { items: MOCK_PACKS.map((p) => ({ ...p })) } as T;
  }
  {
    const m = path.match(/^\/api\/model-packs\/([^/]+)\/([^/?]+)$/);
    if (m && !path.includes(":lint") && !path.endsWith("/bindings")) {
      const type = m[1]!;
      const model = m[2]!;
      const pack = MOCK_PACKS.find((p) => p.type === type && p.model === model);
      if (!pack) throw new Error(`mockGet: pack not found ${type}/${model}`);
      const b = MOCK_BINDINGS.get(packKey(type, model)) ?? {
        notes: "",
        bindings: [] as MockBinding[],
      };
      const lint = MOCK_LINT.get(packKey(type, model));
      return {
        pack: { ...pack },
        bindings: {
          type,
          model,
          notes: b.notes,
          bindings: b.bindings,
          updated_at: new Date().toISOString(),
        },
        lint: lint
          ? {
              type,
              model,
              result: lint.result,
              soft_status: lint.soft_status,
              ran_at: new Date().toISOString(),
            }
          : null,
      } as T;
    }
  }
  if (path === "/api/site-orgs" || path.startsWith("/api/site-orgs?")) {
    const qs = path.includes("?")
      ? new URLSearchParams(path.slice(path.indexOf("?") + 1))
      : new URLSearchParams();
    const q = (qs.get("q") ?? "").trim().toLowerCase();
    let items = [...MOCK_SITE_ORGS];
    if (q) {
      items = items.filter((x) => x.site.toLowerCase().includes(q));
    }
    return { items } as T;
  }
  if (path === "/api/role-bindings" || path.startsWith("/api/role-bindings?")) {
    const qs = path.includes("?")
      ? new URLSearchParams(path.slice(path.indexOf("?") + 1))
      : new URLSearchParams();
    const subject = (qs.get("subject") ?? "").trim();
    const q = (qs.get("q") ?? "").trim().toLowerCase();
    let items = subject
      ? MOCK_ROLE_BINDINGS.filter((x) => x.subject === subject)
      : [...MOCK_ROLE_BINDINGS];
    if (!subject && q) {
      items = items.filter((x) => x.subject.toLowerCase().includes(q));
    }
    return { items } as T;
  }
  if (path === "/api/sites" || path.startsWith("/api/sites?")) {
    // Derive site list from site→org registry so admin pages stay coherent.
    return {
      items: MOCK_SITE_ORGS.map((r) => ({
        crn: r.site,
        type: "site",
        name: r.site,
        org_id: r.org_id,
      })),
      next_page_token: "",
    } as T;
  }
  // Match bare /api/assets and paginated /api/assets?page_size=… (loadAllPortalAssets).
  if (path === "/api/assets" || path.startsWith("/api/assets?")) {
    return {
      items: [
        { crn: "site01", type: "site", name: "Site 01", org: "ops-na" },
        {
          crn: "site01.pod000",
          type: "pod",
          name: "Pod 000",
          parent: "site01",
        },
        {
          crn: "site01.pod000.cdu000",
          type: "cdu",
          name: "CDU 000",
          parent: "site01.pod000",
        },
        { crn: "site02", type: "site", name: "Site 02", org: "ops-na" },
        {
          crn: "site02.pod001",
          type: "pod",
          name: "Pod 001",
          parent: "site02",
        },
        {
          crn: "site02.pod001.cdu001",
          type: "cdu",
          name: "CDU 001",
          parent: "site02.pod001",
        },
      ],
      next_page_token: "",
    } as T;
  }
  if (path === "/api/alarms" || path.startsWith("/api/alarms?")) {
    // PRMT-148: honor ?severity= and ?state= (spec-004 whitelist); honor
    // ?page_token= so callers don't see the same page twice. The unfiltered
    // first page (no ?page_token=) carries a non-empty `next_page_token`
    // so the "Next" control renders; subsequent pages return no further
    // items (loop-terminator for the SSR test).
    //
    // Anchor row kept verbatim: site01.pod000.cdu000 (warning, firing)
    // — PRMT-146/147 rely on it for site-anomaly + drilldown smoke.
    const all = [
      {
        crn: "site01.pod000.cdu000",
        severity: "warning",
        state: "firing",
        summary: "Coolant supply temp high",
      },
      {
        crn: "site02.pod001.cdu001",
        severity: "critical",
        state: "firing",
        summary: "Coolant leak detected",
      },
      // PRMT-147 §4.4: upstream firing alarm on the PDU so the
      // drilldown's deterministic root-cause walk has a real demo.
      // Severity = warning (matches site01's worst in PRMT-146
      // assertions; using critical would override cdu000's worst
      // and break PRMT-146's site-anomaly smoke check).
      {
        crn: "site01.pdu0",
        severity: "warning",
        state: "firing",
        summary: "PDU input out of tolerance",
      },
      // PRMT-148 §4.3: ≥3 rows across info/warning/critical.
      {
        crn: "site02.pod001",
        severity: "info",
        state: "firing",
        summary: "Pod 001 minor threshold drift",
      },
      {
        crn: "site01.pod000.node0",
        severity: "critical",
        state: "resolved",
        summary: "Compute node thermal trip (cleared)",
      },
    ] as const;

    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const sev = qs.get("severity");
    const st = qs.get("state");
    const token = qs.get("page_token");

    if (token) {
      // Second page (and beyond): empty result, no further cursor.
      return { items: [], next_page_token: "" } as T;
    }

    const filtered = all.filter(
      (a) =>
        (sev === null || a.severity === sev) &&
        (st === null || a.state === st),
    );

    return {
      items: filtered,
      next_page_token: "page-2",
    } as T;
  }
  if (path === "/api/tickets" || path.startsWith("/api/tickets?")) {
    // PRMT-156 + PRMT-199: shared mutable MOCK_TICKETS store.
    const all = MOCK_TICKETS;

    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const sev = qs.get("severity");
    const st = qs.get("state");
    const token = qs.get("page_token");

    if (token) {
      // Second page (and beyond): empty result, no further cursor.
      return { items: [], next_page_token: "" } as T;
    }

    const filtered = all.filter(
      (t) =>
        (sev === null || t.severity === sev) &&
        (st === null || t.state === st),
    );

    return {
      items: filtered,
      next_page_token: "page-2",
    } as T;
  }
  // PRMT-233: single ticket by id (live = apigw /api/tickets/{id}).
  if (path.startsWith("/api/tickets/")) {
    const id = decodeURIComponent(path.slice("/api/tickets/".length));
    const t = MOCK_TICKETS.find((x) => x.id === id);
    if (!t) throw new Error(`mockGet: ticket not found ${id}`);
    return t as T;
  }
  if (path === "/api/topology") {
    return {
      edges: [
        { from: "site01.pdu0", to: "site01.pod000.cdu000", kind: "feeds" },
        { from: "site01.pod000.cdu000", to: "site01.pod000.node0", kind: "cools" },
      ],
    } as T;
  }
  if (
    path === "/api/capacity/forecast" ||
    path.startsWith("/api/capacity/forecast?")
  ) {
    // P741: capacity forecast fixture (linear_growth, power+cooling).
    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const g = Number(qs.get("growth_pct_per_year") ?? "0");
    const window = qs.get("window") ?? "7d";
    const mk = (days: number, horizon: string) => {
      const factor = Math.pow(1 + g / 100, days / 365);
      const powerBase = 14500;
      const coolBase = 410;
      return {
        horizon,
        days,
        dimensions: [
          {
            dimension: "power",
            status: "ok",
            unit: "W",
            rated: 24000,
            measured_baseline: powerBase,
            forecast_measured: powerBase * factor,
            forecast_remaining: 24000 - powerBase * factor,
            degraded: false,
          },
          {
            dimension: "cooling",
            status: "ok",
            unit: "kw",
            rated: 600,
            measured_baseline: coolBase,
            forecast_measured: coolBase * factor,
            forecast_remaining: 600 - coolBase * factor,
            degraded: false,
          },
          {
            dimension: "gpu",
            status: "not_implemented",
            note: "GPU capacity forecast gated on P761 DCGM (hardware)",
          },
        ],
      };
    };
    return {
      method: "linear_growth",
      growth_pct_per_year: Number.isFinite(g) ? g : 0,
      baseline_window: window,
      horizons: [mk(30, "30d"), mk(90, "90d")],
      notes: [
        "P741 mock forecast",
        "growth_pct_per_year=0 holds measured flat (default)",
      ],
    } as T;
  }
  if (path === "/api/capacity" || path.startsWith("/api/capacity?")) {
    // PRMT-157: capacity headroom (rated − measured P95) per
    // dimension (spec-008 §10). Fixture rows mirror
    // core.capacityAssetEntry JSON shape (core/capacity.go L82-91).
    // Power + cooling implemented; rack returns a `not_implemented`
    // stub matching the wire shape so the loader's flatten loop is
    // exercised without inventing fields.
    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const window = qs.get("window") ?? "7d";
    return {
      window,
      dimensions: [
        {
          dimension: "power",
          status: "ok",
          unit: "W",
          rated: 24000,
          measured_p95: 14500,
          remaining: 9500,
          degraded: false,
          // Pod-level paths (site.podNNN) so default portal scope=pods shows rows.
          by_asset: [
            {
              path: "site01.pod000",
              lifecycle: "active",
              rated: 15000,
              measured_p95: 9300,
              remaining: 5700,
              degraded: false,
            },
            {
              path: "site02.pod001",
              lifecycle: "active",
              rated: 9000,
              measured_p95: 5200,
              remaining: 3800,
              degraded: false,
            },
          ],
          missing_rated: 0,
        },
        {
          dimension: "cooling",
          status: "ok",
          unit: "kw",
          rated: 600,
          measured_p95: 410,
          remaining: 190,
          degraded: false,
          by_asset: [
            {
              path: "site01.pod000",
              lifecycle: "active",
              rated: 350,
              measured_p95: 240,
              remaining: 110,
              degraded: false,
            },
            {
              path: "site02.pod001",
              lifecycle: "active",
              rated: 250,
              measured_p95: 170,
              remaining: 80,
              degraded: false,
            },
          ],
          missing_rated: 0,
        },
        {
          dimension: "rack",
          status: "not_implemented",
          unit: "watt",
          rated: null,
          measured_p95: null,
          remaining: null,
          degraded: false,
          by_asset: [],
          missing_rated: 0,
        },
        {
          dimension: "gpu",
          status: "not_implemented",
          unit: "watt",
          rated: null,
          measured_p95: null,
          remaining: null,
          degraded: false,
          by_asset: [],
          missing_rated: 0,
        },
      ],
    } as T;
  }
  if (
    path === "/api/maintenance/upcoming" ||
    path.startsWith("/api/maintenance/upcoming?")
  ) {
    // PRMT-158: upcoming maintenance fixture (PM + inspection merged
    // view, M2 P558 — core/maintenance.go L34-48). Shape pinned to
    // core.maintenanceUpcomingItem: kind, id, asset_path, title,
    // next_due (RFC3339), overdue (bool). Mock honors ?before= and
    // ?overdue= (spec-004 whitelist) so the loader's URL pass-through
    // is exercised; ?page_token-style cursor does not apply (upcoming
    // is unpaged).
    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(
      qIdx >= 0 ? path.slice(qIdx + 1) : "",
    );
    const beforeRaw = qs.get("before");
    const beforeTs =
      beforeRaw && beforeRaw !== "" ? Date.parse(beforeRaw) : null;
    const overdueFilter = qs.get("overdue"); // "true" | "false" | null

    const all = [
      {
        kind: "pm",
        id: "pm_AAAAAAAAAAAA0001",
        asset_path: "site01.pod000.cdu000",
        title: "CDU filter replacement",
        next_due: "2026-06-30T08:00:00Z",
        overdue: false,
      },
      {
        kind: "inspection",
        id: "ins_AAAAAAAAAAAA0001",
        asset_path: "site01.pod000.cdu000",
        title: "Quarterly thermal audit",
        next_due: "2026-06-25T00:00:00Z",
        overdue: true,
      },
      {
        kind: "pm",
        id: "pm_AAAAAAAAAAAA0002",
        asset_path: "site02.pod001.cdu001",
        title: "Coolant top-up",
        next_due: "2026-07-10T12:00:00Z",
        overdue: false,
      },
      {
        kind: "inspection",
        id: "ins_AAAAAAAAAAAA0002",
        asset_path: "site02.pod001.cdu001",
        title: "Annual pressure test",
        next_due: "2026-08-01T09:00:00Z",
        overdue: false,
      },
    ] as const;

    const filtered = all.filter((it) => {
      if (beforeTs !== null && Number.isFinite(beforeTs)) {
        const due = Date.parse(it.next_due);
        if (Number.isFinite(due) && due > beforeTs) return false;
      }
      if (overdueFilter === "true" && !it.overdue) return false;
      if (overdueFilter === "false" && it.overdue) return false;
      return true;
    });

    return { items: filtered } as T;
  }
  if (path === "/api/spares" || path.startsWith("/api/spares?")) {
    // PRMT-159: spare inventory fixture (E3.5 / P643 — M2 P541 catalog).
    // Shape pinned to core.SparePart (core/store.go L143-150) and
    // core.listSparesResponse envelope (core/spares.go L77-80). Honors
    // ?page_token= so callers don't see the same page twice; the
    // unfiltered first page carries a non-empty `next_page_token` so
    // the "Next" control renders (mirror of the /api/tickets pattern).
    //
    // First row carries qty (0) < min_qty (5) so the route's
    // `data-low-stock` marker is exercised end-to-end (smoke check).
    const all = [
      {
        id: "sp_AAAAAAAAAAAA0001",
        sku: "CDU-FILTER-12",
        name: "CDU coolant filter, 12in",
        qty: 0,
        min_qty: 5,
        location: "site01.store-A.bin-03",
      },
      {
        id: "sp_AAAAAAAAAAAA0002",
        sku: "PUMP-IMPELLER-08",
        name: "Coolant pump impeller, 8in",
        qty: 12,
        min_qty: 4,
        location: "site01.store-A.bin-07",
      },
      {
        id: "sp_AAAAAAAAAAAA0003",
        sku: "GASKET-EPDM-2",
        name: "EPDM gasket, 2in",
        qty: 3,
        min_qty: 10,
        location: "site02.store-B.bin-01",
      },
      {
        id: "sp_AAAAAAAAAAAA0004",
        sku: "PSU-240V-3000W",
        name: "PSU 240V 3000W",
        qty: 8,
        min_qty: 2,
        location: "site02.store-B.bin-04",
      },
    ] as const;

    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const token = qs.get("page_token");

    if (token) {
      // Second page (and beyond): empty result, no further cursor.
      return { items: [], next_page_token: "" } as T;
    }

    return {
      items: all,
      next_page_token: "page-2",
    } as T;
  }
  if (
    path === "/api/inspections" ||
    path.startsWith("/api/inspections?")
  ) {
    // PRMT-160: inspection template fixture (E3.5 / P643 — M2 P561).
    // Shape pinned to core.InspectionTemplate (core/store.go L222-230)
    // and core.listInspectionsResponse envelope (core/inspection.go
    // L55-57). NOTE: listInspectionsResponse has NO `next_page_token`
    // — M2 inspection scale is operator-set, small (inspection.go
    // L80). Honors ?page_token= anyway so the loader's cursor
    // pass-through is exercised end-to-end; when present, returns an
    // empty list (loop terminator for the SSR test).
    //
    // Second row carries enabled=false so the route's
    // `data-inspection-enabled="false"` marker is exercised
    // (smoke check; same precedent as /api/spares' low-stock row).
    const all = [
      {
        id: "ins_AAAAAAAAAAAA0001",
        asset_path: "site01.pod000.cdu000",
        title: "Quarterly thermal audit",
        items: [
          "Verify coolant supply temp",
          "Check pump impeller wear",
          "Inspect hose connections",
        ],
        interval: 7_776_000_000_000_000, // 90 days in nanoseconds
        next_due: "2026-07-15T08:00:00Z",
        enabled: true,
      },
      {
        id: "ins_AAAAAAAAAAAA0002",
        asset_path: "site02.pod001.cdu001",
        title: "Annual pressure test",
        items: ["Pressure decay check", "Seal integrity"],
        interval: 31_536_000_000_000_000, // 365 days in nanoseconds
        next_due: "2027-01-10T09:00:00Z",
        enabled: false,
      },
      {
        id: "ins_AAAAAAAAAAAA0003",
        asset_path: "site01.pod000",
        title: "Pod filter inspection",
        items: ["Replace pod-level air filter"],
        interval: 2_592_000_000_000_000, // 30 days in nanoseconds
        next_due: "2026-07-01T12:00:00Z",
        enabled: true,
      },
    ] as const;

    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const token = qs.get("page_token");

    if (token) {
      // Mock-only cursor loop terminator (core's live endpoint does
      // not currently paginate — see comment above).
      return { items: [] } as T;
    }

    return { items: all } as T;
  }
  if (path === "/api/capacity/metrics") {
    // PRMT-157: Prometheus text exposition stub (spec-008 §10). The
    // route page does not call this path today (Phase A is JSON-only),
    // but the seam mirrors it so the Gateway handler reference exists
    // for the M2 P521 scraper integration. Hand-built text shape is
    // an intentional subset of core.writePowerExposition output.
    return [
      "# HELP cios_capacity_rated_watt Rated power capacity (W) from CMDB.",
      "# TYPE cios_capacity_rated_watt gauge",
      "cios_capacity_rated_watt 24000",
      "# HELP cios_capacity_remaining_watt Remaining power headroom (rated - measured p95).",
      "# TYPE cios_capacity_remaining_watt gauge",
      'cios_capacity_remaining_watt{asset_path="site01.pod000.cdu000"} 5700',
      'cios_capacity_remaining_watt{asset_path="site02.pod001.cdu001"} 3800',
    ].join("\n") as T;
  }
  if (path.startsWith("/api/metrics/query_range")) {
    const pts = (b: number): [number, string][] => [
      [1700000000, String(b)],
      [1700000300, String(b + 50)],
      [1700000600, String(b + 25)],
    ];
    // PRMT-146: route loader now scopes by ?site=<siteId>; default to site01
    // when the param is absent so /noc (no params) still matches PRMT-145.
    const siteMatch = /site=([^&]+)/.exec(path);
    const siteLabel = siteMatch && siteMatch[1] !== undefined
      ? decodeURIComponent(siteMatch[1])
      : "site01";
    return {
      status: "success",
      data: {
        resultType: "matrix",
        result: [
          {
            metric: { __name__: "cios_facility_power_w", site: siteLabel },
            values: pts(10000),
          },
          {
            metric: { __name__: "cios_it_power_w", site: siteLabel },
            values: pts(7000),
          },
          {
            metric: { __name__: "cios_pue", site: siteLabel },
            values: [
              [1700000000, "1.43"],
              [1700000300, "1.41"],
              [1700000600, "1.42"],
            ],
          },
        ],
      },
    } as T;
  }
  if (path.startsWith("/api/metrics/query")) {
    return {
      status: "success",
      data: {
        resultType: "vector",
        result: [
          {
            metric: {
              __name__: "cios_power_w",
              crn: "site01.pod000.cdu000",
            },
            value: [1700000000, "4200"],
          },
          {
            metric: {
              __name__: "cios_utilization_ratio",
              crn: "site01.pod000.cdu000",
            },
            value: [1700000000, "0.62"],
          },
        ],
      },
    } as T;
  }
  if (path === "/api/twins/scene" || path.startsWith("/api/twins/scene?")) {
    // PRMT-152: twins-v0 scene descriptor fixture (E3.5/3.6a bridge,
    // Phase B render base). Shape pinned to PRMT-169 §4.1 / api-client
    // `SceneDescriptor`. The live gateway route is gated on PRMT-170
    // (feature/m3-auth companion); this mock is the only path exercised
    // by PRMT-152 acceptance.
    //
    // MOCK_GATEWAY=1 FIXTURE-ONLY marker: `site02.pod001.cdu001` is
    // seeded with `access: "ghost"` (L97) so the R2 outline render
    // path is exercised end-to-end. The live Scene Engine emits
    // `full` only (L91/L97; R2 dormant on the live scene). The
    // renderer must NOT derive out-of-scope status from any other
    // source (spec-009 §3.3 red line).
    return {
      contract: "twins-v0",
      site: "site01",
      geometry: { format: "gltf-binary", file: "site01.glb" },
      nodes: [
        {
          path: "site01",
          type: "site",
          gltf_node: "site01",
          model: "placeholder",
          access: "full",
        },
        {
          path: "site01.pod000",
          type: "pod",
          gltf_node: "site01.pod000",
          model: "placeholder",
          access: "full",
        },
        {
          path: "site01.pod000.cdu000",
          type: "cdu",
          gltf_node: "site01.pod000.cdu000",
          model: "ac45",
          access: "full",
        },
        {
          path: "site01.pod000.node0",
          type: "node",
          gltf_node: "site01.pod000.node0",
          model: "placeholder",
          access: "full",
        },
        // FIXTURE-ONLY: L97 ghost for R2 outline path (MOCK_GATEWAY=1).
        {
          path: "site02.pod001.cdu001",
          type: "cdu",
          gltf_node: "site02.pod001.cdu001",
          model: "dc45",
          access: "ghost",
        },
      ],
      edges: [
        {
          from: "site01.pod000",
          to: "site01.pod000.cdu000",
          rel: "feeds",
          rated_kw: 200,
        },
        {
          from: "site01.pod000.cdu000",
          to: "site01.pod000.node0",
          rel: "cools",
          rated_kw: 150,
        },
      ],
      bindings: [
        {
          path: "site01.pod000.cdu000",
          points: ["supply.flow", "return.flow", "inlet.temp", "outlet.temp"],
        },
        {
          path: "site01.pod000.node0",
          points: ["power", "utilization", "inlet.temp"],
        },
      ],
      visual: {
        authority: "spec-009 §4.1",
        note: "visual_state = v0.0.1 (L92). No colors in the wire.",
      },
    } as T;
  }
  if (path === "/api/cases" || path.startsWith("/api/cases?")) {
    // PRMT-161: closed-ticket KB seed fixture (E3.5 / P643 — M2 P572).
    // Shape pinned to canonical `CasesResponse` in
    // @cios/api-client/types.ts (PRMT-161 §4) which projects
    // core.Ticket (core/store.go L72-91) down to the KB seed surface
    // (id / title / summary / crn / closed_at). Honors ?limit= so the
    // loadApi projection loop is exercised (mock-only, since the
    // canonical envelope has NO next_page_token — /v1/cases is unpaged).
    const all: {
      id: string;
      title: string;
      summary?: string;
      crn?: string;
      closed_at?: string;
    }[] = [
      {
        id: "tk_BBBBBBBBBBBB0001",
        title: "CDU deltaT low (recurrence)",
        summary:
          "Coolant supply temperature dropped below the deltaT floor; closed after pump impeller replacement.",
        crn: "site01.pod000.cdu000",
        closed_at: "2026-06-22T11:15:00Z",
      },
      {
        id: "tk_BBBBBBBBBBBB0002",
        title: "Coolant leak detected",
        summary:
          "Pressure-decay test flagged a slow leak at the pod manifold; closed after seal replacement.",
        crn: "site02.pod001.cdu001",
        closed_at: "2026-06-19T09:42:00Z",
      },
      {
        id: "tk_BBBBBBBBBBBB0003",
        title: "PM: CDU filter replacement",
        summary:
          "Scheduled PM cycle completed ahead of schedule.",
        crn: "site01.pod000.cdu000",
        closed_at: "2026-06-15T16:30:00Z",
      },
    ];

    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(qIdx >= 0 ? path.slice(qIdx + 1) : "");
    const limitRaw = qs.get("limit");
    let limit = all.length;
    if (limitRaw) {
      const n = Number.parseInt(limitRaw, 10);
      if (Number.isFinite(n) && n > 0) {
        limit = Math.min(n, all.length);
      }
    }

    return { items: all.slice(0, limit) } as T;
  }
  if (path.startsWith("/api/runbooks/")) {
    // PRMT-161: KB runbook fixture (E3.5 / P643 — M2 P571). Core
    // serves raw markdown (Content-Type: text/markdown, see
    // core/runbooks.go L96-98); the route loader wraps it in
    // {key, title, body} for the UI. Title is the first H1 in the
    // markdown (or falls back to the key). Body is verbatim. Three
    // canned runbooks cover the smoke key set; any other well-formed
    // key returns a 404-shaped ApiError so the route's ErrorBoundary
    // can fall back to a list-only render.
    const key = path.slice("/api/runbooks/".length);
    const FIXTURES: Record<string, { title: string; body: string }> = {
      "rb/cdu-deltat-low": {
        title: "CDU deltaT low",
        body:
          "# CDU deltaT low\n\n" +
          "## Symptoms\n- Inlet/outlet delta below 4C for >5min\n\n" +
          "## Steps\n1. Verify pump impeller wear\n2. Check coolant level\n3. Replace filter if delta stays low\n",
      },
      "rb/coolant-leak": {
        title: "Coolant leak detected",
        body:
          "# Coolant leak detected\n\n" +
          "## Symptoms\n- Pressure-decay test failure\n- Slow reservoir level drop\n\n" +
          "## Steps\n1. Isolate the affected CDU\n2. Inspect manifold seals\n3. Replace seals and re-pressure test\n",
      },
    };
    const fixture = FIXTURES[key];
    if (fixture) {
      return {
        key,
        title: fixture.title,
        body: fixture.body,
      } as T;
    }
    throw new Error(`mockGet: no canned runbook for key ${key}`);
  }
  if (path === "/api/usage" || path.startsWith("/api/usage?")) {
    // PRMT-194/196: usage facts for 对量. Optional ?kind= filter.
    const all = [
      {
        id: "us_MOCKENERGY00001",
        kind: "energy",
        tenant_id: "t_demo",
        org_id: "og_demo",
        site_id: "site01",
        asset_path: "site01.pod000",
        period_start: "2026-06-01T00:00:00Z",
        period_end: "2026-07-01T00:00:00Z",
        granularity: "monthly",
        quantity: 1200.5,
        unit: "kWh",
      },
      {
        id: "us_MOCKRACK0000001",
        kind: "rack_hour",
        tenant_id: "t_demo",
        org_id: "og_demo",
        site_id: "site01",
        asset_path: "site01.pod000.rack001",
        period_start: "2026-06-01T00:00:00Z",
        period_end: "2026-07-01T00:00:00Z",
        granularity: "monthly",
        quantity: 720,
        unit: "h",
      },
    ];
    let items = all;
    const qIdx = path.indexOf("?");
    if (qIdx >= 0) {
      const qs = new URLSearchParams(path.slice(qIdx + 1));
      const kind = qs.get("kind");
      if (kind === "energy" || kind === "rack_hour") {
        items = all.filter((r) => r.kind === kind);
      }
      const gran = qs.get("granularity");
      if (gran === "daily" || gran === "monthly") {
        items = items.filter((r) => r.granularity === gran);
      }
    }
    return { items, next_page_token: "" } as T;
  }
  if (path === "/api/reports/ops" || path.startsWith("/api/reports/ops?")) {
    // PRMT-162: ops report fixture (E3.5 / P642 — M2 P551 MTTR/MTBF
    // + top-alarms stats). Wire shape pinned to
    // core.opsReportResponse (core/reports.go L31-38):
    // mttr_seconds? / mean_response_seconds? / mtbf_seconds? /
    // ticket_counts { by_state, by_severity } / alarm_top[] { path,
    // count } / window? { since? }. Honors ?since= (RFC3339) so
    // the loader's URL pass-through is exercised; the mock echoes
    // it in `window.since` verbatim. Mock values: MTTR ~7m, mean
    // response ~22m, MTBF ~14d, alarm_top mirrors the /api/alarms
    // PRMT-148 anchor row (site01.pod000.cdu000) so the dashboard
    // shows a recognizable recurring asset path.
    const qIdx = path.indexOf("?");
    const qs = new URLSearchParams(
      qIdx >= 0 ? path.slice(qIdx + 1) : "",
    );
    const sinceRaw = qs.get("since");
    const since = sinceRaw && sinceRaw !== "" ? sinceRaw : null;

    return {
      mttr_seconds: 420, // 7m
      mean_response_seconds: 1320, // 22m
      mtbf_seconds: 1_209_600, // 14d
      ticket_counts: {
        by_state: {
          open: 2,
          acknowledged: 1,
          resolved: 0,
          closed: 1,
        },
        by_severity: {
          critical: 1,
          major: 1,
          minor: 1,
          info: 1,
        },
      },
      alarm_top: [
        { path: "site01.pod000.cdu000", count: 4 },
        { path: "site02.pod001.cdu001", count: 2 },
        { path: "site01.pdu0", count: 1 },
      ],
      window: since ? { since } : {},
    } as T;
  }
  throw new Error(`mockGet: no canned response for path ${path}`);
}
