# awesome-mcp-servers 收录提交（MCP 落地后）

> 入口：https://github.com/punkpeye/awesome-mcp-servers（star 最高的 MCP 清单）PR 或 issue
> 竞争情报：RMM/远程运维品类目前在列为零——独占窗口。

## PR 描述

Adds [reins-mcp](https://github.com/V-T-V/reins) — MCP server for self-hosted remote operations.

- Exposes 14 curated tools (run_command, list_agents, task execution with 9 executor types, batch dispatch by tag, sysinfo, power, cron) over stdio, pure Go stdlib, zero external deps
- Backed by [Reins](https://github.com/V-T-V/reins), a self-hosted zero-trust remote operations platform: agents connect via outbound-only reverse WebSockets (zero inbound ports), mTLS + per-device certs + CRL, multi-tenant RBAC, tamper-evident hash-chained audit log
- Includes an execution-layer rules gate: managed projects drop a `.reins/rules.md` (e.g. "never restart during business hours") and mutation tasks from *any* client — CLI, SDK, or MCP — are rejected until the rules are read and acked (content fingerprint; edits invalidate stale acks)
- Binaries for 6 platforms in every release, SBOM + SHA256 manifests included

## 条目（按清单格式，Remote Operations / DevOps 分类）

```markdown
- [reins-mcp](https://github.com/V-T-V/reins) - 🎛️ MCP server for self-hosted remote operations: run commands, orchestrate tasks, and manage a fleet over zero-trust reverse-WebSocket agents — with a rules gate your AI agent must pass before touching production.
```

## 提交时机

发布后第二波（Show HN MCP 帖同日），与 awesome-selfhosted PR 错开 1-2 天避免同一时段双 PR。
