# Pipeline gap PromQL (DATA-RESILIENCE G6 / R-d)

Gateway emits per tick:

```text
cios_pipeline_heartbeat{site,path,top_asset,asset_type} 1 <unix_ms>
```

## Instant gap (no sample for 10m)

VictoriaMetrics / Grafana:

```promql
# assets that have not heartbeated in 10 minutes
(time() - timestamp(cios_pipeline_heartbeat)) > 600
```

Or absence of any heartbeat series for a site:

```promql
absent_over_time(cios_pipeline_heartbeat{site="sgp01"}[10m])
```

## Coupled alarm path

`cios-alarm -freshness-stale=10m` tracks decoded samples + heartbeat
`path` labels and upserts **major** alarms with summary `pipeline gap:…`
(rule name `pipeline-gap`). Ops HTML report lists them under
**Pipeline Gaps**.

## Suggested scrape

- Gateway metrics: `cios_gateway_*` (R-c)
- Edge-writer: last batch success via application logs; heartbeats land in VM via the import path
