#!/usr/bin/env bash
# portal-live.sh — one-shot live stack for E3.5 ops portal (dev only).
#
# Brings up:
#   1) seed-ext + cios-core   (default: this repository)
#   2) cios-apigw no-auth     (default: this repository)
#   3) ops-portal MOCK_GATEWAY=0 → apigw
#
# Usage:
#   scripts/portal-live.sh              # foreground; Ctrl-C tears down
#   scripts/portal-live.sh --check-only # assert health then exit 0/1
#
# Env overrides:
#   MODEL_ROOT  AUTH_ROOT  UI_ROOT
#   CORE_PORT=8090  APIGW_PORT=8089  PORTAL_PORT=3210
#   STORE=/tmp/cios-portal-live-store.json
set -euo pipefail

UI_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODEL_ROOT="${MODEL_ROOT:-$UI_ROOT}"
AUTH_ROOT="${AUTH_ROOT:-$UI_ROOT}"

CORE_PORT="${CORE_PORT:-8090}"
APIGW_PORT="${APIGW_PORT:-8089}"
PORTAL_PORT="${PORTAL_PORT:-3210}"
STORE="${STORE:-/tmp/cios-portal-live-store.json}"
CHECK_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --check-only) CHECK_ONLY=1; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[[ -d /opt/homebrew/opt/node@22/bin ]] && export PATH="/opt/homebrew/opt/node@22/bin:${PATH:-}"

LOG_DIR="${UI_ROOT}/.portal-live-logs"
PID_DIR="${UI_ROOT}/.portal-live-pids"
mkdir -p "$LOG_DIR" "$PID_DIR" "$UI_ROOT/bin"

cleanup() {
  local rc=$?
  for pf in "$PID_DIR"/*.pid; do
    [[ -f "$pf" ]] || continue
    local pid; pid=$(cat "$pf" 2>/dev/null) || continue
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  sleep 1
  for pf in "$PID_DIR"/*.pid; do
    [[ -f "$pf" ]] || continue
    local pid; pid=$(cat "$pf" 2>/dev/null) || continue
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  done
  rm -f "$PID_DIR"/*.pid
  exit "$rc"
}
trap cleanup EXIT INT TERM

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need go; need curl; need python3; need node

echo "== portal-live =="
echo "  UI_ROOT=$UI_ROOT"
echo "  MODEL_ROOT=$MODEL_ROOT"
echo "  AUTH_ROOT=$AUTH_ROOT"
echo "  core=:$CORE_PORT apigw=:$APIGW_PORT portal=:$PORTAL_PORT"

# --- 1) seed + core (prefer model tree for seed-ext) ---
SEED_ROOT="$MODEL_ROOT"
if [[ ! -d "$SEED_ROOT/cmd/seed-ext" ]]; then
  SEED_ROOT="$UI_ROOT"
fi
if [[ ! -d "$SEED_ROOT/cmd/seed-ext" ]]; then
  echo "seed-ext not found under MODEL_ROOT or UI_ROOT" >&2
  exit 1
fi

echo "==> [1/4] seed-ext → $STORE"
(
  cd "$SEED_ROOT"
  go run ./cmd/seed-ext -out "$STORE" -protocol "$SEED_ROOT/protocol" \
    >"$LOG_DIR/seed-ext.log" 2>&1
)

echo "==> [2/4] cios-core :$CORE_PORT"
CORE_ROOT="$SEED_ROOT"
if [[ ! -d "$CORE_ROOT/cmd/cios-core" ]]; then CORE_ROOT="$UI_ROOT"; fi
(
  cd "$CORE_ROOT"
  CGO_ENABLED=0 go build -tags lab -o "$UI_ROOT/bin/cios-core" ./cmd/cios-core
)
"$UI_ROOT/bin/cios-core" \
  -store "$STORE" \
  -protocol "$CORE_ROOT/protocol" \
  -allow-no-auth \
  -listen "127.0.0.1:${CORE_PORT}" \
  >"$LOG_DIR/core.log" 2>&1 &
echo $! >"$PID_DIR/core.pid"

CORE_OK=0
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${CORE_PORT}/v1/assets" >/dev/null 2>&1; then
    CORE_OK=1; break
  fi
  sleep 0.5
done
[[ "$CORE_OK" = 1 ]] || { echo "core not ready; see $LOG_DIR/core.log" >&2; exit 3; }
echo "    core up"

# --- 2) apigw ---
echo "==> [3/4] cios-apigw :$APIGW_PORT"
APIGW_BUILD="$AUTH_ROOT"
(
  cd "$APIGW_BUILD"
  CGO_ENABLED=0 go build -tags lab -o "$UI_ROOT/bin/cios-apigw" ./cmd/cios-apigw
)
CIOS_APIGW_DEV_NO_AUTH=1 \
CIOS_APIGW_LISTEN="127.0.0.1:${APIGW_PORT}" \
CIOS_APIGW_UPSTREAM="http://127.0.0.1:${CORE_PORT}" \
  "$UI_ROOT/bin/cios-apigw" >"$LOG_DIR/apigw.log" 2>&1 &
echo $! >"$PID_DIR/apigw.pid"

APIGW_OK=0
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${APIGW_PORT}/healthz" >/dev/null 2>&1; then
    APIGW_OK=1; break
  fi
  sleep 0.5
done
[[ "$APIGW_OK" = 1 ]] || { echo "apigw not ready; see $LOG_DIR/apigw.log" >&2; exit 4; }
echo "    apigw up"

# --- 3) portal ---
echo "==> [4/4] ops-portal :$PORTAL_PORT (MOCK_GATEWAY=0)"
[[ -d /opt/homebrew/opt/node@22/bin ]] && export PATH="/opt/homebrew/opt/node@22/bin:${PATH}"
(
  cd "$UI_ROOT/web"
  if [[ ! -f apps/ops-portal/build/server/index.js ]]; then
    corepack pnpm install --frozen-lockfile
    pnpm --filter @cios/ops-portal build
  fi
)
(
  cd "$UI_ROOT/web/apps/ops-portal"
  # NODE_ENV=development required for DEV_PORTAL_NO_AUTH (auth.server.ts guard).
  MOCK_GATEWAY=0 \
  GATEWAY_BASE_URL="http://127.0.0.1:${APIGW_PORT}" \
  DEV_PORTAL_NO_AUTH=1 \
  NODE_ENV=development \
  PORT="${PORTAL_PORT}" \
    ./node_modules/.bin/react-router-serve ./build/server/index.js \
    >"$LOG_DIR/portal.log" 2>&1 &
  echo $! >"$PID_DIR/portal.pid"
)
# Give portal a moment to bind before health poll
sleep 1

PORTAL_OK=0
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${PORTAL_PORT}/healthz" >/dev/null 2>&1; then
    PORTAL_OK=1; break
  fi
  sleep 0.5
done
[[ "$PORTAL_OK" = 1 ]] || { echo "portal not ready; see $LOG_DIR/portal.log" >&2; exit 5; }

# quick assertions
A_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${APIGW_PORT}/api/assets" || true)
H_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORTAL_PORT}/" || true)
echo "    portal up  apigw/api/assets=$A_CODE  portal/=$H_CODE"
echo
echo "READY"
echo "  portal  http://127.0.0.1:${PORTAL_PORT}/"
echo "  assets  http://127.0.0.1:${PORTAL_PORT}/assets"
echo "  apigw   http://127.0.0.1:${APIGW_PORT}/healthz"
echo "  core    http://127.0.0.1:${CORE_PORT}/v1/assets"
echo "  logs    $LOG_DIR"
echo "  Ctrl-C to stop"

if [[ "$CHECK_ONLY" = 1 ]]; then
  exit 0
fi
# wait on portal
wait "$(cat "$PID_DIR/portal.pid")" 2>/dev/null || true
