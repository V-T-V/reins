# UI 打磨与打包分发 —— 2026-11-13 推进实录

> 所有者差距判定:「缺 UI 打磨与打包分发」。本文档记录差距盘点 → 逐项落地 → 真实验证证据。
> 所有改动保留在工作区未提交,供所有者审查。

## 一、差距盘点(动手前结论)

| # | 差距 | 证据 |
|---|------|------|
| 1 | 无任务历史浏览 UI——`/api/tasks` 端点存在但前端只用来补拉 10 条 SSE 丢事件;端点无 status/agent/type 过滤与 offset 分页 | relay/web/app.js fetchRecentResults;router.go handleListTasks(仅 limit) |
| 2 | 无审计日志 UI,也无面向控制台的查询端点——只有 SIEM 机器格式导出 `/api/audit/export`;`audit_siem.go` 顶部注释承诺 `/api/audit/tail` SSE 从未注册(文档承诺未兑现) | relay/router.go 仅注册 export |
| 3 | 节点状态薄——API 返回 online_since/tags 但侧栏不展示;全部面板纵向堆叠一整页 | relay/web/index.html(单页堆叠) |
| 4 | **Web 控制台在 `-token` 模式下完全不可用**——所有 fetch/SSE 不带凭证,401 静默;而 token 是生产标准形态 | 浏览器实测:agents 列表空、metrics undefined、SSE 断开 |
| 5 | 打包缺位——`build.bat`/`build-windows.bat` 硬编码指向已不存在的 `D:\M_X_M\reins`;Makefile dist 面向 Linux/GitHub(依赖 zip 命令);无自签证书生成工具、无客户安装说明、无服务升级/回滚脚本 | 两 bat 文件;relay 有 -tls-cert 入口无生成工具 |
| 6 | 三份前端拷贝漂移——`web/` 与 `console/web/`(581 行旧版)落后 `relay/web/`(663 行) | diff 三目录 |

## 二、落地清单

### 后端(为 UI 供数,全部带测试)

1. **`GET /api/audit/query`**(relay/audit_query.go):JSON 查询,event(多值)/agent_id 过滤 + since(duration 或毫秒)+ offset/limit 分页,新的在前,透出 hash chain 字段。`AuditLog.Path()` 访问器。注册路由 + OpenAPI 同步。修正 audit_siem.go 里从未兑现的 `/api/audit/tail` 注释。
2. **`GET /api/tasks` 增强**(relay/router.go):`status`(多值,走 F1 状态索引)/`agent_id`/`type` 过滤 + `offset` 分页(上限 500/页),统一 CreatedAt 降序,响应加 `total`/`offset`。空页返回 `[]` 非 `null`。
3. 测试:+4(audit_query_test.go 过滤/分页/排序/未启用 503/坏行容错/纯函数;tasks_query_test.go 六分支),auth_cookie_test.go +2(见下),gencert +3。

### UI(relay/web/,同步 web/ 与 console/web/ 消除漂移)

4. **Tab 布局**:概览(指标+实时结果流)/ 任务执行(下发+批量+终端+文件传输)/ **任务历史** / **审计日志** 四页,客户第一眼不再面对一整页堆叠。
5. **任务历史页**:状态(含"执行中"三态合并)/类型/Agent(datalist)筛选,表格+状态徽章+提交参数预览,点击行展开详情——`GET /api/tasks/{id}` 拉真实结果,**按类型渲染**(screenshot 出图/file 解 base64 出内容/powershell·script 出 stdout·stderr/guiclick 出步骤/其余出结构化 JSON);上一页/下一页分页;SSE task_result 到达且页面可见时 30s 节流自动刷新。
6. **审计日志页**:时间范围(1h/24h/7d/全部)/事件(16 种常见事件下拉)/Agent 筛选,表格+事件分级徽章(crit 拦截撤销红/warn 失败电源黄/info 上线蓝),点击行展开完整记录含 prev_hash/this_hash(chain 可人工核对);SIEM 导出按钮;分页。
7. **侧栏增强**:tags 胶囊、`● 在线 X 分钟`(online_since 实时计算)、版本号双 v 修复。
8. **Token 鉴权接线(修真 bug#4)**:`?token=` URL 引导(消费后 history.replaceState 从地址栏抹掉)→ localStorage + **同源 cookie `reins_token`**;全部 API 调用走 `apiFetch`(自动 Authorization header + 401 时提示输入重试);SSE/文件下载/SIEM 导出走 cookie(无法带 header 的场景);XHR 上传补 header。

### 打包分发

9. **`scripts/release-package.ps1`**:版本化构建(git describe 或 -Version)→ 打包前强制 `go test ./...` 门禁(拒绝把红的构建发出去,-SkipTest 可显式跳过)→ 产物 `dist/release/<version>/`:
   - `reins-server-windows-<ver>.zip`:reins-relay.exe + reins.exe(CLI) + reins-gencert.exe + start-relay.ps1 + INSTALL.md
   - `reins-agent-windows-<ver>.zip`:reins-agent.exe + service-install.ps1 + config.example.json + AGENT-SETUP.md
   - `reins-relay-linux-<ver>.tar.gz`:交叉编译 relay + systemd unit + 部署脚本
10. **`scripts/start-relay.ps1`**:前台试用 / 开机自启(计划任务 SYSTEM+崩溃 1 分钟自动重启最多 3 次+日志落盘)/ 卸载三模式;防火墙自动放行;`-UseTLS` 自动调 gencert 生成自签证书并以 8443 启动。
11. **`scripts/service-install.ps1`**:Agent 侧 install/uninstall/start/stop/restart/status/**upgrade(停→备份→替换→启动→8s 探活→失败自动回滚)/rollback** 八动作;install 支持标签写入 config.json;uninstall 清残留(-KeepWorkspace 保留数据)。
12. **`cmd/gencert`**:自签证书生成工具(纯标准库,ECDSA P-256,SAN 自动含本机全部网卡 IP,825 天,已存在拒覆盖,-force),生成后 `tls.LoadX509KeyPair` 自检(与 relay 同一加载路径)。+3 测试。
13. **`docs/INSTALL.md`**(随 server 包)与 **`docs/AGENT-SETUP.md`**(随 agent 包):客户从零到能用的一页说明(架构 30 秒→三步走→验证表→升级卸载→安全须知→FAQ)。

## 三、顺手逮到的真 bug(修复+测试锚定)

| # | Bug | 修复 |
|---|-----|------|
| 1 | **Web 控制台 Token 模式全灭**:relay `-token` 运行(生产标准)时控制台所有 API 401 静默——前端从未实现凭证传递,此前只在无 token dev 模式可用 | 前端 apiFetch/cookie/URL 引导三层(见 §二.8) |
| 2 | **EventSource 与 H4 的结构性矛盾**:SSE 无法带 Authorization header;曾有的 `?token=` query 回退被 H4 修复有意移除(防日志/Referer 泄漏,有测试锚定);且 `authorize`(server.go)与 `authorizeRBAC`(rbac.go)行为不一致——后者仍留 query 回退,是漂移的泄漏面 | 两处统一为 **header > 同源 cookie `reins_token`**;query 依旧拒绝(H4 语义保持,原测试不变绿转红);+auth_cookie_test.go 2 测(含"header 错误时不得回落 cookie") |
| 3 | audit_siem.go 注释承诺 `/api/audit/tail` SSE 端点从未注册(文档承诺未兑现) | 注释改为指向真实存在的 /api/audit/query |
| 4 | 侧栏版本号双 v(`vv4.9.3`)——模板硬编码 `v` 前缀撞上 version 本身带 v | 条件前缀 |
| 5 | `build.bat`/`build-windows.bat` 指向已不存在的 `D:\M_X_M\reins`(历史迁移遗留,静默失败) | 不改动旧文件(存档);新打包入口为 scripts/release-package.ps1(文档指向新入口) |
| 6 | 三份 web 拷贝漂移(web/ 与 console/web/ 落后 relay/web/) | 统一同步为 relay/web 版本(改 UI 只改 relay/web,另两处为分发拷贝) |

## 四、真实验证证据(2026-11-13 本机 loopback)

**构建**:四产物真实编译,版本注入核对(`reins-agent/gencert/reins.exe version` → `v4.9.3-ui-20261113`)。

**Loopback 全链路**(relay 127.0.0.1:17890 + token + agent 前台连接):
- `GET /healthz` → `{"status":"ok"}`;`GET /api/agents` → verify-agent-1 在线(11 项能力/hostname/online_since/tags/xfer_stats)
- POST powershell 任务 → 真实执行 → result `success:true`, stdout `"ui-verify-ok"`(远端真跑,非 mock)
- `GET /api/tasks?status=completed` → total:1 过滤正确;`agent_id=nobody` → total:0
- `GET /api/audit/query` → 2 条(task_result/task_submitted),**hash chain 核对:task_result.prev_hash == task_submitted.this_hash(`08841eb1…`)**,新的在前;`?event=task_result` → total:1
- CLI:`reins agent list`(表格含"在线自")、`reins task run -c "Write-Output cli-smoke-ok"` → 成功 658ms
- 鉴权矩阵:header 200 / 无凭证 401 / query token 401(H4 保持)

**浏览器端到端**(IAB 真浏览器,http://127.0.0.1:17890/?token=verify-tok 引导):
- Token 从 URL 消费,地址栏抹掉;SSE `Relay: 已连接 ✓`(cookie 生效)
- 侧栏:verify-agent-1 + `● 在线 不到 1 分钟` + 11 能力胶囊 + `role=slave` 标签;指标面板实数
- UI 提交 powershell 任务 → 实时结果流 `✓ 成功 639ms` + `browser-e2e-ok`(SSE 推送渲染)
- 任务历史页:行(时间/ID/类型/Agent/已完成徽章/命令预览)+ `第 1/1 页 · 共 1 条`;行点击展开详情(stdout/stderr 按类型渲染)
- 审计页:`第 1/1 页 · 共 2 条`,两行事件徽章+详情;视觉检查(截图)确认无破版、配色统一

**打包**:`scripts/release-package.ps1 -Version v4.9.3-ui-20261113` 真实执行,产物(`dist/release/v4.9.3-ui-20261113/`):
- `reins-server-windows-…zip`(8.3 MB):INSTALL.md / reins-gencert.exe / reins-relay.exe / reins.exe / start-relay.ps1
- `reins-agent-windows-…zip`(3.4 MB):AGENT-SETUP.md / config.example.json / reins-agent.exe / service-install.ps1
- `reins-relay-linux-…tar.gz`(3.7 MB):reins-relay / systemd unit / 部署脚本

**打包产物验收**(zip 原样解压到干净目录,zero 源码依赖):server zip 的 relay 起服务(17891)→ agent zip 的 agent 连上(11 能力)→ CLI 任务真实执行 `packaged-artifact-ok` 成功 551ms → 审计 2 条 hash chain 完整(prev_hash 衔接)→ Web UI 四 tab 内嵌。

### 打包脚本踩坑实录(2 个,均已修)

1. **Windows PowerShell 5.1 把无 BOM 的 UTF-8 .ps1 按 ANSI(GBK) 解析**——中文字符串被切坏,`[WARN] tar 打包失败(可忽略,…)` 被解析成命令 `可忽略,zip` 报 CommandNotFoundException。修:三个 .ps1 全部加 UTF-8 BOM(本项目 installscript.go 注释早有此坑记录——"PS 5.1 双坑实测",这次轮到自己的脚本踩)。附带解释了首跑打包测试门禁异常中断(同一 GBK 解析问题)。
2. **Git Bash 的 GNU tar 把 `D:\…` 当远程主机**(host:file 语法)——`tar: Cannot connect to D: resolve failed`。修:Windows 下显式用 `$env:SystemRoot\System32\tar.exe`(bsdtar,驱动器路径原生支持),非 Windows 回退裸 tar。

## 五、测试

全量 `go test ./... -count=1` 两轮真实执行,14 包全绿(exit 0):
agent 6.2s / client 1.8s / cmd/gencert 3.7s / cmd/reins 8.3s / cmd/reins-mcp 3.3s / console 3.7s / executors 20.1s / internal/ca 3.2s / internal/cmdpolicy 3.4s / internal/dagflow 3.2s / internal/taskstore 4.0s / internal/wsx 36.4s / proto 2.4s / relay 11.7s(vision/win32/wsprobe 无测试文件)。

新增测试:
- relay/audit_query_test.go(+3)、relay/tasks_query_test.go(+1)、relay/auth_cookie_test.go(+2)
- cmd/gencert/main_test.go(+3)
- 原 H4 锚定测试 TestAuthorizeQueryTokenRejected 保持通过(未被 cookie 改动破坏——cookie 是新增通道,query 依旧拒绝)

## 六、客户交付还差什么(诚实清单)

1. **代码签名**:Windows Defender 对"未签名新文件 rename 顶替运行中服务 exe"的拦截是已知坑(AGENTS.md 2026-09-17 实录)——正式分发需购买代码签名证书,`upgrade` 路径在无签名机器上可能需客户侧加白。
2. **公网 HTTPS**:自签证书仅适合内网;公网部署需反向代理(nginx/caddy)+正规 CA,INSTALL.md 已指引但未附 nginx 样例文件(仓库有 nginx-reins.conf 可参考)。
3. **版本号管理**:打包版本目前 -Version 显式/git describe;发布纪律(打 tag→打包→checksum)未自动化,可后续接 .goreleaser.yaml 统一。
4. **Linux agent 包**:本次只打了 Windows agent 包(Linux agent 支持存在但跨发场景少);需要时按同样模式补 tar.gz。
5. **安装体验统计**:一行安装(iex irm)依赖 Relay dl/ 目录放 agent exe;离线环境的纯 zip 安装路径已覆盖(service-install.ps1),但两种方式的文档交叉引用可以更紧密。
