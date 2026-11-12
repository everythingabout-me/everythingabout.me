SHELL := /bin/bash

GO ?= go
EAM = cd backend && $(GO) run ./cmd/eam

.PHONY: install dev server build web-build migrate check check-write test test-pg fmt

# Install dependencies after a fresh clone
install:
	cd backend && $(GO) mod download
	cd web && npm install

# Development: Go backend + Vite (frontend proxies /api to 8080)
dev:
	@echo "backend http://127.0.0.1:8080 · frontend http://127.0.0.1:5173"
	@( cd backend && $(GO) run ./cmd/eam server ) & \
	 ( cd web && npm run dev ) & \
	 wait

# Backend only (also serves web/dist if make build has run)
server:
	$(EAM) server

# Production build: emit web/dist first, then compile bin/eam
build: web-build
	@mkdir -p bin
	cd backend && $(GO) build -trimpath -o ../bin/eam ./cmd/eam
	@echo "→ bin/eam"

web-build:
	cd web && npm run build

migrate:
	$(EAM) migrate

check:
	$(EAM) doctor

check-write:
	$(EAM) doctor --write

# Postgres-backed tests create and drop their own databases, so they need a
# disposable service with CREATEDB in EAM_TEST_DATABASE_URL; without it they
# skip. `make test` counts those skips and prints them, because a green run that
# quietly ran none of the upload/operation/change-stream coverage is the bug this
# gate exists to prevent. `make test-pg` requires the database instead.
#
# -count=1 is deliberate: a cached test result means the skip helper never ran,
# so the summary could claim zero skipped tests without having looked.
test:
	@skip_log=$$(mktemp "$${TMPDIR:-/tmp}/eam-test-skip.XXXXXX"); \
	trap 'rm -f "$$skip_log"' EXIT; \
	( cd backend && EAM_TEST_SKIP_LOG="$$skip_log" $(GO) test -count=1 ./... ); \
	status=$$?; \
	tests=0; \
	if [ -s "$$skip_log" ]; then \
	  tests=$$(wc -l < "$$skip_log" | tr -d ' '); \
	fi; \
	if [ "$$tests" -gt 0 ]; then \
	  echo ""; \
	  echo "WARNING: $$tests Postgres-backed test(s) skipped: EAM_TEST_DATABASE_URL is unset."; \
	  echo "         These cover the upload journal, operations and the change stream; run 'make test-pg' to require them."; \
	else \
	  echo "Postgres gate: no Postgres-backed tests were skipped."; \
	fi; \
	( cd web && npm run typecheck && npm test ); \
	frontend=$$?; \
	if [ "$$status" -ne 0 ] || [ "$$frontend" -ne 0 ]; then exit 1; fi

# The release gate for journal, upload, operation and change-stream changes.
# A missing URL is a failure here, never a skip.
test-pg:
	@if [ -z "$$EAM_TEST_DATABASE_URL" ]; then \
	  echo "error: EAM_TEST_DATABASE_URL is not set, so the Postgres-backed suite cannot run." >&2; \
	  echo "       make test-pg requires a disposable PostgreSQL service with CREATEDB; it never skips the suite." >&2; \
	  echo "       Example: EAM_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable make test-pg" >&2; \
	  exit 1; \
	fi
	@echo "Running the Postgres-backed suite with EAM_TEST_DATABASE_URL set; skipped suite = failed run."
	cd backend && EAM_TEST_REQUIRE_DATABASE_URL=1 $(GO) test -count=1 ./...

fmt:
	cd backend && gofmt -w .
