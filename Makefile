# Mammoth developer workflow.
SHELL := /bin/bash
BIN := bin/mammoth
DSN ?= postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable

.PHONY: all build test test-pg generate lint fmt fmt-check vet vuln-check acceptance compose-up compose-down clean

all: fmt generate build test

build:
	go build -o $(BIN) ./cmd/mammoth

# Contract is the single source of truth: regenerate and fail if the
# committed generated code drifted (docs/03-api.md §5).
generate:
	go tool oapi-codegen -config api/cfg.yaml api/openapi.yaml

contract-check: generate
	git diff --exit-code internal/api/gen || \
	  (echo "ERROR: generated code out of date with api/openapi.yaml" && exit 1)

fmt:
	gofmt -w cmd internal

fmt-check:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

vet:
	go vet ./...

test:
	go test ./... -count=1 -race

# Queue/store suites against a disposable PostgreSQL (compose-postgres).
test-pg:
	MAMMOTH_TEST_PG_DSN='$(DSN)' go test ./internal/store/... -count=1

lint: fmt vet

# Dependency vulnerability scan (govulncheck); wired into CI.
# Version pinned (latest at time of writing, requires go >= 1.26).
vuln-check:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Full acceptance against a local all-in-one (expects PG on 5432 and the
# binary built). See scripts/acceptance.py for the exact flow.
acceptance: build
	@export MAMMOTH_DATABASE_URL='$(DSN)' \
	  MAMMOTH_API_TOKEN=$${MAMMOTH_API_TOKEN:-devtoken} \
	  MAMMOTH_MASTER_KEY=$$(openssl rand -base64 32); \
	MAMMOTH_FAKE_BMC_DELAY=8s $(BIN) serve --mode=all & \
	sleep 3; \
	MAMMOTH_FAKE_BMC_DELAY=8s python3 scripts/acceptance.py \
	  --api http://localhost:8080 --token $${MAMMOTH_API_TOKEN:-devtoken} \
	  --dsn '$(DSN)' --binary ./$(BIN); \
	rc=$$?; pkill -f "$(BIN) serve" || true; exit $$rc

compose-up:
	docker compose -f deploy/compose.all-in-one.yml up -d --build

compose-down:
	docker compose -f deploy/compose.all-in-one.yml down -v

clean:
	rm -rf bin/
