#!/usr/bin/env bash
# scripts/m0-smoke.sh — M0 end-to-end smoke (PRMT-013 §4.6).
#
# Validates M0 exit criteria ② (CLI three-step flow) and ④ (Grafana
# connected to edge VM). Idempotent: re-running hits the server's
# 24h dedup table and the fileStore seed upsert path.
#
# Local-only. Never run inside `make ci` or CI. Requires docker.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
BIN="$ROOT/bin"
LOG="$ROOT/.m0-smoke.log"

LOG_FILE="$LOG" ; export LOG_FILE

step() { printf '\n== %s ==\n' "$*" ; }
fail() { printf 'FAIL: %s\n' "$*" >&2 ; exit 1 ; }

# --- 1. .env present; otherwise generate from .env.example --------
step "1. ensure .env"
if [[ ! -f "$EDGE/.env" ]]; then
  cp "$EDGE/.env.example" "$EDGE/.env"
  # Replace CIOS_PG_PASSWORD and CIOS_GRAFANA_PASSWORD with random hex.
  PG_NEW="$(openssl rand -hex 12)"
  GF_NEW="$(openssl rand -hex 12)"
  # Use a temp file to keep BSD/GNU sed portable.
  tmp="$(mktemp)"
  awk -v pg="$PG_NEW" -v gf="$GF_NEW" '
    /^CIOS_PG_PASSWORD=/   { print "CIOS_PG_PASSWORD=" pg; next }
    /^CIOS_GRAFANA_PASSWORD=/ { print "CIOS_GRAFANA_PASSWORD=" gf; next }
    { print }
  ' "$EDGE/.env" > "$tmp"
  mv "$tmp" "$EDGE/.env"
  chmod 600 "$EDGE/.env"
  echo "generated $EDGE/.env"
fi
# shellcheck source=/dev/null
set -a ; source "$EDGE/.env" ; set +a

# --- 2. bring up edge stack ---------------------------------------
step "2. edge stack up"
make -C "$ROOT" edge-up

# --- 3. build all four binaries -----------------------------------
step "3. build binaries"
mkdir -p "$BIN"
CGO_ENABLED=0 go build -o "$BIN/cios"             "$ROOT/cmd/cios"
CGO_ENABLED=0 go build -o "$BIN/cios-core"        "$ROOT/cmd/cios-core"
CGO_ENABLED=0 go build -o "$BIN/cios-gateway"     "$ROOT/cmd/cios-gateway"
CGO_ENABLED=0 go build -o "$BIN/cios-modbussim"   "$ROOT/cmd/cios-modbussim"

# --- 4. start modbussim + core + gateway as background procs -------
step "4. start sim/core/gateway"
"$BIN/cios-modbussim" -listen 127.0.0.1:15020 -unit 1 \
  > "$ROOT/.m0-modbussim.log" 2>&1 &
SIM_PID=$!
"$BIN/cios-core" -listen 127.0.0.1:8080 \
  -store /tmp/cios-m0.json \
  -protocol "$ROOT/protocol" \
  -vm "http://127.0.0.1:8428" \
  -seed-alarms "$EDGE/seed-alarms.yaml" \
  > "$ROOT/.m0-core.log" 2>&1 &
CORE_PID=$!
"$BIN/cios-gateway" -config "$EDGE/gateway.yaml" \
  > "$ROOT/.m0-gateway.log" 2>&1 &
GW_PID=$!

cleanup() {
  set +e
  kill "$SIM_PID" "$CORE_PID" "$GW_PID" 2>/dev/null
  wait "$SIM_PID" "$CORE_PID" "$GW_PID" 2>/dev/null
}
trap cleanup EXIT

# Brief settle so the services can bind sockets.
sleep 2

# Wait for Grafana to be ready (datasource provisioning can lag a
# few seconds behind the container healthcheck). We poll the
# datasources endpoint with admin creds; once it lists our uid the
# stack is fully usable.
for i in $(seq 1 30); do
  resp="$(curl -fsS -u "admin:${CIOS_GRAFANA_PASSWORD}" \
            http://127.0.0.1:3000/api/datasources/uid/cios-edge-vm/health \
            2>/dev/null || true)"
  if printf '%s' "$resp" | grep -qE '"status":"OK"|"message":"OK"'; then
    echo "grafana datasource ready (after ${i}s)"
    break
  fi
  sleep 1
done

# --- 5. exit criterion ② ------------------------------------------
step "5. exit criterion ② — CLI three-step flow"
"$BIN/cios" -s http://127.0.0.1:8080 apply -f "$EDGE/assets/cdu000.yaml"

if ! "$BIN/cios" -s http://127.0.0.1:8080 -o json asset list \
     | grep -q 'site01.pod000.cdu000' ; then
  fail "asset list did not contain site01.pod000.cdu000"
fi

# query may need a few collection cycles before VM has a sample.
value=""
quality=""
for i in $(seq 1 12); do
  resp="$("$BIN/cios" -s http://127.0.0.1:8080 -o json query site01.pod000.cdu000.fws.supply.temp || true)"
  if [[ -n "$resp" ]]; then
    value="$(printf '%s' "$resp" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("value",""))')"
    quality="$(printf '%s' "$resp" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("quality",""))')"
    if [[ -n "$value" && "$quality" == "good" ]]; then break ; fi
  fi
  sleep 5
done
python3 -c "
import sys
v = '$value'
try:
    n = float(v)
except Exception:
    sys.exit('FAIL: query value not numeric: ' + repr(v))
if not (15.0 < n < 35.0):
    sys.exit(f'FAIL: query value out of range (15,35): {n}')
if '$quality' != 'good':
    sys.exit('FAIL: query quality != good: ' + repr('$quality'))
"

if ! "$BIN/cios" -s http://127.0.0.1:8080 -o json alarm list --severity critical \
     | grep -q 'alm-demo-001' ; then
  fail "alarm list (critical) did not contain alm-demo-001"
fi

# --- 6. exit criterion ④ ------------------------------------------
step "6. exit criterion ④ — Grafana"
curl -fsS -u "admin:${CIOS_GRAFANA_PASSWORD}" \
  http://127.0.0.1:3000/api/datasources/uid/cios-edge-vm/health \
  | grep -qE '"status":"OK"|"message":"OK"' \
  || fail "datasource health not OK"

curl -fsS -u "admin:${CIOS_GRAFANA_PASSWORD}" \
  'http://127.0.0.1:3000/api/datasources/proxy/uid/cios-edge-vm/api/v1/query?query=cios_temp_celsius' \
  | python3 -c '
import sys,json
d=json.load(sys.stdin)
r=d.get("data",{}).get("result",[])
if not r: sys.exit("FAIL: VM query returned empty result")
'

curl -fsS -u "admin:${CIOS_GRAFANA_PASSWORD}" \
  http://127.0.0.1:3000/api/dashboards/uid/cios-edge-m0 \
  | python3 -c '
import sys,json
d=json.load(sys.stdin)
t=d.get("dashboard",{}).get("title","")
if t != "CIOS Edge":
    sys.exit("FAIL: dashboard title != CIOS Edge: " + repr(t))
'

echo "M0 SMOKE PASS"