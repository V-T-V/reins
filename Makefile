# go-rmm Makefile —— 跨平台编译三组件
# 用法：
#   make tidy        —— 同步依赖
#   make relay       —— 编译中继服务器（当前平台）
#   make agent       —— 编译 Windows Agent（交叉编译为 .exe）
#   make console     —— 编译 Web 控制台
#   make all         —— 编译全部三组件到 bin/
#   make run-relay   —— 本地前台运行中继服务器
#   make test        —— 运行测试
#   make fmt         —— 格式化 + 静态检查
#   make clean       —— 清理 bin/

GO        ?= go
BIN_DIR   := bin
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)

.PHONY: tidy relay agent console all run-relay run-agent test vet fmt clean

tidy:
	$(GO) mod tidy

## 编译中继服务器（当前平台，产出 bin/go-rmm-relay 或 .exe）
relay:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-relay$(EXT) ./relay

## 交叉编译 Windows Agent（强制 GOOS=windows）
agent:
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-agent.exe ./agent

## 编译 Web 控制台（与 Relay 同进程，可独立部署）
console:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/go-rmm-console$(EXT) ./console

## 编译全部
all: relay agent console

## 本地前台运行中继服务器（默认 :7780）
run-relay:
	$(GO) run ./relay -addr :7780

## 本地前台运行 Agent（调试模式）
run-agent:
	$(GO) run ./agent run -relay ws://127.0.0.1:7780/agent

## 运行测试
test:
	$(GO) test ./...

## 并发安全测试（需 CGO，race detector）
test-race:
	CGO_ENABLED=1 $(GO) test -race -count=3 ./...

## 静态检查
vet:
	$(GO) vet ./...

## 格式化
fmt:
	$(GO) fmt ./...
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

clean:
	rm -rf $(BIN_DIR)
