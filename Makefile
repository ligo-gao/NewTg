# NewTG 仓库根 Makefile —— 唯一构建入口（docs/platform-02-仓库构建与CI-设计.md §4）
# 所有 Go 命令在 backend/ 内执行；前端命令在 frontend/ 内执行。
.DEFAULT_GOAL := help

VERSION    := $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
  -X newtg/gateway/internal/version.Version=$(VERSION) \
  -X newtg/gateway/internal/version.Commit=$(COMMIT) \
  -X newtg/gateway/internal/version.BuildTime=$(BUILD_TIME)
BACKEND    := backend
BIN        := $(BACKEND)/bin
MIGRATIONS := $(BACKEND)/migrations
# 集成测试用真实依赖；本地 dev 指向 compose 暴露的端口
TEST_DB_DSN     ?= postgres://newtg:newtg@localhost:5432/newtg_test?sslmode=disable
TEST_REDIS_ADDR ?= localhost:6379
IMAGE          ?= newtg/gateway:dev
# 本机架构镜像的目标平台：arm64 主机显式 `make image PLATFORM=linux/arm64`（2026-10-18 业主裁决 U-02-5）
PLATFORM       ?= linux/amd64
# 工具链版本由 CI 变量注入固定（2026-10-18 业主裁决 U-02-6），禁止 @latest；
# 仓库内单一取值，本地可用 `make MIGRATE_VERSION=...` 覆盖。
MIGRATE_VERSION     ?= v4.18.1
KIN_OPENAPI_VERSION ?= v0.131.0

.PHONY: help version tidy fmt fmt-check vet lint build run-gateway run-worker \
        test test-cover test-integration \
        migrate-up migrate-down migrate-version migrate-create migrate-verify \
        web-install web-dev web-typecheck web-lint web-test web-build \
        api-lint image image-multiarch compose-up compose-down dev clean

help: ## 列出全部目标（默认目标）
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | awk -F':.*## ' '{printf "%-22s %s\n",$$1,$$2}'

version: ## 打印将注入二进制的构建信息
	@echo "version=$(VERSION) commit=$(COMMIT) build_time=$(BUILD_TIME)"

tidy: ## 整理 go.mod/go.sum
	cd $(BACKEND) && go mod tidy

fmt: ## 就地格式化 Go 源
	cd $(BACKEND) && gofmt -w . && goimports -w .

fmt-check: ## 只检查不改（CI 用）
	cd $(BACKEND) && test -z "$$(gofmt -l .)"

vet: ## 静态检查（含契约包）
	cd $(BACKEND) && go vet ./...

lint: ## Go lint + 导入边界（platform-02 §6.3）
	golangci-lint run --config .golangci.yml ./backend/...

build: ## 构建单产物多角色二进制 bin/newtg（platform-02 §5.1 裁决）
	cd $(BACKEND) && CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/newtg ./cmd/newtg

run-gateway: ## 本机起 gateway（读环境变量配置）
	cd $(BACKEND) && RUN_MODE=gateway go run ./cmd/newtg

run-worker: ## 本机起 worker
	cd $(BACKEND) && RUN_MODE=worker go run ./cmd/newtg

test: ## 单元测试：无外部依赖，CI 门禁（§8.1）
	cd $(BACKEND) && go test ./...

test-cover: ## 覆盖率报告（不上传第三方）
	cd $(BACKEND) && go test -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out

test-integration: ## 集成测试：需真实 PG + Redis（§8.2）
	cd $(BACKEND) && NEWTG_TEST_DB_DSN='$(TEST_DB_DSN)' NEWTG_TEST_REDIS_ADDR='$(TEST_REDIS_ADDR)' go test -tags=integration ./...

migrate-up: ## 应用全部迁移
	migrate -path $(MIGRATIONS) -database "$(TEST_DB_DSN)" up

migrate-down: ## 回退一步（验证 down 可执行）
	migrate -path $(MIGRATIONS) -database "$(TEST_DB_DSN)" down 1

migrate-version: ## 打印当前迁移版本
	migrate -path $(MIGRATIONS) -database "$(TEST_DB_DSN)" version

migrate-create: ## 新建成对迁移（14 位 UTC 时间戳；name= 传入）
	migrate create -ext sql -dir $(MIGRATIONS) -format 20060102150405 $(name)

migrate-verify: ## 幂等 + up/down 往返验证（§8.3，CI 门禁）
	$(MAKE) migrate-up && $(MAKE) migrate-up && $(MAKE) migrate-down && $(MAKE) migrate-up

web-install: ## 冻结锁文件安装前端依赖
	cd frontend && bun install --frozen-lockfile

web-dev: ## 前端开发服务器（Rsbuild，代理到 localhost:3000）
	cd frontend && bun run dev

web-typecheck: ## tsc --noEmit 类型检查
	cd frontend && bun run typecheck

web-lint: ## ESLint
	cd frontend && bun run lint

web-test: ## Vitest（jsdom）
	cd frontend && bun run test

web-build: ## 生产静态构建 → frontend/dist/
	cd frontend && bun run build

api-lint: ## 校验 OpenAPI 骨架（M1+ 挂 codegen）；版本由 CI 变量 KIN_OPENAPI_VERSION 固定
	cd $(BACKEND) && go run github.com/getkin/kin-openapi/cmd/validate@$(KIN_OPENAPI_VERSION) api/openapi.yaml

image: ## 构建本机架构镜像（PLATFORM 默认 linux/amd64）
	docker buildx build --platform $(PLATFORM) -f $(BACKEND)/Dockerfile -t $(IMAGE) $(BACKEND)

image-multiarch: ## 多架构构建并推送（发布用）
	docker buildx build --platform linux/amd64,linux/arm64 -f $(BACKEND)/Dockerfile -t $(IMAGE) --push $(BACKEND)

compose-up: ## 起 PG + Redis + gateway + worker
	docker compose -f deploy/docker-compose.yml up -d --build

compose-down: ## 停栈（保留卷）
	docker compose -f deploy/docker-compose.yml down

dev: ## 起开发依赖（前端热更；后端另开 make run-gateway）
	$(MAKE) web-dev

clean: ## 清构建产物
	rm -rf $(BIN) $(BACKEND)/coverage.out frontend/dist
