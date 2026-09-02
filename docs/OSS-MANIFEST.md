# License manifest

> Authoritative path → license mapping for the public CIOS repository.
> Open-core model: default **Apache-2.0** ([`LICENSE`](../LICENSE)); the paths
> listed as Commercial below are proprietary per
> [`LICENSE-COMMERCIAL.md`](../LICENSE-COMMERCIAL.md).
> Layer labels follow [`cios-layers.html`](cios-layers.html).

## Default: Apache-2.0

Everything in this repository is Apache-2.0 **except** the paths in the
Commercial table below. That includes, among others:

| Area | Paths |
|---|---|
| L1 drivers, L2 telemetry | `gateway/`, `pkg/driver/*`, `pkg/natspub`, `cmd/cios-gateway`, `cmd/cios-edge-writer`, `cmd/cios-*-driver`, `cmd/cios-*sim` |
| Site API | `core/` (except the tenant surfaces below), `cmd/cios-core` |
| Experience gateway | `pkg/apigw/`, `cmd/cios-apigw` (except the tenant-scoped surfaces below) |
| Measurement + Usage | `core/usage*.go`, `migrations/018_usage.sql`, `protocol/spec-010-metering-usage.md`, ops-portal `/usage`, customer-portal `/usage` |
| Protocol & dictionaries | `protocol/` — specs, `types.yaml`, quantities, units, locations |
| CLI | `cli/`, `cmd/cios` |
| Portals | `web/` |
| Ops & deploy | `deploy/`, `config/`, `scripts/`, `migrations/` (except 015–017), `tools/speccheck`, `tools/apicheck` |

## Commercial: in this repository, proprietary

These paths ship here because the Apache-2.0 code depends on them at compile
time. They are **not** covered by `LICENSE`; using, copying, modifying or
redistributing them requires a separate written commercial license.

### L5 — Tenant / STS multi-tenancy substrate

**Packages**

- `pkg/tenant/**`
- `pkg/sts/**`

**Migrations & tooling**

- `migrations/015_tenant_org.sql`
- `migrations/016_site_org.sql`
- `migrations/017_role_bindings.sql`
- `cmd/cios-migrate-v11/main.go`

**Core tenant / org / CRN / role-binding surfaces**

- `core/tenant.go` · `core/tenant_store_test.go`
- `core/tenants_http.go` · `core/tenants_http_test.go` · `core/tenants_admin_http.go`
- `core/orgs_http.go` · `core/orgs_http_test.go` · `core/siteorg_http.go` · `core/rolebindings_http.go`
- `core/crn.go` · `core/crn_test.go`
- `core/rbac_crn_test.go` · `core/rolebinding_test.go` · `core/siteorg_test.go` · `core/authmw_tenant_test.go`
- `core/migrate_v11.go` · `core/migrate_v11_test.go`
- `core/mtls_tenant_gate.go`

**Shared files with Commercial surfaces** (the file is shared; the tenant-scoped
symbols and routes inside it are Commercial)

- `core/store.go` — Tenant / Org / SiteOrg / RoleBinding types + Store methods
- `core/pg_store.go` — PG implementation + migrations 015–017
- `core/server.go` — `/v1/tenants/`, `/v1/orgs` routes
- `core/authmw.go` — `/v1/tenants/`, `/v1/orgs` request mapping
- `core/auth.go` · `core/rbac.go` — RoleBinding load + CRN scope compilation
- `cmd/cios-core/main.go` — `loadRBAC` RoleBinding hook
- `pkg/apigw/server.go` · `pkg/apigw/reads.go` · `pkg/apigw/customer.go` · `pkg/apigw/identity.go` — tenant-scoped reads and STS wiring

## Commercial: not in this repository

The following modules are proprietary and are **not published here**. Absent
files are intentional, not an oversight.

| Layer | Module | What it covers |
|---|---|---|
| L3 | **Digital Twin Renderer** | USD/WebGL chain: scene engine, `usdlint`, `usdmap`, Omniverse extension tooling, `pkg/sceneprune`, apigw `/api/twins`, ops-portal NOC 3D, `assets/usd/**`. Model Studio orchestration (`core/modelpacks*.go`, `core/scene_rebuild.go`, `core/sitelayout.go`, `/v1/site-layouts`, ops-portal `/admin/models*` and `/admin/draw`) is Apache-2.0 and stays in this repository; the USD toolchain and model packs it drives are commercial and unpublished. |
| L3 | **Capacity Engine** | `core/capacity*`, forecast, capacity CLI, `/v1/capacity*`, capacity portal + dashboards |
| L3 | **Alarm Intelligence** | product placeholder — no code |
| L5 | **Pricing / Cost / Statement / ERP Connector** | commercial platform B — no code |
| L6 | **AI Ops** | predictive maintenance (`core/predict*`), ops assistant (`pkg/assistant`, `cmd/cios-assistant`, `cmd/cios-corpus`), assistant portal surface |

Measurement and Usage are **not** part of the commercial platform: they are
Apache-2.0 platform domains and must stay that way.

## License files

| File | Role |
|---|---|
| [`LICENSE`](../LICENSE) | Apache-2.0 for OSS paths |
| [`LICENSE-COMMERCIAL.md`](../LICENSE-COMMERCIAL.md) | Proprietary notice for Commercial paths |
| This file | Path → license authority |
