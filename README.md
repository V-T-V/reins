# Reins

> **Take the reins of your fleet.** Self-hosted zero-trust remote operations platform — one Go binary, zero inbound ports, your data never leaves your network. And your AI agents touch a server only after reading the rules.

[![Go](https://img.shields.io/badge/go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](./LICENSE)
[![CI](https://github.com/V-T-V/reins/actions/workflows/ci.yml/badge.svg)](https://github.com/V-T-V/reins/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/V-T-V/reins?display_name=tag&sort=semver)](../../releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/V-T-V/reins/client.svg)](https://pkg.go.dev/github.com/V-T-V/reins/client)
[![Tests](https://img.shields.io/badge/tests-559-brightgreen)]()
[![CLI](https://img.shields.io/badge/CLI-50%2B%20commands-blue)]()

[English](./README.md) | [简体中文](./README.zh-CN.md)

![demo](./docs/demo/demo-overview.svg)

reins lets a small team run remote operations on a fleet of Windows/Linux machines from one terminal: command execution, file transfer, interactive shell, port forwarding, multi-step deploy orchestration, batch dispatch, monitoring — over **reverse WebSocket connections** that traverse NAT without opening a single inbound port on your hosts.

It is an open-source alternative to Teleport (access plane) and MeshCentral/TacticalRMM (RMM), with enterprise primitives built in: **mTLS with a built-in CA and CRL, multi-tenant RBAC, a tamper-evident audit hash chain with SIEM export**, and a rules gate your **AI agents** must pass before they touch a server.

```
   CLI (50+ cmds) ──HTTPS/SSE──►  Relay (single Go binary)  ◄──reverse WS (NAT traversal)──  Agent (Windows service / Linux)
   Web console (embedded)        mTLS + CA + CRL            wss://, agent dials out         9 task executors:
   Go SDK (AI agents)            RBAC multi-tenant                                         powershell · script · file · screenshot
                                 audit hash chain + SIEM                                   browse (chromedp) · guiclick · update
                                 Prometheus /metrics + pprof                               power · monitor (TUI)
```

## Why reins

| | |
|---|---|
| **Self-hosted, single binary** | Relay is one static Go binary (Docker/Helm/systemd also provided). No database required — bbolt by default, Redis optional for horizontal scale. |
| **Zero inbound ports** | Agents dial *out* over WebSocket/wss. Your hosts stay unreachable from the internet; NAT and home/office networks just work. |
| **Enterprise security primitives** | mTLS with built-in CA, per-device client certs, CRL revocation, `tenant:role:secret` RBAC tokens, command black/allow-lists enforced on both Relay and Agent, append-only audit hash chain, session recording (asciinema v2), secrets with TTL + one-time use. |
| **Ops, not just remote desktop** | Batch dispatch with retries & live progress, DAG orchestration (dependency graphs, concurrency caps, failure cascade skip), deploy playbooks, process watchdog, cron, snapshots, network diagnostics, log aggregation. |
| **AI-agent native** | A Go SDK exposes 30+ ops tools, and `.reins/rules.md` gates machine operations: automation must *read the project's operation rules and carry a content-fingerprint ack* before executing — stale acks are rejected when rules change. |

## Quickstart

```bash
git clone https://github.com/V-T-V/reins && cd reins
echo "REINS_TOKEN=devtoken-please-change" > .env
docker compose up -d          # Relay on http://localhost:7780 (web console included)

# install a Windows agent on a target machine — one line, no pre-setup:
#   (served by your relay; auto UAC elevation, token probe, online check)
iex (((irm 'http://<relay-host>:7780/install.ps1') -replace '^\uFEFF'))

# from your workstation:
reins --relay http://<relay-host>:7780 --token devtoken-please-change agent list
reins --relay http://<relay-host>:7780 --token devtoken-please-change task run -a <agent> -c "hostname"
```

Linux agent: `./reins-agent run -relay ws://<relay>:7780/agent -token <token>` (systemd unit in `deploy/`). Binaries for windows/linux/darwin are on the [Releases](../../releases) page. Production notes: [DEPLOYMENT.md](./DEPLOYMENT.md).

## Feature map

| Area | Highlights |
|---|---|
| Execution | `task run/stream`, multi-agent `exec`, interactive `shell open` (recorded), 9 executor types incl. browser automation (chromedp) and vision-based GUI click |
| Transfer | chunked + resumable file up/download with **per-chunk SHA256**, LAN-direct fast path with relay fallback, `sync push/pull`, rate limiting, `tunnel -L` port forwarding |
| Orchestration | deploy playbooks (`deploy init` templates), batch dispatch (one-to-many, exponential-backoff retries, SSE progress), **DAG flows** (`dag apply`) with failure-cascade skip |
| Monitoring | `monitor top` (glances-style TUI: CPU/mem/disk/net/GPU), snapshots + diff, smart scheduling picks the least-loaded agent, Prometheus `/metrics`, webhook alerts |
| Diagnostics | remote `net ping/dns/trace/ports/check`, `logs tail/search`, `find name/grep/du`, `sysinfo`, fleet `topology`/`diagnose`/`compare` |
| Fleet admin | agent enroll/revoke, service/proc/power management, process watchdog, cron, self-update with health check & automatic rollback |
| Security | mTLS + CA + CRL, RBAC (admin/operator/viewer) + org quotas & isolation, command policy (32-entry blacklist default, optional allow-only, enforced Relay- *and* Agent-side), audit hash chain + CEF/Syslog/JSON SIEM export, secrets (TTL, one-time), script library with path-traversal protection |

Full CLI reference: run `reins` (50+ commands, exit codes 0–5). API contract: [deploy/openapi.yaml](./deploy/openapi.yaml) (40+ endpoints).

## How it compares

| | reins | MeshCentral | TacticalRMM | Teleport | RustDesk |
|---|---|---|---|---|---|
| License | **MIT** | Apache-2.0 | source-available | source-available¹ | AGPL-3.0 |
| Server | **one Go binary** | Node.js | Django+Vue+Go+MQ | Go (heavy) | Rust |
| Full RMM (batch/DAG/monitor) | ✅ | partial | ✅ (MSP) | ✖ (access plane) | ✖ (desktop) |
| Device identity (mTLS+CA+CRL) | ✅ | partial | partial | ✅ | ✖ |
| Multi-tenant RBAC | ✅ | ✖ | partial | enterprise | paid |
| Tamper-evident audit chain + SIEM | ✅ | ✖ | ✖ | ✅ | ✖ |
| AI-agent rules gate | ✅ | ✖ | ✖ | ✖ | ✖ |
| Data plane | self-hosted | self-hosted | self-hosted | self-hosted | self-hosted |

¹ Teleport Community License (2024): free tier limited to companies <100 employees and <$10M revenue.

## Security & responsible use

reins is built for **managing machines you own or are contractually authorized to manage**. Unauthorized use is prohibited by the license and likely illegal in your jurisdiction.

Design choices that matter for defenders:

- Agents make **outbound-only** connections; you can firewall all inbound management ports.
- Every device gets an mTLS client certificate; revoke per-device via CRL (`agent revoke`).
- Command policy is enforced twice (Relay and Agent, incl. offline-cached tasks — a cached high-risk command for an offline agent is rejected at submission, 403 not 202).
- All task submissions land in an **append-only hash-chained audit log** exportable to your SIEM — the artifact CISA AA23-025A asks for when inventorying approved RMM use.

If your antivirus flags the agent, see [docs/antivirus.md](./docs/antivirus.md) — false positives are a known industry-wide issue for this software category (they hit MeshCentral, TacticalRMM and RustDesk too), and every release ships a SHA256 manifest.

Vulnerability reporting: see [SECURITY.md](./SECURITY.md).

## AI agents on your fleet

```go
import "github.com/V-T-V/reins/client"

c := client.New("https://relay:7780", "token")
tools, _ := c.ListTools()                       // 30+ ops tools for your agent
out, _  := c.RunCommand("agent-1", "Get-Date", 10)
res, _  := c.StreamCommand(ctx, "agent-1", "ping 8.8.8.8", 30, func(ev string, d json.RawMessage) { fmt.Println(ev, string(d)) })
```

Automation guardrail — `.reins/rules.md` lives in the *managed project's* repo root (e.g. "never kill the kiosk shell during business hours", "exclude node_modules/ from deploys"). Agents must `rules read` (content-fingerprint ack, 30s cache) before mutation tasks are accepted; edit the rules and old acks are rejected immediately. The rejection never echoes the fingerprint, so "skip reading, replay the ack" doesn't work.

## Project stats

| | |
|---|---|
| Go files / LOC | 253 / 48,000+ |
| Tests / benchmarks | 558 / 26 (`go test ./...`) |
| CLI commands / REST endpoints / executors | 50+ / 40+ / 9 |
| CI | build + `go test -race` + golangci-lint + 3-platform compile gate |
| Deliverables | Docker ×2, Helm, OpenAPI, goreleaser, systemd units |

### Honest limitations

- **Windows agent is first-class**; the Linux agent covers script/monitor + dialect-aware CLI ops (~85%) — real-machine validation is still in progress.
- **Single Relay is the default deployment**; multi-relay failover and the Redis TaskStore/EventBus federation exist and are tested, but production-hardening continues.
- Command blacklist is best-effort by design; enable allow-only mode (`-policy-allow` on Relay, `policy_allow` on Agent) for strict environments.

## Documentation

- [README.zh-CN.md](./README.zh-CN.md) — full Chinese README (config, RBAC, batch/DAG, file transfer internals, rules gate)
- [DEPLOYMENT.md](./DEPLOYMENT.md) — production deployment guide
- [BENCHMARK.md](./BENCHMARK.md) — performance baselines
- [deploy/openapi.yaml](./deploy/openapi.yaml) — REST API contract
- [AGENTS.md](./AGENTS.md) — deep architecture notes (dense, Chinese)
- [CHANGELOG.md](./CHANGELOG.md) — release history

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](./CONTRIBUTING.md). Monthly release cadence.

## License

[MIT](./LICENSE) © 2026 QiuShichang
