#!/bin/bash
# reins Docker 一键部署脚本
#
# 用法:
#   ./scripts/deploy-docker.sh
#   TOKEN=my-secret ./scripts/deploy-docker.sh
#   TOKEN=my-secret PORT=8080 ./scripts/deploy-docker.sh

set -euo pipefail

TOKEN="${TOKEN:-}"
PORT="${PORT:-7780}"
VERSION="${VERSION:-v3.9.0}"
CONTAINER_NAME="reins-relay"

echo "🐳 reins Docker 部署"
echo "   版本: $VERSION"
echo "   端口: $PORT"
echo ""

# 构建镜像(如果不存在)
if ! docker images | grep -q "reins-relay"; then
    echo "[1/4] 构建 Docker 镜像..."
    docker build -f Dockerfile.relay -t reins-relay:$VERSION .
else
    echo "[1/4] 镜像已存在,跳过构建"
fi

# 停止旧容器
echo "[2/4] 清理旧容器..."
docker rm -f $CONTAINER_NAME 2>/dev/null || true

# 启动容器
echo "[3/4] 启动容器..."
DOCKER_ARGS="-d --name $CONTAINER_NAME -p $PORT:7780 -v reins-data:/data"

if [ -n "$TOKEN" ]; then
    DOCKER_ARGS="$DOCKER_ARGS -e REINS_RELAY_TOKEN=$TOKEN"
    echo "   Token: ${TOKEN:0:4}***"
fi

docker run $DOCKER_ARGS reins-relay:$VERSION -addr :7780 $(if [ -n "$TOKEN" ]; then echo "-token $TOKEN"; fi)

# 等待启动
echo "[4/4] 等待启动..."
sleep 3

# 验证
if docker exec $CONTAINER_NAME wget -q -O- http://127.0.0.1:7780/healthz 2>/dev/null | grep -q "ok"; then
    echo ""
    echo "✅ reins Relay Docker 部署成功!"
    echo ""
    echo "  地址: http://localhost:$PORT"
    echo "  健康: curl http://localhost:$PORT/healthz"
    echo "  指标: curl http://localhost:$PORT/metrics"
    echo "  日志: docker logs -f $CONTAINER_NAME"
    echo "  数据: docker volume inspect reins-data"
    echo ""
    echo "  Agent 连接: ws://<本机IP>:$PORT/agent"
else
    echo "⚠️  健康检查未通过,查看日志: docker logs $CONTAINER_NAME"
fi
