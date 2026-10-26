#!/bin/bash
# reins Linux 安装器（从 GitHub Release 下载二进制）
#
# 用法:
#   ./scripts/install.sh                          # 安装最新版 Relay
#   ./scripts/install.sh --version v4.5.0         # 指定版本
#   ./scripts/install.sh --component agent        # relay|agent|cli
#
# 环境变量覆盖: OWNER / REPO / INSTALL_DIR
set -euo pipefail

OWNER="${OWNER:-V-T-V}"
REPO="${REPO:-reins}"
VERSION="${VERSION:-latest}"
COMPONENT="${COMPONENT:-relay}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="$2"; shift 2;;
    --component) COMPONENT="$2"; shift 2;;
    -h|--help)   sed -n '2,9p' "$0"; exit 0;;
    *) echo "未知参数: $1"; exit 1;;
  esac
done

OS=$(uname -s | tr A-Z a-z)
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)        ARCH=amd64;;
  aarch64|arm64) ARCH=arm64;;
  *) echo "暂不支持架构 $ARCH"; exit 1;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$OWNER/$REPO/releases/latest" \
            | grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
  [ -z "$VERSION" ] && { echo "无法解析最新版本"; exit 1; }
fi

BINARY="reins-$COMPONENT"
ARCHIVE="reins-${COMPONENT}_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$OWNER/$REPO/releases/download/$VERSION/$ARCHIVE"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "↓ 下载 $URL"
curl -fSL "$URL" -o "$TMP/$ARCHIVE"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP"

if ! [ -f "$TMP/$BINARY" ]; then
  echo "解压后未找到 $BINARY"; exit 1
fi

install -m 0755 "$TMP/$BINARY" "$INSTALL_DIR/$BINARY" 2>/dev/null || sudo install -m 0755 "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
echo "已安装 $INSTALL_DIR/$BINARY ($VERSION)"
