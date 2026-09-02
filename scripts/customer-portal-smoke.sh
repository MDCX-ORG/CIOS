#!/usr/bin/env bash
# customer-portal-smoke.sh — PRMT-212 / P796: HTTP smoke for customer portal (no browser).
# Prerequisites: customer-portal listening (default :3211) and apigw if live mode.
set -euo pipefail
PORTAL="${CUSTOMER_PORTAL_BASE:-http://127.0.0.1:3211}"
fail=0
check() {
  local name="$1" url="$2" want="${3:-200}" needle="${4:-}"
  local code body
  code=$(curl -sS -o /tmp/csmoke.body -w '%{http_code}' "$url" || echo 000)
  body=$(cat /tmp/csmoke.body 2>/dev/null || true)
  if [[ "$code" != "$want" ]]; then
    echo "FAIL $name HTTP $code (want $want) $url"
    fail=1
    return
  fi
  if [[ -n "$needle" ]] && ! grep -q "$needle" <<<"$body"; then
    echo "FAIL $name missing '$needle' $url"
    fail=1
    return
  fi
  echo "OK   $name ($code)"
}

echo "== customer-portal-smoke PORTAL=$PORTAL =="
# Login page must render without session.
check login "$PORTAL/login" 200 login
# Unauthenticated status should redirect to login (302) or show login gate.
code=$(curl -sS -o /dev/null -w '%{http_code}' -L --max-redirs 0 "$PORTAL/status" || echo 000)
if [[ "$code" != "302" && "$code" != "200" ]]; then
  echo "FAIL status unexpected HTTP $code"
  fail=1
else
  echo "OK   status-gate ($code)"
fi

if [[ "$fail" -ne 0 ]]; then
  echo "customer-portal-smoke FAILED"
  exit 1
fi
echo "customer-portal-smoke OK"
