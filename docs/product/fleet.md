# CIOS Fleet — product description (Cloud / Fleet Plane)

> Audience: platform operators, multi-site NOC, product owners.  
> Authority: `protocol/spec-006` §1.2 · L32 · L33 · L36 · L40 · E3.7 / P67x.  
> Status: **specified, not implemented** — monorepo has `fleet/` placeholder only. Gated on multi-site scale (T3).

---

## 1. What it is

**CIOS Fleet** is the **multi-site** plane: register sites, ingest edge uplink, serve a **superset of the site API** across the estate, and host global identity / tenant / package distribution.

Edge remains authoritative for raw telemetry and local control. Fleet holds the **aggregated control surface** operators use when they manage more than one site.

```text
┌─ Fleet Plane (cloud x86) ─────────────────────────────────┐
│  fleet-registry · fleet-ingest · fleet-api · (fleet-web) │
│  cloud TSDB / PG / object storage                        │
└──────────────▲───────────────────────────────────────────┘
               │ NATS leafnode over TLS (edge outbound only)
┌──────────────┴── Site A ──┐  ┌── Site B ──┐
│  Edge stack (cios-*)      │  │  Edge …    │
└───────────────────────────┘  └────────────┘
```

**Homomorphism (L33):** Fleet is a **superset of a single site**, not a second product. CLI and API verbs stay familiar: `cios query …` works against site **or** fleet URL; fleet adds multi-site routing and global resources only.

---

## 2. Product capabilities (target)

| Capability | Module | Purpose |
|------------|--------|---------|
| Site registry | **fleet-registry** | Allocate unique site codes (L36, e.g. `sgp01`); claim gateways; model-package OTA |
| Telemetry hub | **fleet-ingest** | NATS hub, cloud TSDB write, 5m manifest reconcile (spec-006 L5) |
| Multi-site API | **fleet-api** | Site `/v1` superset + tenant/global RBAC/OIDC |
| Fleet UI | fleet-web | Multi-site dashboard (M2-era surface, still gated with E3.7) |
| Cross-site cluster | Cluster object (L40) | Spec-side today; store lands with fleet implementation |

### Data residency (already locked)

| Data | Location |
|------|----------|
| Full raw telemetry (default 90d) | **Edge** |
| 1m downsampled telemetry + full alarms/events/tickets | **Fleet** (when online) |
| Billing-grade energy / occupancy streams | **Fleet** (when Commercial/Usage consumers need them) |
| On-demand raw history | Fleet requests site API backfill |

### Security

- Edge → cloud: **outbound only** (leafnode TLS)
- Fleet root CA → site intermediate → component leafs (L34 / P793 design)
- Customer-facing cloud must meet spec-006 §5.0bis (mTLS, input validation, audit)

---

## 3. What Fleet is not (today)

| Item | Reality |
|------|---------|
| Runnable compose under `fleet/` | **No** — only `.gitkeep` |
| Replacement for Edge | No — Edge is mandatory per site |
| Commercial Pricing/ERP | Parked (Commercial B); not a fleet prerequisite |
| Open for implementation | **E3.7 / P671** locked until multi-site scale (T3) |

---

## 4. Sequencing

| Gate | Condition |
|------|-----------|
| E3.7 start | Multi-site scale envelope (T3) + edge sync story ready |
| P671 | `fleet-ingest` / `fleet-api` / `fleet-registry` skeleton (L32) |
| P67x rest | UI, OTA packages, self-SLO (T34), gateway PKI lifecycle (T33) |

Until then, multi-site **lab demos** use multiple independent edge stacks or portal site-switcher against seeded data — not a real fleet plane.

---

## 5. License note

Fleet services are **not path-listed as Commercial-only** in the current open-core map by default; tenant multi-tenant substrate and billing products remain Commercial. When code lands, paths must be registered in [`OSS-MANIFEST.md`](../OSS-MANIFEST.md) before any public packaging.

---

## 6. Related docs

| Doc | Use |
|-----|------|
| [`../ops/fleet-install.md`](../ops/fleet-install.md) | Install plan & prerequisites |
| [`edge.md`](edge.md) | Per-site product (implemented) |
| [`../M3-COMPLETION-PLAN.md`](../M3-COMPLETION-PLAN.md) | E3.7 / P67x table |
| `protocol/spec-006-architecture.md` §1.2 | Normative module list |
