# CIOS Edge — install & operations

> Operator runbook for the **Site Plane** stack under `deploy/edge/`.  
> Deep compose notes: [`../../deploy/edge/README.md`](../../deploy/edge/README.md).  
> Product context: [`../product/edge.md`](../product/edge.md).

---

## 1. Prerequisites

| Requirement | Notes |
|-------------|--------|
| Docker + Compose v2 | OrbStack / Docker Desktop / engine |
| Free loopback ports | `3000`, `4222`, `5432`, `8080`, `8222`, `8428` (plus app ports if used) |
| Go 1.25+ | Host builds (`make build`) |
| Node / pnpm | Only if running portals on the host |
| RAM | ≥8 GiB recommended for full `demo-m1` / `demo-m2` |

**Security default:** all published ports bind `127.0.0.1` only.

---

## 2. Quick start (infra only)

```bash
# from repo root
cp deploy/edge/.env.example deploy/edge/.env
# set CIOS_PG_PASSWORD and CIOS_GRAFANA_PASSWORD
make edge-up          # docker compose up -d --wait
```

### Stop / reset

```bash
make edge-down        # stop containers; keep volumes
docker compose --project-directory deploy/edge down -v   # wipe data
```

### Ports

| Port | Service | Use |
|------|---------|-----|
| 8428 | VictoriaMetrics | Ingest / query / health |
| 5432 | PostgreSQL | `cios` DB |
| 4222 | NATS | Client |
| 8222 | NATS | Monitoring `/healthz` |
| 3000 | Grafana | Dashboards |

Grafana: `http://127.0.0.1:3000` — password from `deploy/edge/.env`.

---

## 3. Full site stack (lab)

### Option A — Makefile smokes (recommended)

```bash
make demo             # M0: sim + core + gateway + query/alarm smoke
make demo-m1          # M1: compose business services + end-to-end asserts
make demo-m2          # M2: ticket loop + ops surfaces
make demo-m1-clean    # or demo-m2-clean / demo-clean
```

Smokes are **local-only** (not part of `make ci`).

### Option B — apps profile (core + apigw)

```bash
make edge-up
docker compose --project-directory deploy/edge \
  -f deploy/edge/docker-compose.yml \
  -f deploy/edge/docker-compose.apps.yml \
  --profile apps up -d --wait
```

Portals still typically run on the host:

```bash
pnpm --filter @cios/ops-portal dev
CUSTOMER_DEV_BYPASS=1 pnpm --filter @cios/customer-portal dev
```

Lab credentials: see `config/rbac.lab-sample.yaml` (lab only — never production).

---

## 4. Day-2 operations

### Health

```bash
curl -sS http://127.0.0.1:8428/health
curl -sS http://127.0.0.1:8222/healthz
curl -sS http://127.0.0.1:8080/v1/health/ready   # when core is up
```

### Backup / restore

```bash
make backup
make restore ARGS='--from backups/<UTC-ts>'           # dry-run
make restore ARGS='--from backups/<UTC-ts> --yes'     # apply
```

Artifacts: `backups/<ts>/{pg.dump,vm-snapshot.tar.gz,manifest.txt}`.  
Retention is operator-owned (no automatic prune).

### Alarm rules

- Path: `deploy/edge/rules/*.yaml`
- Engine: `cios-alarm` (NATS self-comparator)
- Bootstrap rule intentionally fires in lab (`leak == 0`) to exercise ticket loop

### Soak (ops loop)

```bash
make soak ARGS="--minutes 5 --cycle 1m --probe 2m"   # short smoke
make soak ARGS="--days 7"                            # PRMT-grade window
```

Evidence under `artifacts/soak/` (gitignored).

### Control / security e2e (optional)

```bash
make control-e2e      # Set southbound write + readback
make mtls-e2e         # component mTLS lab
make pg-parity        # PG store parity
```

mTLS design & rotation: [`../runbooks/P793-mtls-design-dossier.md`](../runbooks/P793-mtls-design-dossier.md),
[`../runbooks/P793-cert-rotation-runbook.md`](../runbooks/P793-cert-rotation-runbook.md).

---

## 5. CLI essentials

```bash
make build
export CIOS_URL=http://127.0.0.1:8080
# lab token if RBAC on — see config/rbac.lab-sample.yaml

./bin/cios asset list
./bin/cios query <path>
./bin/cios set <path> <value>     # risk_class gated
./bin/cios ticket list
./bin/cios alarm list
```

---

## 6. Troubleshooting

| Symptom | Check |
|---------|--------|
| Compose never healthy | `docker compose … ps` · free ports · `.env` passwords set |
| No `cios_*` series in VM | gateway / edge-writer / modbussim logs; pointmap path |
| Alarms never fire | `deploy/edge/rules/` mounted; bootstrap or soak mode |
| Tickets empty | `-auto-ticket` on alarm; core reachable from alarm container |
| Portal 401 | Lab sample credentials / STS bypass flags |

---

## 7. Production notes

1. **Do not** use sample tokens or `-allow-no-auth` outside lab.  
2. Publish only via reverse proxy / private network; keep loopback model or explicit mTLS.  
3. Set retention, backup cron, and alert on `make restore` dry-run failures.  
4. Cloud-facing site deployment follows L104 (x86 cloud) + §5.0bis before customer traffic.  
5. Multi-site aggregation is **not** Edge — see [`fleet-install.md`](fleet-install.md).
