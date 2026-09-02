# CIOS Spec 010 — Metering & Usage (OSS)

> **Version:** v0.1 DRAFT (2026-07-13)  
> **License posture:** **Apache-2.0 / OSS** (L102) — Measurement + Usage only.  
> **Authority:** L102 (D34), L103 (`feature/m3-cp`), questionnaire §2 Domain.  
> **HTTP conventions:** [spec-004](spec-004-api-conventions.md) (pagination, RFC 7807, RBAC).  
> **Non-goals (Commercial, later specs/annex):** Pricing Policy, Cost, Statement (invoice-like), ERP, tax, payment, AR, GL.

---

## 1. Purpose

Define the **platform** data model for:

1. **Measurement** — meter observations (facts; not an “engine”).  
2. **Usage** — normalized, rollup-ready **usage facts** consumed by Commercial *and* Operations.

Usage is a **first-class platform**, not a billing side-car.

---

## 2. Domain objects

| Object | Description |
|--------|-------------|
| **Measurement** | One observation: asset path, timestamp, quantity, unit, optional quality. No price/contract. |
| **UsageRecord** | One normalized usage fact for a period + subject dimensions. |
| **UsageRollup** | Materialized aggregate (daily / monthly). May equal a UsageRecord with `granularity` set. |

### 2.1 UsageRecord (normative fields)

| Field | Type | Notes |
|-------|------|--------|
| `id` | string | Stable id (`us_` + base32) |
| `kind` | enum | `energy` \| `rack_hour` (MVP); later `gpu_hour`, … |
| `tenant_id` | string | Required when known; empty only for system fixtures |
| `org_id` | string | Optional if site→org resolvable |
| `site_id` | string | First path segment or explicit |
| `asset_path` | string | CIOS asset path (spec-001) |
| `period_start` / `period_end` | RFC3339 | Half-open `[start, end)` preferred in impl notes |
| `granularity` | enum | `daily` \| `monthly` |
| `quantity` | number | Non-negative |
| `unit` | string | `kWh` (energy) \| `h` (rack_hour) |

### 2.2 MVP computation rules (L102)

| kind | Input | Rule | unit |
|------|--------|------|------|
| `energy` | Measurements of energy (kWh) or power integrated per PRMT pin | Sum over period on asset; map tenant via asset ownership / site | `kWh` |
| `rack_hour` | Assets with `type=rack` (or path leaf rack*) and lifecycle **active** | Hours in period while active (MVP: full period if active at compute time) | `h` |

**Not in MVP:** GPU-hour, TOU, dynamic kWh allocation across tenants, money fields.

---

## 3. API surface (implements via spec-004)

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/v1/usage` | List UsageRecords; query: `tenant_id`, `site_id`, `kind`, `granularity`, `period_start`, `period_end`, `page_size`, `page_token` |
| `GET` | `/v1/usage:export` | Same filters; `Accept: text/csv` or `?format=csv` → CSV export |

Responses: JSON envelope `{ "items": [ UsageRecord, ... ], "next_page_token": "" }` per spec-004 §3.  
Errors: RFC 7807 via existing core helpers.  
Authz: reuse path-glob / RoleAdmin / TenantFromContext patterns (PRMT pin).

Gateway (ops portal): `GET /api/usage` → core `/v1/usage` (mirror other Phase-A reads).

---

## 4. Events (hook)

Implementations **SHOULD** emit a subscribe-friendly signal when usage is computed (NATS subject or in-process hook). Subject naming pinned in implementing PRMT; payload = UsageRecord JSON or id list. Forecast/Carbon/Commercial **subscribe later**.

---

## 5. Out of scope (hard)

- Invoice number, tax, payment, AR, GL  
- Rate plans, discounts, commitments (Pricing Policy)  
- Cost models (Cost domain)  
- Statement engine productization beyond raw usage export  

---

## 6. Cross-reference

- **spec-004** §1 resource list: add `/v1/usage` (see §1 patch).  
- **spec-001** asset paths / lifecycle.  
- **OSS-MANIFEST** appendix D.

---

*v0.1 DRAFT — sufficient to pin PRMT-192–198 (implemented on main 2026-07-13: store/HTTP/portal/scanner/NATS/monthly+tz). Full governance freeze later.*
