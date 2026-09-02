# CIOS Web (Ops Portal monorepo)

PRMT-140 scaffold: pnpm workspace, React Router v7 SSR ops portal,
shared shadcn/ui design system, typed `/api/*` client.

## Layout

- `apps/ops-portal` — SSR app (the only app in this PRMT)
- `packages/ui` — shared Tailwind preset + shadcn components
- `packages/api-client` — typed read-only `fetch` wrapper over `/api/*`

## Quick start

```bash
# requires Node 22 LTS and pnpm 9
cd web
pnpm install --frozen-lockfile
pnpm -r typecheck
pnpm -r lint
pnpm --filter ops-portal build
MOCK_GATEWAY=1 PORT=3210 node tests/smoke.mjs
```

## Auth model (L81 / D31)

Portal is a thin `/api/*` consumer. Identity comes from the gateway-set
`cios_session` cookie, exchanged server-side for a bearer via
`POST {GATEWAY_BASE_URL}/auth/ops/token`. The bearer (not the cookie)
authenticates `/api/*` calls. No `fetch` from the browser reaches the
gateway or infra directly.
