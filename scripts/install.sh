#!/bin/bash
# ╔══════════════════════════════════════════════════════════╗
# ║  go-rmm Linux 安装器（从 GitHub Release 下载二进制）       ║
# ╚══════════════════════════════════════════════════════════╝
#
# 用法:
#   ./scripts/install.sh                          # 安装最新版 Relay 到 /usr/local/bin
#   ./scripts/install.sh --version v4.4.0         # 指定版本
#   ./scripts/install.sh --component agent        # 安装 Agent（agent|relay|cli）
#   ./scripts/install.sh --service                # 同时注册 systemd 服务（relay，需在源码仓库内）
#
# 环境变量覆盖: OWNER / REPO / INSTALL_DIR
set -euo pipefail

OWNER="${OWNER:-V-T-V}"            # GitHub 仓库 owner（与 git remote 一致）
REPO="${REPO:-go-rmm}"
VERSION="${VERSION:-latest}"
COMPONENT="${COMPONENT:-relay}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
REGISTER_SERVICE=false

while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="$2"; shift 2;;
    --component) COMPONENT="$2"; shift 2;;
    --service)   REGISTER_SERVICE=true; shift;;
    -h|--help)   sed -n '2,12p' "$0"; exit 0;;
    *) echo "未知参数: $1（用 -h 查看帮助）"; exit 1;;
  esac
done

case "$COMPONENT" in
  relay|agent|cli) ;;
  *) echo "✗ --component 仅支持 relay|agent|cli"; exit 1;;
esac

OS=$(uname -s | tr A-Z a-z)
[ "$OS" = "linux" ] || { echo "✗ install.sh 仅支持 Linux；Windows 请用 install.ps1，macOS 可手动下载"; exit 1; }
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)         ARCH=amd64;;
  aarch64|arm64)  ARCH=arm64;;
  *) echo "✗ 暂不支持架构 $ARCH"; exit 1;;
esac

# latest → 实际版本号
if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$OWNER/$REPO/releases/latest" \
            | grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
  [ -z "$VERSION" ] && { echo "✗ 无法解析最新版本（检查网络或指定 --version）"; exit 1; }
fi

BINARY="go-rmm-$COMPONENT"
ARCHIVE="go-rmm-${COMPONENT}_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$OWNER/$REPO/releases/download/$VERSION/$ARCHIVE"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "↓ 下载 $URL"
curl -fSL "$URL" -o "$TMP/$ARCHIVE"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP"

if ! [ -f "$TMP/$BINARY" ]; then
  echo "✗ 解压后未找到 $BINARY（archive 结构可能已变）"; ls -la "$TMP"; exit 1
fi

install_bin() {
  if [ -w "$INSTALL_DIR" ] || [ "$(id -u)" = "0" ]; then
    install -m 0755 "$1" "$2"
  else
    echo "⚠️  $INSTALL_DIR 不可写，改用 sudo"
    sudo install -m 0755 "$1" "$2"
  fi
}
install_bin "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
echo "✅ 已安装 $INSTALL_DIR/$BINARY ($VERSION)"

# 可选：注册 systemd 服务（relay，需在源码仓库内读取 unit 文件）
if [ "$REGISTER_SERVICE" = true ] && [ "$COMPONENT" = "relay" ]; then
  UNIT="deploy/go-rmm-relay.service"
  if [ -f "$UNIT" ]; then
    sudo install -m 0644 "$UNIT" /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now go-rmm-relay
    echo "✅ systemd 服务 go-rmm-relay 已启用"
  else
    echo "⚠️  未找到 $UNIT（--service 需在源码仓库内运行）"
  fi
fi

echo ""
echo "下一步:"
echo "  $BINARY -h"
