#!/usr/bin/env bash
# scripts/pg-parity.sh — PRMT-211 / P795: run PG-backed core tests against a live Postgres.
#
# Local-only (not in `make ci` by default). Boots throwaway Postgres via docker
# unless CIOS_PG_DSN is already set. Tears down container on exit when we started it.
#
# Usage:
#   make pg-parity
#   make pg-parity ARGS='--check-only'   # shorter package set
#   CIOS_PG_DSN=postgres://... make pg-parity   # use existing DSN (no docker)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

CHECK_ONLY=0
for a in "$@"; do
  case "$a" in
    --check-only) CHECK_ONLY=1 ;;
    -h|--help)
      sed -n '2,12p' "$0"
      exit 0
      ;;
  esac
done

CONTAINER=""
STARTED=0
cleanup() {
  if [[ "$STARTED" = "1" && -n "$CONTAINER" ]]; then
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

if [[ -z "${CIOS_PG_DSN:-}" ]]; then
  if ! command -v docker >/dev/null 2>&1; then
    echo "pg-parity: docker missing and CIOS_PG_DSN unset" >&2
    exit 2
  fi
  # Ephemeral PG; random name; publish on free high port.
  CONTAINER="cios-pg-parity-$$"
  PORT=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
)
  echo "==> starting throwaway postgres on 127.0.0.1:${PORT}"
  docker run -d --rm --name "$CONTAINER" \
    -e POSTGRES_USER=cios \
    -e POSTGRES_PASSWORD=cios \
    -e POSTGRES_DB=cios \
    -p "127.0.0.1:${PORT}:5432" \
    postgres:16.14-alpine >/dev/null
  STARTED=1
  export CIOS_PG_DSN="postgres://cios:cios@127.0.0.1:${PORT}/cios?sslmode=disable"
  # Wait ready (postgres:alpine can take >30s under load).
  for i in $(seq 1 120); do
    if docker exec "$CONTAINER" pg_isready -U cios >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done
  if ! docker exec "$CONTAINER" pg_isready -U cios >/dev/null 2>&1; then
    echo "pg-parity: postgres not ready" >&2
    exit 3
  fi
else
  echo "==> using existing CIOS_PG_DSN"
fi

echo "==> CIOS_PG_DSN=${CIOS_PG_DSN%%@*}@…"

# Core packages that gate on CIOS_PG_DSN (append-only audit, usage, tenants, …).
PKGS=(./core/)
if [[ "$CHECK_ONLY" = "1" ]]; then
  echo "==> check-only: focused PG tests"
  CGO_ENABLED=1 go test -count=1 -timeout 180s "${PKGS[@]}" \
    -run 'TestPG_|TestUsage.*PG|TestSparePG_|TestRoleBinding.*PG|TestSiteOrg.*PG|TestTenant.*PG'
else
  echo "==> full package tests with CIOS_PG_DSN (skips without DSN are now live)"
  CGO_ENABLED=1 go test -count=1 -timeout 600s "${PKGS[@]}"
fi

echo "==> pg-parity OK"
