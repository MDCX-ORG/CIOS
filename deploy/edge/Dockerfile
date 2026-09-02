# PRMT-026 §4.1 — multi-stage Dockerfile for the cios-edge:local
# image. One image carries all 6 business binaries; compose picks
# the right one per service via `command:`.
#
# Stage 1 (build):
#   - golang:1.25 matches go.mod (go 1.25.0).
#   - CGO_ENABLED=0 → static binaries; alpine runtime needs no libc.
#   - Binaries are listed EXPLICITLY (not ./cmd/...) so the cios-cli,
#     cios-modbus-driver and cios-snmpsim binaries stay out of the
#     image — they're not in §3 whitelist and not used by §4.2.
#
# Stage 2 (runtime):
#   - alpine:3.20 ships busybox (wget + sh) which the §4.2 core
#     healthcheck and §4.5 smoke both rely on.
#   - No default ENTRYPOINT and no CMD — each compose service picks
#     its binary via `command:`. The image is compose-driven only;
#     `docker run cios-edge:local` would error out (intentional).

FROM golang:1.25 AS build
WORKDIR /src

# GOPROXY: goproxy.cn is the default (works in restricted networks);
# proxy.golang.org is the upstream fallback. This unblocks `go build`
# inside the build container when the host is offline to Google's
# module proxy.
# GOSUMDB: kept at the canonical `sum.golang.org` (NOT the cn mirror)
# so the Go module trust path is not single-pointed to a single
# operator. The cn mirror is intentionally not used for sumdb to keep
# the proxy/sumdb trust paths on different operators — see PRMT-026
# §9-sexies.2 S1.
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org

# Copy the whole repo first so go module resolution works. Build
# context is the repo root (see §4.2 `build.context: ../..`); this
# Dockerfile is reached via `deploy/edge/Dockerfile`.
COPY . .

# Build the 6 binaries explicitly. Order is alphabetical for stable
# diffs.
# -tags lab: image is the edge lab/demo stack (allow-no-auth + inject).
# Production binaries must be built without this tag (PRMT-217).
RUN CGO_ENABLED=0 go build -tags lab -buildvcs=false -o /out/ \
    ./cmd/cios-alarm \
    ./cmd/cios-core \
    ./cmd/cios-edge-writer \
    ./cmd/cios-gateway \
    ./cmd/cios-modbussim \
    ./cmd/cios-rules

FROM alpine:3.20
# wget + sh come from busybox already in alpine:3.20; no extra
# packages needed for the §4.2 core healthcheck or §4.5 smoke.
COPY --from=build /out/ /usr/local/bin/

# No ENTRYPOINT and no CMD — each compose service invokes its binary
# directly via `command:`. The image is compose-driven only; bare
# `docker run cios-edge:local` errors out, which is intentional
# (catches accidental ad-hoc invocations that might bypass compose
# networking guarantees).
