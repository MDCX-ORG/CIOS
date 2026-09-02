#!/usr/bin/env bash
# scripts/m3-seed-dev.sh — PRMT-166: dev bring-up for feature/m3-model.
#
# What this script does:
#   1. Runs `go run ./cmd/seed-ext -out <store>` to produce a JSON
#      store containing both asset hierarchy (EXT-001 / spec-001)
#      and ops data (tickets + alarms, PRMT-165 default ops.yaml).
#   2. Boots `cios-core -store <store> -protocol ./protocol
#      -allow-no-auth -listen 127.0.0.1:<port>` as a background
#      process; captures its PID.
#   3. Polls /v1/assets until 200 (max ~30 × 0.5s).
#   4. Asserts /v1/assets, /v1/tickets, /v1/alarms each return a
#      paginated envelope whose `items` array is non-empty
#      (paginated envelope shape: `{items:[...], next_page_token:"..."}`
#      — see core/{assets,tickets,alarms}.go — so a raw `jq 'length'`
#      on the response would measure the WRONG thing; this script
#      always uses python3 to dig into `items`). Each list must
#      contain >0 items; any zero or non-numeric length fails the
#      run with print-last-20-lines context.
#   5. Prints `assets=<N> tickets=<M> alarms=<K>
#      core=http://127.0.0.1:<port>`.
#   6. In --check-only mode: kill the core process and exit 0.
#      Otherwise: `wait $PID` (foreground until Ctrl-C).
#
# A trap ensures the background cios-core is killed on EXIT/INT/TERM
# so the port is freed and no zombies survive the script.
#
# SCOPE / NON-GOALS (per PRMT-166 §1):
#   - Portal MOCK_GATEWAY=0 live view requires a feature/m3-auth
#     dev recipe (gateway + auth wiring) — not in this script.
#   - Portal must go through the gateway; never direct /v1 (L81).
#     This script is the seed + core half of that pipeline only;
#     it does NOT introduce any portal direct-/v1 wiring and does
#     NOT touch any portal/gateway/auth code.
#   - dev-only; never wired into `make ci`.
#   - Never modifies the repository tree (store goes under $STORE,
#     default /tmp/cios-seed.json).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# --- arg parsing (manual; no getopts dep) ---
PORT="8090"
STORE="/tmp/cios-seed.json"
CHECK_ONLY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --port)
      [[ $# -ge 2 ]] || { echo "--port requires a value" >&2; exit 1; }
      PORT="$2"; shift 2 ;;
    --store)
      [[ $# -ge 2 ]] || { echo "--store requires a value" >&2; exit 1; }
      STORE="$2"; shift 2 ;;
    --check-only)
      CHECK_ONLY=1; shift ;;
    -h|--help)
      sed -n '2,40p' "$0"; exit 0 ;;
    *)
      echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

# --- pre-flight: curl + python3 (jq is nice-to-have but we don't
# require it; python3 is the canonical parser for the paginated
# envelope and is always available on macOS dev hosts) + go ---
missing=()
for tool in curl python3 go; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "missing: ${missing[*]}" >&2
  echo "install Go toolchain + curl + python3, then retry" >&2
  exit 1
fi
# Soft warning if jq is missing — python3 is the actual parser we
# use, so this is informational only and never fails the run.
if ! command -v jq >/dev/null 2>&1; then
  echo "note: jq not on PATH — python3 used for envelope parsing (correct for {items:[...]})" >&2
fi

echo "==> m3-seed-dev: seed → cios-core bring-up (PRMT-166)"
echo "    root=$ROOT"
echo "    store=$STORE"
echo "    port=$PORT"
echo "    mode=$([[ $CHECK_ONLY -eq 1 ]] && echo check-only || echo foreground)"

# --- 1. seed the store (assets + ops, PRMT-164/165 default seeds) ---
echo "==> step 1: seed-ext → $STORE"
(
  cd "$ROOT"
  # -out is required; ops default points at cmd/seed-ext/seed/ops.yaml.
  go run ./cmd/seed-ext -out "$STORE"
)

# --- 2. boot cios-core in the background, capture PID + log ---
echo "==> step 2: cios-core -listen 127.0.0.1:$PORT"
LOG="$(mktemp -t cios-core-dev.XXXXXX.log)"
(
  cd "$ROOT"
  # -tags lab: this script runs with the auth bypass (PRMT-217).
  go run -tags lab ./cmd/cios-core \
    -store "$STORE" \
    -protocol "$ROOT/protocol" \
    -allow-no-auth \
    -listen "127.0.0.1:$PORT" \
    >"$LOG" 2>&1 &
  echo $! >"$LOG.pid"
)
PID="$(cat "$LOG.pid")"

# Trap: kill the background cios-core on any exit path (success,
# check-only teardown, SIGINT, SIGTERM, parse failure, ...). The
# leading "|| true" defends against re-entry on EXIT after a kill
# has already succeeded.
#
# `go run ./cmd/cios-core … &` on macOS exec's the compiled binary
# as a separate child whose PID ≠ $!, so kill-by-PID leaves the
# listener orphaning the port. The port-based kill line is the
# fallback that catches the actual `tcp *:PORT` owner; it is a
# no-op when the wrapper kill already worked.
trap 'kill "$PID" 2>/dev/null || true; lsof -ti:"$PORT" 2>/dev/null | xargs -r kill -9 2>/dev/null || true; wait "$PID" 2>/dev/null || true; rm -f "$LOG" "$LOG.pid"' EXIT INT TERM

# --- 3. poll /v1/assets until 200 (or ~15s) ---
echo "==> step 3: poll /v1/assets"
ready=0
for i in $(seq 1 30); do
  if curl -fsS -o /dev/null "127.0.0.1:$PORT/v1/assets" 2>/dev/null; then
    ready=1
    echo "    /v1/assets 200 (after ${i} × 0.5s)"
    break
  fi
  sleep 0.5
done
if [[ $ready -ne 1 ]]; then
  echo "FAIL: /v1/assets never returned 200 within ~15s" >&2
  echo "--- last 20 lines of cios-core log ---" >&2
  tail -n 20 "$LOG" >&2 || true
  exit 1
fi

# --- 4. assert all three list endpoints have items >0 ---
# Routes return {items:[...], next_page_token:"..."}; naive `jq
# 'length'` would count object keys. Use python3 — robust and
# already required by pre-flight.
count_items() {
  local path="$1"
  curl -fsS "127.0.0.1:$PORT$path" \
    | python3 -c "import json,sys; d=json.load(sys.stdin); items=d.get('items') or []; print(len(items))"
}

echo "==> step 4: assert /v1/{assets,tickets,alarms} > 0"
ASSETS="$(count_items /v1/assets)"
TICKETS="$(count_items /v1/tickets)"
ALARMS="$(count_items /v1/alarms)"

fail_count() {
  local label="$1" n="$2"
  if ! [[ "$n" =~ ^[0-9]+$ ]]; then
    echo "FAIL: $label length is non-numeric: '$n'" >&2
    return 1
  fi
  if (( n == 0 )); then
    echo "FAIL: $label is empty (0 items); seed-ext default should include data" >&2
    return 1
  fi
}

if ! fail_count assets "$ASSETS"; then
  echo "--- last 20 lines of cios-core log ---" >&2
  tail -n 20 "$LOG" >&2 || true
  exit 1
fi
if ! fail_count tickets "$TICKETS"; then
  echo "--- last 20 lines of cios-core log ---" >&2
  tail -n 20 "$LOG" >&2 || true
  exit 1
fi
if ! fail_count alarms "$ALARMS"; then
  echo "--- last 20 lines of cios-core log ---" >&2
  tail -n 20 "$LOG" >&2 || true
  exit 1
fi

# --- 5. announce the live bring-up ---
echo "assets=$ASSETS tickets=$TICKETS alarms=$ALARMS  core=http://127.0.0.1:$PORT"

# --- 6. check-only: teardown and exit; otherwise wait foreground ---
if [[ $CHECK_ONLY -eq 1 ]]; then
  echo "==> check-only: tearing down cios-core (pid=$PID)"
  kill "$PID" 2>/dev/null || true
  # Exit 0 explicitly via the trap → clean teardown.
  exit 0
fi

echo "==> foreground: waiting on cios-core (Ctrl-C to stop; trap will kill PID)"
wait "$PID"
