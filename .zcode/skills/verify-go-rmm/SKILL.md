---
name: verify-go-rmm
description: >-
  go-rmm/reins（Go 反向 WebSocket 远程执行平台：relay+agent+CLI）的仓内验证技能——服务形态：
  loopback 构建并起真实 relay（-token 生产模式）+ agent（前台模式反向 WS 上线），HTTP 探针断言
  鉴权契约/Agent 清单/powershell 真实任务执行/审计 hash chain 衔接。当 agent 在本仓改过
  relay 路由/鉴权/任务投递/执行器/审计后需要自证行为不变时使用
  （doctor → launch → probe → cleanup 一条链）。
---

# verify-go-rmm — go-rmm 验证技能（服务形态·loopback relay+agent）

产物形态 = 反向 WS 中继服务（relay 常驻 + agent 反向连接 + REST API）→ 验证通道 = **loopback 起真实双进程 + HTTP 探针断言**（LaurenTan「形态随产物」：不 mock relay、不假 Agent——真二进制真注册真执行）。

- 仓根：`D:\M_X_M\ai-agent-platform\go-rmm`（技能目录 = 仓根下 `.zcode/skills/verify-go-rmm/`，随仓提交）。
- 零新依赖：go toolchain + Git Bash（bash/curl/sed/netstat/taskkill）。**全 loopback 离线**。
- token 是技能自生成的一次性测试 token（`verify-tok-<ts>`），非任何真实凭证。

## Launch

```bash
bash .zcode/skills/verify-go-rmm/verify.sh launch            # 动态空闲端口；构建→relay→agent→功能性就绪
bash .zcode/skills/verify-go-rmm/verify.sh launch --port 28631
```

四步（与 e2e/ console 套件同款驱动方式）：
1. 构建 relay+agent 到**全新** `runs/bin-<ts>/`（★ dataDir 是 **exe 同目录** 的 `data/`——e2e 在案坑位；全新目录=任务/审计数据按实例彻底隔离）；
2. 起 relay：`relay.exe -addr 127.0.0.1:<port> -token <一次性token>`（**-token 生产模式，无 -dev**）→ 轮询 `/healthz` 200（30s）；
3. 起 agent：`agent.exe run -relay ws://127.0.0.1:<port>/agent -token <token> -id verify-agent-01`（前台模式=AGENT-SETUP.md「手动方式」，与 Windows 服务安装同一条注册链路，免服务特权）；
4. **功能性就绪** = `/api/agents`（Bearer）出现 verify-agent-01（WS 注册→Registry→API 闭环；Go log 走 stderr 不作就绪信号）。

已有活实例时 launch 拒绝（防状态覆盖）；双实例并行：先手工记录第一实例 port/token（state.json 单实例档案）。

## Doctor

```bash
bash .zcode/skills/verify-go-rmm/verify.sh doctor
```

go build ./...（7 核心包）+ go vet ./... + `go test ./proto/` 快测 + 活实例探活。relay 包全测约 10s+、14 包全量按需手动 `go test ./... -count=1`（605 PASS + 1 既有 SKIP；函数口径 610——AGENTS 同源，2026-11-24 同步）。

## Drive（probe 探针组）

```bash
bash .zcode/skills/verify-go-rmm/verify.sh probe healthz       # 深浅双形状健康契约
bash .zcode/skills/verify-go-rmm/verify.sh probe agents-auth   # 鉴权契约 + Agent 清单
bash .zcode/skills/verify-go-rmm/verify.sh probe task-run      # 真实任务执行（含 audit-chain）
bash .zcode/skills/verify-go-rmm/verify.sh probe all           # 全量
```

| 探针组 | 断言 |
|---|---|
| healthz | 无鉴权 **200 浅形状**（只 status=ok，无 agents/uptime——P1-S7 不泄漏组件信息）｜带 Bearer **200 深形状**（relay=ok + agents=1 + uptime_s） |
| agents-auth | 错 token **401** + `{"error":"无效的鉴权 token"}`（错误体逐字段）｜对 token 200 含 verify-agent-01 且 count=1 |
| task-run | POST /api/tasks（powershell `Write-Output '<唯一marker>'`）→ 轮询 GET /api/tasks/{id} 到 **completed** + success=true + **stdout 含 marker**（loopback agent 真实执行，非 mock） |
| audit-chain | GET /api/audit/query?limit=10：本任务 task_submitted+task_result 落账在场（marker 唯一绑定）｜**hash chain 衔接**：task_result.prev_hash == task_submitted.this_hash（newest-first 第 1 条 prev == 第 2 条 this，逐位） |

## Evidence

- 每次运行落 `runs/evidence/<时间戳>-<pid>-<tag>/`：`report.tsv/report.txt`（逐断言三态）+ 每探针响应 body 原样字节（`api*.body`/`healthz*.body`）。
- 服务日志 `runs/relay.log`、`runs/agent.log` 持久保留（任务下发/SSE/上线记录即证据——与 e2e artifact 留痕口径一致）。
- **cleanup 不删证据**——收尾后 report/body/双日志仍在。

```bash
bash .zcode/skills/verify-go-rmm/verify.sh report [--json]
```

## Cleanup

```bash
bash .zcode/skills/verify-go-rmm/verify.sh cleanup --dry-run   # 先看会杀谁
bash .zcode/skills/verify-go-rmm/verify.sh cleanup             # taskkill //F //T 杀 agent+relay 进程树 + 删 bin 目录（含 dataDir 数据）
```

- 杀后核验 port 无监听；bin 目录（含其 data/ 任务与审计数据）删——每实例一次性；relay.log/agent.log/evidence 保留；无实例时幂等（rc=0）。

## Helpers

`verify.sh` 子命令面：`doctor` · `launch [--port N]` · `probe <healthz|agents-auth|task-run|audit-chain|all>` · `report [--json]` · `cleanup [--dry-run]`。
退出码 0=PASS / 1=FAIL / 2=INCONCLUSIVE；报错都带「下一步」。

坑位（脚本内已固化，改脚本时勿破坏）：
- **dataDir=exe 同目录**（relay/config.go，与 cwd 无关）——bin 必须放全新目录；断言用唯一 marker 不锚计数（e2e 同款纪律）。
- **agent 就绪不看进程看功能**（/api/agents 侧栏逻辑的 API 版）；agent 日志走 stderr。
- **curl 一律 --max-time**（audit/query 偶发慢查不挂死探针）。
- Windows go.exe 输出落盘再 grep（工作区红线）。
- 后台起服子 shell 与调用管道脱钩（exec + 外层重定向）——否则 `| grep` 挂等 EOF。
- Git Bash 下 `&` 与前置赋值的组合会把变量划进后台子 shell——token 先 export/单行赋值再起服。

**验收门槛**（生成器已真实跑通一遍）：doctor → launch → probe all（6 断言全 PASS）→ report → cleanup → 确认 evidence 与双日志在 cleanup 后仍存在。
