# awesome-selfhosted 收录 PR（发布后第一周提交）

> 数据仓库：https://github.com/awesome-selfhosted/awesome-selfhosted-data（条目数据在此仓）
> 展示仓库：awesome-selfhosted/awesome-selfhosted（PR 前先读其 CONTRIBUTING.md）
> 硬要求自检：✅ MIT（自由许可证）✅ 活跃维护（月度发版）✅ 一行非推销描述 ✅ 语法过 awesome-lint

## 目标分类

`Remote Access`（当前 14 个条目中完整 RMM 形态只有 MeshCentral——空位明显）

## PR 标题

Add reins to remote access

## PR 正文

Adds [reins](https://github.com/V-T-V/reins) — self-hosted remote operations platform (Go, MIT).

- Self-hosted relay (single binary) + agents with outbound-only reverse WebSocket connections (zero inbound ports on managed hosts)
- mTLS with built-in CA + CRL revocation, multi-tenant RBAC, tamper-evident hash-chained audit log with SIEM export
- Fleet operations: batch dispatch with retries, DAG orchestration, deploy playbooks, file transfer (chunked/resumable), port forwarding, monitoring
- Windows agent first-class, Linux agent ~85%; releases ship SBOMs and SHA256 manifests

License: MIT. Actively maintained (monthly releases, race-enabled CI, 559 tests).

## 条目内容（awesome-selfhosted-data 的 markdown 行）

```markdown
- [Reins](https://github.com/V-T-V/reins) - Self-hosted remote operations platform with reverse-WebSocket agents (zero inbound ports), mTLS, RBAC, tamper-evident audit log and fleet orchestration. `MIT` `Go`
```

## 后续同类清单（依次提交）

| 清表 | 备注 |
|---|---|
| awesome-sysadmin | 同条目微调英文描述 |
| awesome-go | **须等项目满 5 个月历史（2026-12-22 后）**，CONTRIBUTING 硬规定 |
| awesome-mcp-servers | 等 MCP 适配层落地后提交 |
