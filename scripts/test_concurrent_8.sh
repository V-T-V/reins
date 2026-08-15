#!/bin/bash
# 8 个并发 task 打同一 agent（测 WorkerPool=4 限流 + 排队）
cd "$(dirname "$0")/.."   # 项目根(相对脚本定位,避免绝对路径随归档搬迁失效)
# relay 地址/凭据从环境变量读取(真实值不入库;轮换 token 后配 RMM_RELAY/RMM_TOKEN 即可)
RELAY="${RMM_RELAY:-https://your-relay-host/rmm}"
TOKEN="${RMM_TOKEN:-your-relay-token}"
R="./go-rmm.exe --relay $RELAY --token $TOKEN"
AGENT="agent-DESKTOP-UAKKLED-76824dff"

echo "=== 8 并发 task 打同一 agent (WorkerPool=4) ==="
for i in $(seq 1 8); do
  $R exec -a $AGENT -c "powershell -NoProfile -Command \"Start-Sleep -Milliseconds 500; Write-Output CONCURRENT-$i\"" > /tmp/cc$i.txt 2>&1 &
done
wait
echo "--- 结果 ---"
ok=0; fail=0
for i in $(seq 1 8); do
  line=$(grep -o "CONCURRENT-$i" /tmp/cc$i.txt 2>/dev/null)
  if [ -n "$line" ]; then ok=$((ok+1)); else fail=$((fail+1)); fi
  echo "  [$i] $(tail -1 /tmp/cc$i.txt)"
done
echo "=== 成功 $ok / 失败 $fail ==="
