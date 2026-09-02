# CIOS — Compute Infrastructure Operating System

Monitoring and management for **AI compute MDCs** (modular data centers): standardized
telemetry, operations workflows, usage measurement, and CLI-addressable points.

```text
Infrastructure  →  Compute  →  Measurement / Usage  →  Ops & Experience
```

| | |
|---|---|
| **Languages** | Go 1.25 (amd64 + arm64; pod gateways are often ARM) · TypeScript / React portals |
| **Data plane** | VictoriaMetrics (telemetry) · PostgreSQL (assets / state / usage) · NATS JetStream |
| **Addressing** | Path-shaped points, e.g. `sgp01.pod002.cdu000.fws.supply.flow` |
| **CLI** | `cios query <path>` · `cios set <path>` (policy-gated) |
| **License** | **Apache-2.0**, with a small proprietary substrate — see [License](#license) |

---

## What it does

| Capability | What you get |
|------------|--------------|
| **Asset model & paths** | CMDB-style assets with dictionary-backed types and quantities; `protocol/` is the authority |
| **Telemetry ingest** | Drivers (Modbus / SNMP / Go plugins) → gateway → NATS → edge-writer → VictoriaMetrics |
| **Query / Set** | Instant and range metrics; `set` guarded by a `risk_class` policy with optional southbound control |
| **Alarms → tickets** | Rule engine, ticket lifecycle, SLA scanning, webhooks, fail-soft notification |
| **Email notify** | SMTP as a second notification channel |
| **Ops workflows** | PM schedules, inspections, spare parts, reconciliation, ops reports |
| **Usage (measurement)** | Measurement → UsageRecord / UsageRollup; list, export, compute |
| **Portals** | Ops portal (NOC, assets, alarms, tickets, usage, admin) · Customer portal (status, SLA, usage) |
| **Identity** | OIDC / session auth, RBAC with path-scoped grants, component mTLS |

---

## Quick start

```bash
make build            # → bin/
make test
make speccheck        # validate protocol dictionaries
make ci               # full local gate
```

Local stacks and smoke runs:

```bash
make demo / demo-m1 / demo-m2      # local bring-up smokes
make control-e2e / mtls-e2e        # live e2e (local)
```

Portals (dev):

```bash
pnpm --filter @cios/ops-portal dev                              # ops portal
CUSTOMER_DEV_BYPASS=1 pnpm --filter @cios/customer-portal dev   # customer portal
```

Install and day-2 operations: [`docs/ops/edge-install.md`](docs/ops/edge-install.md).

---

## Architecture

```text
                    ┌─────────────────────────────────────┐
  Browsers          │  ops-portal · customer-portal (TS)  │  Experience
                    └───────────────┬─────────────────────┘
                                    │  /api/*  (apigw)
                    ┌───────────────▼─────────────────────┐
  Identity          │  cios-apigw  (STS, proxy, mTLS peer)│
                    └───────────────┬─────────────────────┘
                                    │  /v1/*  (+ mTLS in require mode)
                    ┌───────────────▼─────────────────────┐
  Site control      │  cios-core  (assets, auth, tickets,  │
                    │  usage, set policy, …)               │
                    └───────┬─────────────┬───────────────┘
            PostgreSQL ◄────┘             │
            VictoriaMetrics ◄─────────────┤ metrics query
            NATS (optional usage events)  │
                                          │ HTTP control (optional)
                    ┌─────────────────────▼───────────────┐
  Edge              │  cios-gateway + drivers / sims       │
                    │  cios-edge-writer → VictoriaMetrics  │
                    │  cios-alarm / cios-rules             │
                    └─────────────────────────────────────┘
```

---

## Repository layout

```text
CIOS/
├─ cmd/                      Service and tool entrypoints
│  ├─ cios                   CLI
│  ├─ cios-core              Site API
│  ├─ cios-apigw             Experience gateway
│  ├─ cios-gateway           Edge gateway
│  ├─ cios-edge-writer       NATS → VictoriaMetrics
│  └─ cios-alarm · cios-rules · drivers / sims · benches
├─ core/                     Site API implementation
├─ gateway/ · cli/ · pkg/    Edge, CLI, shared libraries
├─ web/                      Portals + shared UI packages
├─ protocol/                 ★ Specs + machine dictionaries (the constitution)
├─ migrations/               PostgreSQL
├─ deploy/ · config/ · scripts/ · tools/
└─ docs/                     See docs/README.md
```

---

## Conventions

- **`protocol/` is the constitution.** The YAML dictionaries are authoritative; `make speccheck`
  enforces vocabulary mutual exclusion. Do not invent quantities or types in code.
- **Specs are additive-only** once frozen.
- Go code must pass `go vet`, `gofmt`, and the repo `golangci-lint` config.

---

## Model Studio (bring your own USD toolchain)

The model-pack, site-layout and scene-rebuild endpoints (`/v1/site-layouts`,
model-pack import/export, and the `/admin/models` + `/admin/draw` portal pages)
ship here, but the USD tooling they drive — `usdlint`, the scene engine, and the
`assets/usd/**` model packs — is part of the commercial Digital Twin module and is
not published. These endpoints fail soft when the tooling is absent: imports are
rejected with a lint error and scene rebuilds report a missing-script status.

Point them at your own implementation with:

| Env var | Default | Purpose |
|---|---|---|
| `CIOS_MODEL_PACK_ROOT` | `assets/usd` | Where `<type>/<MODEL>.usdc` geometry lives |
| `CIOS_MODEL_STUDIO_DIR` | `artifacts/model-studio` | Staging, lint reports, bindings, scene jobs |
| `CIOS_USDLINT_SCRIPT` | `tools/usdlint/usdlint.py` | USD validator entrypoint |
| `CIOS_USDLINT_PYTHON` | `python3` | Interpreter for the validator |
| `CIOS_SCENE_SCRIPT` | `tools/scene-engine/build.py` | Scene transcode entrypoint |
| `CIOS_SCENE_PYTHON` | falls back to `CIOS_USDLINT_PYTHON`, else `/tmp/usdlint-venv/bin/python3` | Interpreter for the scene engine |
| `CIOS_SCENE_OUT` | `artifacts/scene` | Scene build output dir |

---

## License

Default license: **Apache-2.0** — see [`LICENSE`](LICENSE) (Copyright 2026 YURI MENG).

CIOS follows an open-core model. Most of this repository is Apache-2.0. A small
multi-tenancy substrate (`pkg/tenant`, `pkg/sts`, and the tenant / org / CRN /
role-binding surfaces in `core/`) ships here because the rest of the tree depends
on it, but it is **proprietary** — see [`LICENSE-COMMERCIAL.md`](LICENSE-COMMERCIAL.md)
and the exact path list in [`docs/OSS-MANIFEST.md`](docs/OSS-MANIFEST.md).

Commercial modules that are **not** part of this repository at all: the digital twin
renderer (USD / WebGL / scene engine), the capacity engine, and AI Ops
(predictive maintenance, ops assistant).

Review [`docs/OSS-MANIFEST.md`](docs/OSS-MANIFEST.md) before redistribution.
