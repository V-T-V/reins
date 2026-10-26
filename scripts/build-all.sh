#!/usr/bin/env bash
# ╔══════════════════════════════════════════════════════════════╗
# ║  reins 一键编译 + 部署准备                                    ║
# ║  编译三组件 + 把 agent.exe 拷到 dl/ 让 Relay 自动托管          ║
# ╚══════════════════════════════════════════════════════════════╝
#
# 用法:
#   ./scripts/build-all.sh                # 默认 v4.1.2
#   ./scripts/build-all.sh v4.2.0         # 指定版本
#
# 产出:
#   bin/reins-relay.exe   — 服务端
#   bin/reins.exe         — CLI 控制端
#   bin/reins-agent.exe   — Agent(被控端)
#   bin/dl/reins-agent.exe — Relay 自动托管的 Agent 副本
#
# 编译后启动 Relay,Agent 即可通过 /dl/reins-agent.exe 下载,
# 配合 /install.ps1 实现"一行命令远程部署"。

set -e
cd "$(dirname "$0")/.."

VERSION="${1:-v4.1.2}"
LDFLAGS="-s -w -X main.version=$VERSION"

echo "═══════════════════════════════════════════════════════════════"
echo " reins 编译 $VERSION"
echo "═══════════════════════════════════════════════════════════════"

mkdir -p bin/dl

echo "[1/3] 编译 Relay..."
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/reins-relay.exe ./relay/

echo "[2/3] 编译 Agent + CLI..."
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/reins-agent.exe ./agent/
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o bin/reins.exe ./cmd/reins/

echo "[3/3] 拷贝 Agent 到 dl/(供 Relay 托管下载)..."
cp bin/reins-agent.exe bin/dl/reins-agent.exe

echo ""
echo "✅ 编译完成 $VERSION"
echo ""
ls -lh bin/*.exe bin/dl/*.exe 2>/dev/null
echo ""
echo "下一步:"
echo "  1. 启动 Relay:  ./bin/reins-relay.exe -addr :7780"
echo "  2. 验证 bootstrap: curl -s http://localhost:7780/api/bootstrap -H 'Authorization: Bearer <token>'"
echo "  3. 在目标 Win10 执行 bootstrap 返回的 one_line_cmd"
