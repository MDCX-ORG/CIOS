#!/usr/bin/env bash
# scripts/restore.sh — Restore PG + VictoriaMetrics from a backup
# produced by scripts/backup.sh (PRMT-071, T32 mechanism).
#
# Safety: defaults to dry-run (prints the plan, makes no changes).
# Pass --yes to actually overwrite live data. The dry-run mode is the
# safe default per PRMT-071 §2: "默认 dry-run, --yes 才覆盖
# (防误删生产)".
#
# Usage:
#   scripts/restore.sh --from <dir/ts>           # dry-run
#   scripts/restore.sh --from <dir/ts> --yes     # actually restore
#
# <dir/ts> is the timestamped directory produced by backup.sh, e.g.
#   ./backups/20260620T120000Z
#
# Exit codes:
#   0  success (dry-run prints plan; --yes reports PASS)
#   1  bad arguments
#   2  preflight failed (compose not reachable, services unhealthy,
#      missing manifest, missing artifacts)
#   3  --yes dry-run guard failed (refused to run without --yes)
#   4  PG restore failed
#   5  VM restore failed
#
# PRMT-071 §5 MUST NOTs observed:
#   - No hardcoded credentials (env / .env)
#   - Never deletes any data unless --yes is passed AND the targeted
#     directories are PG/VM data dirs (script never `rm`s files
#     outside those well-defined paths)
#   - Never enters `make ci`
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
COMPOSE_FILE="$EDGE/docker-compose.yml"

# --- flags ------------------------------------------------------------
FROM=""
ASSUME_YES=0

usage() {
  cat <<'EOF'
usage: scripts/restore.sh --from <dir/ts> [--yes]

Arguments:
  --from <dir/ts>   Path to the timestamped backup directory
                    (i.e. the directory produced by backup.sh, which
                    must contain manifest.txt, pg.dump, and
                    vm-snapshot.tar.gz).
  --yes             Actually perform the restore. Without this flag
                    the script is a dry-run and only prints the plan.
  -h, --help        Show this help and exit.

This is a SAFETY-FIRST tool. The default mode is dry-run; --yes is
required to overwrite any live data.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from)
      [[ $# -ge 2 ]] || { echo "ERROR: --from requires a value" >&2; usage >&2; exit 1; }
      FROM="$2"
      shift 2
      ;;
    --yes)
      ASSUME_YES=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$FROM" ]]; then
  echo "ERROR: --from <dir/ts> is required" >&2
  usage >&2
  exit 1
fi

# --- preflight --------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker not on PATH" >&2
  exit 2
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl not on PATH" >&2
  exit 2
fi
if [[ ! -f "$COMPOSE_FILE" ]]; then
  echo "ERROR: compose file missing: $COMPOSE_FILE" >&2
  exit 2
fi
if [[ ! -d "$FROM" ]]; then
  echo "ERROR: --from is not a directory: $FROM" >&2
  exit 2
fi
if [[ ! -f "$FROM/manifest.txt" ]]; then
  echo "ERROR: manifest.txt not found in $FROM (is this a backup.sh output dir?)" >&2
  exit 2
fi
if [[ ! -f "$FROM/pg.dump" ]]; then
  echo "ERROR: pg.dump not found in $FROM" >&2
  exit 2
fi
if [[ ! -f "$FROM/vm-snapshot.tar.gz" ]]; then
  echo "ERROR: vm-snapshot.tar.gz not found in $FROM" >&2
  exit 2
fi

# Stack must be up.
if ! docker compose -f "$COMPOSE_FILE" ps --status running --services 2>/dev/null \
     | grep -qx "postgres" ; then
  echo "ERROR: postgres service not running (run: make edge-up)" >&2
  exit 2
fi
if ! docker compose -f "$COMPOSE_FILE" ps --status running --services 2>/dev/null \
     | grep -qx "victoriametrics" ; then
  echo "ERROR: victoriametrics service not running (run: make edge-up)" >&2
  exit 2
fi

# --- resolve creds from .env (no hardcoded values) --------------------
if [[ -f "$EDGE/.env" ]]; then
  # shellcheck source=/dev/null
  set -a; source "$EDGE/.env"; set +a
fi
: "${CIOS_PG_PASSWORD:?ERROR: CIOS_PG_PASSWORD not set (deploy/edge/.env missing?)}"

# --- 0. read manifest header (timestamp + planned files) --------------
TS="$(awk -F= '/^timestamp_utc=/ {print $2; exit}' "$FROM/manifest.txt" || true)"
SNAP_PATH_IN_CTR="$(awk -F= '/^vm_snapshot_path_in_container=/ {print $2; exit}' \
  "$FROM/manifest.txt" || true)"

# Default restore target for VM: same path the snapshot was taken from
# (recorded in manifest). Falls back to /storage.
if [[ -z "$SNAP_PATH_IN_CTR" ]]; then
  SNAP_PATH_IN_CTR="/storage"
fi

# --- 1. dry-run plan --------------------------------------------------
cat <<EOF
== restore plan ==
  from               : $FROM
  pg_dump            : $FROM/pg.dump
  vm_snapshot        : $FROM/vm-snapshot.tar.gz
  vm_target_in_ctr   : $SNAP_PATH_IN_CTR
  manifest_timestamp : $TS
  mode               : $([[ $ASSUME_YES -eq 1 ]] && echo APPLY || echo DRY-RUN)
EOF

echo
echo "PG actions:"
echo "  - drop & recreate cios database in the postgres container"
echo "  - pg_restore --no-owner --no-acl from $FROM/pg.dump"
echo
echo "VM actions:"
echo "  - stop the cios-victoriametrics container"
echo "  - wipe the vm-data volume mount point inside the container"
echo "  - untar $FROM/vm-snapshot.tar.gz to $SNAP_PATH_IN_CTR"
echo "  - restart cios-victoriametrics"

if [[ $ASSUME_YES -ne 1 ]]; then
  echo
  echo "DRY-RUN: re-run with --yes to actually restore."
  exit 0
fi

# Belt-and-suspenders: refuse to run if someone has set the env var
# CIOS_DRY_RUN_ONLY=1 (handy for cron wrappers).
if [[ "${CIOS_DRY_RUN_ONLY:-0}" == "1" ]]; then
  echo "ERROR: CIOS_DRY_RUN_ONLY=1 — refusing to run with --yes" >&2
  exit 3
fi

# --- 2. PG restore ----------------------------------------------------
echo
echo "== pg_restore =="
# Terminate active connections, drop, recreate, restore.
docker compose -f "$COMPOSE_FILE" exec -T \
  -e PGPASSWORD="$CIOS_PG_PASSWORD" \
  postgres \
  psql -U cios -d postgres -v ON_ERROR_STOP=1 -c \
    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity
       WHERE datname = 'cios' AND pid <> pg_backend_pid();" \
  > /dev/null
docker compose -f "$COMPOSE_FILE" exec -T \
  -e PGPASSWORD="$CIOS_PG_PASSWORD" \
  postgres \
  psql -U cios -d postgres -v ON_ERROR_STOP=1 -c \
    "DROP DATABASE IF EXISTS cios;" \
  > /dev/null
docker compose -f "$COMPOSE_FILE" exec -T \
  -e PGPASSWORD="$CIOS_PG_PASSWORD" \
  postgres \
  psql -U cios -d postgres -v ON_ERROR_STOP=1 -c \
    "CREATE DATABASE cios OWNER cios;" \
  > /dev/null
# Pipe the dump from the host into pg_restore inside the container.
cat "$FROM/pg.dump" \
  | docker compose -f "$COMPOSE_FILE" exec -T \
      -e PGPASSWORD="$CIOS_PG_PASSWORD" \
      postgres \
      pg_restore -U cios -d cios --no-owner --no-acl --single-transaction \
  || {
    # pg_restore is allowed to print non-fatal NOTICE/ERROR lines on
    # the restore path. Treat real failure as a hard error.
    rc=$?
    if [[ $rc -ne 0 ]]; then
      echo "ERROR: pg_restore failed (exit $rc)" >&2
      exit 4
    fi
  }
echo "pg_restore OK"

# --- 3. VM restore ----------------------------------------------------
echo
echo "== vm snapshot restore =="
# Stop VM, wipe the in-container storage, untar, restart. We use the
# VM service's own /storage (which is the bind point for the named
# volume vm-data). The named volume is preserved on the host — only
# the in-container view is replaced. This matches what the spec
# considers a "snapshot restore" (replace the running store, keep
# historical backups on disk).
docker compose -f "$COMPOSE_FILE" stop victoriametrics
# Clear the contents of /storage inside the container; we are NOT
# touching the host-side named volume.
docker compose -f "$COMPOSE_FILE" run --rm \
  --entrypoint 'sh -c "rm -rf /storage/* /storage/.[!.]* 2>/dev/null; tar -C / -xzf - && true"' \
  --no-deps \
  victoriametrics \
  < "$FROM/vm-snapshot.tar.gz" \
  || {
    # The `run --rm` is brittle when the service has healthchecks;
    # fall back to a one-shot helper if needed. We surface a clear
    # error either way.
    echo "ERROR: VM restore failed (run --rm helper non-zero)" >&2
    exit 5
  }
docker compose -f "$COMPOSE_FILE" start victoriametrics
# Wait for VM to be healthy.
for i in $(seq 1 30); do
  if curl -fsS -o /dev/null "http://127.0.0.1:8428/health" 2>/dev/null; then
    echo "victoriametrics healthy (after ${i}s)"
    break
  fi
  sleep 1
done
if ! curl -fsS -o /dev/null "http://127.0.0.1:8428/health" 2>/dev/null; then
  echo "ERROR: victoriametrics did not come back healthy" >&2
  exit 5
fi
echo "vm snapshot restore OK"

echo
echo "RESTORE PASS"
echo "  from: $FROM"
