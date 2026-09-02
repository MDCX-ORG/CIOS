# CIOS Fleet — install & operations (planned)

> **Not deployable yet.** `fleet/` is an empty placeholder.  
> Product context: [`../product/fleet.md`](../product/fleet.md).  
> Gate: E3.7 / P671 after multi-site scale (T3).

This document freezes the **operator-facing install story** so Edge and Fleet docs stay paired. When code lands, replace placeholders with concrete compose/Helm paths without changing the section structure.

---

## 1. Prerequisites (when unlocked)

| Requirement | Notes |
|-------------|--------|
| ≥2 healthy Edge sites | Each site runs Edge stack; site codes from registry policy (L36) |
| Outbound TLS from each site | NATS leafnode to fleet hub; **no inbound** to edge required |
| Fleet host(s) | Cloud x86 (L104); sizing gated on D10 benchmarks |
| PKI | Fleet root CA → site intermediate → leaves (L34 / P793) |
| Identity | Global OIDC/RBAC plan for fleet-api |

Fleet implementation is gated; this document describes the planned install path.

---

## 2. Target install shape (spec)

```text
1. Provision fleet-registry, fleet-ingest, fleet-api (+ cloud VM/PG)
2. Issue site intermediate certs; register site codes (e.g. sgp01)
3. Configure each Edge cios-sync / NATS leafnode → fleet-ingest
4. Verify 5m manifest reconcile (count + checksum)
5. Point ops clients at fleet-api base URL (same /v1 verbs + multi-site filters)
```

### Planned modules

| Module | Install artifact (future) | Role |
|--------|---------------------------|------|
| fleet-registry | container / unit | Site IDs, model packages, OTA |
| fleet-ingest | container / unit | Leafnode hub + TSDB write + reconcile |
| fleet-api | container / unit | Multi-site REST superset |
| fleet-web | optional UI | Multi-site dashboard |

Exact images, ports, and compose files will live under `deploy/fleet/` (not present today).

---

## 3. Day-0 checklist (future)

- [ ] Fleet CA material offline / HSM procedure documented  
- [ ] Site code allocation policy published  
- [ ] Leafnode auth + subject ACL reviewed  
- [ ] Downsample + retention jobs scheduled  
- [ ] Reconcile gap alert wired to NOC  
- [ ] `cios` CLI profile for fleet URL + operator token  
- [ ] Backup of cloud PG + object store for packages  

---

## 4. Day-2 operations (future)

| Task | Intent |
|------|--------|
| Site join | Register → claim gateway → push model package |
| Site leave | Drain leafnode, revoke certs, freeze site code |
| Package OTA | Versioned pointmaps + alarm rules via registry |
| Reconcile gap | Inspect 5m manifests; trigger edge backfill |
| Multi-site query | `GET /v1/...` with site scope / CRN org segment |

Until implemented, multi-site work stays on **per-site Edge** installs plus portal site switcher against seeded data.

---

## 5. What to run today instead

| Need | Use |
|------|-----|
| Single site lab | [`edge-install.md`](edge-install.md) |
| Dual portal + usage | Edge apps profile + host portals |
| Multi-tenant substrate | Site `cios-core` tenant/org APIs (E3.1 on main) — **not** fleet-registry |
| Architecture truth | `protocol/spec-006-architecture.md` §1.2 |

---

## 6. Related decisions

| ID | Topic |
|----|--------|
| L32 | Module list includes fleet-ingest/api/registry |
| L33 | Fleet = site-homomorphic superset |
| L34 | Fleet root CA hierarchy |
| L36 | Site code allocation |
| L40 | Cross-site Cluster |
| L104 | Cloud x86 deployment direction |
| E3.7 / P671 | Implementation gate |
