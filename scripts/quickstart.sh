#!/bin/bash
# ╔══════════════════════════════════════════════════════════╗
# ║  reins 极简安装(一行命令,3 步完成)                      ║
# ╚══════════════════════════════════════════════════════════╝
#
# 用法:
#   curl -fsSL http://your-server:7780/q | bash              # 服务端
#   curl -fsSL http://your-server:7780/q | bash -s -- agent   # 被控端
#   curl -fsSL http://your-server:7780/q | bash -s -- cli     # 操作端
#
# 或本地:
#   ./scripts/quickstart.sh              # 服务端(编译+启动)
#   ./scripts/quickstart.sh agent        # 被控端
#   ./scripts/quickstart.sh cli          # 操作端
set -euo pipefail
VERSION="${VERSION:-v3.9.0}"
DIR="${DIR:-$HOME/.reins}"
MODE="${1:-server}"
G='\033[0;32m'; Y='\033[1;33m'; R='\033[0;31m'; N='\033[0m'
ok()   { echo -e "${G}✅${N} $*"; }
warn() { echo -e "${Y}⚠️${N} $*"; }
die()  { echo -e "${R}✗${N} $*"; exit 1; }

# === 编译/下载 ===
build_or_download() {
    mkdir -p "$DIR/bin"
    if command -v go &>/dev/null; then
        # 本地有 Go 源码?
        if [ -f "go.mod" ]; then
            ok "从源码编译..."
            CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$VERSION" -o "$DIR/bin/reins-relay" ./relay/
            CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$VERSION" -o "$DIR/bin/reins-agent" ./agent/
            CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$VERSION" -o "$DIR/bin/reins" ./cmd/reins/
        else
            die "请在本项目根目录运行,或指定二进制下载地址: SERVER=http://..."
        fi
    else
        die "无 Go 环境。请先安装 Go 1.21+: https://go.dev/dl/"
    fi
}

# === PATH ===
add_path() {
    local profile=""
    [ -f ~/.bashrc ] && profile=~/.bashrc
    [ -f ~/.zshrc ] && profile=~/.zshrc
    if [ -n "$profile" ] && ! grep -q "$DIR/bin" "$profile"; then
        echo "export PATH=\"$DIR/bin:\$PATH\"" >> "$profile"
        ok "PATH 已更新(source $profile 或重开终端)"
    fi
    export PATH="$DIR/bin:$PATH"
}

# === 生成 Token ===
gen_token() {
    if [ -f "$DIR/token" ]; then
        cat "$DIR/token"
    else
        local t=$(cat /dev/urandom | tr -dc 'a-f0-9' | head -c 32)
        echo "$t" > "$DIR/token"
        chmod 600 "$DIR/token"
        echo "$t"
    fi
}

# === 本机 IP ===
my_ip() {
    hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1"
}

# ================================================================
# 服务端模式
# ================================================================
if [ "$MODE" = "server" ] || [ "$MODE" = "serve" ] || [ "$MODE" = "relay" ]; then
    echo "🚀 reins 服务端安装"
    build_or_download
    add_path
    TOKEN=$(gen_token)
    IP=$(my_ip)

    # 启动
    ok "启动 Relay..."
    nohup "$DIR/bin/reins-relay" -addr :7780 -token "$TOKEN" > "$DIR/relay.log" 2>&1 &
    echo $! > "$DIR/relay.pid"
    sleep 2

    # 验证
    if curl -sf "http://127.0.0.1:7780/healthz" | grep -q ok 2>/dev/null; then
        ok "Relay 运行中"
    else
        warn "健康检查未通过,查看日志: cat $DIR/relay.log"
    fi

    echo ""
    echo "╔════════════════════════════════════════════════════╗"
    echo "║  reins 服务端已就绪!                              ║"
    echo "╠════════════════════════════════════════════════════╣"
    echo "║                                                    ║"
    echo "║  Web UI:  http://$IP:7780                        "
    echo "║  Token:   $TOKEN           "
    echo "║                                                    ║"
    echo "║  ─── 接下来 ───                                    ║"
    echo "║                                                    ║"
    echo "║  在被控端(Windows/Linux)执行:                     ║"
    echo "║  reins-agent run -relay ws://$IP:7780/agent -token $TOKEN"
    echo "║                                                    ║"
    echo "║  在你的电脑执行:                                   ║"
    echo "║  reins --relay http://$IP:7780 --token $TOKEN agent list"
    echo "║                                                    ║"
    echo "╚════════════════════════════════════════════════════╝"

# ================================================================
# 被控端(Agent)模式
# ================================================================
elif [ "$MODE" = "agent" ]; then
    RELAY="${RELAY:-ws://$(my_ip):7780/agent}"
    TOKEN="${TOKEN:-$(cat $DIR/token 2>/dev/null || echo '')}"
    if [ -z "$TOKEN" ]; then die "需要 TOKEN 环境变量"; fi

    echo "📡 reins Agent 安装"
    build_or_download
    add_path

    ok "启动 Agent(连接 $RELAY)..."
    nohup "$DIR/bin/reins-agent" run -relay "$RELAY" -token "$TOKEN" > "$DIR/agent.log" 2>&1 &
    echo $! > "$DIR/agent.pid"
    sleep 2
    ok "Agent 已启动(日志: $DIR/agent.log)"

    echo ""
    echo "╔════════════════════════════════════════════════════╗"
    echo "║  Agent 已连接到 Relay!                             ║"
    echo "║                                                    ║"
    echo "║  在操作端执行验证:                                 ║"
    echo "║  reins --relay http://<relay-ip>:7780 --token $TOKEN agent list"
    echo "╚════════════════════════════════════════════════════╝"

# ================================================================
# 操作端(CLI)模式
# ================================================================
elif [ "$MODE" = "cli" ]; then
    echo "💻 reins CLI 安装"
    build_or_download
    add_path
    ok "CLI 就绪"
    echo ""
    echo "用法:"
    echo "  reins --relay http://<relay-ip>:7780 --token <token> agent list"
    echo "  reins --relay http://<relay-ip>:7780 --token <token> task run -a <agent> -c \"hostname\""
else
    echo "用法: quickstart.sh [server|agent|cli]"
    exit 1
fi
