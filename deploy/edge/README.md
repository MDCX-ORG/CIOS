# CIOS Edge Base Stack

> **Product:** [`docs/product/edge.md`](../../docs/product/edge.md) ·
> **Install / ops:** [`docs/ops/edge-install.md`](../../docs/ops/edge-install.md) ·
> **Fleet (planned):** [`docs/product/fleet.md`](../../docs/product/fleet.md)

Three storage/bus foundations by default (VM / PostgreSQL / NATS / Grafana).
Optional **apps** profile (PRMT-212) adds `cios-core` + `cios-apigw` for a
lab browse path. Portals still run on the host (`make portal-live` or
`pnpm --filter customer-portal dev`).

## Start

```bash
cp deploy/edge/.env.example deploy/edge/.env
$EDITOR deploy/edge/.env                 # set CIOS_PG_PASSWORD
make edge-up                             # = docker compose --project-directory deploy/edge up -d --wait

# Optional: core + apigw (lab; DEV_NO_AUTH / allow-no-auth — not production)
docker compose --project-directory deploy/edge \
  -f deploy/edge/docker-compose.yml -f deploy/edge/docker-compose.apps.yml \
  --profile apps up -d --wait
```

`--wait` blocks until all three healthchecks are green.

## Stop / reset

```bash
make edge-down                           # stop & remove containers, KEEP volumes
docker compose --project-directory deploy/edge down -v   # full reset: drop volumes too
```

## Data locations (named volumes)

- `vm-data`     → `/storage` (VictoriaMetrics TSDB)
- `pg-data`     → `/var/lib/postgresql/data`
- `nats-data`   → `/data` (JetStream store; stream/KV creation is M1)

## Ports (127.0.0.1 only — spec-006 §5.2 zero-inbound)

| port | service            | purpose                          |
|------|--------------------|----------------------------------|
| 8428 | victoriametrics    | HTTP `/health`, ingestion, query |
| 5432 | postgres           | `cios` DB, user `cios`           |
| 4222 | nats               | client connections (NATS)        |
| 8222 | nats               | HTTP monitoring `/healthz`       |
| 3000 | grafana            | web UI `/login` + datasources    |
| 8080 | cios-core (edge)   | site API `/v1/*` (when apps up)  |
| 3210 | ops-portal (host)  | **Portal UI** via `make portal-live` — **not** Grafana |

> **Do not confuse :3000 with the ops portal.** Grafana owns `127.0.0.1:3000`.
> Lab portal is typically `http://127.0.0.1:3210/` (see `make portal-live` /
> `scripts/portal-live.sh`). Opening :3000 in a browser is metrics dashboards,
> not Admin Tenants/Sites.

## Grafana

The compose stack includes Grafana (12.3.x) provisioned with one
VictoriaMetrics datasource (`CIOS-Edge-VM`, uid `cios-edge-vm`)
pointing at the in-network VM, and one dashboard (`CIOS Edge`,
uid `cios-edge-m0`) with four panels driven by the M0 demo point
map (supply/return temp, FWS flow, device status, leak).

Access from a browser on the host:

```bash
# credentials live in deploy/edge/.env
grep ^CIOS_GRAFANA_PASSWORD deploy/edge/.env
open http://127.0.0.1:3000    # admin / $CIOS_GRAFANA_PASSWORD
```

The provisioning directory (`deploy/edge/grafana/provisioning`) and
dashboard directory (`deploy/edge/grafana/dashboards`) are
bind-mounted read-only; Grafana reloads them on container restart.

## Backup / restore (PRMT-071, T32 mechanism)

The edge stack can be snapshotted and restored with two `local-only`
ops scripts. They are **mechanism-only** — RPO/RTO target values are
out of scope here and will be set by decision D27.

```bash
make backup                      # produce backups/<UTC-ts>/{pg.dump,vm-snapshot.tar.gz,manifest.txt}
make restore ARGS='--from backups/<UTC-ts>'         # dry-run: prints the plan, makes no changes
make restore ARGS='--from backups/<UTC-ts> --yes'   # actually restore (refuses without --yes)
```

Backups land in `backups/<UTC-ts>/` (override with `--out <dir>`).
Each directory carries:

- `pg.dump` — `pg_dump -Fc` of the `cios` database, produced via
  `docker compose exec postgres` (no host-side port access).
- `vm-snapshot.tar.gz` — VM snapshot via `POST /snapshot/create`
  (port 8428 is 127.0.0.1-published per the table above).
- `manifest.txt` — UTC timestamp, component versions, and sha256 of
  the two artifacts.

The restore script **defaults to dry-run**: it prints what it would
do without touching data. Pass `--yes` to actually overwrite the
running PG database and VM storage. There is a second safety net
`CIOS_DRY_RUN_ONLY=1` env var that forces dry-run even with `--yes`.

The script does **not** delete old backups — retention is the
caller's / cron job's responsibility. `make backup` and
`make restore` never enter `make ci`; they require the stack to be
running (`make edge-up`) and a working `docker compose` on `PATH`.

## M0 end-to-end demo

The `make demo` target runs `scripts/m0-smoke.sh`, which:

1. Generates `deploy/edge/.env` from `.env.example` if missing (random
   hex for both passwords).
2. Brings the edge stack up (`make edge-up`).
3. Builds `bin/{cios,cios-core,cios-gateway,cios-modbussim}`.
4. Starts the simulator (127.0.0.1:15020), `cios-core` (8080), and
   `cios-gateway` (configured by `gateway.yaml`) as background procs.
5. Exercises M0 exit criterion ②: `cios apply` → `cios asset list`
   → `cios query` → `cios alarm list --severity critical`.
6. Exercises M0 exit criterion ④: Grafana datasource health +
   VM proxy query + dashboard title.
7. Prints `M0 SMOKE PASS`.

```bash
make demo           # last line: M0 SMOKE PASS
make demo           # second run also passes (idempotent via request_id + seed upsert)
make demo-clean     # kill background cios-* procs, docker compose down (KEPS volumes)
```

`make demo` is local-only and intentionally **not** part of `make ci`
— it requires docker and free ports 3000/8080/8428/15020.

## M1 single-site full-stack e2e

`make demo-m1` runs `scripts/m1-smoke.sh`, which brings up the four
infra containers **plus the six M1 business services** in one
`docker compose up -d --build`:

| service           | purpose                                                            |
|-------------------|--------------------------------------------------------------------|
| `cios-modbussim`  | single-device Modbus TCP sim (binds 0.0.0.0 in compose, no host)   |
| `cios-gateway`    | modbus → NATS JetStream + VM import, gateway.compose.yaml          |
| `cios-edge-writer`| NATS consumer → VictoriaMetrics `/api/v1/import/prometheus`        |
| `cios-core`       | site API (HTTP 8080 on 127.0.0.1, PG-backed, no RBAC for demo); Usage scan 1h + NATS sink (PRMT-198) |
| `cios-rules`      | derived-quantity recording rules (`-interval 30s`)                 |
| `cios-alarm`      | AlarmRule engine; reads `deploy/edge/rules/`, writes PG + CE       |

The smoke asserts four end-to-end properties:
1. VM has at least one `cios_*` series (gateway → VM worked).
2. `curl http://127.0.0.1:8080/v1/assets` contains `site01.pod000.cdu000`.
3. PG `alarms` table contains ≥1 firing row for
   `site01.pod000.cdu000` within 60s (R5 §4.7; the firing row may
   come from the bootstrap rule or any PRMT-027 facility rule).
4. (Optional) VM has `cios_deltat_celsius` from `cios-rules`.

```bash
make demo-m1           # full stack up + smoke PASS (or fail non-zero)
make demo-m1           # idempotent: re-run passes again
make demo-m1-clean     # docker compose down -v
```

**`make demo-m1` is local-only** — it never enters `make ci`. It
needs docker compose, free ports 3000/4222/5432/8080/8222/8428, and
a previously-built `Makefile` `build` (the Dockerfile rebuilds all
six binaries on `--build` so a stale host build is harmless).

## Usage live path (L102 / PRMT-198 · §M3-1 = 用量查看)

With the compose stack up (`make demo-m1` or equivalent):

1. **cios-core flags (compose default):**  
   `-usage-scan-interval=1h` · `-nats-url=nats://nats:4222` · `-pg-dsn=…` · `-vm=http://victoriametrics:8428`
2. **Manual recompute (admin token if RBAC on):**  
   `POST /v1/usage:compute` with `period_start` / `period_end` / `granularity` (`daily`|`monthly`) and optional measurements.  
   Scanner ticks auto-materialize previous UTC day + previous calendar month (site `Spec.timezone` or UTC).
3. **List / export:**  
   `GET /v1/usage?tenant_id=…&kind=energy` · `GET /v1/usage:export?…` (CSV)
4. **Portal:** ops-portal `/usage` (filters → API). §M3-1 exit = **用量查看** in Portal (not invoice / bill reconciliation).
5. **NATS:** subject `cios.usage.upserted` (JSON UsageRecord) after each upsert when `-nats-url` is set.
6. **PG unit test (host):** set `CIOS_PG_DSN` to published Postgres (see `.env.example`) then  
   `go test ./core/ -run UsageStore_PGParity -count=1`

## M2 ops-loop end-to-end smoke

`make demo-m2` runs `scripts/m2-smoke.sh` (PRMT-051). It mirrors
`make demo-m1` for stack bring-up, then asserts the seven M2
operations-loop properties for `site01.pod000.cdu000`:

1. **Stack up** — same `docker compose up -d --build` as
   `demo-m1`; wait for `cios-core /v1/assets` 200; apply the
   seed asset (idempotent dedup).
2. **Alarm → auto-opened ticket** — poll `/v1/tickets` until a
   row with `alarm_id` non-empty and `state=open` appears
   (cios-alarm runs with `-auto-ticket` per
   `deploy/edge/docker-compose.yml`; spec-008 §4 / L69).
3. **Ticket lifecycle** — `cios ticket ack|resolve|close
   <id>` → assert each transition returns 2xx and the
   `acked_at` / `resolved_at` / `closed_at` timestamps become
   non-empty on the resulting ticket.
4. **`alarm_id` dedup invariant** — across the live ticket
   set, every `alarm_id` maps to at most one ticket with
   `state != "closed"` (spec-008 §4 / Q2 → L69).
5. **Ops report** — `GET /v1/reports/ops` returns `mttr_seconds`
   plus `ticket_counts.{by_state,by_severity}` populated.
6. **Wiring probes** — `/v1/pm/schedules` and
   `/v1/assets/{path}:history` each return 2xx (PRMT-043 /
   045 wiring health, no deep numerical check).
7. **PASS/FAIL** — all six properties above → `M2 SMOKE PASS`,
   exit 0; any failure prints context and exits 1.

```bash
make demo-m2           # full stack up + 7-step smoke PASS (or fail non-zero)
make demo-m2           # idempotent: re-run passes (tickets closed in step 3)
make demo-m2-clean     # docker compose down -v
```

**`make demo-m2` is local-only** — it never enters `make ci`. It
needs the same docker compose / ports / build state as `demo-m1`
(the M1 stack is a strict subset; M2 adds no new containers). All
M2 surfaces (tickets, ops report, PM schedules, audit
history) ride on the existing `cios-core` service.

### M2 ops-loop soak (PRMT-098 / §M2-1)

`make soak` runs `scripts/m2-soak.sh`, the long-running closure
harness for the §M2-1 ops loop. It mirrors `demo-m2` for
stack bring-up, then:

1. Runs **one** startup firing → open → ack → resolve → close
   closed loop (relies on the bootstrap rule + `-auto-ticket`,
   same source as `demo-m2` step 2–3).
2. Loops every `--cycle` (default 5m), polling for an open
   ticket with `alarm_id` set; walks ack → resolve → close when
   one appears.
3. Probes every `--probe` (default 1h): `GET /v1/health/ready`
   (down = 503), `GET /v1/health/scanners`, and snapshots
   `GET /v1/reports/ops` → `artifacts/soak/<ts>-ops.json`.
4. On exit (clean or signal) writes
   `artifacts/soak/SUMMARY.md` with firing/open/closed counts,
   down events, MTTR/MTBF, and a closed-loop sample.

The release/m2.1 simulator has no controlled threshold API, so
periodic per-cycle firings depend on whatever state the running
stack produces; in practice the soak sees **one** startup loop
plus N periodic health/ops snapshots. This is a smoke-grade
observation window, not a 7-day §M2-1 pass — the PRMT §6/§7
notes call this out explicitly.

```bash
make soak                       # default 4h (operator override of PRMT 7d)
make soak ARGS="--days 7"       # full PRMT-098 default (PRMT says 7 days)
make soak ARGS="--minutes 5 --cycle 1m --probe 2m"   # smoke-grade
make soak ARGS="--resume --hours 4"                  # resume from SUMMARY.md
make soak-clean                 # rm -rf artifacts/soak
```

`make soak` is local-only and never enters `make ci`. It writes
evidence to `artifacts/soak/` (gitignored). Exit code 0 only if
the full duration elapsed with no unrecovered dependency-down
event; otherwise 1 plus a red SUMMARY header.

### Host-boundary safety (spec-006 §5.2)

Only the infra ports and `cios-core`'s 8080 are published to
127.0.0.1. Business-to-business traffic (gateway ↔ modbussim,
alarm ↔ postgres, etc.) stays on the compose bridge network
`cios-edge` and never reaches the host. The `-allow-public-bind`
flag on `cios-modbussim` (§4.8 in PRMT-026) only relaxes the
simulator's own loopback guard so the gateway can reach it across
the bridge; no port is published to the host for the flag to
escape through.

### Bootstrap rule

`deploy/edge/rules/bootstrap.yaml` carries one `AlarmRule` whose
`expr: "leak == 0"` is a deliberately-true sentinel: the modbussim
seeds register `0x0031` to `0` and never overwrites it (jitter only
touches `0x0010`/`0x0011`/`0x0012`), so the expression fires on the
first truthy tick (`for: 0s`).

See `deploy/edge/pointmaps/cdu-sim.yaml:31-32` for the `leak` ident
binding; ident grammar `[a-z0-9.]+` in `pkg/alarm/expr.go:547`;
PRMT-027 coexistence in §4.6 of PRMT-026 R5 prompt.

### PRMT-121 soak mode (drives alarm→ticket→close repeats)

`scripts/m2-soak.sh` exports four env vars before `compose up` so
`cios-modbussim` toggles on a deterministic-randomised spike loop
on `tcs.opening` (register `0x0020`). Default behaviour is
unchanged — the gate is opt-in.

| env var                    | default | meaning                                                       |
|----------------------------|---------|---------------------------------------------------------------|
| `CIOS_SOAK_MODE`           | `0`     | `"1"` enables the soak goroutine; any other value leaves the sim byte-identical to pre-PRMT-121 |
| `CIOS_SOAK_PERIOD_MIN_S`   | `60`    | lower bound of random wait between spikes (seconds)           |
| `CIOS_SOAK_PERIOD_MAX_S`   | `180`   | upper bound of random wait between spikes (seconds; clamped to `MIN` if `< MIN`) |
| `CIOS_SOAK_DWELL_S`        | `15`    | HIGH duration per spike (seconds; must exceed gateway modbus poll interval, default 5s) |

The matching rule `deploy/edge/rules/cdu-tcs-opening-soak.yaml`
(`tcs.opening > 90`, `for: 0s`, `minor`) is loaded unconditionally
but inert under default-off — `tcs.opening` stays at baseOpening
(45) so the threshold is never crossed.

To run a soak outside `scripts/m2-soak.sh`, export the vars in your
shell before `make edge-up`:

```bash
export CIOS_SOAK_MODE=1
export CIOS_SOAK_PERIOD_MIN_S=60
export CIOS_SOAK_PERIOD_MAX_S=180
export CIOS_SOAK_DWELL_S=15
make edge-up
```

### vmalert: site-level derived quantities (PRMT-028)

The vmalert service loads `deploy/edge/vmalert/site-derived.yaml` and
evaluates 4 recording rules every 30s, remoteWriting back to
VictoriaMetrics with `producer=vmalert` (spec-006 §5.3 audit):

| record | expr | host |
|--------|------|------|
| cios_itload_watt | `sum by (site, quality) (cios_power_watt{asset_type="tou"})` | site |
| cios_facilityload_watt | `sum by (site, quality) (cios_power_watt{asset_type="meter"})` | site |
| cios_pue_ratio | `cios_facilityload_watt / on(site, quality) group_left cios_itload_watt` | site |
| cios_wue_l_per_kwh | `sum by (site, quality) (rate(cios_volume_liter{...}[1h])) / on(site, quality) group_left sum by (site, quality) (rate(cios_energy_kwh{...}[1h]))` | site |

Per-instance derivations (deltat/heat/cop) are NOT here — those live
in cios-rules (PRMT-024/PRMT-021). L67 keeps aggregation at vmalert;
L23 keeps record names byte-identical to `promproj.MetricName(q, dict)`.

### Service table

11 containers: 4 infra (VM/PG/NATS/Grafana) + 6 business (modbussim/
gateway/edge-writer/core/rules/alarm) + 1 vmalert. Only core:8080 and
the 4 infra ports are published to 127.0.0.1 (spec-006 §5.2).
vmalert:8880 is container-internal; uncomment the ports block in
docker-compose.yml for local debugging.

### Grafana (12.4.3, pinned in docker-compose.yml)

3 M1 facility dashboards provision from `deploy/edge/grafana/dashboards/`:
- site-overview (uid=cios-site-overview): site-level PUE/WUE/itload/facilityload
- ac40-overview (uid=cios-ac40): CDU / cooling loop (fws)
- dc40-overview (uid=cios-dc40): rack / DLC loop
