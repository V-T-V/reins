#!/bin/bash
# 混合并发: 2个task + 2个shell + 2个截图 同时打同一 agent
cd "$(dirname "$0")/.."   # 项目根(相对脚本定位,避免绝对路径随归档搬迁失效)
# relay 地址/凭据从环境变量读取(真实值不入库;轮换 token 后配 RMM_RELAY/RMM_TOKEN 即可)
RELAY="${RMM_RELAY:-https://your-relay-host/rmm}"
TOKEN="${RMM_TOKEN:-your-relay-token}"
R="./go-rmm.exe --relay $RELAY --token $TOKEN"
AGENT="agent-DESKTOP-UAKKLED-76824dff"
T=$(date +%s)

echo "=== 混合并发: 2task + 2shell + 2screenshot ==="
# 2 个普通 task
$R exec -a $AGENT -c "echo MIX-TASK-1" > /tmp/mix1.txt 2>&1 &
$R exec -a $AGENT -c "echo MIX-TASK-2" > /tmp/mix2.txt 2>&1 &
# 2 个 shell open（短命令）
$R exec -a $AGENT -c "powershell -NoProfile -Command \"Write-Output MIX-SHELL-1\"" > /tmp/mix3.txt 2>&1 &
$R exec -a $AGENT -c "powershell -NoProfile -Command \"Write-Output MIX-SHELL-2\"" > /tmp/mix4.txt 2>&1 &
# 2 个截图（轻量）
$R screenshot -a $AGENT > /tmp/mix5.txt 2>&1 &
$R screenshot -a $AGENT > /tmp/mix6.txt 2>&1 &
wait
echo "--- 结果 ---"
for i in 1 2 3 4 5 6; do
  status=$(grep -oE "✅|❌" /tmp/mix$i.txt | head -1)
  detail=$(tail -1 /tmp/mix$i.txt | head -c 80)
  echo "  [mix$i] $status $detail"
done
echo "=== 完成 ==="
