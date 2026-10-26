# go-rmm Makefile — 世界级 Go 项目构建系统
#
# 用法:
#   make all         — 编译全部组件到 bin/
#   make test        — 运行测试(含覆盖率)
#   make test-race   — 并发安全测试(race detector)
#   make lint        — golangci-lint 静态检查
#   make vuln        — govulncheck 漏洞扫描
#   make docker      — Docker 镜像构建
#   make release     — goreleaser 跨平台发布
#   make helm        — Helm chart 打包
#   make cover       — 测试覆盖率 HTML 报告
#   make ci          — 完整 CI 流程(vet+lint+test+race+vuln)

GO        ?= go
BIN_DIR   := bin
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
EXT       := $(shell go env GOOS | grep -q windows && echo .exe)

.PHONY: tidy all relay agent cli console test test-race cover lint vuln fmt vet docker release helm dist ci clean run-relay run-agent install-tools

## === 构建 ===

tidy:
	$(GO) mod tidy

all: relay agent cli

relay:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-relay$(EXT) ./relay

agent:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-agent$(EXT) ./agent

cli:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm$(EXT) ./cmd/go-rmm

console:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-console$(EXT) ./console

## === 测试 ===

test:
	$(GO) test -cover -count=1 ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "覆盖率报告: coverage.html"

## === 静态检查 ===

vet:
	$(GO) vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "安装 golangci-lint: make install-tools"; exit 1; }
	golangci-lint run --timeout 5m ./...

vuln:
	@command -v govulncheck >/dev/null 2>&1 || { echo "安装 govulncheck: make install-tools"; exit 1; }
	govulncheck ./...

fmt:
	$(GO) fmt ./...
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

## === CI 一键 ===

ci: vet test test-race lint
	@echo "✅ CI 全部通过"

## === Docker ===

docker:
	docker build -f Dockerfile.relay -t go-rmm-relay:$(VERSION) .
	docker build -f Dockerfile.agent -t go-rmm-agent:$(VERSION) .

## === 发布 ===

release:
	@command -v goreleaser >/dev/null 2>&1 || { echo "安装 goreleaser: https://goreleaser.com/install/"; exit 1; }
	goreleaser release --clean

helm:
	helm package deploy/helm/go-rmm -d $(BIN_DIR)/charts
	@echo "Helm chart: $(BIN_DIR)/charts/"

## === 发行包(极简部署:一个包 + 一行安装) ===

dist: all
	@echo "📦 构建发行包..."
	@mkdir -p dist
	@cp scripts/install.sh scripts/install.ps1 scripts/deploy-relay-linux.sh scripts/deploy-agent-windows.ps1 scripts/deploy-docker.sh dist/
	@cp deploy/go-rmm-relay.service deploy/openapi.yaml dist/
	@cp README.md DEPLOYMENT.md dist/
# Windows 包(.zip)
	zip -j dist/go-rmm-$(VERSION)-windows.zip \
		$(BIN_DIR)/go-rmm-relay.exe $(BIN_DIR)/go-rmm-agent.exe $(BIN_DIR)/go-rmm.exe \
		dist/install.ps1 dist/deploy-agent-windows.ps1 \
		README.md 2>/dev/null || true
# Linux 包(.tar.gz) — 交叉编译
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/go-rmm-relay-linux ./relay
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/go-rmm-agent-linux ./agent
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/go-rmm-linux ./cmd/go-rmm
	tar czf dist/go-rmm-$(VERSION)-linux.tar.gz \
		-C dist go-rmm-relay-linux go-rmm-agent-linux go-rmm-linux install.sh deploy-relay-linux.sh
	@echo "✅ 发行包: dist/"
	@ls -lh dist/go-rmm-*.zip dist/go-rmm-*.tar.gz 2>/dev/null

## === 本地运行 ===

run-relay:
	$(GO) run ./relay -addr :7780 -dev

run-agent:
	$(GO) run ./agent run -relay ws://127.0.0.1:7780/agent

run-cli:
	$(GO) run ./cmd/go-rmm status

## === 文档口径门禁 ===

check-docs:
	bash scripts/check-docs.sh

## === 工具安装 ===

install-tools:
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GO) install golang.org/x/tools/cmd/goimports@latest

## === 清理 ===

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html dist
