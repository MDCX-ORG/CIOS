# @cios/customer-portal

E3.4 customer portal (PRMT-207+): tenant **Status** + **SLA** + **Usage** (对量).

Separate app from ops-portal — different port, brand, and routes (no tickets /
Set / NOC).

## Stack

- React Router 7 (SSR) + Vite + TypeScript + Tailwind
- Workspace packages: `@cios/ui`, `@cios/api-client`

## Dev

```bash
cd web
pnpm install   # if needed
# mock login required for Status/SLA
CUSTOMER_DEV_BYPASS=1 pnpm --filter @cios/customer-portal dev
# → http://127.0.0.1:3211
```

| Env | Default | Meaning |
|-----|---------|---------|
| `PORT` | `3211` | Dev server port (ops uses 3210) |
| `CUSTOMER_DEV_BYPASS` / `CIOS_CUSTOMER_DEV_BYPASS` | off | Allow mock `tenant_id` login |
| `CUSTOMER_API_BASE` | `http://127.0.0.1:8081` | Gateway base for `/api/customer/*` |

## Pages

| Path | Auth | Notes |
|------|------|-------|
| `/login` | public | Mock tenant_id + optional label |
| `/status` | required | Site health; fail-soft mock if API down |
| `/sla` | required | 99.9% / calendar_month / display-only credit |
| `/usage` | required | Energy / rack-hour facts via `/api/customer/usage`; fail-soft mock |
| `/logout` | — | Clears session cookie |
| `/healthz` | public | `{status:ok}` |

## Scripts

```bash
pnpm --filter @cios/customer-portal typecheck
pnpm --filter @cios/customer-portal build
```

## Auth model (v0)

Cookie `cios_customer_session` (HttpOnly) stores base64url JSON
`{tenant_id,label}`. Enabled only when `CUSTOMER_DEV_BYPASS=1` (or
`CIOS_CUSTOMER_DEV_BYPASS=1`). Real STS / customer realm is a later PRMT.
Live fetches use `CUSTOMER_API_BASE` only (no direct core).
