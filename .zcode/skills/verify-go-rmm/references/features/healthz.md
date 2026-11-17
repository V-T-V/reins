# /healthz 深浅双形状

深度健康（relay/store/agents/uptime）只对有效 Bearer 可见；无/错鉴权退化为浅检查 200 {"status":"ok"}——不泄漏组件信息（relay P1-S7 设计）。

## Sub-features

- 无鉴权：200 + 仅 status=ok（无 agents/uptime_s 字段）
- 有效 Bearer：200 + relay=ok + agents=N + uptime_s

## How to get to it (user POV)

运维对 relay 探活（load balancer 用浅形状）；带 token 拿深度诊断。

## Driving it with verify-go-rmm

```bash
bash .zcode/skills/verify-go-rmm/verify.sh probe healthz
```

## Gotchas

- 浅形状**变深**（无鉴权也回 agents/uptime）= 信息泄漏面扩大——FAIL 而不是「信息变多更好」。
- 浅形状不是 401——这是刻意设计（健康探针不该被鉴权挡死）；reins-mcp 真连通冒烟 B3 同款断言。
