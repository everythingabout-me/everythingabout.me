SHELL := /bin/bash

GO ?= go
EAM = cd backend && $(GO) run ./cmd/eam

.PHONY: install dev server build web-build migrate check check-write test fmt

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

test:
	cd backend && $(GO) test ./...
	cd web && npm run typecheck && npm test

fmt:
	cd backend && gofmt -w .
