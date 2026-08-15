#!/bin/bash
# SSE 多订阅者测试: 2 个 CLI 订阅 events，同时第 3 个 CLI 触发 task
cd "$(dirname "$0")/.."   # 项目根(相对脚本定位,避免绝对路径随归档搬迁失效)
# relay 地址/凭据从环境变量读取(真实值不入库;轮换 token 后配 RMM_RELAY/RMM_TOKEN 即可)
RELAY="${RMM_RELAY:-https://your-relay-host/rmm}"
TOKEN="${RMM_TOKEN:-your-relay-token}"
R="./go-rmm.exe --relay $RELAY --token $TOKEN"
SLAVE="${RMM_SLAVE_AGENT:-agent-slave}"

echo "=== SSE 多订阅 + 触发 ==="
# 启动 2 个 SSE 订阅（后台，各跑 8 秒）
timeout 8 $R events > /tmp/sse1.txt 2>&1 &
SSE1=$!
timeout 8 $R events > /tmp/sse2.txt 2>&1 &
SSE2=$!
sleep 2  # 等订阅建立
# 第 3 个 CLI 触发 task（产生 task_dispatched + task_result 事件）
$R exec -a $SLAVE -c "echo SSE-TRIGGER" > /tmp/sse_trigger.txt 2>&1
sleep 3  # 等事件传播
wait $SSE1 $SSE2 2>/dev/null
echo "--- SSE 订阅者 1 收到事件数 ---"
grep -c "event" /tmp/sse1.txt 2>/dev/null || echo 0
echo "--- SSE 订阅者 2 收到事件数 ---"
grep -c "event" /tmp/sse2.txt 2>/dev/null || echo 0
echo "--- 触发结果 ---"
tail -3 /tmp/sse_trigger.txt
echo "--- sse1 样本 ---"
head -5 /tmp/sse1.txt
echo "=== 完成 ==="
