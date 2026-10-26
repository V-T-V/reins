# Show HN（英文脉冲 · 第 4-6 周主战役）

> 发帖时间：周二-周四 美东 8:00-11:00（北京当晚 20:00-23:00）。发帖后第一小时守帖秒回。
> 绝不找朋友集中点赞（vote manipulation 会被 kill）。首战失利等 1-2 周带 MCP 钩子再战。

## 主帖

**标题**：Show HN: Reins – Self-hosted zero-trust remote operations platform

## 作者首评（发帖后立即抢首楼）

Hi HN, I'm the author.

Reins is a self-hosted remote operations platform I built after getting priced out of Teleport's 2024 license change and tired of MeshCentral agents getting flagged by antivirus (scammers abused it, now every AV distrusts it).

The core idea: agents make **outbound-only reverse WebSocket connections** to your relay. Your fleet can be fully unreachable from the internet and you can still run ops on it. That's the most literal implementation of "eliminate implicit network trust" from NIST 800-207 I could build.

What's in the box:
- One static Go binary for the relay (bbolt embedded storage, zero external deps; Redis optional for horizontal scale)
- One-line Windows agent install (`iex (irm .../install.ps1)`), Linux agent at ~85%
- 50+ CLI commands: diagnostics, exec, file transfer (chunked, resumable, per-chunk SHA256, LAN-direct fast path), port forwarding, deploy playbooks, DAG orchestration, monitoring
- Security primitives: mTLS with built-in CA, per-device certs + CRL revocation, multi-tenant RBAC, tamper-evident hash-chained audit log with CEF/Syslog export, session recording, dual-sided command black/allowlists

The part I'm most interested in feedback on: a rules gate for AI agents. Managed projects can drop a `.reins/rules.md` in their repo root ("never kill the kiosk shell during business hours", "exclude node_modules from deploys"). Automation must `reins rules read` and carry a content-fingerprint ack before mutation tasks are accepted; edit the rules and stale acks are rejected instantly. The rejection never echoes the fingerprint, so replaying an ack without reading doesn't work. As we let LLM agents touch production, I want the guardrail at the execution layer, not in the prompt.

Honest limitations: Windows agent is first-class, Linux is script/monitor + dialect-aware CLI (~85%, real-machine validation ongoing); single relay is the default topology (multi-relay failover + Redis federation exist and are tested but not yet production-hardened); the default command blacklist is best-effort by design — allow-only mode for strict environments.

MIT, 48K lines of Go, 559 tests (race-enabled CI), every release ships SBOMs + SHA256 manifests. Docs: README.zh-CN.md is the deep one; English README covers the essentials. Antivirus false positives are a category-wide phenomenon (they hit MeshCentral/TacticalRMM/RustDesk too) — docs/antivirus.md explains how we handle it.

Roadmap: an MCP server adapter so Claude/Cursor-style agents can drive Reins natively — with the same rules gate in front.

Happy to answer anything about the protocol design, the priority write engine (P0 control frames never starve behind P1 data), or why DAG orchestration was worth building when batch dispatch already existed.

## 评论区弹药（常见问题的回答要点）

- **"How is this different from Teleport?"** — Teleport is an access plane (get me a shell on infra, SSO, certs); Reins is an operations plane (run commands/fleet orchestration/monitoring on machines you own, agents behind NAT). Also: MIT vs source-available with a 100-employee cap.
- **"Why not MeshCentral?"** — MeshCentral is great but it's remote-access-first, not a full RMM, and its agent downloads are now AV-flagged because of abuse. We start with a clean reputation and publish SBOM + hash manifests from day one.
- **"RAT concerns?"** — Outbound-only agents, per-device certs you can revoke, tamper-evident audit, and no stealth/anti-detection features ever (explicitly rejected in SECURITY.md). CISA AA23-025A asks orgs to inventory approved RMM — self-hosted + audit chain is exactly that story.
- **"Scaling?"** — TaskStore interface with bbolt default and Redis backend, pub/sub event federation, agent-side multi-relay failover (tested E2E), production hardening is the current P0.
