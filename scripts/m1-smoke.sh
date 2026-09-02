#!/usr/bin/env bash
# scripts/m1-smoke.sh — M1 single-site end-to-end smoke
# (PRMT-026 R5 §4.7 / §4.8).
#
# Brings the M1 stack up (4 infra + 6 business containers), waits
# for core to be healthy, then asserts four end-to-end properties:
#   1. VictoriaMetrics has at least one cios_* sample (gateway → VM).
#   2. cios-core's /v1/assets returns the seeded site01.pod000.cdu000.
#   3. PG alarms table contains ≥1 firing row for site01.pod000.cdu000
#      within 60s (R5 §4.7; the row may come from bootstrap or any
#      PRMT-027 facility rule — NOT filtered on rule_name or count=1).
#   4. (Optional, non-fatal) VM contains the cios_deltat_celsius
#      derived quantity, confirming cios-rules ran at least once.
#
# Step 5-bis (R5 §4.8) asserts cios-alarm boot log `rules=N` matches
# `$(ls deploy/edge/rules/*.yaml | wc -l)` at runtime — not a literal
# `rules=1` or `rules=11`. The smoke recomputes N each run.
#
# Idempotent: re-running this script hits cios-core's asset
# dedup, cios-alarm's upsert-by-id, and cios-gateway's interval
# tick. It is local-only — never added to `make ci` — and
# requires docker compose on PATH plus free ports 3000/4222/5432/
# 8080/8222/8428.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
COMPOSE_FILE="$EDGE/docker-compose.yml"
LOG="$ROOT/.m1-smoke.log"
BIN="$ROOT/bin"

step() { printf '\n== %s ==\n' "$*" ; }
fail() { printf 'FAIL: %s\n' "$*" >&2 ; exit 1 ; }

# --- 1. .env present; otherwise generate from .env.example --------
step "1. ensure .env"
if [[ ! -f "$EDGE/.env" ]]; then
  cp "$EDGE/.env.example" "$EDGE/.env"
  PG_NEW="$(openssl rand -hex 12)"
  GF_NEW="$(openssl rand -hex 12)"
  # Pre-compute the resolved DSN — .env's docker-compose loader does
  # NOT interpolate ${VAR} inside .env values, so the .env.example
  # placeholder CIOS_PG_DSN=postgres://...${CIOS_PG_PASSWORD}... would
  # land in the cios-core container as a literal string with an
  # unexpanded ${}. We resolve it here.
  PG_DSN="postgres://cios:${PG_NEW}@postgres:5432/cios?sslmode=disable"
  tmp="$(mktemp)"
  awk -v pg="$PG_NEW" -v gf="$GF_NEW" -v dsn="$PG_DSN" '
    /^CIOS_PG_PASSWORD=/      { print "CIOS_PG_PASSWORD=" pg; next }
    /^CIOS_GRAFANA_PASSWORD=/ { print "CIOS_GRAFANA_PASSWORD=" gf; next }
    /^CIOS_PG_DSN=/           { print "CIOS_PG_DSN=" dsn; next }
    { print }
  ' "$EDGE/.env" > "$tmp"
  mv "$tmp" "$EDGE/.env"
  chmod 600 "$EDGE/.env"
  echo "generated $EDGE/.env"
fi
# shellcheck source=/dev/null
set -a ; source "$EDGE/.env" ; set +a

# --- 2. bring up the M1 stack ------------------------------------
step "2. compose up (--build)"
docker compose -f "$COMPOSE_FILE" up -d --build \
  > "$ROOT/.m1-compose-up.log" 2>&1 \
  || { tail -50 "$ROOT/.m1-compose-up.log" >&2 ; fail "docker compose up failed; see .m1-compose-up.log" ; }

# --- 3. wait for cios-core healthy -------------------------------
step "3. wait for cios-core healthy"
for i in $(seq 1 60); do
  if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/assets 2>/dev/null || echo 000)" == "200" ]]; then
    echo "cios-core /v1/assets responds 200 (after ${i}s)"
    break
  fi
  sleep 2
done
if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/assets 2>/dev/null || echo 000)" != "200" ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2 || true
  fail "cios-core /v1/assets never returned 200"
fi

# --- 3-bis. build bin/cios and apply seed asset -------------------
step "3-bis. build bin/cios and apply seed asset"
mkdir -p "$BIN"
CGO_ENABLED=0 go build -buildvcs=false -o "$BIN/cios" "$ROOT/cmd/cios"
# Apply the pre-existing M0 seed asset (deploy/edge/assets/cdu000.yaml).
# request_id is pinned to m0-smoke-cdu000, so a re-run is idempotent
# against cios-core's 24h dedup (PRMT-011 §4.2) — first run POSTs,
# subsequent runs return 200 from dedup with the same id.
if ! "$BIN/cios" -s http://127.0.0.1:8080 apply -f "$EDGE/assets/cdu000.yaml" \
     > "$ROOT/.m1-apply.log" 2>&1 ; then
  tail -20 "$ROOT/.m1-apply.log" >&2
  fail "bin/cios apply -f cdu000.yaml failed; see .m1-apply.log"
fi
for i in $(seq 1 30); do
  resp="$(curl -fsS http://127.0.0.1:8080/v1/assets 2>/dev/null || true)"
  if printf '%s' "$resp" | grep -q 'site01.pod000.cdu000'; then
    echo "seed asset site01.pod000.cdu000 visible (after ${i}s)"
    break
  fi
  sleep 2
done
resp="$(curl -fsS http://127.0.0.1:8080/v1/assets 2>/dev/null || true)"
if ! printf '%s' "$resp" | grep -q 'site01.pod000.cdu000'; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2 || true
  fail "cios-core /v1/assets never returned site01.pod000.cdu000 after seed apply"
fi

# --- 4. assertion 1: VM has cios_* samples -----------------------
step "4. assert VM has cios_* samples"
# Allow a few collection cycles (gateway interval = 5s).
samples=0
for i in $(seq 1 12); do
  raw="$(curl -fsS 'http://127.0.0.1:8428/api/v1/query?query=count(%7B__name__%3D~%22cios_.%2B%22%7D)' || true)"
  samples="$(printf '%s' "$raw" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    v = d.get("data", {}).get("result", [{}])[0].get("value", ["0", "0"])[1]
    print(int(float(v)))
except Exception:
    print(0)
')"
  if [[ "$samples" -gt 0 ]]; then
    echo "VM has ${samples} cios_* series (after ${i} polls)"
    break
  fi
  sleep 5
done
if [[ "$samples" -le 0 ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=50 cios-gateway cios-edge-writer >&2 || true
  fail "VM has no cios_* series after 60s; gateway/edge-writer logs above"
fi

# --- 5. assertion 2: core /v1/assets contains seed asset --------
step "5. assert core /v1/assets"
resp="$(curl -fsS http://127.0.0.1:8080/v1/assets)"
if ! printf '%s' "$resp" | grep -q 'site01.pod000.cdu000'; then
  fail "core /v1/assets did not contain site01.pod000.cdu000: $resp"
fi
echo "core /v1/assets has site01.pod000.cdu000"

# --- 5-bis. cios-alarm boot rules=N dynamic assertion (R5 §4.8) -
step "5-bis. cios-alarm boot rules=N matches rules/ file count"
EXPECTED_RULES=$(ls "$EDGE/rules/"*.yaml 2>/dev/null | wc -l | tr -d ' ')
RULES_LINE=$(docker compose -f "$COMPOSE_FILE" \
  logs cios-alarm 2>/dev/null | grep -oE 'rules=[0-9]+' | tail -n 1 | tr -d ' ')
if [[ "$RULES_LINE" != "rules=$EXPECTED_RULES" ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-alarm >&2 || true
  fail "cios-alarm boot log rules=N mismatch: expected rules=$EXPECTED_RULES, got $RULES_LINE"
fi
echo "== 5-bis. cios-alarm boot $RULES_LINE ($(ls "$EDGE/rules/"*.yaml | xargs -n1 basename | tr '\n' ',' | sed 's/,$//'))"

# --- 6. assertion 3: PG alarms has ≥1 firing row for cdu000 ------
step "6. assert PG alarms has ≥1 firing row for site01.pod000.cdu000"
# R5 §4.7: assert ≥1 firing row for site01.pod000.cdu000 within
# 60s. Do NOT filter on rule_name (the firing row may come from
# either the bootstrap rule or any of PRMT-027's facility rules
# whose appliesTo=cdu); do NOT assert exactly 1 row (PRMT-027
# rules can stack). The alarm id is a SHA-256-derived hex of
# (rule, asset) so it's not human-meaningful; we don't filter on it.
FIRING_ROWS=0
for i in $(seq 1 12); do
  FIRING_ROWS=$(docker compose -f "$COMPOSE_FILE" exec -T postgres \
    psql -U cios -d cios -At -c \
      "SELECT count(*) FROM alarms WHERE path = 'site01.pod000.cdu000' AND state = 'firing';" \
      2>/dev/null || echo 0)
  if [[ "$FIRING_ROWS" -ge 1 ]]; then
    echo "== 6. PG alarms firing rows: $FIRING_ROWS (after $i polls)"
    break
  fi
  sleep 5
done
if [[ "$FIRING_ROWS" -lt 1 ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-alarm cios-gateway >&2 || true
  fail "alarms: no firing row for site01.pod000.cdu000 within 60s (got $FIRING_ROWS)"
fi

# --- 7. assertion 4 (optional): VM has cios_deltat_celsius ------
step "7. (optional) VM has cios_deltat_celsius"
deltat_raw="$(curl -fsS 'http://127.0.0.1:8428/api/v1/query?query=cios_deltat_celsius' || true)"
if printf '%s' "$deltat_raw" | grep -q '"result":\s*\[[^]]' ; then
  echo "PASS-note: cios_deltat_celsius derived quantity present"
else
  echo "PASS-note: cios_deltat_celsius not yet emitted (rules interval is 30s; re-run smoke to confirm)"
fi

echo "M1 SMOKE PASS"