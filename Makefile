.PHONY: run swagger help dev dev-api dev-web dev-stop docker-up docker-build docker-down ensure-air ensure-pnpm

ROOT := $(CURDIR)
CONFIG ?= $(ROOT)/config.yaml
DEV_CONFIG ?= $(ROOT)/config.dev.yaml
export PATH := /usr/local/go/bin:$(HOME)/go/bin:/root/go/bin:$(PATH)

help:
	@echo "Grok2API 开发命令"
	@echo "  make dev           本机热重载（API air + 前端 Vite），自动停 Docker grok2api"
	@echo "  make dev-api       仅后端热重载 → http://127.0.0.1:18000"
	@echo "  make dev-web       仅前端 HMR  → http://127.0.0.1:5173"
	@echo "  make dev-stop      停本机 dev 进程"
	@echo "  make run           单次 go run（无热重载）"
	@echo "  make docker-build  稳定后打包 grok2api:console-4.5"
	@echo "  make docker-up     启动/恢复 Docker 现网容器"
	@echo "  make docker-down   停止 Docker grok2api"
	@echo "  make swagger       重新生成 swagger"

run:
	cd backend && GOCACHE=$(abspath $(ROOT)/.gocache) go run ./cmd/grok2api --config "$(abspath $(CONFIG))" $(RUN_ARGS)

# ---- 本机热重载（推荐日常改代码）----
dev: ensure-air
	@chmod +x "$(ROOT)/scripts/dev-local.sh"
	"$(ROOT)/scripts/dev-local.sh" all

dev-api: ensure-air
	@chmod +x "$(ROOT)/scripts/dev-local.sh"
	"$(ROOT)/scripts/dev-local.sh" api

dev-web: ensure-pnpm
	@chmod +x "$(ROOT)/scripts/dev-local.sh"
	"$(ROOT)/scripts/dev-local.sh" web

dev-stop:
	@chmod +x "$(ROOT)/scripts/dev-local.sh"
	"$(ROOT)/scripts/dev-local.sh" stop

dev-stop-docker docker-down:
	@docker stop grok2api 2>/dev/null || true
	@echo "grok2api container stopped (if it was running)"

docker-up:
	@# 先停本机 dev，再恢复容器自动重启
	@"$(ROOT)/scripts/dev-local.sh" stop 2>/dev/null || true
	@docker update --restart=unless-stopped grok2api >/dev/null 2>&1 || true
	cd /opt/grok2api && docker compose up -d grok2api
	@echo "Docker grok2api → http://127.0.0.1:18000  /  https://grok2api.xdb23.com"

docker-build:
	docker build -t grok2api:console-4.5 \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=$$(go env GOARCH 2>/dev/null || echo arm64) \
		"$(ROOT)"
	@echo "Image grok2api:console-4.5 built. Run: make docker-up"

ensure-air:
	@command -v air >/dev/null 2>&1 || go install github.com/air-verse/air@latest

ensure-pnpm:
	@command -v pnpm >/dev/null 2>&1 || npm install -g pnpm@11.5.2

swagger:
	cd backend && GOCACHE=$(ROOT)/.gocache go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
		-g main.go \
		-d cmd/grok2api,internal/transport/http \
		--parseInternal \
		--output docs \
		--outputTypes go,json,yaml
