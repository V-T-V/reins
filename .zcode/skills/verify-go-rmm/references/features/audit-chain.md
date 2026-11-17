# 审计 hash chain 衔接

任务提交与结果双事件落账；每条 entry 带 prev_hash/this_hash 串联成防篡改链。本探针核对相邻两事件的衔接逐位成立。

## Sub-features

- GET /api/audit/query?limit=10（Bearer）：newest-first entries
- 本任务 task_submitted + task_result 双事件在场（task_id 绑定唯一 marker）
- **衔接核对**：第 1 条（task_result，最新）的 prev_hash == 第 2 条（task_submitted）的 this_hash——链不断不篡改

## How to get to it (user POV)

Web 审计日志 Tab 行展开（prev_hash/this_hash 可见）+ SIEM 导出；`reins` CLI 审计查询。

## Driving it with verify-go-rmm

```bash
bash .zcode/skills/verify-go-rmm/verify.sh probe task-run    # task-run 后自动跑
bash .zcode/skills/verify-go-rmm/verify.sh probe audit-chain # 单独跑（用 runs/last-marker）
```

## Gotchas

- newest-first 假设：若 relay 改排序，衔接断言会红——先核对排序再判篡改。
- 第 2 条 this_hash 的提取用「第 2 个 this_hash 字段」（grep 序提取）；entry 间插入了其他事件时用 event 过滤参数收窄。
- hash chain 校验的完整 API（整链重算）在 reins-mcp 冒烟有覆盖；本探针锚「相邻衔接」这个最小不变量。
