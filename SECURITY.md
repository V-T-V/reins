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
- **A credential always maps to one server-side identity.** The Relay compares only the secret; the tenant and role come from the Relay's own configuration entry, never from what the caller claims (see [Credentials](#credentials-and-identity)).
- **Object-level tenant isolation.** An agent belongs to the tenant whose credential registered it (recorded by the Relay, persisted). Agents, tasks, batches, DAGs, cron rules, scripts, secrets, audit records and the live event stream are all scoped to that ownership on read, write and subscribe paths.
- **Roles are enforced, not advisory.** `viewer` is read-only by default-deny; admin-only operations (revocation, power, self-update, secret management, enrollment tokens) cannot be reached through side endpoints.
- Command policy enforced on both Relay and Agent (including offline-queued tasks — a blacklisted command targeted at an offline agent is rejected at submission with 403, never queued for later delivery).
- Append-only **hash-chained audit log** with SIEM export (CEF/Syslog/JSON) and session recording (asciinema v2). The chain continues across Relay restarts.
- Secrets are reference-based with TTL and one-time semantics; secret values are never written to the audit log.

## Trust model

**One Relay is one trust domain.** Whoever operates the Relay (holds its configuration and data directory) is trusted for everything on it. Tenant isolation keeps *credentialed users* of a shared Relay apart from each other; it is not a hard boundary between mutually untrusting parties. To separate customers, departments with different clearance levels, or production from everything else, **deploy separate Relays**.

### Credentials and identity

The Relay's `-token` (or `REINS_RELAY_TOKEN`) holds one credential or a comma-separated list:

| Form | Meaning |
|---|---|
| `secret` (no comma in the setting) | a single global **admin** credential — the legacy format, unchanged |
| `tenant:role:secret` (no comma in the setting) | a single credential bound to that tenant and role |
| `:admin:s1,acme:operator:s2,globex:viewer:s3` | a list. **Every entry must be written in full**; the global admin entry is `:admin:secret` |

- The secret is the only thing that authenticates. Callers may present `s2`, `acme:operator:s2` or even `victim:admin:s2` — the identity is always the Relay's entry for `s2` (here `acme` / `operator`). Dropping or forging the prefix grants nothing.
- Listing a bare secret among comma-separated entries is rejected at startup: it is indistinguishable from a legacy single token that happens to contain a comma being split in two.
- Organizations for every tenant in the list are provisioned at startup. Disabling one is a runtime kill switch that persists across restarts; to remove a tenant permanently, remove its credential.
- Bearer header and the same-origin `reins_token` cookie go through the same authentication and the same authorization. There is no client that yields a different role for the same credential.

### Roles

| Role | Can | Cannot |
|---|---|---|
| `admin` | everything | — |
| `operator` | run tasks, shells, transfers, tunnels, scripts, cron, DAG/batch | revoke agents, power operations, self-update, manage secrets, mint enrollment tokens |
| `viewer` | read-only endpoints; submit `monitor` tasks | everything else, by default-deny (a new endpoint is invisible to `viewer` until explicitly allow-listed) |

`sysinfo`, `service` and `proc` in the CLI are wrappers over PowerShell tasks, so they are not available to `viewer`: the Relay cannot tell "list services" from an arbitrary command.

### Agent ownership

- Ownership is decided by the **credential used on the registering connection** and persisted (`agent_tenants`). Tags an agent reports about itself (including a `tenant` tag) are display data and never authorize anything.
- A tenant credential may register an agent ID that is unbound or bound to its own tenant. Other tenants, the deployment token and certificate-only mTLS connections cannot take over a bound ID — otherwise an impostor could receive that agent's queued tasks. The global admin credential is not subject to this.
- Tasks queued for an offline agent are only ever dispatched over a connection of the same tenant (enforced centrally in the dispatch path).
- In mTLS mode the certificate authenticates the agent ID; the tenant still comes from a credential presented alongside it (certificates carry no tenant).

### Deployment token (`-install-token`)

`/install.ps1` is served without authentication and embeds this token, so **treat it as public**. It can:

- register **new, unbound** agent IDs (they land in the admin domain), and
- answer "is agent X online" for one ID at a time (`GET /api/agents?agent_id=X`) — nothing more: no listing, no hostnames, tags or capabilities.

It reaches no other endpoint. Enable it for a deployment window and rotate it; on internet-facing Relays prefer mTLS enrollment.

### Other surfaces

- `/metrics` accepts the dedicated `-metrics-token` or a **tenant-less** credential; tenant credentials cannot scrape Relay-wide metrics.
- `GET /api/bootstrap` echoes only the caller's own credential. It never returns the credential list.
- Startup and structured logs never contain credentials (the configuration type redacts itself for `slog`).

## Known limits

These are deliberate or documented trade-offs, not oversights:

- **Not a hard multi-tenant boundary.** All tenants on a Relay share one process, one command policy, one update repository (admin-only) and Relay-wide aggregate counters. The Relay operator can do anything. Use separate Relays for hard separation.
- **Non-mTLS mode:** the credential on each agent *is* that agent's identity. Anyone holding a credential that is valid on the Relay can register agents within its scope.
- **Deployment token = public registration** for unbound IDs in non-mTLS mode (see above).
- **`-mtls` requires the Relay to terminate TLS itself.** Do not put a TLS-terminating reverse proxy in front of an `-mtls` Relay; client certificates must reach the Relay.
- **Offline-queued tasks have no expiry.** They run when the agent reconnects — possibly much later. Cancel them explicitly if that is not what you want.
- **Relay-level events** (those not tied to an agent) are delivered to every authenticated subscriber.
- The default command blacklist is best-effort — strict environments should enable allow-only mode (`-policy-allow` on the relay, `policy_allow` in agent config). See [README.zh-CN.md](./README.zh-CN.md#安全与负责任使用).

## Responsible use

reins is intended for managing machines you own or are contractually authorized to manage. Building stealth, anti-detection, or anonymous-access functionality into this project will not be accepted.
