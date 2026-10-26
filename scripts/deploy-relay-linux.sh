#!/bin/bash
# reins 一键部署脚本 — Linux Relay 端
#
# 用法:
#   # 方式1: 直接下载运行
#   curl -fsSL https://raw.githubusercontent.com/QiuShichang/reins/main/scripts/deploy-relay-linux.sh | bash
#
#   # 方式2: 本地运行
#   chmod +x scripts/deploy-relay-linux.sh
#   ./scripts/deploy-relay-linux.sh
#
#   # 方式3: 带参数
#   ADDR=:7780 TOKEN=my-secret ./scripts/deploy-relay-linux.sh
#
# 功能:
#   1. 检测架构(amd64/arm64)
#   2. 下载/编译 reins-relay
#   3. 安装为 systemd 服务(开机自启 + 崩溃恢复)
#   4. 配置防火墙
#   5. 验证运行

set -euo pipefail

# === 配置 ===
VERSION="${VERSION:-v3.9.0}"
ADDR="${ADDR:-:7780}"
TOKEN="${TOKEN:-}"
INSTALL_DIR="/opt/reins"
DATA_DIR="${INSTALL_DIR}/data"
SERVICE_NAME="reins-relay"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# === Step 1: 检测环境 ===
info "检测系统环境..."
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    *)       error "不支持的架构: $ARCH" ;;
esac
info "架构: $ARCH → GOARCH=$GOARCH"

# 检查 root
if [ "$EUID" -ne 0 ]; then
    warn "非 root 用户,某些操作(如 systemd 安装)可能需要 sudo"
fi

# === Step 2: 编译或下载 ===
mkdir -p "$INSTALL_DIR" "$DATA_DIR"
cd "$INSTALL_DIR"

if command -v go &>/dev/null; then
    info "检测到 Go,从源码编译..."
    if [ -f "go.mod" ]; then
        # 已有源码
        info "当前目录有源码,直接编译"
    else
        # 克隆源码
        info "克隆 reins 源码..."
        git clone --depth 1 https://github.com/V-T-V/reins.git /tmp/reins-src || true
        if [ -d "/tmp/reins-src" ]; then
            cp -r /tmp/reins-src/* .
        fi
    fi
    CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH \
        go build -ldflags "-s -w -X main.version=$VERSION" -o reins-relay ./relay/
    info "编译完成"
else
    info "无 Go 环境,下载预编译二进制..."
    # 尝试从 GitHub Release 下载
    DOWNLOAD_URL="https://github.com/V-T-V/reins/releases/download/${VERSION}/reins-relay-linux-${GOARCH}"
    if curl -fsSL "$DOWNLOAD_URL" -o reins-relay; then
        chmod +x reins-relay
        info "下载完成"
    else
        error "下载失败($DOWNLOAD_URL),请安装 Go 或手动上传二进制到 $INSTALL_DIR/reins-relay"
    fi
fi

# === Step 3: 生成配置 ===
info "生成配置..."
CONFIG_ARGS="-addr $ADDR"
if [ -n "$TOKEN" ]; then
    CONFIG_ARGS="$CONFIG_ARGS -token $TOKEN"
    info "鉴权 token 已配置"
else
    warn "未配置 TOKEN,API 无鉴权(仅内网/测试环境安全)"
fi

# === Step 4: 安装 systemd 服务 ===
if [ "$EUID" -eq 0 ] || command -v sudo &>/dev/null; then
    info "安装 systemd 服务..."
    cat > /tmp/${SERVICE_NAME}.service << EOF
[Unit]
Description=reins Relay Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/reins-relay ${CONFIG_ARGS}
WorkingDirectory=${INSTALL_DIR}
Restart=always
RestartSec=3
LimitNOFILE=65536

# 安全加固
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${DATA_DIR}
PrivateTmp=true

StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

    SUDO="${SUDO:-sudo}"
    $SUDO cp /tmp/${SERVICE_NAME}.service /etc/systemd/system/
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable ${SERVICE_NAME}
    $SUDO systemctl restart ${SERVICE_NAME}
    info "systemd 服务已安装并启动"
else
    warn "无 root 权限,跳过 systemd 安装"
    warn "手动运行: cd $INSTALL_DIR && ./reins-relay $CONFIG_ARGS"
fi

# === Step 5: 防火墙 ===
if command -v ufw &>/dev/null && [ "$EUID" -eq 0 ]; then
    PORT=$(echo "$ADDR" | grep -oE '[0-9]+$')
    info "配置 UFW 防火墙(端口 $PORT)..."
    ufw allow $PORT/tcp 2>/dev/null || true
fi

# === Step 6: 验证 ===
sleep 2
if curl -sf "http://127.0.0.1:${ADDR#:}/healthz" | grep -q "ok"; then
    info "✅ reins Relay 部署成功!"
    echo ""
    echo "  地址: http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost')${ADDR#:}"
    echo "  健康: http://127.0.0.1${ADDR#:}/healthz"
    echo "  指标: http://127.0.0.1${ADDR#:}/metrics"
    echo "  日志: journalctl -u ${SERVICE_NAME} -f"
    echo ""
    echo "  下一步: 在 Windows 机器上安装 Agent:"
    echo "    reins-agent.exe install -relay ws://<本机IP>${ADDR#:}/agent"
else
    warn "健康检查未通过,检查日志: journalctl -u ${SERVICE_NAME} -f"
fi
