#!/usr/bin/env bash
# scripts/m3-apigw-dev.sh — M3 apigw dev bring-up (no-auth) (PRMT-172).
#
# Scope (L93 Path B / L81 / feature/m3-auth):
#   Brings up cios-apigw in no-auth mode pointed at an already-seeded
#   core /v1 (default http://127.0.0.1:8090, sourced from feature/m3-model
#   `make dev-seed` per PRMT-166 — that target/seed lives OUTSIDE this
#   branch). Polls /healthz, then asserts (no Authorization header) that
#   GET /api/{sites,assets,alarms} return 200 with a non-empty JSON body,
#   proving no-auth pass-through (server.go: sts==nil && pdp==nil) and
#   /v1 → /api/* proxy fidelity.
#
# Env contract (only these four are set; rest are explicitly UNSET so
# cios-apigw stays on the no-auth boot path):
#   CIOS_APIGW_UPSTREAM=<--upstream>      # required by apigw.LoadConfig
#   CIOS_APIGW_LISTEN=:<--port>           # overrides default :8443
#   CIOS_APIGW_DEV_NO_AUTH=1              # PRMT-173: opt-in dev claims injection
#                                        # so handler-layer ClaimsFrom returns ok;
#                                        # sanity-check keeps it off if STS/OPA set.
#   NOT set: CIOS_APIGW_AUTH_MODE, CIOS_STS_SIGNING_KEY, CIOS_OPA_URL,
#            CIOS_APIGW_TLS_{CA,CERT,KEY}
#
# Out of scope (NOT IMPLEMENTED here — architect decision pending):
#   - Portal `MOCK_GATEWAY=0` login session. feature/m3-auth ships only
#     the apigw half; the portal/dev login end-to-end is a NEW auth
#     surface (mock-IdP + AUTH_MODE=on + dev cookie forge, OR portal
#     dev bypass) and must be decided by the architect. Portal MUST go
#     through cios-apigw — never directly to /v1 or NATS (L81).
#   - Core startup / seeding. Belongs to PRMT-166 / feature/m3-model
#     (`make dev-seed`); this script never starts or seeds core.
#
# Usage:
#   scripts/m3-apigw-dev.sh [--upstream URL] [--port N] [--check-only]
#
# Defaults: --upstream http://127.0.0.1:8090   --port 8091
#
# --check-only: assert, then exit 0 on success / 1 on any failure or
# timeout (do not foreground-stay). Without it: foreground, Ctrl-C to
# stop (trap kills apigw).
#
# Idempotent / safe: binary builds into a mktemp dir (never into repo
# tree); trap EXIT/INT/TERM ensures apigw is killed and the tmp dir is
# removed so no zombies / no leftover ports.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

UPSTREAM="http://127.0.0.1:8090"
PORT="8091"
CHECK_ONLY="0"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --upstream)   UPSTREAM="$2"; shift 2 ;;
    --port)       PORT="$2"; shift 2 ;;
    --check-only) CHECK_ONLY="1"; shift ;;
    -h|--help)
      sed -n '2,40p' "$0"
      exit 0
      ;;
    *) printf 'unknown arg: %s\n' "$1" >&2; exit 2 ;;
  esac
done

# --- build apigw into a tmp dir (never into repo tree) ---------------
BUILD_DIR="$(mktemp -d)"
APIGW_BIN="$BUILD_DIR/cios-apigw"
APIGW_PID=""
cleanup() {
  local rc=$?
  if [[ -n "$APIGW_PID" ]] && kill -0 "$APIGW_PID" 2>/dev/null; then
    kill "$APIGW_PID" 2>/dev/null || true
    wait "$APIGW_PID" 2>/dev/null || true
  fi
  rm -rf "$BUILD_DIR" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT INT TERM

printf '== build cios-apigw (no auth deps) ==\n'
# -tags lab: this script runs with the auth bypass (PRMT-217).
( cd "$ROOT" && CGO_ENABLED=0 go build -tags lab -o "$APIGW_BIN" ./cmd/cios-apigw )

# --- launch apigw with the contract env ------------------------------
printf '== launch cios-apigw (no-auth) upstream=%s listen=:%s ==\n' "$UPSTREAM" "$PORT"
export CIOS_APIGW_UPSTREAM="$UPSTREAM"
export CIOS_APIGW_LISTEN=":$PORT"
# PRMT-173 (R1 PASS @ 5a4308a7): opt-in dev claims injection so handler-layer
# ClaimsFrom (sites.go:76 / reads.go:125) returns ok=true; without this flag,
# pass-through AuthMiddleware alone does NOT bypass handler-layer 401.
# LoadConfig sanity check forces DevNoAuth=false if STS/OPA env present;
# explicitly unset here so sanity check does not demote us mid-script.
export CIOS_APIGW_DEV_NO_AUTH=1
unset CIOS_APIGW_AUTH_MODE CIOS_STS_SIGNING_KEY CIOS_OPA_URL 2>/dev/null || true
unset CIOS_APIGW_TLS_CA CIOS_APIGW_TLS_CERT CIOS_APIGW_TLS_KEY 2>/dev/null || true

"$APIGW_BIN" > "$BUILD_DIR/apigw.log" 2>&1 &
APIGW_PID=$!

# --- readiness: poll /healthz up to 30x1s -----------------------------
printf '== probe /healthz (up to 30s) ==\n'
ready="0"
for i in $(seq 1 30); do
  code="$(curl -fsS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/healthz" 2>/dev/null || echo 000)"
  if [[ "$code" == "200" ]]; then
    printf 'healthz 200 (after %ds)\n' "$i"
    ready="1"
    break
  fi
  sleep 1
done
if [[ "$ready" != "1" ]]; then
  printf 'FAIL: /healthz never returned 200 in 30s; apigw log:\n' >&2
  tail -80 "$BUILD_DIR/apigw.log" >&2 || true
  exit 1
fi

# --- assertions: no Authorization header; HTTP 200; body non-empty ---
# Count helper mirrors m2-smoke.sh's python3 -c style: parse JSON,
# return count for the standard {items|data|[]} shapes (length, or 1
# for non-list non-null objects, or 0 for null/empty).
assert_route() {
  local route="$1"
  local url="http://127.0.0.1:${PORT}${route}"
  local body http
  body="$(curl -sS -o - -w '\n__HTTP__%{http_code}' "$url" 2>/dev/null || true)"
  http="$(printf '%s' "$body" | sed -n 's/^__HTTP__//p')"
  body="$(printf '%s' "$body" | sed '$d')"
  if [[ "$http" != "200" ]]; then
    printf 'FAIL: %s returned HTTP %s (expected 200)\n' "$route" "$http" >&2
    return 1
  fi
  local count
  count="$(printf '%s' "$body" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(0)
    sys.exit(0)
if d is None:
    print(0)
elif isinstance(d, list):
    print(len(d))
elif isinstance(d, dict):
    # non-empty object counts as 1; empty {} counts as 0
    print(1 if d else 0)
else:
    print(1)
')"
  if [[ -z "$count" || "$count" == "0" ]]; then
    printf 'FAIL: %s returned empty body (HTTP 200 but null/[]/{})\n' "$route" >&2
    printf 'body head: %s\n' "$(printf '%s' "$body" | head -c 200)" >&2
    return 1
  fi
  echo "$count"
}

printf '== assert /api/{sites,assets,alarms} (no Authorization) ==\n'
SITES_COUNT="$(assert_route /api/sites)" || exit 1
ASSETS_COUNT="$(assert_route /api/assets)" || exit 1
ALARMS_COUNT="$(assert_route /api/alarms)" || exit 1

printf 'sites=%s assets=%s alarms=%s   apigw=http://127.0.0.1:%s\n' \
  "$SITES_COUNT" "$ASSETS_COUNT" "$ALARMS_COUNT" "$PORT"

if [[ "$CHECK_ONLY" == "1" ]]; then
  exit 0
fi

# Foreground stay: wait for apigw (Ctrl-C / SIGTERM triggers trap).
printf 'foreground; Ctrl-C to stop apigw\n'
wait "$APIGW_PID"