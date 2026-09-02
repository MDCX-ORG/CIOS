#!/usr/bin/env bash
# gen-dev-mtls-certs.sh — PRMT-216 / P793 Phase 0: local lab CA + core/apigw leaves.
# NOT for production. Output default: artifacts/mtls-dev/ (gitignored).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/artifacts/mtls-dev}"
SITE="${CIOS_DEV_MTLS_SITE:-sgp01}"
mkdir -p "$OUT"
cd "$OUT"

if ! command -v openssl >/dev/null 2>&1; then
  echo "gen-dev-mtls-certs: openssl required" >&2
  exit 2
fi

# CA
openssl genrsa -out ca.key 2048 2>/dev/null
openssl req -x509 -new -nodes -key ca.key -sha256 -days 365 \
  -subj "/CN=cios-dev-ca" -out ca.pem 2>/dev/null

gen_leaf() {
  local name="$1" uri="$2"
  openssl genrsa -out "${name}.key" 2048 2>/dev/null
  openssl req -new -key "${name}.key" -subj "/CN=${name}" -out "${name}.csr" 2>/dev/null
  cat > "${name}.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=URI:${uri}
EOF
  openssl x509 -req -in "${name}.csr" -CA ca.pem -CAkey ca.key -CAcreateserial \
    -out "${name}.pem" -days 365 -sha256 -extfile "${name}.ext" 2>/dev/null
  rm -f "${name}.csr" "${name}.ext"
}

gen_leaf core "cios://${SITE}/core"
gen_leaf apigw "cios://${SITE}/apigw"
# Phase 4 lab leaf: edge gateway component identity (multi-tenant cloud ingest later).
gen_leaf gateway "cios://${SITE}/gateway"

cat > README.txt <<EOF
CIOS lab mTLS material (P793 / gen-dev-mtls-certs.sh)
site=${SITE}
files: ca.pem, core.{pem,key}, apigw.{pem,key}, gateway.{pem,key}

core:
  CIOS_MTLS_MODE=require \\
  CIOS_CORE_TLS_CERT=$OUT/core.pem \\
  CIOS_CORE_TLS_KEY=$OUT/core.key \\
  CIOS_CORE_TLS_CLIENT_CA=$OUT/ca.pem \\
  go run ./cmd/cios-core ... -listen 127.0.0.1:8443

apigw (upstream client):
  CIOS_MTLS_MODE=require \\
  CIOS_APIGW_TLS_CA=$OUT/ca.pem \\
  CIOS_APIGW_TLS_CERT=$OUT/apigw.pem \\
  CIOS_APIGW_TLS_KEY=$OUT/apigw.key \\
  CIOS_APIGW_UPSTREAM=https://127.0.0.1:8443 \\
  CIOS_APIGW_ALLOW_NO_AUTH=1 \\
  go run ./cmd/cios-apigw

gateway leaf (P793 Phase 4 lab identity; wire into edge binary when cloud ingest mTLS lands):
  URI SAN: cios://${SITE}/gateway
  files: $OUT/gateway.pem + $OUT/gateway.key

data-plane TLS (Phase 3, product-native — reuse site CA or vendor CA):
  CIOS_DATA_PLANE_TLS=require \\
  CIOS_PG_TLS_CA=... CIOS_NATS_TLS_CA=... CIOS_VM_TLS_CA=... \\
  CIOS_VM=https://...

Rotation: see docs/runbooks/P793-cert-rotation-runbook.md (manual 90d; auto-issue open).
EOF

echo "gen-dev-mtls-certs: wrote $OUT"
ls -la "$OUT"
