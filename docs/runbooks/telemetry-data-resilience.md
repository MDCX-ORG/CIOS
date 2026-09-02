# Telemetry data resilience (gateway → NATS → edge-writer / alarm → VM/PG)

> Authority: `docs/DATA-RESILIENCE-PLAN.md` (architect rulings 2026-07-16)  
> Implemented: **G1** NakWithDelay + **G2** reconnect-forever (2026-07-16)

---

## Supported profile

**NATS JetStream is the only loss-tolerant path.**  
When `gateway.yaml` has no `nats:` block, the gateway POSTs straight to VictoriaMetrics (**M0 direct path**). That path has **no WAL** — a failed POST loses the tick. Lab/bootstrap only; do not use for production resilience (plan §4-G3 document-only ruling).

---

## What survives an outage

| Hop | Mechanism |
|-----|-----------|
| Gateway → NATS down | Local WAL (`wal_path`, default max ~1 GiB); replay on next ticks |
| JetStream | Stream `CIOS_TLM` FileStorage, MaxAge 7d |
| edge-writer → VM | Durable consumer, Ack after 2xx; **NakWithDelay** on transport/5xx (G1) |
| cios-alarm → PG | Same: **NakWithDelay** on upsert failure (G1) |
| NATS process flap | Client **MaxReconnects(-1)** (G2); WAL absorbs publish gaps |

Poison = **malformed JSON / unknown encoding** only → Ack and drop.  
**Not** poison: VM down, network error, PG unreachable.

---

## WAL sizing (G4)

Default WAL is **drop-newest when full** (`ErrWALFull`). Size so the WAL covers the longest WAN outage you care about:

```text
wal_max_bytes ≥ bytes_per_tick_gzip × ticks_per_hour × target_outage_hours
```

Rough lab: ~2 KiB/tick/asset gzipped → 1 GiB covers **days** at modest cadence.  
On full: WARN + (when counters land in R-c) `cios_gateway_wal_full_drops_total`.  
**Ring-buffer (drop-oldest) is parked** until a field WAL-full incident.

`gateway.yaml` example:

```yaml
nats:
  url: nats://127.0.0.1:4222
  stream_name: CIOS_TLM
  wal_path: /var/lib/cios/gateway.wal
  wal_max_bytes: 1073741824   # 1 GiB
```

---

## Operator drills (acceptance shape)

1. **N3 VM outage:** run gateway + edge-writer under load; stop VM 30 min; restart VM; sample count in window ≈ expected ticks (no permanent drop after five Naks).  
2. **N2 NATS outage:** stop nats-server 10 min; restart; gateway WAL drains; edge-writer resumes; disconnect/reconnect lines in logs.  
3. Unexpected NATS close on edge-writer/alarm → process **exits** (compose restart policy brings it back).

Template for automation: extend `scripts/control-e2e.sh` style → future `scripts/outage-drill.sh`.

---

## Explicit non-goals (by design)

- No fabrication of sensor samples when Collect fails  
- No auto-retry of control Set (human after readback; control API is loopback+token, M4 F1)  
- Notifications remain best-effort fail-soft  

---

## Pipeline freshness (G6 / R-d)

1. **Gateway** appends `cios_pipeline_heartbeat{site,path,top_asset,asset_type}` on every device tick (NATS and direct-HTTP paths).  
2. **cios-alarm** `-freshness-stale` (default 10m) tracks last-seen assets (samples + heartbeat path labels). Silent assets → major alarm rule `pipeline-gap`.  
3. **Ops report HTML** section **Pipeline Gaps** lists firing/acked alarms with “pipeline gap” in the summary.  
4. **PromQL** examples: `deploy/edge/alerts/pipeline-gap.promql.md`.

---

## Metrics scrape (G5 / R-c)

Optional loopback HTTP — empty = disabled (zero regression).

| Binary | Flag / env | Example series |
|--------|------------|----------------|
| gateway | `-metrics-listen` / `CIOS_GATEWAY_METRICS_LISTEN` | `cios_gateway_publish_failures_total`, `cios_gateway_wal_frames_total`, `cios_gateway_wal_bytes`, `cios_gateway_wal_full_drops_total` |
| edge-writer | `-metrics-listen` / `CIOS_EDGE_WRITER_METRICS_LISTEN` | `cios_edge_writer_nak_total`, `cios_edge_writer_poison_drops_total{reason}`, `cios_edge_writer_vm_post_failures_total{class}` |
| cios-alarm | `-metrics-listen` / `CIOS_ALARM_METRICS_LISTEN` | `cios_alarm_nak_total`, `cios_alarm_poison_drops_total{reason}`, `cios_alarm_upsert_failures_total` |

```bash
cios-gateway -config gw.yaml -metrics-listen 127.0.0.1:9102
curl -s http://127.0.0.1:9102/metrics
```

WAL-full also logs a WARN line for human grepping before counters move.

---

## Related code

- `pkg/natspub/drop.go` — `NakBackoff`, `NakTransient`, `TransientMaxDeliver`  
- `pkg/natspub/connect.go` — `ConnectOpts` (reconnect forever)  
- `pkg/resilmetrics` — text /metrics helper  
- `cmd/cios-edge-writer`, `cmd/cios-alarm`, `gateway/run.go`  
