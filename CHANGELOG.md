# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。版本单一真相源：git tag。

## [v4.9.0] — 2026-10-27 MCP Server(stdio)

### 新增:cmd/reins-mcp——AI 代理的原生接入层
- **MCP(Model Context Protocol)stdio 传输**:JSON-RPC 2.0 换行分隔帧,零新依赖(纯标准库),Claude Desktop/Cursor/ZCode 等客户端直接配置即用。
- **curated 14 工具**:list_agents/status/run_command/task_run/task_get/task_list/batch_run/sysinfo/power/cron_list/cron_add/cron_delete/enroll_token/list_relay_tools——逐个映射 client SDK 导出方法,带 JSON Schema 输入描述。
- 工具内部错误按 MCP 语义回 `isError:true`(协议层成功);日志一律走 stderr(stdout 是协议通道)。
- 配置:`REINS_RELAY`/`REINS_TOKEN` 环境变量(与 CLI 同名);goreleaser 增发 `reins-mcp` 六平台产物。
- +8 测试:握手/tools-list schema 校验/ping/未知方法 -32601/通知无响应/run_command 成败两路(假 relay 完整投递链)/list_agents。

### 与规则门禁的关系
MCP 是接入层,门禁仍在执行层:`.reins/rules.md` 的 ack 校验在 Agent 侧(变更类任务无 ack/过期 ack 拒绝),换任何客户端(CLI/SDK/MCP)同一道闸。

## [v4.8.0] — 2026-10-27 品牌重命名 go-rmm → reins

### 为什么改名
- `reins`(缰绳)与产品叙事同构:**缰绳在你手里**——零入站端口的反向连接、每设备证书可吊销、防篡改审计链,以及给 AI 代理的规则门禁(先读规则再动手)。`go-rmm` 缩写难念、无记忆点,且与"远控"品类词汇距离过近。
- 撞名核查(GitHub/域名/知名产品/恶意软件联想四项)全绿;`go-rmm` 时代未公开发布,改名零迁移成本。

### 重命名范围(全量机械清扫 + 语义修正)
- module path:`github.com/QiuShichang/go-rmm` → `github.com/V-T-V/reins`(148 处 import)。
- 二进制:CLI `reins` / `reins-relay` / `reins-agent`;目录 `cmd/reins`、`deploy/helm/reins`;`nginx-reins.conf`。
- 环境变量:`GO_RMM_*` → `REINS_*`(105 处);规则门禁目录 `.go-rmm/` → `.reins/`;bash 补全函数 `_reins`。
- Prometheus 指标前缀:`gormmm_` → `reins_`(90 处,含测试断言);回归测试改写为"旧前缀(gormm_/gormmm_)零残留"断言。
- 全部文档(README 双语/AGENTS/DEPLOYMENT/docs)同步;历史条目中 CLI 示例一并更新为新名。

### 不变的兼容性承诺
- 协议(Envelope/消息类型)、API 路径(/api/*)、bbolt 存储结构、配置文件字段均未变——仅命名空间换装。

## [v4.7.8] — 2026-10-24 sync pull 并行化

### sync pull 并行下载(-j,与 push 同语义)
- worker 池并行下载远端目录:直连/数据面下多文件天然多流;`-j` flag 剥离与 push 共用代码路径。
- 下载侧 syncQuiet 守卫补齐(progressWriter.maybePrint/直连下载完成/中继下载完成三组打印点),并行时静默逐文件输出只留汇总+失败清单。
- E2E:32 文件×1MB push(-j=4)+pull(-j=4)全 32/32,逐文件字节对拍 ALL_IDENTICAL。

## [v4.7.7] — 2026-10-23 sync 并行化 + 数据面 isLast 赛跑修复

### sync push 并行化(-j,默认 1=串行不变)
- worker 池并行上传部署目录:直连/数据面下多文件天然多流;`-j N` flag(与 --concurrency 同风格,-j=N/-j N 双形态,剥离防位置参数误吞)。
- 并行时静默 fileUpload 逐文件输出(syncQuiet 全局,progressReader/直连/中继三组打印点守卫),只留汇总+失败清单;子目录预建(串行幂等,免 worker 竞争 mkdir 任务)。
- E2E:32 文件×1MB,-j=4 直连/中继各 3 轮全绿(2.5s vs 2.0s 串行,公网多流收益更大)。

### 真 bug:数据面 isLast 帧赛跑(E2E 并行上传逮住)
- `handleTransferUpload` 数据帧走 writeFrame(按 transferID 绑数据连接),但 isLast 帧仍走主连接——主连接 recvLoop 比数据连接队列快时 isLast 先到,Agent 对不完整数据算 hash → SHA 不匹配(data_conns=2 下 32 文件并行,直连/中继两路均 2~5 个失败,nondeterministic)。
- 修复:isLast 帧走同一 writeFrame(同 transferID 恒绑同一条连接,端到端保序)。修复后 data_conns=2 + -j=4 六轮压力(直连×3+中继×3)全 32/32。

## [v4.7.6] — 2026-10-23 SDK Update 服务 + 文档同步

### SDK 补齐(第三方消费者可驱动自动更新链路)
- 新增 `UpdateService`:List(仓库列表)/Latest(auto_update 同款轮询)/Push(默认 download 模式,binaryPath 非空自动切 path 引用模式)/DownloadStream(流式下载+X-Xfer-SHA256 头返回)。
- 新增 `Client.Version()`(/api/version,部署漂移观测)。SDK 此前有 Batch/Shell/Power/Cron/Secrets 五服务,唯独缺 Update。
- +4 测(httptest 假 relay:Version/List+Latest/Push 双模式/DownloadStream 含 404)。

### 文档同步
- openapi.yaml 补 v4.7.1 三端点(/api/updates/latest、/api/updates/download、/api/version)。
- config.example.json 补 data_conns 示例与语义说明。
- fuzz 种子语料经 Go 1.18+ 语义已被 CI 的 go test -race 自动覆盖(核实,无需新增任务)。

## [v4.7.5] — 2026-09-18 正确性加固 + 实测调优

### Race 检测(本机 mingw64 编译器首次启用)
四核心包(relay/agent/proto/cmd)`-race` 全清——v4.7.0~4 新增并发代码(数据面池/直连服务器/票据库/共享 client)零竞争;顺带修测试自身 6 处竞争(裸指针写读改带锁 sink)。

### Fuzz(帧解析器,面向远端输入的安全面)
三引擎 1200 万次执行零 panic:**逮住并修复一个真协议缺陷**——旧格式帧首字节是 id_len,≥0x80(长 ID)且 ID 首字节恰为 0x10/0x11 时被误判为隧道帧(往返失实,恶意对端可构造歧义帧)。修复:首字节 ≥0x80 划为隧道格式专用命名空间,旧格式 id_len 上限 255→127,Build/Parse 双侧对齐,歧义帧显式拒绝。隧道帧/Envelope 宽容解码同步 fuzz 覆盖。

### 传输基准(实测驱动的调优决策)
帧构造 2.0~3.3GB/s、SHA256 1.29GB/s、下载流核心 843MB/s、落盘 202MB/s——引擎各环节均远超千兆线速,**256KB 帧尺寸保持不变**(瓶颈在网络层,已由 Phase1/2 架构性解决),数据入档 BENCHMARK.md。

### 路径级指标
Agent 新增 `reins_agent_xfer_direct_bytes_total/direct_total`(直连命中率/回落频率观测);Relay 上传统计增加 `dataconn_frames`(数据面分流量)。

### 测试
+3(直连计量断言/127 命名空间契约三锚/fuzz 种子),总数 548→551 全绿。

## [v4.7.4] — 2026-09-18 Phase 2 数据面分离 + CLI 即时性

### Phase 2 数据面分离(吞吐架构续章)
- **结构**:Agent 配置 `data_conns`(默认 0=关,建议 2~4)后额外建立 N 条数据 WS(同端点 `?data=<i>`),`data_join→data_join_ack` 握手绑定;此后文件传输二进制帧走数据面,心跳/任务/隧道留控制连接——**根治"传输挤死心跳"**(单 TCP 时代生产实证:P2-12 被迫加发送限速)并突破单流吞吐天花板。
- **按 transferID 稳定绑连接**:同一传输恒走同一条 TCP(保字节流顺序——逐帧轮转的乱序落盘被 E2E SHA 对拍逮住后修正),跨传输天然分散:CLI 分片并发 N 片 = N 条 TCP 并行。双侧同款 fnv32 规则。
- **滚动升级零风险**:旧 Relay 不回 ack → 握手超时回落单连接(行为与旧版一致);数据连接断开退避重连,主连接重连全池重铸。
- E2E 实证(data_conns=2):30MB 单发 2.2s SHA 一致 / 60MB 分片+断点续传+合并全过 / 下载逐位一致。

### CLI 即时性(日志驱动续)
- **waitResult SSE 即时通道**:结果事件推送到达即返回(快任务省去最长 1s 轮询周期),1s 轮询兜底保留(SSE 断连/取消类终态仍可靠)。
- **`-s` 自动推断 script 类型**:不带 `-t` 直接 `-s x.ps1` 不再报"powershell 任务需要 -c"(日志实证高频误用);script 类默认超时 60→120s(与 deploy PlayStep 对齐,日志 15 次超时多为 script 类)。

### 测试
+4(数据面 E2E:join/ack/入池/数据连接帧路由到主连接通道/离线拒绝;池稳定绑定;inferScriptDefaults 六分支),总数 544→548 全绿。

## [v4.7.3] — 2026-09-18 使用日志驱动的 CLI 深度优化

### 分析方法与证据
从 ZCode 会话库(sqlite part 表)提取全部 reins 真实调用:1535 条命令提及、508 条严格 CLI 调用、**12% 失败率**;错误分类:PS 语法 20/超时 15/远端退出码 14/路径策略 7/占用 3。UX 摩擦:500/508 条手敲 `--relay --token` 前缀(平均 72 字符,累计 3.6 万字符);327 次上传 .ps1 却只有 30 次用 `-t script -s` 正解通道。

### 优化(按证据权重)
- **-File 反模式实时引导**:`task run -t powershell -c "powershell -File x.ps1"`(日志最大失败源:四层引号转义 ParserError + 路径 403 + 60s 默认超时三连坑)——CLI 检测到该模式时 stderr 提示正解(本地文件→`-t script -s <路径>` 一步到位;远端路径→确认已上传允许根)。不阻断,仅引导。
- **结果失败 hint**:printResult 按失败特征(PS 语法/路径策略/占用/超时)追加"--- 建议 ---"段,给出可执行的下一步。
- **env 补齐与曝光**:`REINS_INSECURE`/`REINS_CACERT` 补齐(relay/token/output 原已支持但无人知晓);usage 新增"环境变量"段(一次 export 终身有效)。
- 占用冲突类已由 v4.7.0 sweepIdleSessions 修复;上传/合并类已由 v4.7.0 绝对路径分流修复——本版本针对的是日志中仍在发生的坑。

### 测试
+3(反模式检测七分支/失败 hint 六分支/env 布尔语义),总数 541→544 全绿。

## [v4.7.2] — 2026-09-18 深度审计 + 三处修复

### 审计定位的三处真问题(全部修复)
- **自更新 rename 无回退(Defender 根治)**:`execUpdate` 的 rename 舞步被安全软件确定性拦截"未签名新文件顶替服务 exe"(4060 生产 4 次复现)后直接失败——现 `placeBinary` rename 被拒即回退**流式 copy 落位**(copy 通道畅通是生产实证),4060 两台恢复标准自更新/auto_update 能力,不再依赖人工 copy 舞步。renameFn 参数化,三路径测试注入覆盖。
- **probe 超时泄漏**:直连选址的手动 goroutine+select 超时路径会泄漏等待中的请求与响应体——改请求级 context 超时,超时即中断连接,零游离 goroutine。
- **直连 client 每调用新建**:分片传输每片 ticket/probe/begin/data 四次调用各自新建 client(四次 TLS 握手)——改进程级共享 client 连接复用;票据请求同步改走 `controlClient`(尊重 --insecure/--cacert,不再对持有效证书的 relay 无条件降级校验)。

### 测试
+6(placeBinary rename 直走/回退 copy/双失败聚合 + copyBinaryStream 跨缓冲保真 + client 单例 + probe 黑洞快速失败),总数 535→541 全绿;gofmt 全清。

## [v4.7.1] — 2026-09-17 自动更新链路(深度排查 + 优化)

### 排查结论(生产实证:http://203.0.113.99)
- 更新链路是**纯推送制**——Agent 无任何自主升级路径,仓库最新停在 v4.5.7(8/16)后无人推送,线上 agent 永远停在旧版;线上 relay 早于 v4.6.0(/api/dag 404 实锤),在线 agent 跑 dev 构建。
- CLI `update push -f` 默认走 base64 内联——代码注释自认公网 7MB+ 必超时(2026-08-18 实证),最顺手的用法恰恰是坏用法。
- 重启时序(U2)与健康回滚(60s+.bak)核实健壮,未发现问题。

### 新能力
- **Agent 自动更新**(`auto_update` 配置,默认关):周期查 `GET /api/updates/latest` → 数值比较(dev 当前版本按 0.0.0 处理,线上 dev 构建可被拉正)→ HTTP 流式下载+SHA256 校验 → 复用 execUpdate 统一收尾(防降级/.bak/两步 rename/pendingRestart/60s 健康回滚全套安全网)。仅 slave 角色;首轮延迟 90s 避开健康自检窗口;与推送式更新 updateMu 互斥。
- **Relay 版本分发端点**:`GET /api/updates/latest`(Agent 轮询)+ `GET /api/updates/download?version=`(流式二进制,带 X-Xfer-SHA256 头)+ `GET /api/version`(部署漂移一眼可辨)。
- **apply download 模式**:update 任务只带 download_url(Agent 自己 HTTP 拉取,零内联零 WS 帧);CLI `update push` 默认请求此模式,`--path` 显式引用优先。
- **update_dl 能力协商**:新 Agent 注册该能力;Relay 只对有能力的 Agent 走 download,旧 Agent 自动降级 legacy 内联(滚动升级安全:先升 relay/CLI,agent 逐台跟上)。
- **dev 版本拒收**:仓库 Save 拒绝显式 dev 版本(未版本化构建推上生产=版本比较/防降级全失效)。

### E2E 实证(本地三组件)
旧版 agent(v4.7.0-old,auto_update=1m)→ 上传 v4.7.1-new → 61s 后自动发现→下载→更新→进程退出 → 重启后 agent list 显示 v4.7.1-new,健康标记确认清除,.bak 就位,.old-running 自动清理。

### 测试
+13 测(relay 7:latest 数值感知/空仓库/download 404+内容+sha 头/dev 拒收/apply download 零内联/能力门降级内联/URL 推导三态;agent 4:HTTP 基地址推导表/升级决策表含 dev 拉正/下载 sha 校验+失败清理/间隔校验;CLI 2:push 默认 download/--path 模式),另修 config.example.json heartbeat "15s" 文档 bug(Duration 只认纳秒数字)。

## [v4.7.0] — 2026-09-16 内网直连传输(Phase 1) + 分片绝对路径修复

### Phase 1 内网直连传输(吞吐数量级提升)
- **架构**:CLI ↔ Agent 字节流不再绕道云端 Relay——Agent 配置 `xfer_listen`(如 `:7783`,默认关)后监听本网段直传端口,CLI 自动探测直连可达性(Relay 只做信令/授权面),千兆内网从几 MB/s(出口上行瓶颈)提到线速;不可达/未开启时**静默回落**中继路径,零配置成本。
- **票据授权(零信任姿态不变)**:Relay 新端点 `POST /api/transfer/direct` 签发一次性票据(32B 随机 secret,明文只回 CLI,SHA256 hash 经控制 WS 推给 Agent)——60s 过期、路径(含 .ftpart 分片前缀)+方向绑定、用后即焚、恒时比对、consume 失败也消费防在线穷举;Agent 侧 begin 再签发 data_token 防会话劫持。
- **Agent 直连端点**:自签 ECDSA P256 TLS(票据不落明文)+ `/xfer/probe|begin|data|fetch` 四端点;`begin` 复用 `HandleInit`(路径策略/续传偏移/ALREADY_COMPLETE 快路径全套中继语义);`SendDownloadData` 重构抽出 `streamDownload` 共核(限速/分片/进度/计数两路同语义)。
- **CLI 直连优先**:`file upload/download` 单发与分片每片先试直连;`--no-direct`/`REINS_NO_DIRECT` 显式关闭;直连熔断(首个 probe 失败/Agent 未开启即进程级禁用,后续分片零探测开销);路径策略 403 映射 blocked 终态不回落(中继同样会被拒)。
- **本地三组件 E2E 五场景实证**:5MB 单发直连 0.6s / 60MB 绝对路径分片(4 路并发+PowerShell 合并)5.3s SHA 一致 / 60MB 直连下载字节逐位一致 / `--no-direct` 中继 / 黑洞直连地址静默回落。

### 分片上传绝对路径修复(sess_8d55d252 复盘)
- **根因**:列目录/合并的 PowerShell 胶水假设 remotePath 为相对路径,绝对路径时 `Join-Path` 拼出 `dir\C:\...` 非法路径(PS 5.1 实证),回退 CWD=`C:\Windows\system32` 后必然 `MERGE_FAIL notfound`,断点续传同病根失效(会话实证 3 轮重传 3×159MB 全废)。
- **修复**:按绝对/相对分流(绝对路径直接 `-LiteralPath` 完整路径,跳过 workspace 自省;`remoteIsAbs` 兼容 Windows 盘符——CLI 可能跑在 Linux 控制 Windows Agent);解析前缀用 base name;`MERGE_FAIL` 判不可重试;含 Windows 真实 PowerShell 执行闭环测试(含 `"$($base).ftpart"` 插值陷阱锚——裸 `$base.ftpart` 会被 PS 解析为属性访问变空串)。

### E2E 逮住的 6 处真 bug(同轮修复)
- CLI `ds.begin`/Agent `handleBegin` 双端 UnixNano transferID 并发撞车(4 worker 同纳秒 → sessions/dataTk 相互覆盖 → data_token 401 + pathInUse 泄漏级联"路径被占用"),双端改 uuid。
- 直连失败后 section 已部分消费未回卷,中继收到残缺流(`Seek(0)` 回卷)。
- 直连分片成功误用 `continue` 留在重试循环(每片白传 3 次,改 `break`)。
- `--no-direct` 布尔 flag 被 `stripValueFlags` 当值型 flag 吞掉后参(纯 token 过滤)。
- 悬空会话(begin 后 data 永不到达)pathInUse 永久泄漏(`sweepIdleSessions` 2min 空闲清扫)。
- `handleData` EOF 后死循环读 body(上传成功但永不回响应,goroutine dump 定位)。

### 测试
+26 测(分片绝对路径 9/proto 票据 2/relay 票据端点 E2E 4/agent 直连 5/CLI 直连 5),总数 496→522,12 包全绿。

## [Unreleased] — 2026-08 深度推进（22 轮迭代重建）

### 稳定性（S0 修复）
- **mTLS 证书续签字塔（W7）**：新增 `/api/renew`（mTLS 客户端证书自鉴权 + `SignAgentCSRNoRevoke` 不撤销旧证不断连）+ Agent 后台 `certRenewLoop`（每小时检查）+ 证书三件套原子写（temp+rename）。原续签复用一次性 token → 24h 必 401 → Agent 永久离线。
- **服务模式自更新激活（U1）**：成功路径补 `os.Exit(0)` 触发 SCM 重启。原实现新二进制永不生效（所有服务模式自更新都是假象）。
- **更新原子替换（U2）**：备份改 copy（exe 保留原位），rename 失败/断电不再"砖"。
- **ca-state fail-closed（CRL1）**：状态文件损坏拒绝启动（原静默清空撤销集→已撤销 Agent 重新放行）+ saveState 原子写。

### 连接可靠性（W1-W5）
- **多 Relay 故障切换（W1）**：候选列表轮询（主→备用→…→回主）。原 relayIdx 仅在主 URL 为空时生效，故障切换形同虚设。
- **CAS 原子化（W2）**：`CASDispatchToPending` 单事务完成 pending→dispatched+Attempt++，dispatchTask 删 SaveTask 裸覆盖（原快任务 completed 可被覆盖回 dispatched→重复执行）。
- **panic 隔离（W3）**：recvLoop 全 handler safeCall + pool recover（原单任务 panic 崩整个 Agent）。
- **隧道/下载写超时（W4）**：SetWriteDeadline + 下载写 15s（原无限持锁拖垮心跳）。
- **退避重置（W5）**：健康连接(≥60s)断开后 backoff 归位（原永不重置，抖动后每次断线等 30s）。

### 弱网韧性
- **写优先级双侧（A1/A2）**：wsConn 从无优先级 writeMu 重构为 P0(control:心跳/ack)/P1(data) 双 channel + 单 writer goroutine + done 同步——弱网大传输不再饿死心跳致反复重连。
- watchdog 2×→3×、排队超时兜底、resultQueue 硬上限 1000、幂等 TTL 1h→2h。

### 安全
- **RBAC 执行（短1）**：RequireRole 守卫高危端点（revoke/enroll-token/power/secrets）+ power/update 任务类型守卫 + authorize 按 Secret 部分比较（支持 tenant:role:secret；原 viewer 格式 token 连鉴权都过不了）。
- **browse SSRF 防护（H3）**、token query 移除（H4）、登录限流 XFF 白名单（M1）、审计丢弃可观测（M3+audit_dropped 指标）。
- **命令策略统一（短2）**：internal/cmdpolicy 32 条单一真相源（原 Relay 15 条/Agent 32 条漂移）。

### 深层风险
- **离线任务契约（H-2）**：离线+曾注册+不支持→400；从未上线/支持→202 缓存（原 API 400 但落库→上线补投→非幂等命令延迟执行）。
- **in-flight 计数（H2/H-3）**：atomic ±1 + CAS 钳 0（防负漂移→调度失衡）。
- **isLast 写错检查（H-5）**：上传/下载终态帧写失败立即收尾（原悬挂至 30 分钟）。
- **关停顺序（H-1）**：Agent 连接先于 store 关闭。

### 架构
- **internal/ 共享域包**：ca（PKI）/cmdpolicy（命令策略）/taskstore（存储域+状态机+CAS+Batch）/wsx（优先级写引擎）——relay main 包减重 ~1400 行。
- **ExtractCommand 注册表**（proto 层）：命令提取单点化，加新命令类零遗漏。
- **update 纳入 Dispatcher**：能力协商修复+统一分发。
- **db.Batch**（taskstore 全部写事务）。

### 可观测性
- 任务延迟直方图（reins_task_duration_ms histogram）、tasks_acked/nacked、audit_dropped 进 /metrics；转发透传 env.Payload（省 re-marshal）。

### 修复（其他）
- PowerShell exit code（try/catch 前缀 + -NonInteractive + TestMain 预热）、GetCronRule O(1)、H-4 payload 透传、make dist 所需 install 脚本、quickstart 去硬编码、openapi 重复键。

### 部署与治理
- docker-compose（Relay+可选 Agent）、.env.example、config.example.json、release.yml（ghcr+goreleaser）、install.sh/ps1、LICENSE、CONTRIBUTING。

## [4.5.0] — relay 架构梳理 v4.5.0

## [历史] — 4.x 及之前
反向 WebSocket 中继穿透 NAT + Windows Agent 服务化 + 9 类任务执行器；
mTLS + 内置 CA + CRL + RBAC + hash chain 审计 + SIEM 导出；
Web 控制台 + Go SDK + Helm chart + goreleaser。
