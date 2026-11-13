SHELL := /bin/bash

GO ?= go
EAM = cd backend && $(GO) run ./cmd/eam

.PHONY: install dev server build web-build migrate check check-write test test-pg measure-journal fmt

# Install dependencies after a fresh clone
install:
	cd backend && $(GO) mod download
	cd web && npm install

# Development: Go backend + Vite (frontend proxies /api to 8080).
# APP_ENV is required and has no default (see config.appEnv), so this target — the local
# entry point — supplies development. An exported APP_ENV still wins, so a developer can
# deliberately run production semantics; nothing here touches a deployed host, which runs
# bin/eam from its own unit and .env.
dev:
	@echo "backend http://127.0.0.1:8080 · frontend http://127.0.0.1:5173"
	@( cd backend && APP_ENV="$${APP_ENV:-development}" $(GO) run ./cmd/eam server ) & \
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
	leak_log=$$(mktemp "$${TMPDIR:-/tmp}/eam-test-leak.XXXXXX"); \
	trap 'rm -f "$$skip_log" "$$leak_log"' EXIT; \
	( cd backend && EAM_TEST_SKIP_LOG="$$skip_log" EAM_TEST_LEAK_LOG="$$leak_log" $(GO) test -count=1 ./... ); \
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
	leaks=0; \
	if [ -s "$$leak_log" ]; then \
	  leaks=$$(wc -l < "$$leak_log" | tr -d ' '); \
	fi; \
	if [ "$$leaks" -gt 0 ]; then \
	  echo ""; \
	  echo "WARNING: $$leaks scratch database(s) were left behind because DROP DATABASE failed:"; \
	  sed 's/^/         /' "$$leak_log"; \
	  echo "         Drop each with: drop database \"<name>\" with (force);"; \
	fi; \
	( cd web && npm run typecheck && npm test ); \
	frontend=$$?; \
	if [ "$$status" -ne 0 ] || [ "$$frontend" -ne 0 ]; then exit 1; fi

# Phase 1 measurement for docs/design/journal-entries.md: drives a real move through
# the settlement path and prints the journal's document write volume, writes per entry
# and the traced time inside journal statements. Needs a disposable CREATEDB service.
# FILES defaults to 500 (about six minutes). A full 5,000-file settlement would write
# roughly 500 GB of JSON, so measure that size with FREEZE=1 (admission and manifest
# freeze only), which is what the design doc reports.
measure-journal:
	@if [ -z "$$EAM_TEST_DATABASE_URL" ]; then \
	  echo "error: EAM_TEST_DATABASE_URL is not set; the measurement needs a disposable PostgreSQL service." >&2; \
	  echo "       Example: EAM_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable make measure-journal" >&2; \
	  exit 1; \
	fi
	cd backend && EAM_JOURNAL_COST_MEASUREMENT=1 \
	  EAM_JOURNAL_COST_FILES="$${FILES:-500}" \
	  EAM_JOURNAL_COST_FREEZE_ONLY="$${FREEZE:-0}" \
	  $(GO) test ./internal/fileops -run TestJournalEntriesDocumentCost -count=1 -v -timeout 1800s

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
	@leak_log=$$(mktemp "$${TMPDIR:-/tmp}/eam-test-leak.XXXXXX"); \
	trap 'rm -f "$$leak_log"' EXIT; \
	( cd backend && EAM_TEST_REQUIRE_DATABASE_URL=1 EAM_TEST_LEAK_LOG="$$leak_log" $(GO) test -count=1 ./... ); \
	status=$$?; \
	leaks=0; \
	if [ -s "$$leak_log" ]; then \
	  leaks=$$(wc -l < "$$leak_log" | tr -d ' '); \
	fi; \
	if [ "$$leaks" -gt 0 ]; then \
	  echo ""; \
	  echo "WARNING: $$leaks scratch database(s) were left behind because DROP DATABASE failed:"; \
	  sed 's/^/         /' "$$leak_log"; \
	  echo "         Drop each with: drop database \"<name>\" with (force);"; \
	fi; \
	exit $$status

fmt:
	cd backend && gofmt -w .
