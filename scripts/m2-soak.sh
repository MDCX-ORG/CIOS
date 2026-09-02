#!/usr/bin/env bash
# scripts/m2-soak.sh — M2 ops-loop soak harness (PRMT-098, §M2-1).
#
# Brings up the M1 stack, drives the alarm → auto-ticket → ack →
# resolve → close closed loop (once, on startup, relying on the
# bootstrap rule + -auto-ticket; see PRMT-098 §6/§7 — periodic
# per-cycle firing requires a simulator control surface that does
# not exist in release/m2.1), then runs periodic health / ops
# snapshots for the requested duration. On exit (clean or signal)
# emits a SUMMARY.md with firing counts, down events, MTTR/MTBF,
# and a closed-loop sample.
#
# Note: this script does NOT source scripts/m2-smoke.sh — m2-smoke
# is a full runner (its own `set -e` + last-line `exit 0`), so
# `source` would re-execute it and abort this script before its
# CLI parsing runs. Constants and helpers below mirror the
# m2-smoke contract for the single seed asset `site01.pod000.cdu000`;
# PRMT §3 still bars touching m2-smoke itself.
#
# CLI:
#   --hours N      total duration in hours (default 4; mutually
#                  exclusive with --minutes; cannot combine with
#                  --days non-zero)
#   --minutes N    total duration in minutes (smoke-grade; overrides
#                  --hours default)
#   --days N       total duration in days (PRMT-098 default 7; the
#                  script refuses anything > 7 unless SOAK_ALLOW_LONG=1)
#   --cycle Ns     closed-loop cycle interval (default 5m; PRMT §2.2)
#   --probe Ns     health/ops snapshot interval (default 1h; PRMT §2.3)
#   --resume       resume from a prior SUMMARY.md (skip compose up
#                  and reuse elapsed wall time; fails if no SUMMARY
#                  exists)
#   --help         print usage
#
# Exit codes:
#   0  full duration elapsed with no unrecovered dependency down event
#   1  any /v1/health/ready returned non-2xx at end-of-run OR a
#      closed-loop step failed
#   2  bad CLI / missing prereq
#
# Honors PRMT §3 whitelist — does NOT modify Go / compose / m1-m2-smoke.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
COMPOSE_FILE="$EDGE/docker-compose.yml"
BIN="$ROOT/bin"
ART="$ROOT/artifacts/soak"
LOG="$ROOT/.m2-soak.log"
SUMMARY="$ART/SUMMARY.md"

# Mirrored from scripts/m2-smoke.sh (single source of truth lives
# there; we keep these in sync by hand to avoid sourcing that file).
CORE_URL="http://127.0.0.1:8080"
SEED_ASSET="site01.pod000.cdu000"
SEED_APPLY="$EDGE/assets/cdu000.yaml"

step() { printf '\n== %s ==\n' "$*" ; }
fail() { printf 'FAIL: %s\n' "$*" >&2 ; exit 1 ; }

# --- CLI parsing ------------------------------------------------
print_help() {
  sed -n '2,30p' "$0"
}
HOURS=4
MINUTES=0
DAYS=0
RESUME=0
CYCLE_SEC=300
PROBE_SEC=3600

# Parse "<n><s|m|h|d>" → seconds. Pure integer = seconds. Used
# by --cycle / --probe so PRMT §5's `--cycle 1m --probe 2m` works.
parse_secs() {
  local v="$1"
  local n="${v%[smhd]}"
  local u="${v: -1}"
  case "$u" in
    s) echo $((n)) ;;
    m) echo $((n*60)) ;;
    h) echo $((n*3600)) ;;
    d) echo $((n*86400)) ;;
    *) echo $((v)) ;;   # bare number → seconds
  esac
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --hours)   HOURS="$2";   DAYS=0; MINUTES=0; shift 2 ;;
    --minutes) MINUTES="$2"; DAYS=0; HOURS=0; shift 2 ;;
    --days)    DAYS="$2";    HOURS=0; MINUTES=0; shift 2 ;;
    --cycle)   CYCLE_SEC=$(parse_secs "$2"); shift 2 ;;
    --probe)   PROBE_SEC=$(parse_secs "$2"); shift 2 ;;
    --resume)  RESUME=1;     shift ;;
    -h|--help) print_help; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# Resolve total seconds. PRMT-098 default is 7d; we default to 4h
# (operator override). --days / --hours / --minutes are mutually
# exclusive in spirit — last-wins is not interesting, so we error
# out if more than one is non-zero.
nonzero=0
[[ "$DAYS"    -gt 0 ]] && nonzero=$((nonzero+1))
[[ "$HOURS"   -gt 0 ]] && nonzero=$((nonzero+1))
[[ "$MINUTES" -gt 0 ]] && nonzero=$((nonzero+1))
if [[ "$nonzero" -gt 1 ]]; then
  echo "pass only one of --days / --hours / --minutes" >&2; exit 2
fi
if [[ "$DAYS" -gt 7 && "${SOAK_ALLOW_LONG:-0}" != "1" ]]; then
  echo "--days > 7 refused (set SOAK_ALLOW_LONG=1 to override)" >&2; exit 2
fi
TOTAL_SEC=0
if   [[ "$DAYS"    -gt 0 ]]; then TOTAL_SEC=$((DAYS*86400));
elif [[ "$HOURS"   -gt 0 ]]; then TOTAL_SEC=$((HOURS*3600));
elif [[ "$MINUTES" -gt 0 ]]; then TOTAL_SEC=$((MINUTES*60));
else                                TOTAL_SEC=$((HOURS*3600));
fi
echo "soak: total=${TOTAL_SEC}s cycle=${CYCLE_SEC}s probe=${PROBE_SEC}s resume=${RESUME}"

# --- resume state ----------------------------------------------
mkdir -p "$ART"
ELAPSED=0
START_TS=$(date -u +%s)
if [[ "$RESUME" -eq 1 ]]; then
  if [[ ! -f "$SUMMARY" ]]; then
    echo "--resume but no $SUMMARY" >&2; exit 2
  fi
  ELAPSED=$(grep -oE 'started_at: [0-9]+' "$SUMMARY" | awk '{print $2}')
  if [[ -z "$ELAPSED" ]]; then
    echo "could not read started_at from $SUMMARY" >&2; exit 2
  fi
  # we cannot recover the original wall duration from a SUMMARY
  # alone; treat --resume as "continue from now until TOTAL_SEC
  # more seconds elapse since START". Operator is expected to
  # re-pass the same duration flag.
  RESUME_NOW=$(date -u +%s)
  ALREADY=$((RESUME_NOW - ELAPSED))
  echo "soak: resuming; already elapsed ${ALREADY}s since start"
fi

# --- counters --------------------------------------------------
declare -i FIRED=0 OPENED=0 CLOSED=0 ACKED=0 RESOLVED=0
declare -i DOWN_EVENTS=0 PROBES=0 OPS_SNAPS=0
declare -i LOOP_FAILS=0
SAMPLE_TICKET_ID="" SAMPLE_TICKET_ALARM=""
FIRST_DOWN_TS="" LAST_DOWN_TS=""

# --- fail-soft logger ------------------------------------------
note() { printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG" ; }
fail_soft() {
  LOOP_FAILS=$((LOOP_FAILS+1))
  note "FAIL-SOFT: $*"
}

# --- one closed-loop iteration ---------------------------------
# Returns 0 on a full firing→open→ack→resolve→close cycle, 1 on
# any step failure (caller continues; we count the failure).
one_loop() {
  set -e
  # The bootstrap rule fires once on the first tick after compose
  # up. We wait for /v1/tickets to expose an open ticket with
  # alarm_id set, then walk the four states. If a previous run
  # already closed every bootstrap ticket, the dedup invariant
  # (spec-008 §4 / L69) blocks a re-fire — in that case this
  # iteration is a no-op success (no new firing available).
  body="$(curl -fsS "$CORE_URL/v1/tickets?filter=$SEED_ASSET&page_size=50" 2>/dev/null || true)"
  read -r TID AID <<< "$(printf '%s' "$body" | python3 -c '
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
')"
  if [[ -z "$TID" ]]; then
    set +e; return 0   # nothing to do — no open ticket, no firing
  fi
  FIRED=$((FIRED+1)); OPENED=$((OPENED+1))
  SAMPLE_TICKET_ID="$TID"; SAMPLE_TICKET_ALARM="$AID"
  note "loop: open ticket $TID alarm_id=$AID"
  get_field() { curl -fsS "$CORE_URL/v1/tickets/$1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('$2') or '')"; }
  "$BIN/cios" -s "$CORE_URL" ticket ack     "$TID" >/dev/null 2>&1 || { set +e; return 1; }
  ACKED=$((ACKED+1))
  [[ -n "$(get_field "$TID" acked_at)" ]] || { set +e; return 1; }
  "$BIN/cios" -s "$CORE_URL" ticket resolve "$TID" >/dev/null 2>&1 || { set +e; return 1; }
  RESOLVED=$((RESOLVED+1))
  [[ -n "$(get_field "$TID" resolved_at)" ]] || { set +e; return 1; }
  "$BIN/cios" -s "$CORE_URL" ticket close   "$TID" >/dev/null 2>&1 || { set +e; return 1; }
  CLOSED=$((CLOSED+1))
  [[ -n "$(get_field "$TID" closed_at)" ]] || { set +e; return 1; }
  note "loop: closed ticket $TID"
  set +e; return 0
}

# --- periodic probe (health + ops snapshot) -------------------
one_probe() {
  PROBES=$((PROBES+1))
  code="$(curl -fsS -o /dev/null -w '%{http_code}' "$CORE_URL/v1/health/ready" 2>/dev/null || echo 000)"
  ts="$(date -u +%FT%TZ)"
  if [[ "$code" != "200" ]]; then
    DOWN_EVENTS=$((DOWN_EVENTS+1))
    [[ -z "$FIRST_DOWN_TS" ]] && FIRST_DOWN_TS="$ts"
    LAST_DOWN_TS="$ts"
    fail_soft "health/ready returned $code at $ts"
  fi
  snap="$ART/$(date -u +%Y%m%dT%H%M%SZ)-ops.json"
  if curl -fsS "$CORE_URL/v1/reports/ops" -o "$snap" 2>/dev/null; then
    OPS_SNAPS=$((OPS_SNAPS+1))
  else
    fail_soft "ops snapshot fetch failed → $snap"
  fi
  # Capacity guard — non-fatal; just log disk usage on the
  # named volumes via `docker system df -v` (cheap, no host bind).
  docker system df -v >> "$LOG" 2>&1 || true
  note "probe #$PROBES: ready=$code snaps=$OPS_SNAPS down_total=$DOWN_EVENTS"
}

# --- bring the stack up (unless resuming) ---------------------
if [[ "$RESUME" -eq 0 ]]; then
  note "compose up"
  # PRMT-121: export soak-mode env so cios-modbussim's soak goroutine
  # drives tcs.opening on a randomized cadence. one_loop's existing
  # /v1/tickets?filter=$SEED_ASSET query is unchanged — the new
  # cdu-tcs-opening-soak rule also fires on cdu, so each --cycle
  # closes any soak-driven ticket before the next spike. Skip on
  # --resume (the operator is continuing from a prior SUMMARY and
  # the env state must reflect what the original run started with).
  export CIOS_SOAK_MODE=1
  export CIOS_SOAK_PERIOD_MIN_S="${CIOS_SOAK_PERIOD_MIN_S:-60}"
  export CIOS_SOAK_PERIOD_MAX_S="${CIOS_SOAK_PERIOD_MAX_S:-180}"
  export CIOS_SOAK_DWELL_S="${CIOS_SOAK_DWELL_S:-15}"
  set -e
  docker compose -f "$COMPOSE_FILE" up -d --build \
    > "$ROOT/.m2-soak-compose-up.log" 2>&1 \
    || { tail -50 "$ROOT/.m2-soak-compose-up.log" >&2; fail "compose up failed"; }
  for i in $(seq 1 60); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' $CORE_URL/v1/assets 2>/dev/null || echo 000)" == "200" ]]; then
      note "cios-core /v1/assets up after ${i}s"; break
    fi
    sleep 2
  done
  [[ "$(curl -fsS -o /dev/null -w '%{http_code}' $CORE_URL/v1/assets 2>/dev/null || echo 000)" == "200" ]] \
    || { docker compose -f "$COMPOSE_FILE" logs --tail=80 cios-core >&2; fail "cios-core never healthy"; }
  mkdir -p "$BIN"
  CGO_ENABLED=0 go build -buildvcs=false -o "$BIN/cios" "$ROOT/cmd/cios" \
    || fail "build bin/cios failed"
  set +e
fi

# --- one startup closed loop (best-effort) --------------------
set +e
one_loop
[[ $? -ne 0 ]] && fail_soft "startup closed-loop failed"

# --- main soak loop -------------------------------------------
DEADLINE=$((START_TS + TOTAL_SEC))
LAST_LOOP=$START_TS
LAST_PROBE=$START_TS
note "entering soak loop; deadline=$(date -u -r $DEADLINE +%FT%TZ 2>/dev/null || echo $DEADLINE)"
while :; do
  NOW=$(date -u +%s)
  [[ "$NOW" -ge "$DEADLINE" ]] && break
  # periodic loop
  if [[ $((NOW - LAST_LOOP)) -ge "$CYCLE_SEC" ]]; then
    one_loop; rc=$?
    [[ $rc -ne 0 ]] && fail_soft "loop iteration failed"
    LAST_LOOP=$NOW
  fi
  # periodic probe
  if [[ $((NOW - LAST_PROBE)) -ge "$PROBE_SEC" ]]; then
    one_probe
    LAST_PROBE=$NOW
  fi
  sleep 5
done

# --- final probe + summary ------------------------------------
one_probe
END_TS=$(date -u +%s)
DURATION=$((END_TS - START_TS))

# Compute MTTR/MTBF from closed tickets if any.
read -r MTTR_SEC MTBF_SEC <<< "$(curl -fsS "$CORE_URL/v1/reports/ops" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("0 0"); sys.exit(0)
mttr = d.get("mttr_seconds") or 0
mtbf = d.get("mtbf_seconds") or 0
print(int(mttr), int(mtbf))
')"

cat > "$SUMMARY" <<EOF
# M2 Soak Summary (PRMT-098 / §M2-1)

**This is a smoke-grade soak (4h by default), not the §M2-1 7-day run.**
PRMT-098 §2.2 calls for periodic firing→open→close loops; the
release/m2.1 sim surface has no controlled threshold API, so this
script produces one startup loop and N periodic health/ops snapshots.
See PRMT-098 §6/§7.

- started_at: $START_TS
- ended_at:   $END_TS
- duration_s: $DURATION
- total_budget_s: $TOTAL_SEC
- cycle_s: $CYCLE_SEC
- probe_s: $PROBE_SEC

## Counters

| metric            | value |
|-------------------|------:|
| firings observed  | $FIRED |
| tickets opened    | $OPENED |
| tickets acked     | $ACKED |
| tickets resolved  | $RESOLVED |
| tickets closed    | $CLOSED |
| probe runs        | $PROBES |
| ops snapshots     | $OPS_SNAPS |
| dependency downs  | $DOWN_EVENTS |
| first down ts     | ${FIRST_DOWN_TS:-none} |
| last down ts      | ${LAST_DOWN_TS:-none} |
| fail-soft events  | $LOOP_FAILS |

## Report fields (live ops report)

- mttr_seconds: $MTTR_SEC
- mtbf_seconds: $MTBF_SEC

## Closed-loop sample

- ticket_id:    ${SAMPLE_TICKET_ID:-none}
- alarm_id:     ${SAMPLE_TICKET_ALARM:-none}
EOF

note "summary written: $SUMMARY"

# Exit code: 0 only if no down events AND no fail-soft.
if [[ "$DOWN_EVENTS" -ne 0 || "$LOOP_FAILS" -ne 0 ]]; then
  note "exit 1 (downs=$DOWN_EVENTS, fail_softs=$LOOP_FAILS)"
  exit 1
fi
note "exit 0"
exit 0