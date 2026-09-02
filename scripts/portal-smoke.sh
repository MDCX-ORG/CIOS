#!/usr/bin/env bash
# portal-smoke.sh — HTTP smoke for live E3.5 ops portal (no browser).
# Prerequisites: core:8090 apigw:8089 portal:3210 (make portal-live).
set -euo pipefail
PORTAL="${PORTAL_BASE:-http://127.0.0.1:3210}"
APIGW="${APIGW_BASE:-http://127.0.0.1:8089}"
fail=0
check() {
  local name="$1" url="$2" want="${3:-200}" needle="${4:-}"
  local code body
  code=$(curl -sS -o /tmp/psmoke.body -w '%{http_code}' "$url" || echo 000)
  body=$(cat /tmp/psmoke.body 2>/dev/null || true)
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

echo "== portal-smoke PORTAL=$PORTAL APIGW=$APIGW =="
check apigw-assets "$APIGW/api/assets?page_size=5" 200 path
check home         "$PORTAL/" 200 data-ops-portal-ready
check assets       "$PORTAL/assets/" 200 DC45
check assets-ac45  "$PORTAL/assets/?model=AC45" 200 AC45
check noc          "$PORTAL/noc" 200 data-noc-ready
check alarms       "$PORTAL/alarms" 200 data-alarms-page
check tickets      "$PORTAL/tickets" 200 data-tickets-page
check maint        "$PORTAL/maintenance" 200 data-maintenance
check spares       "$PORTAL/spares" 200 data-spares
check inspections  "$PORTAL/inspections" 200 data-inspections
check runbooks     "$PORTAL/runbooks" 200 data-runbooks
check reports      "$PORTAL/reports" 200 data-reports
check logout       "$PORTAL/logout" 302

# AC45/DC45 presence via API
python3 - <<'PY'
import json,urllib.request,sys
base="http://127.0.0.1:8089/api/assets?page_size=200"
items=json.load(urllib.request.urlopen(base))["items"]
models=set()
for a in items:
  spec=a.get("spec") or {}
  if spec.get("type")=="pod":
    models.add(spec.get("model"))
need={"DC45","AC45"}
if not need.issubset(models):
  print("FAIL pods models", models, "want", need)
  sys.exit(1)
print("OK   seed pods", sorted(models))
PY

if [[ $fail -ne 0 ]]; then
  echo "portal-smoke: FAILED"
  exit 1
fi
echo "portal-smoke: PASS"
