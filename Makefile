SHELL := /bin/bash

GO ?= go
EAM = cd backend && $(GO) run ./cmd/eam

.PHONY: install dev server build web-build migrate check check-write test fmt

# 首次拉仓库后装依赖
install:
	cd backend && $(GO) mod download
	cd web && npm install

# 开发：Go 后端 + Vite（前端 /api 代理到 8080）
dev:
	@echo "后端 http://127.0.0.1:8080 · 前端 http://127.0.0.1:5173"
	@( cd backend && $(GO) run ./cmd/eam server ) & \
	 ( cd web && npm run dev ) & \
	 wait

# 只起后端（如果已经 make build，会同时托管 web/dist）
server:
	$(EAM) server

# 生产构建：先出 web/dist，再编 bin/eam
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
	cd web && npm run typecheck

fmt:
	cd backend && gofmt -w .
