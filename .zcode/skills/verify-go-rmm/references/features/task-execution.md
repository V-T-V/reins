# powershell 真实任务执行回路

G2 主链路：POST /api/tasks → relay WS 转发 → agent 真实执行 PowerShell → 结果落 store → GET /api/tasks/{id} 轮询到 completed。

## Sub-features

- 下发：`{"task_id","type":"powershell","agent_id","payload":{"command":"Write-Output '<marker>'"},"timeout":60}` → 200/202 dispatched
- 终态轮询：GET /api/tasks/{id} → `status=completed` + `result.success=true` + `result.stdout` 含唯一 marker
- marker 唯一（时间戳）——dataDir 若被复用累积历史任务也不会误判

## How to get to it (user POV)

`reins task run -t powershell -c "..."` 或 Web 控制台任务执行 Tab。

## Driving it with verify-go-rmm

```bash
bash .zcode/skills/verify-go-rmm/verify.sh probe task-run   # 自动含 audit-chain
```

## Gotchas

- PowerShell 首跑有 CLIXML 进度噪音进 stderr——**断言只锚 stdout 的 marker**，别锚 stderr 干净。
- stdout 是 `\r\n` 结尾（Windows）——用 grep 子串匹配不做整行相等。
- 轮询上限 45s：powershell 冷启动偶尔 >20s；超时先看 agent.log 再下结论。
