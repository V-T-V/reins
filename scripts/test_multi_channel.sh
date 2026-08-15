#!/bin/bash
# 多通道并发: task run + exec + 文件上传 + SSE 订阅
cd "$(dirname "$0")/.."   # 项目根(相对脚本定位,避免绝对路径随归档搬迁失效)
# relay 地址/凭据从环境变量读取(真实值不入库;轮换 token 后配 RMM_RELAY/RMM_TOKEN 即可)
RELAY="${RMM_RELAY:-https://your-relay-host/rmm}"
TOKEN="${RMM_TOKEN:-your-relay-token}"
R="./go-rmm.exe --relay $RELAY --token $TOKEN"
SLAVE="${RMM_SLAVE_AGENT:-agent-slave}"
MASTER="${RMM_MASTER_AGENT:-agent-master}"

echo "=== 多通道并发 ==="
# 1. task run（task 通道）打 slave
$R task run -a $SLAVE -c "echo TASK-RUN-CH" > /tmp/mc1.txt 2>&1 &
# 2. exec（exec 通道）打 slave
$R exec -a $SLAVE -c "echo EXEC-CH" > /tmp/mc2.txt 2>&1 &
# 3. task run 打 master
$R task run -a $MASTER -c "echo TASK-MASTER" > /tmp/mc3.txt 2>&1 &
# 4. 文件上传到 slave（小文件测并发）
echo "test-content-concurrent" > /tmp/concurrent_upload.txt
$R file upload -a $SLAVE /tmp/concurrent_upload.txt concurrent_upload_$$.txt > /tmp/mc4.txt 2>&1 &
wait
echo "--- 结果 ---"
echo "[mc1 task-run slave]"; tail -5 /tmp/mc1.txt
echo "[mc2 exec slave]"; tail -4 /tmp/mc2.txt
echo "[mc3 task-run master]"; tail -5 /tmp/mc3.txt
echo "[mc4 upload slave]"; tail -4 /tmp/mc4.txt
echo "=== 完成 ==="
