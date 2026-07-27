#!/bin/bash
# ╔══════════════════════════════════════════════════════════╗
# ║  go-rmm 部署到 203.0.113.99(一键脚本)                   ║
# ╚══════════════════════════════════════════════════════════╝
#
# 用法(在本地开发机执行):
#   ./scripts/deploy-to-server.sh
#   ./scripts/deploy-to-server.sh --ssh-root root@203.0.113.99
#
# 前提:
#   - 本机有 Go 编译器
#   - 能 SSH 到 203.0.113.99
#   - 服务器有 Nginx + SSL 证书
set -euo pipefail

SERVER="${SERVER:-203.0.113.99}"
SSH_USER="${SSH_USER:-root}"
SSH_ADDR="${SSH_ADDR:-$SSH_USER@$SERVER}"
REMOTE_DIR="/opt/go-rmm"
VERSION="${VERSION:-v3.9.0}"
RELAY_ADDR="127.0.0.1:7780"

G='\033[0;32m'; Y='\033[1;33m'; R='\033[0;31m'; N='\033[0m'
ok()   { echo -e "${G}✅${N} $*"; }
warn() { echo -e "${Y}⚠️${N} $*"; }
die()  { echo -e "${R}✗${N} $*"; exit 1; }

echo "🚀 部署 go-rmm $VERSION 到 $SERVER"
echo ""

# === Step 1: 交叉编译 Linux 二进制 ===
ok "编译 Linux amd64 二进制..."
cd "$(dirname "$0")/.."

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /tmp/go-rmm-relay-linux ./relay/

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /tmp/go-rmm-linux ./cmd/go-rmm/

ok "编译完成"

# === Step 2: 上传到服务器 ===
ok "上传到 $SSH_ADDR:$REMOTE_DIR..."
ssh "$SSH_ADDR" "mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/data"

scp /tmp/go-rmm-relay-linux "$SSH_ADDR:$REMOTE_DIR/bin/go-rmm-relay"
scp /tmp/go-rmm-linux "$SSH_ADDR:$REMOTE_DIR/bin/go-rmm"
scp nginx-rmm.conf "$SSH_ADDR:/tmp/nginx-rmm.conf"
scp deploy/go-rmm-relay.service "$SSH_ADDR:/tmp/go-rmm-relay.service"

ok "上传完成"

# === Step 3: 安装 systemd 服务 ===
ok "安装 systemd 服务..."
ssh "$SSH_ADDR" << 'EOF'
chmod +x /opt/go-rmm/bin/go-rmm-relay /opt/go-rmm/bin/go-rmm

# 安装 systemd 服务(如果不存在)
if ! systemctl is-active go-rmm-relay &>/dev/null; then
    cp /tmp/go-rmm-relay.service /etc/systemd/system/
    systemctl daemon-reload
    systemctl enable go-rmm-relay
fi

# 重启(用新二进制)
systemctl restart go-rmm-relay
sleep 2

# 验证
if systemctl is-active go-rmm-relay; then
    echo "✅ go-rmm-relay 服务运行中"
else
    echo "⚠️ 服务未运行,检查: journalctl -u go-rmm-relay -f"
fi

# 获取或生成 token
TOKEN_FILE="/opt/go-rmm/data/.token"
if [ -f "$TOKEN_FILE" ]; then
    TOKEN=$(cat "$TOKEN_FILE")
else
    TOKEN=$(cat /dev/urandom | tr -dc 'a-f0-9' | head -c 32)
    echo "$TOKEN" > "$TOKEN_FILE"
    chmod 600 "$TOKEN_FILE"
fi

echo ""
echo "Token: $TOKEN"
echo ""
echo "Nginx 配置已上传到 /tmp/nginx-rmm.conf"
echo "请手动添加到 Nginx server 块(443 SSL):"
echo "  cat /tmp/nginx-rmm.conf"
echo "  # 把 location /rmm/ { ... } 部分加到你的 server { ... } 里"
echo "  nginx -t && nginx -s reload"
EOF

ok "部署完成"
echo ""
echo "╔════════════════════════════════════════════════════╗"
echo "║  go-rmm 已部署到 $SERVER           "
echo "╠════════════════════════════════════════════════════╣"
echo "║                                                    ║"
echo "║  下一步:配置 Nginx(在服务器上执行)               ║"
echo "║                                                    ║"
echo "║  1. 查看配置: cat /tmp/nginx-rmm.conf             ║"
echo "║  2. 编辑 Nginx: vi /etc/nginx/conf.d/default.conf  ║"
echo "║     把 location /rmm/ { ... } 加到 server { } 里  ║"
echo "║  3. 测试+重载: nginx -t && nginx -s reload        ║"
echo "║                                                    ║"
echo "║  验证:                                             ║"
echo "║  curl -k https://$SERVER/rmm/healthz        "
echo "║                                                    ║"
echo "║  Agent 连接(在被控端):                            ║"
echo "║  go-rmm-agent run -relay wss://$SERVER/rmm/agent   ║"
echo "║                                                    ║"
echo "║  CLI 操作:                                         ║"
echo "║  go-rmm --relay https://$SERVER/rmm agent list     ║"
echo "║                                                    ║"
echo "╚════════════════════════════════════════════════════╝"
