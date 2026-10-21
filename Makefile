BINARY      := bin/beaconx
MODULE      := github.com/orzl1/BeaconX
VERSION     ?= dev
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
  -X $(MODULE)/internal/version.Version=$(VERSION) \
  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
  -X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: help build web run test test-race cover lint docs-check fmt tidy brand docker-build up down clean

help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

web: ## 构建前端并嵌入（需要 Node.js 与 npm）
	cd web && npm_config_cache=../.cache/npm npm ci && NEXT_EXPORT=1 npm run build
	rm -rf internal/webui/dist && cp -r web/out internal/webui/dist

build: ## 构建二进制到 bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/beaconx

run: ## 本地运行（需要 PostgreSQL；见 docker compose）
	go run ./cmd/beaconx serve

test: ## 运行单元测试
	go test ./...

test-race: ## 开启竞态检测运行单元测试
	go test -race ./...

cover: ## 运行测试并输出覆盖率
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

docs-check: ## 校验唯一真源文档覆盖全部 HTTP 路由
	go test ./internal/httpapi -run TestProjectMapDocumentsAllRoutes -count=1

lint: ## 格式检查、静态检查与品牌门禁
	@out=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	  if [ -n "$$out" ]; then echo "gofmt required:"; echo "$$out"; exit 1; fi
	go vet ./...
	./scripts/check-brand.sh

fmt: ## 格式化 Go 源码
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

tidy: ## 整理 go.mod / go.sum
	go mod tidy

brand: ## 检查仓库中是否存在第三方项目痕迹
	./scripts/check-brand.sh

docker-build: ## 构建容器镜像
	docker build -t beaconx:dev .

up: ## 用 docker compose 启动整套服务
	docker compose up -d --build

down: ## 停止整套服务
	docker compose down

clean: ## 删除构建产物
	rm -rf bin dist coverage.out
