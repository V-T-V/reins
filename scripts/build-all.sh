#!/usr/bin/env bash
# ╔══════════════════════════════════════════════════════════════╗
# ║  go-rmm 一键编译 + 部署准备                                    ║
# ║  编译三组件 + 把 agent.exe 拷到 dl/ 让 Relay 自动托管          ║
# ╚══════════════════════════════════════════════════════════════╝
#
# 用法:
#   ./scripts/build-all.sh                # 默认 v4.1.2
#   ./scripts/build-all.sh v4.2.0         # 指定版本
#
# 产出:
#   bin/go-rmm-relay.exe   — 服务端
#   bin/go-rmm.exe         — CLI 控制端
#   bin/go-rmm-agent.exe   — Agent(被控端)
#   bin/dl/go-rmm-agent.exe — Relay 自动托管的 Agent 副本
#
# 编译后启动 Relay,Agent 即可通过 /dl/go-rmm-agent.exe 下载,
# 配合 /install.ps1 实现"一行命令远程部署"。

set -e
cd "$(dirname "$0")/.."

VERSION="${1:-v4.1.2}"
LDFLAGS="-s -w -X main.version=$VERSION"

echo "═══════════════════════════════════════════════════════════════"
echo " go-rmm 编译 $VERSION"
echo "═══════════════════════════════════════════════════════════════"

mkdir -p bin/dl

echo "[1/3] 编译 Relay..."
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/go-rmm-relay.exe ./relay/

echo "[2/3] 编译 Agent + CLI..."
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/go-rmm-agent.exe ./agent/
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/go-rmm.exe ./cmd/go-rmm/

echo "[3/3] 拷贝 Agent 到 dl/(供 Relay 托管下载)..."
cp bin/go-rmm-agent.exe bin/dl/go-rmm-agent.exe

echo ""
echo "✅ 编译完成 $VERSION"
echo ""
ls -lh bin/*.exe bin/dl/*.exe 2>/dev/null
echo ""
echo "下一步:"
echo "  1. 启动 Relay:  ./bin/go-rmm-relay.exe -addr :7780"
echo "  2. 验证 bootstrap: curl -s http://localhost:7780/api/bootstrap -H 'Authorization: Bearer <token>'"
echo "  3. 在目标 Win10 执行 bootstrap 返回的 one_line_cmd"
