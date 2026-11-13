# go-rmm · e2e-fusion 接入（2026-11-13 首次接入）

reins Web 控制台的 E2E 接入（被测项目零源码改动，全部资产在本目录）。
覆盖 2026-11-13 商业化深推新增能力：四 Tab 控制台 + `-token` 模式凭证传递 + GET /api/audit/query。

## 架构（每用例自动拉起，用完即杀）

```
Playwright chromium ──HTTP/SSE──► relay.exe -addr 127.0.0.1:$PORT -token <e2e-token>
                                        ▲
                                        └──反向WS── agent.exe run -relay ws://127.0.0.1:$PORT/agent -id e2e-agent-01
```

- relay 走平台标准 `${PORT}` 隔离（每用例独立端口）；`-token` 生产模式（无 `-dev`）。
- agent 用**前台模式**＝docs/AGENT-SETUP.md「手动方式」（与 Windows 服务安装同一条注册链路，
  免服务安装特权）；就绪判定放在 web 端侧栏 waitForCondition（功能性就绪，非进程就绪）。
- token 是本 suite 自生成的一次性测试 token（`env.local.yaml`），非任何真实凭证；
  经 `?token=` URL 引导消费后由页面 `history.replaceState` 抹掉。

## 隔离口径（重要）

relay 的 `dataDir()` 是 **exe 同目录** 的 `data/`（`relay/config.go`），与 cwd 无关——
构建产物在 `bin/e2e/`（gitignore），任务/审计数据跨用例持久累积。因此断言不锚定精确计数
（如"共 1 条"），而是锚定本用例唯一 marker `e2e-console-ok` + newest-first 顺序
（CreatedAt 降序，历史/审计首行恒为本用例记录）。如需彻底清零：删 `bin/e2e/data/`。

## 用例（suite: console）

| 用例 | 覆盖 |
|---|---|
| `01-console-token-flow` | `?token=` 引导（地址栏抹掉+localStorage+同源 cookie）→ SSE cookie 鉴权连接 → 侧栏 Agent 上线 → 四 Tab 逐个切换渲染 → UI 提交 powershell 任务（loopback agent 真实执行）→ SSE 实时结果流 → 任务历史行展开（stdout 按类型渲染）→ 审计行展开（prev_hash/this_hash 可见）→ **hash chain 衔接核对**（页内带凭证 fetch /api/audit/query，task_result.prev_hash === task_submitted.this_hash 逐位比对） |
| `02-gencert-selfsigned` | cmd/gencert 自签证书（打包三产物之一）：退出码 0 + 证书/私钥 PEM 头 + IP SAN 含 127.0.0.1，产物落 `${RUN_DIR}` |

数据治理：UI 下发任务声明 `effect: create` + recordData（manual-review——任务落在 relay 本地
e2e 数据目录，无外部副作用）。

## 运行

```bash
cd D:/M_X_M/ai-agent-platform/e2e-fusion
node apps/runner/dist/cli.js project run D:/M_X_M/ai-agent-platform/go-rmm/e2e/e2e.project.yaml --suite console --out reports/go-rmm-console/
```

## 真实输出（2026-11-13，本机）

```
✓ 项目 go-rmm 接入校验通过（1 个 suite）
▶ 准备被测项目: go build -o bin/e2e/ ./relay ./agent ./cmd/gencert（D:\M_X_M\ai-agent-platform\go-rmm）
  ✓ 01-console-token-flow  (28 通过 / 0 失败, 16415ms)
  ✓ 02-gencert-selfsigned  (5 通过 / 0 失败, 1379ms)
■ 汇总: 2 用例, 33 通过, 0 失败 — ALL PASSED
```

复跑验证（`--skip-build`，任务/审计数据已累积的口径）：2 用例 33 通过 0 失败 ALL PASSED——
持久化数据下的断言口径稳定。relay/agent 进程日志（含任务下发、SSE 订阅、Agent 上线）
被平台自动留痕为 artifact 证据；HTML/JUnit/质量报告落 `reports/go-rmm-console*/`。
