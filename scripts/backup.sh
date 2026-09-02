#!/usr/bin/env bash
# scripts/backup.sh — PG + VictoriaMetrics backup for the CIOS edge
# stack (PRMT-071, T32 mechanism).
#
# Mechanism-only: produces timestamped backups of PG (pg_dump custom
# format) and VM (/snapshot/create via HTTP). RPO/RTO target values
# are out of scope here — that is D27's decision.
#
# Idempotent: re-running just creates a new timestamped directory.
# Old backups are NEVER removed by this script — retention is the
# caller's / cron job's responsibility (PRMT-071 §2).
#
# Usage:
#   scripts/backup.sh [--out <dir>]
#
# Output:
#   <dir>/<UTC-ts>/pg.dump
#   <dir>/<UTC-ts>/vm-snapshot.tar.gz
#   <dir>/<UTC-ts>/manifest.txt   (timestamp, versions, sha256)
#
# Exit codes:
#   0  success
#   1  bad arguments
#   2  preflight failed (compose not reachable, services unhealthy)
#   3  pg_dump failed
#   4  VM snapshot failed
#   5  manifest / checksum step failed
#
# PRMT-071 §5 MUST NOTs observed:
#   - No hardcoded credentials (env / .env)
#   - Never deletes existing data
#   - Never enters `make ci`
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EDGE="$ROOT/deploy/edge"
COMPOSE_FILE="$EDGE/docker-compose.yml"
OUT_ROOT="$ROOT/backups"

usage() {
  cat <<EOF
usage: $0 [--out <dir>]

Options:
  --out <dir>   Parent directory for the timestamped backup folder.
                Defaults to: $ROOT/backups
EOF
}

# --- arg parse ---------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      [[ $# -ge 2 ]] || { echo "ERROR: --out requires a value" >&2; usage >&2; exit 1; }
      OUT_ROOT="$2"
      shift 2
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

# --- preflight ---------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker not on PATH" >&2
  exit 2
fi
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo "ERROR: neither sha256sum nor shasum found on PATH" >&2
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
# Stack must be up (the script does NOT bring it up — that's `make edge-up`).
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
# Per PRMT-071 §5 MUST NOT: do not hardcode production credentials.
# PG creds live in deploy/edge/.env. Source it if present; otherwise
# let pg_dump fall back to the env vars the caller exported.
if [[ -f "$EDGE/.env" ]]; then
  # shellcheck source=/dev/null
  set -a; source "$EDGE/.env"; set +a
fi
: "${CIOS_PG_PASSWORD:?ERROR: CIOS_PG_PASSWORD not set (deploy/edge/.env missing?)}"
: "${CIOS_PG_DSN:?ERROR: CIOS_PG_DSN not set (deploy/edge/.env missing?)}"

# --- timestamped output dir -------------------------------------------
TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$OUT_ROOT/$TS"
mkdir -p "$DEST"
echo "backup target: $DEST"

# --- 1. PG: pg_dump (custom format) -----------------------------------
echo "== pg_dump =="
PG_DUMP="$DEST/pg.dump"
# Run inside the running postgres container so the script needs no
# host-side port access (PRMT-071 §4: "经 docker compose exec 操作
# 容器 … 不假设宿主直连 PG/VM 端口").
# PGPASSWORD must be exported for pg_dump in the container; CIOS_PG_PASSWORD
# is the source of truth (.env), and the postgres service expects that
# exact role.
docker compose -f "$COMPOSE_FILE" exec -T \
  -e PGPASSWORD="$CIOS_PG_PASSWORD" \
  postgres \
  pg_dump -U cios -d cios -Fc --no-owner --no-acl \
  > "$PG_DUMP"
if [[ ! -s "$PG_DUMP" ]]; then
  echo "ERROR: pg_dump produced empty file" >&2
  exit 3
fi
echo "pg_dump OK: $PG_DUMP ($(wc -c < "$PG_DUMP" | tr -d ' ') bytes)"

# --- 2. VM: /snapshot/create -----------------------------------------
# PRMT-071 §2: "VM snapshot (/snapshot/create API …)".
# VM service is published at 127.0.0.1:8428 on the host (per
# deploy/edge/docker-compose.yml), so a host-side curl is the
# canonical way. We resolve the VM host port from the compose file
# (port published to 127.0.0.1:8428).
echo "== vm snapshot =="
VM_URL="http://127.0.0.1:8428"
SNAP_RESP="$(curl -fsS "$VM_URL/snapshot/create" || true)"
if [[ -z "$SNAP_RESP" ]]; then
  echo "ERROR: VM /snapshot/create returned empty" >&2
  exit 4
fi
# The response is a JSON object with key "status" / "snapshot" depending
# on VM build. We persist the raw response for the manifest; the actual
# snapshot lives in /storage inside the container. Tar it for portability.
SNAP_RAW="$DEST/vm-snapshot.json"
printf '%s\n' "$SNAP_RESP" > "$SNAP_RAW"
SNAP_TAR="$DEST/vm-snapshot.tar.gz"
# Snapshot folder is /storage inside the VM container. We do NOT
# touch the named volume directly (PRMT-071 §5: do not delete data;
# snapshot must be created via the supported API). We bind through
# a one-shot helper: `docker cp` the snapshot tree from /storage to a
# local tar. The /snapshot/create response typically includes a path
# to the snapshot directory under /storage; we ship a tar of that
# subtree, falling back to the whole /storage tree if we can't parse.
SNAP_DIR="$(printf '%s' "$SNAP_RESP" | sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true)"
if [[ -z "$SNAP_DIR" || "$SNAP_DIR" == "$SNAP_RESP" ]]; then
  # Could not parse a path; fall back to a tar of /storage (full VM
  # data dir) — bigger but always works.
  SNAP_DIR="/storage"
fi
# Resolve the snapshot path inside the container; the response path
# is typically already absolute starting with /storage.
if [[ "${SNAP_DIR:0:1}" != "/" ]]; then
  SNAP_DIR="/storage/$SNAP_DIR"
fi
# Stream the snapshot dir from the container to a host tar.gz.
docker compose -f "$COMPOSE_FILE" exec -T victoriametrics \
  tar -C "$(dirname "$SNAP_DIR")" -czf - "$(basename "$SNAP_DIR")" \
  > "$SNAP_TAR" 2>/dev/null \
  || {
    # Fallback: full /storage tar.
    docker compose -f "$COMPOSE_FILE" exec -T victoriametrics \
      tar -C / -czf - storage > "$SNAP_TAR"
  }
if [[ ! -s "$SNAP_TAR" ]]; then
  echo "ERROR: VM snapshot tar is empty" >&2
  exit 4
fi
echo "vm snapshot OK: $SNAP_TAR ($(wc -c < "$SNAP_TAR" | tr -d ' ') bytes)"

# --- 3. manifest + checksums -----------------------------------------
echo "== manifest =="
MANIFEST="$DEST/manifest.txt"
{
  echo "timestamp_utc=$TS"
  echo "compose_file=$COMPOSE_FILE"
  echo "pg_dump_file=pg.dump"
  echo "vm_snapshot_file=vm-snapshot.tar.gz"
  echo "vm_snapshot_path_in_container=$SNAP_DIR"
  echo "vm_snapshot_api_response=vm-snapshot.json"
  echo
  echo "--- versions ---"
  PG_VER="$(docker compose -f "$COMPOSE_FILE" exec -T postgres \
    pg_dump --version 2>/dev/null | head -n1 || echo unknown)"
  VM_VER="$(curl -fsS "$VM_URL/metrics" 2>/dev/null \
    | awk '/^vm_version/ {sub(/^vm_version[^ ]* ?/,""); print; exit}' \
    || echo unknown)"
  TAR_VER="$(tar --version 2>/dev/null | head -n1 || echo unknown)"
  echo "pg_dump=$PG_VER"
  echo "victoriametrics=$VM_VER"
  echo "tar=$TAR_VER"
  echo
  echo "--- sha256 ---"
} > "$MANIFEST"

# sha256 helper: prefer sha256sum (GNU coreutils); fall back to shasum (macOS).
if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$DEST" && sha256sum pg.dump vm-snapshot.tar.gz >> "$MANIFEST" )
else
  ( cd "$DEST" && shasum -a 256 pg.dump vm-snapshot.tar.gz >> "$MANIFEST" )
fi
echo "manifest OK: $MANIFEST"

echo
echo "BACKUP PASS"
echo "  $DEST"
