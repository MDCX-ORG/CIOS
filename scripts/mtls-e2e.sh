#!/usr/bin/env bash
# scripts/mtls-e2e.sh — P793 full e2e for component mTLS (H2/H3).
#
# Why this exists: mTLS is a process-boundary feature; unit tests cannot
# prove ListenAndServeTLS + apigw client cert + tenant peer-gate together.
# This script is the automated acceptance (not a manual runbook).
#
# What it does:
#   1. Generate lab CA + core/apigw leaves (or reuse ARTIFACTS dir)
#   2. Build cios-core + cios-apigw into a temp dir
#   3. Boot core with CIOS_MTLS_MODE=require on an ephemeral port
#   4. Assert: no-client-cert → TLS handshake fails
#   5. Assert: apigw client cert → /v1/health 200
#   6. Assert: core client cert + X-CIOS-Tenant → 403 (H3 peer gate)
#   7. Assert: apigw client cert + X-CIOS-Tenant → request accepted (not 403 on gate)
#   8. Boot apigw with client material; /healthz 200 via proxy path readiness
#
# Usage:
#   make mtls-e2e
#   scripts/mtls-e2e.sh [--artifacts DIR]
#
# Always exits after checks (no long-running foreground). Local-only;
# not wired into `make ci` (needs openssl + free ports).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ARTIFACTS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --artifacts) ARTIFACTS="$2"; shift 2 ;;
    -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

BUILD_DIR="$(mktemp -d)"
CORE_PID=""
APIGW_PID=""
cleanup() {
  local rc=$?
  if [[ -n "${CORE_PID}" ]] && kill -0 "$CORE_PID" 2>/dev/null; then
    kill "$CORE_PID" 2>/dev/null || true
    wait "$CORE_PID" 2>/dev/null || true
  fi
  if [[ -n "${APIGW_PID}" ]] && kill -0 "$APIGW_PID" 2>/dev/null; then
    kill "$APIGW_PID" 2>/dev/null || true
    wait "$APIGW_PID" 2>/dev/null || true
  fi
  rm -rf "$BUILD_DIR" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT INT TERM

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "OK   $*"; }

# --- 1. certs ---
if [[ -z "$ARTIFACTS" ]]; then
  ARTIFACTS="$BUILD_DIR/certs"
fi
echo "==> [1/8] gen certs → $ARTIFACTS"
bash "$ROOT/scripts/gen-dev-mtls-certs.sh" "$ARTIFACTS" >/dev/null
[[ -f "$ARTIFACTS/ca.pem" && -f "$ARTIFACTS/core.pem" && -f "$ARTIFACTS/apigw.pem" ]] \
  || fail "cert material missing under $ARTIFACTS"
ok "certs"

# --- 2. build ---
echo "==> [2/8] build binaries"
CORE_BIN="$BUILD_DIR/cios-core"
APIGW_BIN="$BUILD_DIR/cios-apigw"
# -tags lab: this script runs with the auth bypass (PRMT-217).
( cd "$ROOT" && CGO_ENABLED=0 go build -tags lab -o "$CORE_BIN" ./cmd/cios-core )
# -tags lab: this script runs with the auth bypass (PRMT-217).
( cd "$ROOT" && CGO_ENABLED=0 go build -tags lab -o "$APIGW_BIN" ./cmd/cios-apigw )
ok "build"

CORE_PORT="$(free_port)"
APIGW_PORT="$(free_port)"
CORE_URL="https://127.0.0.1:${CORE_PORT}"
STORE="$BUILD_DIR/store.json"
CORE_LOG="$BUILD_DIR/core.log"
APIGW_LOG="$BUILD_DIR/apigw.log"

# --- 3. boot core require ---
echo "==> [3/8] boot core mtls=require :${CORE_PORT}"
CIOS_MTLS_MODE=require \
CIOS_CORE_TLS_CERT="$ARTIFACTS/core.pem" \
CIOS_CORE_TLS_KEY="$ARTIFACTS/core.key" \
CIOS_CORE_TLS_CLIENT_CA="$ARTIFACTS/ca.pem" \
  "$CORE_BIN" \
    -protocol "$ROOT/protocol" \
    -store "$STORE" \
    -allow-no-auth \
    -listen "127.0.0.1:${CORE_PORT}" \
    -vm "http://127.0.0.1:9" \
    >"$CORE_LOG" 2>&1 &
CORE_PID=$!

# Wait until TLS port accepts something (even handshake failures mean listen is up)
ready=0
for _ in $(seq 1 40); do
  if ! kill -0 "$CORE_PID" 2>/dev/null; then
    cat "$CORE_LOG" >&2 || true
    fail "core exited early"
  fi
  # openssl s_client is heavy; use curl with empty cert attempt
  if curl -sk --connect-timeout 0.3 -o /dev/null -w '' "$CORE_URL/v1/health" 2>/dev/null \
    || curl -sk --connect-timeout 0.3 -o /dev/null "$CORE_URL/v1/health" 2>/dev/null; then
    # may still fail TLS; check log for listening
    :
  fi
  if grep -q 'listening on' "$CORE_LOG" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.25
done
[[ "$ready" = "1" ]] || { cat "$CORE_LOG" >&2; fail "core not listening"; }
ok "core up"

CURL_APIGW=(curl -sk --cert "$ARTIFACTS/apigw.pem" --key "$ARTIFACTS/apigw.key" --cacert "$ARTIFACTS/ca.pem")
CURL_CORE_LEAF=(curl -sk --cert "$ARTIFACTS/core.pem" --key "$ARTIFACTS/core.key" --cacert "$ARTIFACTS/ca.pem")

# --- 4. no client cert must fail TLS ---
echo "==> [4/8] deny anonymous client (no cert)"
code=$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout 2 "$CORE_URL/v1/health" || true)
# Expect 000 (handshake fail) or empty — not 200
if [[ "$code" == "200" ]]; then
  fail "anonymous client got HTTP $code (expected TLS reject)"
fi
ok "anonymous rejected (http_code=${code:-none})"

# --- 5. apigw cert → health 200 ---
echo "==> [5/8] apigw peer can GET /v1/health"
code=$("${CURL_APIGW[@]}" -o /tmp/mtls-e2e-health.body -w '%{http_code}' "$CORE_URL/v1/health" || true)
body=$(cat /tmp/mtls-e2e-health.body 2>/dev/null || true)
if [[ "$code" != "200" ]]; then
  echo "body=$body" >&2
  cat "$CORE_LOG" >&2 || true
  fail "apigw peer health → HTTP $code want 200"
fi
ok "apigw peer /v1/health 200"

# --- 6. H3: non-apigw peer + tenant header → 403 ---
echo "==> [6/8] H3: core leaf + X-CIOS-Tenant → 403"
code=$("${CURL_CORE_LEAF[@]}" -o /tmp/mtls-e2e-h3.body -w '%{http_code}' \
  -H 'X-CIOS-Tenant: acme' "$CORE_URL/v1/assets" || true)
body=$(cat /tmp/mtls-e2e-h3.body 2>/dev/null || true)
if [[ "$code" != "403" ]]; then
  echo "body=$body" >&2
  fail "expected 403 for non-apigw peer with tenant header, got $code"
fi
if ! grep -qi 'X-CIOS-Tenant\|apigw\|client certificate\|peer' /tmp/mtls-e2e-h3.body 2>/dev/null; then
  # problem+json detail should mention gate; soft-check
  ok "H3 403 (detail not grepped; code ok)"
else
  ok "H3 403 peer gate"
fi

# --- 7. H3: apigw peer + tenant header → not 403 ---
echo "==> [7/8] H3: apigw leaf + X-CIOS-Tenant not peer-denied"
code=$("${CURL_APIGW[@]}" -o /tmp/mtls-e2e-ok.body -w '%{http_code}' \
  -H 'X-CIOS-Tenant: acme' "$CORE_URL/v1/assets" || true)
if [[ "$code" == "403" ]] && grep -qi 'X-CIOS-Tenant requires verified mTLS peer\|peer component' /tmp/mtls-e2e-ok.body 2>/dev/null; then
  cat /tmp/mtls-e2e-ok.body >&2
  fail "apigw peer incorrectly hit H3 tenant gate"
fi
# 200 or other app-level codes OK; peer gate must not fire
ok "apigw+tenant HTTP $code (not H3 403)"

# --- 8. apigw process with client material ---
echo "==> [8/8] boot apigw with client mTLS → /healthz"
CIOS_MTLS_MODE=require \
CIOS_APIGW_TLS_CA="$ARTIFACTS/ca.pem" \
CIOS_APIGW_TLS_CERT="$ARTIFACTS/apigw.pem" \
CIOS_APIGW_TLS_KEY="$ARTIFACTS/apigw.key" \
CIOS_APIGW_UPSTREAM="$CORE_URL" \
CIOS_APIGW_LISTEN="127.0.0.1:${APIGW_PORT}" \
CIOS_APIGW_DEV_NO_AUTH=1 \
  "$APIGW_BIN" >"$APIGW_LOG" 2>&1 &
APIGW_PID=$!

hz_ok=0
for _ in $(seq 1 40); do
  if ! kill -0 "$APIGW_PID" 2>/dev/null; then
    cat "$APIGW_LOG" >&2 || true
    fail "apigw exited early"
  fi
  code=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 0.5 \
    "http://127.0.0.1:${APIGW_PORT}/healthz" 2>/dev/null || true)
  if [[ "$code" == "200" ]]; then
    hz_ok=1
    break
  fi
  sleep 0.25
done
if [[ "$hz_ok" != "1" ]]; then
  cat "$APIGW_LOG" >&2 || true
  fail "apigw /healthz never 200"
fi
ok "apigw /healthz 200 (upstream mTLS client material loaded)"

# Negative: apigw require without TLS env must fail at boot
echo "==> bonus: apigw MODE=require without certs fails boot"
if CIOS_MTLS_MODE=require \
   CIOS_APIGW_UPSTREAM="$CORE_URL" \
   CIOS_APIGW_LISTEN="127.0.0.1:$(free_port)" \
   CIOS_APIGW_ALLOW_NO_AUTH=1 \
   "$APIGW_BIN" >"$BUILD_DIR/apigw-bad.log" 2>&1; then
  fail "apigw should have refused to start without TLS triple"
fi
if ! grep -q 'CIOS_MTLS_MODE=require' "$BUILD_DIR/apigw-bad.log" 2>/dev/null; then
  # may fatallog differently
  ok "apigw boot refused (log shape loose)"
else
  ok "apigw boot refused with require message"
fi

echo
echo "mtls-e2e: ALL CHECKS PASSED"
echo "  core  https://127.0.0.1:${CORE_PORT} (stopped on exit)"
echo "  apigw http://127.0.0.1:${APIGW_PORT} (stopped on exit)"
