# Security Policy

## Scope

This policy covers the reins repository (relay, agent, CLI, SDK, protocol, web console). It applies to all released binaries and the default branch.

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

- Email: 1207565967@qq.com *(与 git 提交作者邮箱一致;如需专用安全邮箱请替换)*
- Include: affected version/tag, reproduction steps (PoC), impact assessment, and any relevant logs.
- Please withhold public disclosure until the fix is released.

**Response timeline:**

| Step | Target |
|---|---|
| Acknowledgment | within 72 hours |
| Triage & severity assessment | within 7 days |
| Fix release (critical) | within 14 days if feasible |
| Fix release (others) | next monthly release |
| Public advisory | with the release, crediting the reporter unless anonymous reporting is requested |

## Supported versions

Only the latest tagged release receives security fixes.

## Verification of release artifacts

Every release publishes a `SHA256SUMS` file. Verify before deploying:

```powershell
# Windows
Get-FileHash .\reins-agent-windows-amd64.exe -Algorithm SHA256
```

```bash
# Linux/macOS
sha256sum -c SHA256SUMS
```

Binaries are built by GitHub Actions from the tagged commit (`.github/workflows/release.yml`); build provenance is recorded by the workflow run. Prefer building from source if your threat model requires it — the project is pure Go with no C dependencies.

## Security architecture summary

- Agents connect **outbound only** (reverse WebSocket); no inbound ports are required on managed hosts.
- Optional **mTLS** with a built-in CA; per-device client certificates; CRL-based revocation (`POST /api/agents/revoke`).
- Command policy enforced on both Relay and Agent (including offline-cached tasks — a blacklisted command targeted at an offline agent is rejected at submission with 403, never cached for later delivery).
- Multi-tenant RBAC tokens (`tenant:role:secret`) with organization enable/quota enforcement (fail-closed).
- Append-only **hash-chained audit log** with SIEM export (CEF/Syslog/JSON) and session recording (asciinema v2).
- Secrets are reference-based with TTL and one-time semantics; secret values are never written to the audit log.

Known limits (documented, by design): the default command blacklist is best-effort — strict environments should enable allow-only mode (`-policy-allow` on the relay, `policy_allow` in agent config). See [README.zh-CN.md](./README.zh-CN.md#安全与负责任使用).

## Responsible use

reins is intended for managing machines you own or are contractually authorized to manage. Building stealth, anti-detection, or anonymous-access functionality into this project will not be accepted.
