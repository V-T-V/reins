# 鉴权契约与 Agent 清单

Bearer token 鉴权链：错 token 401 结构化错误体；对 token 拿在线 Agent 清单（反向 WS 注册的可见面）。

## Sub-features

- 错 token：401 + `{"error":"无效的鉴权 token"}`（逐字段——与 reins-mcp 离线剧场/真连通冒烟同形）
- 对 token：GET /api/agents 200，`agents[].agent_id=verify-agent-01`（含 capabilities/os/tags）+ count=1

## How to get to it (user POV)

CLI `reins agent list` / Web 侧栏都走这条 API。

## Driving it with verify-go-rmm

```bash
bash .zcode/skills/verify-go-rmm/verify.sh probe agents-auth
```

## Gotchas

- 401 变 2xx=鉴权被绕过（最高优先级红）。
- count=1 依赖全新 bin 目录（每实例 dataDir 隔离）——若复用旧 bin 目录，历史 agent 若在 offline 列表会污染计数（e2e 坑位：断言不锚计数；本技能靠隔离锚住了 count=1）。
