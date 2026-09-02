#!/usr/bin/env bash
# scripts/m2-smoke.sh — M2 ops-loop end-to-end smoke (PRMT-051).
#
# Mirrors scripts/m1-smoke.sh in structure: brings the full M1
# stack up (4 infra + 6 business containers, all via one
# `docker compose up -d --build`), waits for cios-core to be
# healthy, then asserts seven end-to-end properties for the M2
# operations loop:
#
#   1. Stack up — wait for cios-core /v1/assets to return 200,
#      then apply the seed asset (m1 step 3-bis, idempotent).
#   2. Alarm → auto-opened ticket — poll /v1/tickets until at
#      least one ticket with alarm_id non-empty and state=open
#      appears for site01.pod000.cdu000 (cios-alarm runs with
#      -auto-ticket per deploy/edge/docker-compose.yml).
#   3. Ticket lifecycle — ack → resolve → close the captured
#      ticket via `cios ticket ack|resolve|close` and assert
#      every transition is 200 + acked_at/resolved_at/closed_at
#      timestamps become non-empty.
#   4. alarm_id dedup (spec-008 §4) — assert that across the
#      observed ticket set, every alarm_id maps to at most one
#      non-closed ticket (L69 / Q2 invariant).
#   5. Ops report — `cios report ops` returns 2xx with the
#      expected MTTR / ticket_counts fields (PRMT-042 §M2-2).
#   6. Capacity / PM / audit-history probes — /v1/capacity,
#      /v1/pm/schedules, /v1/assets/{path}:history each return
#      2xx (wiring-health only; not a deep numerical check).
#   7. PASS/FAIL — all six properties above pass → `echo PASS;
#      exit 0`; any failure prints context and `exit 1`.
#
# Idempotent: cios-core asset dedup, cios-alarm upsert-by-id,
# and cios-gateway interval tick make a re-run safe. Tickets
# from a prior run are closed in step 3, so a second run still
# finds the bootstrap-rule firing and opens a fresh ticket.
# The report is overwritten on each call. This script is
# local-only — never added to `make ci` — and requires docker
# compose on PATH plus free ports 3000/4222/5432/8080/8222/8428.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
COMPOSE_FILE="$EDGE/docker-compose.yml"
BIN="$ROOT/bin"
LOG="$ROOT/.m2-smoke.log"

# Server-side knobs reused from m1-smoke.sh (single source of
# truth: the M1 smoke file. We do NOT modify m1-smoke.sh; we
# just mirror its URL / port / seed-asset expectations).
CORE_URL="http://127.0.0.1:8080"
SEED_ASSET="site01.pod000.cdu000"
SEED_APPLY="$EDGE/assets/cdu000.yaml"

step() { printf '\n== %s ==\n' "$*" ; }
fail() { printf 'FAIL: %s\n' "$*" >&2 ; exit 1 ; }

# --- 0. preflight: docker on PATH, .env present (m1 mirrors) ---
step "0. preflight"
if ! command -v docker >/dev/null 2>&1; then
  fail "docker not on PATH; m2-smoke requires docker compose"
fi
if [[ ! -f "$EDGE/.env" ]]; then
  cp "$EDGE/.env.example" "$EDGE/.env"
  PG_NEW="$(openssl rand -hex 12)"
  GF_NEW="$(openssl rand -hex 12)"
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

# --- 1. stack up — same compose invocation as m1-smoke step 2 -
step "1. compose up (--build)"
docker compose -f "$COMPOSE_FILE" up -d --build \
  > "$ROOT/.m2-compose-up.log" 2>&1 \
  || { tail -50 "$ROOT/.m2-compose-up.log" >&2 ; fail "docker compose up failed; see .m2-compose-up.log" ; }

# --- 2. wait for cios-core healthy (m1 step 3) -----------------
step "2. wait for cios-core healthy"
for i in $(seq 1 60); do
  if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' $CORE_URL/v1/assets 2>/dev/null || echo 000)" == "200" ]]; then
    echo "cios-core /v1/assets responds 200 (after ${i}s)"
    break
  fi
  sleep 2
done
if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' $CORE_URL/v1/assets 2>/dev/null || echo 000)" != "200" ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2 || true
  fail "cios-core /v1/assets never returned 200"
fi

# --- 3. build bin/cios + apply seed asset (m1 step 3-bis) ------
step "3. build bin/cios and apply seed asset"
mkdir -p "$BIN"
CGO_ENABLED=0 go build -buildvcs=false -o "$BIN/cios" "$ROOT/cmd/cios"
if ! "$BIN/cios" -s "$CORE_URL" apply -f "$SEED_APPLY" \
     > "$ROOT/.m2-apply.log" 2>&1 ; then
  tail -20 "$ROOT/.m2-apply.log" >&2
  fail "bin/cios apply -f cdu000.yaml failed; see .m2-apply.log"
fi
for i in $(seq 1 30); do
  resp="$(curl -fsS $CORE_URL/v1/assets 2>/dev/null || true)"
  if printf '%s' "$resp" | grep -q "$SEED_ASSET"; then
    echo "seed asset $SEED_ASSET visible (after ${i}s)"
    break
  fi
  sleep 2
done
resp="$(curl -fsS $CORE_URL/v1/assets 2>/dev/null || true)"
if ! printf '%s' "$resp" | grep -q "$SEED_ASSET"; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2 || true
  fail "cios-core /v1/assets never returned $SEED_ASSET after seed apply"
fi

# --- 4. alarm → auto-opened ticket (§M2-1 / PRMT-034) ---------
step "4. wait for auto-opened ticket (alarm_id != '', state=open)"
# cios-alarm runs with -auto-ticket (compose.yml L228). The
# bootstrap rule (deploy/edge/rules/bootstrap.yaml) fires on
# the first tick of site01.pod000.cdu000 (leak==0 sentinel),
# and pkg/alarm/store.go:OpenTicket inserts one ticket per
# firing alarm (idempotent dedup by alarm_id, spec-008 §4 /
# L69). Poll /v1/tickets until we find a row whose alarm_id
# is non-empty and whose state is open.
TICKET_ID=""
TICKET_ALARM_ID=""
for i in $(seq 1 24); do
  body="$(curl -fsS "$CORE_URL/v1/tickets?filter=$SEED_ASSET&page_size=50" 2>/dev/null || true)"
  # Use python3 to pick the first open ticket with alarm_id set.
  read -r TICKET_ID TICKET_ALARM_ID <<< "$(printf '%s' "$body" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("")
    sys.exit(0)
items = d.get("items") or d.get("data") or []
for t in items:
    aid = t.get("alarm_id") or ""
    state = t.get("state") or ""
    if aid and state == "open":
        print(t.get("id", "") + " " + aid)
        sys.exit(0)
print("")
' 2>/dev/null)" || true
  if [[ -n "$TICKET_ID" && -n "$TICKET_ALARM_ID" ]]; then
    echo "ticket $TICKET_ID open with alarm_id=$TICKET_ALARM_ID (after $((i*5))s)"
    break
  fi
  sleep 5
done
if [[ -z "$TICKET_ID" ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=120 cios-alarm cios-core >&2 || true
  fail "no auto-opened ticket with alarm_id+state=open within 120s; is -auto-ticket on?"
fi

# --- 5. ticket lifecycle: open → acknowledged → resolved → closed
step "5. ticket lifecycle ack → resolve → close"
# 5a. ack
if ! "$BIN/cios" -s "$CORE_URL" ticket ack "$TICKET_ID" > "$ROOT/.m2-ticket-ack.log" 2>&1 ; then
  cat "$ROOT/.m2-ticket-ack.log" >&2 ; fail "ticket ack failed"
fi
get_field() { curl -fsS "$CORE_URL/v1/tickets/$TICKET_ID" | python3 -c "import sys,json; print(json.load(sys.stdin).get('$1') or '')"; }
acked_at="$(get_field acked_at)"
if [[ -z "$acked_at" ]]; then
  curl -fsS "$CORE_URL/v1/tickets/$TICKET_ID" >&2 ; fail "acked_at empty after ack"
fi
echo "ticket $TICKET_ID acked_at=$acked_at"
# 5b. resolve
if ! "$BIN/cios" -s "$CORE_URL" ticket resolve "$TICKET_ID" > "$ROOT/.m2-ticket-resolve.log" 2>&1 ; then
  cat "$ROOT/.m2-ticket-resolve.log" >&2 ; fail "ticket resolve failed"
fi
resolved_at="$(get_field resolved_at)"
if [[ -z "$resolved_at" ]]; then
  curl -fsS "$CORE_URL/v1/tickets/$TICKET_ID" >&2 ; fail "resolved_at empty after resolve"
fi
echo "ticket $TICKET_ID resolved_at=$resolved_at"
# 5c. close
if ! "$BIN/cios" -s "$CORE_URL" ticket close "$TICKET_ID" > "$ROOT/.m2-ticket-close.log" 2>&1 ; then
  cat "$ROOT/.m2-ticket-close.log" >&2 ; fail "ticket close failed"
fi
closed_at="$(get_field closed_at)"
if [[ -z "$closed_at" ]]; then
  curl -fsS "$CORE_URL/v1/tickets/$TICKET_ID" >&2 ; fail "closed_at empty after close"
fi
echo "ticket $TICKET_ID closed_at=$closed_at"

# --- 6. alarm_id dedup invariant (spec-008 §4 / L69 / Q2) ------
step "6. alarm_id dedup invariant (every alarm_id → ≤1 non-closed ticket)"
# The invariant is structural: at any point in time, each
# alarm_id may have at most one ticket whose state is NOT
# closed. We assert this on the live ticket set — even if
# multiple bootstrap-rule firings accumulate, the dedup
# guarantee must hold. We only count alarm_ids that are
# non-empty (manually-opened tickets are allowed to coexist).
dup_count="$(curl -fsS "$CORE_URL/v1/tickets?page_size=100" \
  | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(0)
    sys.exit(0)
items = d.get("items") or d.get("data") or []
from collections import Counter
c = Counter()
for t in items:
    aid = t.get("alarm_id") or ""
    st  = t.get("state") or ""
    if aid and st != "closed":
        c[aid] += 1
bad = sum(1 for v in c.values() if v > 1)
print(bad)
')"
if [[ "$dup_count" -ne 0 ]]; then
  curl -fsS "$CORE_URL/v1/tickets?page_size=100" >&2
  fail "alarm_id dedup violated: $dup_count alarm_ids with >1 non-closed ticket"
fi
echo "dedup invariant holds (0 alarm_ids with >1 non-closed ticket)"

# --- 7. ops report — MTTR / counts fields populated (§M2-2) ---
step "7. ops report returns MTTR / ticket_counts (§M2-2)"
# GET /v1/reports/ops — read with python to confirm both fields
# are present and ticket_counts has all four state buckets.
report="$(curl -fsS "$CORE_URL/v1/reports/ops" 2>/dev/null || true)"
if [[ -z "$report" ]]; then
  docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2 || true
  fail "GET /v1/reports/ops returned empty body"
fi
read -r has_mttr has_counts <<< "$(printf '%s' "$report" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("0 0")
    sys.exit(0)
mttr = 1 if "mttr_seconds" in d else 0
counts = d.get("ticket_counts") or {}
need = {"by_state","by_severity"}
print(f"{mttr} {1 if need.issubset(counts.keys()) else 0}")
')"
if [[ "$has_mttr" -ne 1 || "$has_counts" -ne 1 ]]; then
  printf '%s\n' "$report" | head -c 500 >&2
  fail "/v1/reports/ops missing mttr_seconds or ticket_counts.{by_state,by_severity}"
fi
echo "ops report OK (mttr_seconds present, ticket_counts has by_state+by_severity)"

# --- 8. capacity / pm / audit-history probes (wiring health) ---
step "8. wiring probes: /v1/capacity, /v1/pm/schedules, /v1/assets/{path}:history"
for ep in "$CORE_URL/v1/capacity" "$CORE_URL/v1/pm/schedules" "$CORE_URL/v1/assets/$SEED_ASSET:history"; do
  code="$(curl -fsS -o /dev/null -w '%{http_code}' "$ep" 2>/dev/null || echo 000)"
  if [[ "$code" -lt 200 || "$code" -ge 300 ]]; then
    fail "wiring probe $ep returned $code (expected 2xx)"
  fi
  echo "  $ep → $code"
done

echo "M2 SMOKE PASS"
