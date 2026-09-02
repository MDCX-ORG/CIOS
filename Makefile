.PHONY: build build-lab verify-no-lab-bypass verify-lab-tag-consumers test speccheck apicheck ci edge-up edge-down demo demo-clean demo-m1 demo-m1-clean demo-m2 demo-m2-clean proto vet fmt lint tidy-check backup restore soak soak-clean dev-apigw

build:
	CGO_ENABLED=0 go build ./...

# Lab stack (portal-live, edge demo): compiles auth bypasses.
build-lab:
	CGO_ENABLED=0 go build -tags lab ./...

# PRMT-217: production binary must refuse -allow-no-auth; lab binary must start.
# Behavioural assertion (not symbol grep — prod keeps a no-op of the same name).
verify-no-lab-bypass:
	@CGO_ENABLED=0 go build -o /tmp/cios-core-prod ./cmd/cios-core
	@if /tmp/cios-core-prod -protocol ./protocol -store /tmp/cios-prod-probe.json \
		-allow-no-auth -listen 127.0.0.1:19099 >/tmp/cios-core-prod-probe.log 2>&1; then \
		echo "FAIL: production build accepted -allow-no-auth"; exit 1; \
	fi
	@if ! grep -q 'lab build' /tmp/cios-core-prod-probe.log; then \
		echo "FAIL: prod refuse message missing 'lab build':"; cat /tmp/cios-core-prod-probe.log; exit 1; \
	fi
	@CGO_ENABLED=0 go build -tags lab -o /tmp/cios-core-lab ./cmd/cios-core
	@/tmp/cios-core-lab -protocol ./protocol -store /tmp/cios-lab-probe.json \
		-allow-no-auth -listen 127.0.0.1:19098 >/tmp/cios-core-lab-probe.log 2>&1 & \
		lab_pid=$$!; \
		ok=0; \
		for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do \
			if curl -sf http://127.0.0.1:19098/v1/health >/dev/null 2>&1; then ok=1; break; fi; \
			if ! kill -0 $$lab_pid 2>/dev/null; then break; fi; \
			sleep 0.15; \
		done; \
		kill -TERM $$lab_pid 2>/dev/null || true; wait $$lab_pid 2>/dev/null || true; \
		if [ "$$ok" != 1 ]; then \
			echo "FAIL: lab build did not listen with -allow-no-auth"; cat /tmp/cios-core-lab-probe.log; exit 1; \
		fi
	@echo "OK: production refuses -allow-no-auth; lab build accepts it"

# PRMT-217 R2: any script/compose that runs with the auth bypass MUST build
# with -tags lab. Enumeration by humans has failed twice; assert it instead.
# Scope is scripts/ + deploy/ only (Makefile itself holds -allow-no-auth in
# verify-no-lab-bypass and must not false-positive).
verify-lab-tag-consumers:
	@bad=0; \
	for f in $$(grep -rl -- '-allow-no-auth\|CIOS_APIGW_DEV_NO_AUTH' \
		scripts deploy --include='*.sh' --include='*.yml' --include='Dockerfile' 2>/dev/null); do \
		if grep -qE 'go (build|run)' "$$f" && ! grep -q 'tags lab' "$$f"; then \
			echo "FAIL: $$f uses the auth bypass but builds without -tags lab"; bad=1; \
		fi; \
	done; \
	[ $$bad -eq 0 ] && echo "OK: all bypass consumers build with -tags lab"

proto:
	protoc \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  proto/driver.proto

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out" >&2; echo "fmt: the above files are not gofmt-clean; run 'gofmt -w .'" >&2; exit 1; fi

lint:
	golangci-lint run ./...

# Reports any drift without rewriting; exits non-zero if `go mod
# tidy` would change go.mod/go.sum. We use `tidy -diff` which
# only prints the diff (no file mutation, no cp+rm dance — race
# and permission hazards avoided per PRMT-029 §5.2 finding #15,
# #21). The local ci target only *verifies* — touching go.mod /
# go.sum is out of scope (per PRMT-029 §4.2: "tidy -diff 报
# direct/indirect 漂移, 记入 §8 交由审核").
tidy-check:
	go mod verify
	@out=$$(go mod tidy -diff 2>&1); \
	  if [ -n "$$out" ]; then \
	    echo "$$out"; \
	    echo ":: go.mod / go.sum drift detected — see PRMT-029 §8 for follow-up"; \
	    exit 1; \
	  fi

speccheck:
	go run ./tools/speccheck ./protocol

# PRMT-073: drift report between core/server.go and protocol/openapi.yaml.
# ERROR (impl, no doc) is fatal; WARN (doc, no impl) is informational.
# Intentionally NOT wired into `ci` — promotion to a gating check is an
# architecture decision (the prompt notes the openapi.yaml is owned by
# the architect; current drift is expected while M2 endpoints are being
# caught up). Run locally: make apicheck; or under -strict: make apicheck-strict.
apicheck:
	go run ./tools/apicheck

apicheck-strict:
	go run ./tools/apicheck -strict

# `ci` is local + offline: no golangci-lint (that lives in CI's
# separate lint job). Order matters: build → vet → test (with
# race) → speccheck → fmt → tidy-check, so a broken compile or
# vet error fails fast.
ci: build vet test speccheck fmt tidy-check verify-no-lab-bypass verify-lab-tag-consumers

edge-up:
	docker compose --project-directory deploy/edge up -d --wait

edge-down:
	docker compose --project-directory deploy/edge down

demo:        ## M0 end-to-end smoke (standards ② & ④)
	bash scripts/m0-smoke.sh

demo-clean:
	-pkill -f cios-modbussim  || true
	-pkill -f cios-core       || true
	-pkill -f cios-gateway    || true
	$(MAKE) edge-down

demo-m1:        ## M1 single-site full-stack e2e smoke (local; never enters `make ci`)
	bash scripts/m1-smoke.sh

demo-m1-clean:
	docker compose -f deploy/edge/docker-compose.yml down -v

demo-m2:        ## M2 ops-loop end-to-end smoke (local; never enters `make ci`)
	bash scripts/m2-smoke.sh

demo-m2-clean:
	docker compose -f deploy/edge/docker-compose.yml down -v

# Backup / restore are local-only ops tools (PRMT-071, T32 mechanism).
# They require the M1 stack to be up (`make edge-up`) and never enter
# `make ci`. Retention policy is the caller's responsibility — these
# targets only produce / consume timestamped directories.
backup:        ## PG + VictoriaMetrics snapshot to backups/<UTC-ts>/ (PRMT-071)
	bash scripts/backup.sh

restore:       ## Dry-run restore from a backup dir; pass ARGS='--from <dir/ts> [--yes]'
	bash scripts/restore.sh $(ARGS)

# M2 ops-loop soak (PRMT-098, §M2-1). Local-only; never enters `make ci`.
# Usage: make soak ARGS="--hours 4"  (default 4h; PRMT default --days 7).
# Smoke: make soak ARGS="--minutes 5 --cycle 1m --probe 2m".
# Resume: make soak ARGS="--resume --hours 4"  (reuses prior SUMMARY.md).
soak:          ## M2 ops-loop soak harness (PRMT-098 / §M2-1)
	bash scripts/m2-soak.sh $(ARGS)

soak-clean:    ## Remove artifacts/soak/ (evidence dir, never committed)
	rm -rf artifacts/soak

# M3 apigw dev bring-up (PRMT-172 / feature/m3-auth). Local-only dev tool;
# never enters `make ci`. Requires a seeded core /v1 at --upstream (default
# http://127.0.0.1:8090 — run `make dev-seed` in feature/m3-model checkout).
# Usage:
#   make dev-apigw                          # foreground, Ctrl-C to stop
#   make dev-apigw ARGS="--check-only"      # assert then exit 0/1
#   make dev-apigw ARGS="--port 9091 --upstream http://127.0.0.1:8090"
dev-apigw:     ## M3 apigw no-auth dev bring-up + /api/* assert (PRMT-172)
	bash scripts/m3-apigw-dev.sh $(ARGS)

# --- merged from feature branch ---
.PHONY: cardinality-bench gateway-bench pg-parity customer-portal-smoke
.PHONY: dev-seed
dev-seed:  ## M3 dev: seed core/store from EXT-001+spec-008 and boot cios-core (L93/L94, feature/m3-model)
	bash scripts/m3-seed-dev.sh

cardinality-bench:  ## PRMT-183: per-tenant active-series vs VM query-latency sweep (local-only)
	@command -v docker >/dev/null 2>&1 || { echo "cardinality-bench: docker CLI missing on PATH" >&2; exit 2; }
	go run ./cmd/cardinality-bench $(ARGS)

gateway-bench:  ## PRMT-210 / D10: pod-gateway driver×rate resource model (local-only, advisory)
	go run ./cmd/gateway-bench $(ARGS)

mtls-dev-certs:  ## P793: generate lab CA + core/apigw leaves under artifacts/mtls-dev/
	bash scripts/gen-dev-mtls-certs.sh

mtls-e2e:  ## P793: automated mTLS e2e (gen certs, boot core+apigw, H2/H3 asserts)
	bash scripts/mtls-e2e.sh

control-e2e:  ## P722: core Set → gateway control → modbussim write
	bash scripts/control-e2e.sh

pg-parity:  ## PRMT-211 / P795: run core tests against live Postgres (local-only)
	bash scripts/pg-parity.sh $(ARGS)

# --- merged from feature branch ---
.PHONY: portal-live portal-live-check portal-smoke
portal-live:  ## seed+core+apigw+ops-portal live (Ctrl-C teardown)
	bash scripts/portal-live.sh

portal-live-check:  ## same stack, assert health, exit
	bash scripts/portal-live.sh --check-only

portal-smoke:  ## HTTP smoke against running live portal (no browser)
	bash scripts/portal-smoke.sh

customer-portal-smoke:  ## PRMT-212: HTTP smoke for customer portal (no browser)
	bash scripts/customer-portal-smoke.sh
