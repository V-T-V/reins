#!/bin/bash
# 多客户端并发测试
cd "$(dirname "$0")/.."   # 项目根(相对脚本定位,避免绝对路径随归档搬迁失效)
# relay 地址/凭据从环境变量读取(真实值不入库;轮换 token 后配 RMM_RELAY/RMM_TOKEN 即可)
RELAY="${RMM_RELAY:-https://your-relay-host/rmm}"
TOKEN="${RMM_TOKEN:-your-relay-token}"
R="./go-rmm.exe --relay $RELAY --token $TOKEN"
SLAVE="${RMM_SLAVE_AGENT:-agent-slave}"
MASTER="${RMM_MASTER_AGENT:-agent-master}"

echo "=== 并发 task 测试: 4 个同时 ==="
$R exec -a $SLAVE -c "hostname" > /tmp/c1.txt 2>&1 &
$R exec -a $SLAVE -c "echo TASK-B-from-CLI2" > /tmp/c2.txt 2>&1 &
$R exec -a $MASTER -c "echo TASK-C-from-CLI3" > /tmp/c3.txt 2>&1 &
$R exec -a $MASTER -c "echo TASK-D-from-CLI4" > /tmp/c4.txt 2>&1 &
wait
echo "--- 结果 ---"
echo "[CLI1→slave hostname]"; tail -4 /tmp/c1.txt
echo "[CLI2→slave TASK-B]"; tail -4 /tmp/c2.txt
echo "[CLI3→master TASK-C]"; tail -4 /tmp/c3.txt
echo "[CLI4→master TASK-D]"; tail -4 /tmp/c4.txt
