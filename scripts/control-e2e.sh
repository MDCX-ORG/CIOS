#!/usr/bin/env bash
# scripts/control-e2e.sh — P722 live Set path: core → gateway control → modbussim.
#
# Automated (not a manual runbook):
#   1. modbussim on free port (holding 0x0020 = tcs.opening)
#   2. dummy VM import sink
#   3. cios-gateway -control-listen + pointmap cdu-sim
#   4. cios-core -control-url + allow-no-auth
#   5. PUT :set with risk_class B (require_readback) → assert 202 + dispatched
#   6. unknown point → 403 (default ro)
#
# Usage: make control-e2e
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$(mktemp -d)"
PIDS=()
cleanup() {
  local rc=$?
  for p in "${PIDS[@]:-}"; do
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
  rm -rf "$BUILD" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT INT TERM

free_port() {
  python3 - <<'PY'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
}
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "OK   $*"; }

echo "==> [1/7] build"
( cd "$ROOT" && CGO_ENABLED=0 go build -o "$BUILD/modbussim" ./cmd/cios-modbussim )
( cd "$ROOT" && CGO_ENABLED=0 go build -o "$BUILD/gateway" ./cmd/cios-gateway )
# -tags lab: this script runs with the auth bypass (PRMT-217).
( cd "$ROOT" && CGO_ENABLED=0 go build -tags lab -o "$BUILD/core" ./cmd/cios-core )
ok build

SIM_PORT=$(free_port)
VM_PORT=$(free_port)
CTRL_PORT=$(free_port)
CORE_PORT=$(free_port)

echo "==> [2/7] modbussim :${SIM_PORT}"
"$BUILD/modbussim" -listen "127.0.0.1:${SIM_PORT}" -unit 1 >"$BUILD/sim.log" 2>&1 &
PIDS+=($!)
sleep 0.3
ok sim

echo "==> [3/7] dummy VM :${VM_PORT}"
# Accept any POST as 204 (import/prometheus)
python3 - <<PY >"$BUILD/vm.log" 2>&1 &
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get("Content-Length") or 0)
        self.rfile.read(n)
        self.send_response(204); self.end_headers()
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", ${VM_PORT}), H).serve_forever()
PY
PIDS+=($!)
sleep 0.2
ok vm

# gateway config (site01 matches deploy/edge demo; paths parse in cpath)
GW_CFG="$BUILD/gateway.yaml"
cat >"$GW_CFG" <<EOF
site: site01
protocol_dir: ${ROOT}/protocol
vm_write_url: http://127.0.0.1:${VM_PORT}/api/v1/import/prometheus
interval: 30s
devices:
  - asset: site01.pod000.cdu000
    pointmap: ${ROOT}/deploy/edge/pointmaps/cdu-sim.yaml
    endpoint: 127.0.0.1:${SIM_PORT}
    unit_id: "1"
EOF

CTRL_TOKEN="cios-control-e2e-token"
echo "==> [4/7] gateway control :${CTRL_PORT} (loopback + bearer)"
"$BUILD/gateway" -config "$GW_CFG" -control-listen "127.0.0.1:${CTRL_PORT}" \
  -control-token "$CTRL_TOKEN" \
  >"$BUILD/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${CTRL_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.25
done
curl -sf "http://127.0.0.1:${CTRL_PORT}/healthz" >/dev/null || {
  cat "$BUILD/gw.log" >&2
  fail "gateway control not up"
}
ok gateway

echo "==> [5/7] core :${CORE_PORT}"
"$BUILD/core" \
  -protocol "$ROOT/protocol" \
  -store "$BUILD/store.json" \
  -allow-no-auth \
  -listen "127.0.0.1:${CORE_PORT}" \
  -vm "http://127.0.0.1:${VM_PORT}" \
  -control-url "http://127.0.0.1:${CTRL_PORT}" \
  -control-token "$CTRL_TOKEN" \
  >"$BUILD/core.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 40); do
  code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${CORE_PORT}/v1/health" 2>/dev/null || true)
  [[ "$code" == "200" ]] && break
  sleep 0.25
done
[[ "$code" == "200" ]] || { cat "$BUILD/core.log" >&2; fail "core not up"; }
ok core

POINT="site01.pod000.cdu000.tcs.opening"
echo "==> [6/7] PUT :set (class B + readback) ${POINT}"
body='{"value":77,"ttl_seconds":30,"require_readback":true}'
code=$(curl -sS -o "$BUILD/set.json" -w '%{http_code}' \
  -X PUT "http://127.0.0.1:${CORE_PORT}/v1/points/${POINT}:set" \
  -H 'Content-Type: application/json' \
  -H 'X-CIOS-Actor: e2e-operator' \
  -d "$body" || true)
echo "  response HTTP $code $(cat "$BUILD/set.json")"
[[ "$code" == "202" ]] || { cat "$BUILD/core.log" "$BUILD/gw.log" >&2; fail "set HTTP $code want 202"; }
python3 - <<PY
import json
r=json.load(open("$BUILD/set.json"))
assert r.get("dispatched") is True, r
assert r.get("risk_class") in ("b","B") or r.get("risk_class")=="b", r
# readback_value may be 77 from sim
print("dispatched", r.get("dispatched"), "readback_value", r.get("readback_value"), "note", r.get("note"))
PY
ok "set dispatched"

echo "==> [7/7] default-ro deny"
code=$(curl -sS -o "$BUILD/deny.json" -w '%{http_code}' \
  -X PUT "http://127.0.0.1:${CORE_PORT}/v1/points/site01.pod000.cdu000.fws.supply.flow:set" \
  -H 'Content-Type: application/json' \
  -H 'X-CIOS-Actor: e2e-operator' \
  -d '{"value":1,"ttl_seconds":10}' || true)
[[ "$code" == "403" ]] || fail "ro deny want 403 got $code"
ok "ro deny 403"

echo
echo "control-e2e: ALL CHECKS PASSED"
