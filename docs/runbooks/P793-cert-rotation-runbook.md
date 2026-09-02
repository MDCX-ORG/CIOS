# P793 — Certificate rotation runbook (manual 90d)

> **Status:** Phase 4 **manual** path (L34 90d intent).  
> **Not yet automated** — no ACME / step-ca / internal CA service in-tree.  
> Lab material: `make mtls-dev-certs` → `artifacts/mtls-dev/` (365d, **not** production).

## Scope

| Plane | What rotates | Where |
|-------|----------------|-------|
| Component mTLS | core + apigw (+ gateway) leaf certs | site intermediate / lab CA |
| Data-plane | PG/NATS/VM server certs + optional client certs | product CA or site intermediate |
| Browser → apigw | edge/TLS terminator cert | out of P793 (platform LB) |

## Target lifetime (L34 posture)

- **Leaf certs:** 90 days recommended production lifetime.
- **Renewal window:** re-issue at **T−14 days** (overlap dual-cert if possible).
- **CA:** longer-lived intermediate; root offline. Lab CA is 365d self-signed — **replace before any customer cloud**.

## Manual rotate procedure (component mTLS)

1. **Inventory** current mounts:
   - core: `CIOS_CORE_TLS_CERT` / `_KEY` / `_CLIENT_CA`
   - apigw: `CIOS_APIGW_TLS_CERT` / `_KEY` / `_CA`
   - gateway (when wired): leaf under same client-CA trust
2. **Issue** new leaves signed by the **same** client CA (URI SAN `cios://{site}/{component}`).
3. **Stage** new files beside live ones (e.g. `core-next.pem`).
4. **Reload / rolling restart**:
   - apigw first (outbound client cert) → core (server + client CA unchanged if only leaves change).
   - If **CA** rotates: update both sides' trust packs in one maintenance window.
5. **Verify**:
   ```bash
   make mtls-e2e   # lab
   # production: curl --cert apigw.pem --key apigw.key --cacert ca.pem https://core:8443/v1/health
   ```
6. **Revoke / delete** previous leaf after success; record rotate date in ops log.

## Manual rotate procedure (data-plane)

1. Postgres: rotate server cert; keep `sslmode=verify-full` + `CIOS_PG_TLS_CA` pointing at current CA; restart core last.
2. NATS: rotate server TLS; core uses `CIOS_NATS_TLS_CA` (+ optional client cert).
3. VictoriaMetrics: HTTPS only under `CIOS_DATA_PLANE_TLS=require`; update `CIOS_VM_TLS_CA` / `-vm https://…`.
4. Smoke: `pg-parity` (if PG), usage path with NATS if enabled, `/v1/metrics/query` for VM.

## Failure modes

| Symptom | Likely cause | Action |
|---------|--------------|--------|
| core boot exit `mtls require` | missing cert/key/CA | restore material; do not flip mode to off in cloud |
| handshake fail apigw→core | leaf expired / wrong CA | re-issue; check system clock |
| `data-plane tls require: postgres…` | DSN without sslmode and no `CIOS_PG_TLS_CA` | set CA or explicit `sslmode=verify-full` |
| VM 502 after rotate | HTTP still used or CA mismatch | force `https://` + CA |

## Automation backlog (not in this runbook)

- [ ] Internal CA / ACME for site intermediate
- [ ] Dual-cert hot reload without process restart
- [ ] Calendar alert T−14d per leaf
- [ ] Wire `gateway` leaf into edge binary cloud-ingest client

## Related

- `docs/P793-mtls-design-dossier.md`
- `scripts/gen-dev-mtls-certs.sh` / `make mtls-e2e`
- L34 / L104 / spec-006 §5.0bis
