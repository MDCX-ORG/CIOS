# CIOS Edge — product description (Site Plane)

> Audience: operators, field engineers, integrators.  
> Authority: `protocol/spec-006` §1.1 · L32 · `deploy/edge/`.  
> Status: **shipped** for single-site lab / site-local production shape (M0–M3 A-track).

---

## 1. What it is

**CIOS Edge** is the **per-site** runtime: collect device telemetry, run local control and alarm loops, store short-horizon history, and expose the site REST API (`/v1`) used by CLI, ops portal, and (optionally) customer portal via `cios-apigw`.

Design goals:

| Goal | Meaning |
|------|---------|
| **Single-box start** | Full stack on one host via Docker Compose (HA dual-box later, optional) |
| **Edge-first** | Full raw telemetry stays on site (90d VM default); cloud is not required for ops loop |
| **Explosion isolation** | Alarm path does **not** depend on core API liveness (L32) |
| **Zero inbound** | Host-published ports bind `127.0.0.1` only (spec-006 §5.2) |
| **Dictionary law** | Paths, units, types only from `protocol/*.yaml` |

```text
Devices / sims  →  cios-gateway (+ drivers)
                        │
                        ├─ NATS JetStream  →  cios-edge-writer  →  VictoriaMetrics
                        ├─ cios-rules (derived points)
                        └─ cios-alarm (rules → PG + events → tickets)

cios-core  ←→  PostgreSQL · VM · NATS
    │
    └─ (optional) cios-apigw  →  ops-portal / customer-portal
```

---

## 2. Product capabilities

### 2.1 Telemetry & assets

- Path-shaped points (e.g. `sgp01.pod002.cdu000.fws.supply.flow`)
- Drivers: Modbus TCP (primary), SNMP v2c; simulators for lab
- Asset CMDB + apply/list via CLI and `/v1/assets`
- Prometheus-style projection (`pkg/promproj`) into VictoriaMetrics

### 2.2 Operations loop

- Alarm rules under `deploy/edge/rules/` (expression engine over NATS, not PromQL)
- Auto-ticket from firing alarms (dedup by `alarm_id`)
- Ticket lifecycle: open → acknowledged → resolved → closed
- PM schedules, inspections, spares, capacity headroom, ops reports
- Maintenance windows suppress alarms when configured

### 2.3 Usage (measurement, OSS)

- Usage scan / compute / list / export (`spec-010`, L102)
- **Not** Pricing / invoice / ERP (Commercial B parked)

### 2.4 Experience (local lab)

- Ops portal (tickets, capacity, NOC/3D commercial surfaces, usage facts)
- Customer portal (status / SLA / usage) via apigw
- Grafana dashboards provisioned with the edge stack

### 2.5 Security posture (lab vs production)

| Mode | Posture |
|------|---------|
| **Lab compose** | Loopback binds; optional `-allow-no-auth` / sample tokens |
| **Pre-prod** | Component mTLS + data-plane TLS options (see `docs/runbooks/P793-*`) |
| **Customer cloud** | Must complete spec-006 §5.0bis hardening before public exposure |

---

## 3. What Edge is not

| Out of scope | Where it lives |
|--------------|----------------|
| Multi-site fleet aggregation | **Fleet Plane** (E3.7, gated on scale) |
| Pricing / Cost / Statement / ERP | Commercial B (**parked**) |
| Full Omniverse Kit production twin | Lab Blender path shipped; Kit deferred |
| Field PdM recall ≥70% | Needs multi-month data window (not closeout gate) |

---

## 4. Module map (site)

| Module | Role | Typical host |
|--------|------|--------------|
| `cios-gateway` | Driver host, pointmap, WAL, control executor | Site or pod (ARM) |
| `cios-edge-writer` | NATS → VM import | Site |
| `cios-core` | Site API, CMDB, tickets, usage, set policy | Site / cloud x86 (L104) |
| `cios-alarm` | Alarm engine | Site |
| `cios-rules` | Derived quantities | Site |
| `cios-apigw` | Portal proxy, STS (optional apps profile) | Site / cloud |
| VictoriaMetrics | Edge TSDB | Site |
| PostgreSQL | State / assets / tickets / usage | Site |
| NATS JetStream | Site bus + buffer | Site |
| Grafana | Lab dashboards | Site |

Multi-level gateway (pod ARM + site amd64) is specified in spec-006 §1.4; compose today models the **site stack** with optional simulators.

---

## 5. License note

Edge telemetry path (gateway, drivers, edge-writer, most protocol dictionaries) is **OSS (Apache-2.0)** by default. Capacity engine, digital twin renderer, multi-tenant substrate, and AI Ops surfaces are **Commercial** per [`OSS-MANIFEST.md`](../OSS-MANIFEST.md).

---

## 6. Related docs

| Doc | Use |
|-----|-----|
| [`../ops/edge-install.md`](../ops/edge-install.md) | Install & operations |
| [`../../deploy/edge/README.md`](../../deploy/edge/README.md) | Compose ports, backup, smokes |
| [`fleet.md`](fleet.md) | Multi-site product (planned) |
| [`../cios-layers.html`](../cios-layers.html) | Layer view L1–L6 |
