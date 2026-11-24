# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。版本单一真相源：git tag。

## [未发布] — 2026-11-24 租户隔离加固(凭据绑定身份 + 对象级授权)

起因:一次外部只读安全审查(针对 `d739e85`)的结论——**共享 Relay 的租户隔离没有贯穿实际接口**。本条把隔离做成真的,并如实列出顺带修掉的既有缺陷。信任模型的权威表述见 [SECURITY.md](SECURITY.md)。

### 变更前的问题(源码确认,验收测试在旧代码上逐条跑红)

- **凭据与身份脱钩(P0)**:`authorize` 只比较 secret,tenant/role 取请求方**自称**的前缀。知道共享 secret 的低权限用户省略前缀即得全局 admin(旧代码上裸 secret 调 `secret.put` 返回 201)。
- **cookie 路径恒 admin(P1)**:角色守卫只读 Authorization 头,经 cookie 过鉴权的请求一律被当成 admin。
- **对象级授权缺席(P0)**:Agent 清单全局可见;任务详情/历史、取消、批量与 DAG 的目标解析都没有归属校验。
- **审计查询/导出/SSE 跨租户可见(P1)**;**Secret 引用无归属校验**,部署 Token 可拉全局 Agent 清单。
- 角色矩阵形同虚设:只有 6 个端点调用 `CanPerform`,viewer 能开 shell、建 cron,`/api/updates/apply` 绕过 update.push。

### 安全

- **凭据绑定身份**:Relay 维护「凭据 → 固定身份」索引,身份一律取服务端配置条目;出示时的前缀不参与判定。Bearer 与 cookie 走同一套鉴权与授权(cookie 补 URL 解码)。
- **多凭据**:`-token` 支持逗号分隔列表。**语法无歧义**:不含逗号 = 单凭据(旧格式纯 secret 行为不变);含逗号 = 列表,**每条必须写全** `[:tenant:]role:secret`,全局 admin 写 `:admin:secret`。角色/租户 ID 非法、secret 复用、列表里出现裸条目 → 启动即报错。
- **Agent 归属由服务端维护**:注册时按连接凭据写入并持久化(`agent_tenants`)。自报标签(含 `tenant` 标签)只是展示数据。其它租户、部署 Token、仅证书的连接都**不能顶替**已绑定的 agent ID(否则顶替者可接走缓存任务)。
- **对象级授权**覆盖:Agent 清单、任务(下发/详情/列表/取消/流式)、同步与异步批量、DAG(提交/列表/状态)、shell、传输(含直连票据)、隧道、自更新、脚本库(按租户分命名空间)、cron(派生任务继承租户)、智能调度、电源、审计查询与导出、SSE 事件流、Secret、签发注册令牌、吊销证书。拒绝码约定:动作 403、详情 404、列表直接滤掉。
- **投递中央闸**:租户任务只经同租户连接投递——全部 11 条投递路径汇于 `dispatchTask` 一处,任何路径的目标解析有疏漏也出不了本租户。
- **离线归属**:已绑定的 Agent 离线后其租户仍可寻址(给自己离线的机器缓存任务);未知 Agent 对租户 fail-closed。
- **组织自动供给**:凭据里的租户在启动时建组织(此前生产没有任何建组织的途径,租户凭据会永远 401)。禁用是运行期闸门,跨重启保持。
- **viewer 真只读**(默认拒绝,新增端点对 viewer 自动不可见);只能提交 `monitor` 类型任务。`/api/updates/upload`、`/api/updates/apply` 归入 `update.push`(admin-only)。
- **部署 Token** 收窄为 `GET /api/agents?agent_id=X` 单机探活,只回 `agent_id`;`install.ps1` 已适配。
- **Secret 归属**:绑定创建者租户,读取须 operator 及以上;归属校验在消费 one-time 之前(被拒不烧)。admin 可在创建时指定 `tenant` 把凭据放进某租户作用域。

### 修复(加固过程中发现的既有缺陷,均非本次引入)

- **启动日志明文写出全部凭据**:`slog.Info(..., "config", cfg)` 把 Config **结构体**交给 JSON handler,`Token`/`InstallToken`/`MetricsToken` 原样序列化进日志。现由 `Config.LogValue` 在类型层面脱敏。
- **`GET /api/bootstrap` 回显整个 `cfg.Token`**(单凭据时代无害;多凭据下等于任何低权限凭据都能换走 admin secret)。现只回显调用者自己那条。
- **离线任务从未落盘**:给离线 Agent 提交任务回 202「已缓存待投递」,但该分支直接返回,CLI 还会打印「已缓存,上线后自动投递」——实际被静默丢弃且无审计。现真正落盘;未启用持久化时如实返回 503。
- **审计 hash chain 每次重启自断**:`OpenAuditLog` 以追加模式打开却不回放末条哈希,重启后首条记录 `prev_hash` 为空。现打开时回放尾部续链(已断的旧链不掩盖)。
- `/metrics` 的凭据比较改为常量时间;多凭据下接受任一无租户条目。

### 行为变更(升级前请看)

- 含逗号的旧式单 token **不再合法**(会被当成凭据列表)。请更换,或写成列表语法。
- 单凭据 `tenant:role:secret` 部署下,省略前缀出示 secret 得到的是该条目的租户身份,**不再是 admin**。需要 admin 请单独配一条 admin 凭据。
- `viewer` 凭据不再能执行写操作;`operator` 不再能调用 `/api/updates/*`。
- 部署 Token 无 `agent_id` 访问 `/api/agents` 由 200 变为 403。
- 离线提交的任务现在会真正排队并在 Agent 上线后执行(无过期时间;不需要请显式取消)。

### 验证

- 新增验收 `relay/tenant_isolation_test.go`:真实 WS Agent + bbolt + HTTP,双租户 × admin/operator/viewer 共 15 个场景。该文件只用加固前后都存在的 API——**在旧代码(`1e1d265`)上原样运行 7/7 失败,加固后全绿**(红绿锚定)。
- 单元断言 `relay/tenant_units_test.go`、`internal/taskstore/tenant_store_test.go`:bbolt 与内存两个存储实现逐条对拍(租户过滤、绑定 insert-if-absent、并发首注册)、注册准入规则(三种存储形态)、投递中央闸、审计旧格式链兼容与跨重启续链、脚本库命名空间、配置守卫、结构化日志脱敏。
- `go test ./... -count=1` 14 包全绿;`go test -race` relay + taskstore 零数据竞争;`go vet ./...` 干净;golangci-lint v2.12.2 对 relay/taskstore 0 issues;CI 矩阵(linux/amd64、windows/amd64、darwin/arm64)及另外 3 个目标交叉编译通过。
- 真实二进制回路(loopback):仓内 verify 技能 6/6(单凭据旧格式部署零回归:401 错误体逐字段保留、真实 PowerShell 任务执行、审计链衔接);另起真实 Relay + 两个真实 Agent 的多凭据实例 37/37(启动日志无 secret、清单隔离、伪造前缀提不了权、跨租户拒绝、bootstrap 不泄漏、部署 Token 收窄、顶替被拒并入审计)。
- 测试函数口径 `^func Test`:585 → 610。

### 文档

- SECURITY.md:新增「信任模型」(一个 Relay = 一个信任域;硬隔离请分部署)、凭据语法、角色、Agent 归属、部署 Token、已知边界。
- DEPLOYMENT.md:多凭据与租户配置;README 双语安全表述改为与实际强制行为一致。

## [未发布] — 2026-11-18 债务收口战役(文档口径 + 门禁修复 + 补账)

### 补账(v4.9.2 之后三笔提交未入 CHANGELOG)

- **F5 批 C 真死收口**(6ad06a7,2026-11-17):删 win32 三对零消费输入 API——`MoveTo`/`GetCursorPos`/`TypeText`(Windows 实现+非 Windows stub 两份),guiclick 执行器只消费 `ClickAt`;连带收走其私用机械(sendKeys/keyboardInput/inputKeyboard/keyeventf* 常量/两 proc 变量/inputUnion)。**修正下方 2026-11-16 节的「未动(产品裁决留所有者)」口径**——三件套已于次日按 owner 裁决删除,textinput 半成品能力位一并消失。
- docs/mcp.md 顶部「另见」行指向 TypeScript 参考实现 reins-mcp(41159ff,2026-11-17)。
- verify-go-rmm 仓内验证技能入库(bd1c2ac,2026-11-17):loopback relay+agent 真实回路探针,doctor/launch/probe all/cleanup 四步自证。

### 修复(本轮)

- **check-docs.sh 门禁静默死**:`set -e` 下裸 `grep -qF`/`[ ]` 失败会在 `check` 打印 FAIL **之前**杀掉脚本——徽章漂移被逮住时表现为 exit 1 且**零输出**,门禁形同虚设还误导排查。判失败一律改走 if/else 收集退出码再交 `check` 汇总。
- **README 双语 tests 徽章漂移 13**:569 → 582(门禁 grep `^func Test` 全仓实测口径;运行口径 577 PASS + 1 既有 SKIP=非 Windows 守卫,见下)。v4.9.2 后四轮(商业化深推 +13/性能优化轮 +4/死码收口/F5 批 C)只记提交未回写徽章——正是 B8 门禁要防的「README 停在旧测试数」事故复发,而门禁自身静默死没拦住。
- AGENTS.md 口径刷新:Go 文件 256→264、测试 569→582、代码行 49K→50.8K、各包测试分布表重测、BENCHMARK.md 头部补「最新一轮见 CHANGELOG」指针(本文停在 v3.9.0 仅作历史基线)。

### 验证

- `go test ./... -count=1` 14 包全绿:577 PASS + 1 SKIP(`TestScriptNonWindowsGuard` 非 Windows 守卫,既有)+ 0 FAIL;`go vet ./...` 全清;`gofmt -l` 仅既有 CRLF 假红,本轮编辑文件全干净。
- `scripts/check-docs.sh` 修复后四项全 ok、exit 0(修复前:exit 1 零输出)。
- `scripts/release-tag.ps1 -Version v5.0.0` dry-run 验证:CHANGELOG 多节 `[未发布]` 全量并入 notes(脚本对每个未发布头 continue、只在版本化头 break——多轮堆叠不丢节),零 git 写操作。

## [未发布] — 2026-11-16 死码收口(agent-census 首扫真死桶)

出处:`AGENT_CENSUS_SWEEP_2026-11-16.md`(工作区根,agent-census v0.1.0 首扫)——go-rmm F5 真死 11 处发现/7 函数,本轮收口 4 函数:

- 删 `executors.WithFilePolicy`(file.go):functional option,生产+测试零引用;成对的 `WithAllowedRoots` 有 9 处测试引用保留。
- 删 `executors.IsGatedTaskType`(rulesgate.go):谓词零引用——门禁本体 rulesgate.go:180 直接读 `gatedTaskTypes` map(走别路),map 本体保留。
- 删 `proto.IsCommandTask`(payload_policy.go):判型谓词零引用;注册表 map/ExtractCommand/init 注册链不动。
- 删 `win32.FindWindow`(windows_windows.go+stub_other.go 平台孪生一对):精确 hwnd 查找零引用,消费者全走 ByProcess/ByTitle;其私有辅助 getWindowText/getClassName/getPid/getRect 均有 EnumWindows/screenshot 消费,不孤儿化。
- **未动(产品裁决留所有者)**:win32 输入注入三件套 `MoveTo`/`GetCursorPos`/`TypeText`(真死但属"能力在库不在链"裁决面——textinput 任务类型半成品能力位,接线或删待定)。
- 验证:`go build ./...`+`go vet ./...` 清;`go test ./... -count=1` 14 包全绿(577 PASS/0 FAIL/1 SKIP——SKIP 为非 Windows 守卫平台条件跳过,既有)。gofmt -l 告警均为既有 CRLF 假红(AGENTS 已有坑位实录),本轮 5 个编辑文件全 LF 干净。

## [未发布] — 2026-11-14 性能优化轮(基准驱动 5 热路径)

前兵遗留 `relay/perf_bench_test.go`(写到一半)续跑收口:修复编译(`ackCtx` declared-and-not-used)后成套。基准落盘 `build/perf/`(baseline/opt1..5/A-B/final)。

### 基准套件(9 基准 × 5 热路径,全真实代码路径)

1. **WS 中继转发吞吐**(真实环回:httptest+websocket.Dial,agent→serveAgent→EventBus;预 marshal 单帧重复发送剥离 agent 侧噪声;事件丢失即 Fatal 防假绿)
2. **心跳全路径环回 RTT**(agent→TouchWithStats→ack 回读)+ **纯管线**(marshal 口径)两口径
3. **task_result 处理管线**(直调真实 `onTaskResult`,非镜像)
4. **审计 hash chain 追加**(writeEntry 全工作量:双 marshal+sha256+落盘)
5. **tasks/audit 查询过滤分页**(handler 全路径,2000 任务/20000 审计规模)

### 基准表(前=首轮基线,后=收尾轮;allocs/B 确定性,ns 受环境噪声见坑位)

| 基准 | ns/op 前→后 | B/op 前→后 | allocs/op 前→后 |
|---|---|---|---|
| WSForwardLoopback(对照,路径未改) | 171µs→178µs | 1890→1890 | 28→28 |
| HeartbeatLoopback | 2.02ms→1.37ms | 3877→**3766** | 55→**54** |
| TaskResultPipeline | 127µs→(净轮 74µs) | 4281→**2952** | 22→**18** |
| AuditHashChainAppend | 78µs→(A/B min 341→84µs) | 897→**609** | 8→**7** |
| TasksQueryFilterPage | 43.0ms→**12.4ms** | 2.77MB→**226KB** | 26082→**2645** |
| TasksQueryStatusPage | 54.3ms→噪声带内 | 2.99MB→2.98MB | 31758→31751 |
| AuditQueryFilterPage | 421ms→**225ms** | 46.8MB→**13.9MB** | 280131→**52113** |
| AuditQueryNoFilter | 574ms→噪声带内 | 57.0MB→**23.9MB** | 280115→**260091** |
| HeartbeatPipeline(对照,路径未改) | 35µs→62µs | 1603→1603 | 27→27 |

### 优化账(5 项全部实测保留,无回退)

1. **审计查询单遍扫描** `queryAuditFile`(audit_query.go):旧 readAuditFile 三宗罪——`strings.Split(string(data))` 整文件转 string+逐行 `[]byte(line)` 双拷贝、无过滤也全量 unmarshal(profile:checkValid+decode ~45% CPU)、append 无预分配。新路径 splitLines 零切分行+**必要条件子串预筛**(精确匹配 agent_id/event 的行必含该值子串→子串不命中免整行 unmarshal,20000 条只解 4000)+结果预分配。**FilterPage allocs -81%/B -70%**;NoFilter B -58%(纯拷贝消除)。**预筛只对 JSON 安全串生效**(可打印 ASCII 无引号反斜杠)——等价性测试当场逮住首版 bug:混合安全/不安全 event 值时,不安全值的行必不含安全 needle,整体禁用该维预筛才是正确回退。
2. **task_result 重复 marshal 消除**(server.go):广播载荷 `mustMarshal(res)` 改 `env.Payload` 原始字节(与 TaskProgress 同语义)——Decode 后不必再全量 marshal 一遍 stdout。顺带把 case 体抽成 `onTaskResult` 单入口,**基准从镜像改直调真实代码**(防基准与实现漂移的假绿)。B/op -31%(4281→2952),allocs 22→18。
3. **tasks 查询下推存储层** `ListTasksQuery`(taskstore 接口+bbolt/MemoryStore 双实现+handler):agent/type 过滤在存储层用同款必要条件子串预筛——非命中记录免 json.Unmarshal,handler 内存过滤块删除。**FilterPage allocs -90%(26k→2.6k)/B -92%/ns -78%(43ms→9-12ms)**。Limit 语义按"已收录匹配数"计(与 ListTasks 的解析成功数上限同义,内存上界不变)。顺带修 MemoryStore.ListTasks(0) 返回 1 条的口径漂移(与 bbolt "0=不限"对齐;生产无调用方,仅测试口径)。
4. **wsx done channel 池化**(internal/wsx):WriteControl/WriteData 每消息 `make(chan error,1)` 改 sync.Pool 复用。**池化纪律:取→投递→收→还;ctx 先到期未收到不还池**(writer goroutine 迟到写入会污染下一个使用者——宁可丢给 GC);投递失败(writer 没见过)安全还池。心跳 RTT allocs 55→54、-111B/op(任务分发路径同受益,基准未覆盖如实注明)。
5. **审计 writeEntry 锁整合+缓冲复用**(audit.go):两段锁(读 prev→算 hash→写 prev)合并整条单锁——**修复理论链分叉**(两并发 writeEntry 可读到同一 prev 各自成链;现唯一调用方 writeLoop 串行不触发,单锁后并发也不可能分叉);sha256 实例+行编码缓冲(持久 json.Encoder,输出与 Marshal+`\n` 逐字节一致,hash 输入不变链校验兼容)复用。B/op -32%(897→609),allocs 8→7;同会话 A/B min 341µs→84µs。

### 调查不修(如实)

- **心跳环回 RTT 1.3-2ms**:profile 56% runtime.cgocall(websocket writeFrame 的写 syscall)——OS/库层延迟,应用侧无可修项(coder/websocket 依赖不动)。alloc 侧已由 wsx 池化收 1。
- **TasksQueryStatusPage allocs 持平(31.7k)**:status 索引路径 1980 条全命中,预筛无从跳过,unmarshal bound——该路径的优化(F1 状态索引)是前一轮的功,本轮如实记"无增量"。
- **NoFilter 审计查询 ns 带内**:无过滤时预筛不生效,剩余成本=全量 unmarshal+文件读,方向性优化已到"换缓存架构"的门槛(mtime 缓存),收益/风险比不划算,未做。

### 坑位实录

- **本机 ns/op 噪声 ±2-5x**(Ryzen 4750U/Win11,疑似 Defender/文件系统):两条**未改代码的对照基准**同会话波动 2.3x(WSForward min 389µs vs 169µs)。对策:结论以 allocs/op·B/op(确定性)为准;ns 只报同会话 A/B(git stash 定向回退 audit.go/wsx.go 同状态对拍)的 min-of-5;跨时段基线 ns 仅参考。
- git stash 往返把工作区改写 CRLF→gofmt 全线假红,`gofmt -w` 收口;stash -u 会把未跟踪的基准文件一并收走,定向 A/B 用 `git checkout HEAD -- <path>` 替代。
- **并发跑测试+基准互相污染**:全套测试曾假绿红(executors PowerShell 10s 超时×2)——重跑即绿;本机验证必须串行。
- 临时备份 .go 副本放进 build/ 会让 `go vet ./...` 报"两 package main"(vet 扫全部目录)——备份要么删要么改非 .go 后缀。

### 测试

- +4 测试:审计单遍扫描等价性(9 组过滤器×含 JSON 非安全值回退)+截断语义;ListTasksQuery 等价性(bbolt/MemoryStore×9 组)+Limit 口径。
- `go test ./... -count=1` **14 包全绿**;`go vet ./...` 全清;relay -race 28s 绿、wsx/taskstore -race 绿。

## [未发布] — 2026-11-13 UI 打磨与打包分发 + 发布纪律收口(工作区待审)

完整推进实录见 `docs/UI-PACKAGING-2026-11-13.md`。

### 发布纪律收口:checksum + Linux agent 包 + tag 脚本(收掉 UI 轮诚实清单 ③④)

- **SHA256SUMS.txt 随产物自动生成**:`release-package.ps1` 打包完成后对全部 zip/tar.gz 出校验和(小写十六进制+双空格,`sha256sum -c` 兼容;ASCII 落盘防 BOM/宽字符破坏校验)。INSTALL.md 增"第零步:校验产物完整性"(PowerShell Get-FileHash 逐行比对 / certutil / sha256sum -c 三环境)。
- **Linux Agent 交付包** `reins-agent-linux-<ver>.tar.gz`:GOOS=linux GOARCH=amd64 CGO=0 交叉编译,内含 reins-agent + `reins-agent-linux.service`(systemd 单元样例)+ `AGENT-LINUX-SETUP.md`(一页安装+**能力分账表**)+ config.example.json。能力分账如实标注:script(sh)/file/monitor(/proc)/update/shell(bash)/传输全链在 Linux 可用;powershell/screenshot/guiclick/watchdog 不可用(执行器按平台注册,Relay 据能力路由不误派);**power 执行器代码内有 Linux 分支但注册仅 Windows——如实标注不硬编**,Linux 电源操作走 script。验证边界如实声明:交叉编译+SHA256+WSL2 Ubuntu-24.04 真机 loopback 冒烟,未做裸机发行版矩阵端到端。
- **`scripts/release-tag.ps1` tag 纪律脚本**:CHANGELOG `[未发布]` 节 → release notes(不另写一份,防两处漂移)+ 产物 SHA256SUMS 并入 → annotated tag(`git tag -a -F`)。**默认 dry-run 零 git 写操作**,所有者加 `-Apply` 才真打;即使 -Apply 也只打本地 tag 绝不 push。tag 冲突/版本号形状/工作区脏三项前置检查。
- **`scripts/tarpack.go` 交付 tar 打包辅助(根治执行位坑)**:Windows 侧 tar 打出的 Linux 包所有条目 0644——二进制解压后无执行位,`systemctl start` 直接 permission denied(上轮 relay 包靠 deploy 脚本 chmod 兜住,agent 包无此层)。System32 bsdtar 不支持 `--mode`,Git Bash GNU tar 又有 `D:\` 冒号坑——tarpack 纯标准库 archive/tar,模式位定死(二进制/.sh 0755,其余 0644,ustar 最广兼容,gzip 零 mtime 重打包字节稳定)。两个 linux tar 全部改走 tarpack,WSL 解压实测:无需 chmod,`./reins-agent version` 直接执行。
- `release-package.ps1` 七步化:Linux relay/agent 同块交叉编译;tar 失败由 WARN 升级为 ERROR(防"静默半套产物"过 checksum 关);汇总清单含 SHA256SUMS.txt 并指路 release-tag.ps1。

### 修复(UI 打磨轮)
- **Web 控制台在 `-token` 模式下完全不可用**(生产标准形态即灭):前端从未实现凭证传递,所有 API/SSE 401 静默。修复:URL `?token=` 引导(消费后从地址栏抹掉)+ localStorage + 401 提示重试;SSE/下载/导出走新增的**同源 cookie `reins_token` 鉴权回退**——EventSource 无法带 Authorization header,而 query token 已被 H4 有意拒绝(防日志/Referer 泄漏);`authorize` 与 `authorizeRBAC` 两处统一为 header > cookie,后者顺带移除遗留的 query 回退泄漏面。+2 测试(含"header 错误时不得回落 cookie");原 H4 锚定测试保持通过。
- 审计注释承诺的 `/api/audit/tail` SSE 从未注册(文档承诺未兑现)——注释修正,由新查询端点接棒。
- 侧栏版本号双 v 显示;三份 web 拷贝漂移(`web/`、`console/web/` 落后 `relay/web/`)统一同步。

### 新增
- **任务历史/审计日志控制台页**:Tab 布局(概览/任务执行/任务历史/审计日志),任务历史支持状态/类型/Agent 筛选+分页+行展开详情(结果按类型渲染:screenshot 出图/file 解内容/命令出 stdout·stderr);审计日志支持时间范围/事件/Agent 筛选+分页+行展开(hash chain 字段可核对)+SIEM 导出;侧栏 Agent 增 tags/在线时长。
- **`GET /api/audit/query`**:审计 JSON 查询端点(过滤+分页+新的在前+hash chain 透出);**`GET /api/tasks` 增强**:status(状态索引)/agent_id/type 过滤+offset 分页+total。OpenAPI 同步。+4 测试。
- **打包分发**:`scripts/release-package.ps1`(版本化构建+打包前全量测试门禁→server/agent zip+linux tar.gz)、`scripts/start-relay.ps1`(前台/开机自启计划任务+防火墙+-UseTLS 自签 HTTPS)、`scripts/service-install.ps1`(Agent 服务 install/uninstall/start/stop/restart/status/**upgrade 探活回滚**/rollback)、`cmd/reins-gencert`(纯标准库自签证书工具,SAN 自动枚举网卡,+3 测试)、`docs/INSTALL.md`+`docs/AGENT-SETUP.md`(客户从零到能用一页说明,随包分发)。
- 打包产物验收:zip 原样解压→relay 起服务→agent 连上→CLI 任务真实执行→审计 hash chain 完整(2026-11-13 本机 loopback 实录)。

### 已知坑位(打包脚本实录)
- Windows PowerShell 5.1 把无 BOM UTF-8 .ps1 按 GBK 解析,中文字符串切碎致命令解析错误——三个 .ps1 加 UTF-8 BOM(与 installscript.go 记录的 PS 5.1 双坑同源)。
- Git Bash GNU tar 把 `D:\…` 解析为远程主机(host:file 语法)——Windows 下显式调用 System32 bsdtar。
- **`WriteAllLines` 在 Windows 写 CRLF,GNU `sha256sum -c` 不剥 `\r`——首版 SHA256SUMS.txt 四条全 "FAILED open or read"**(声称"sha256sum -c 兼容"实为假绿,真机校验逮住);改 `WriteAllText` 显式 LF。
- System32 bsdtar(Windows 自带)不支持 `--mode`,Windows 打的 tar 全条目 0644(见 tarpack 条目);WSL `systemd-analyze verify` 对 /mnt/d(DrvFs)上的 unit 会误报 executable/world-writable(挂载属性,非文件真实属性,拷到 Linux 文件系统即消失)。

## [v4.9.2] — 2026-10-27 dagflow 取消竞态根治 + 发布树净化

- **引擎修复(CI 两次 flake 的根因)**:Run 的取消清扫曾把 running 中、Action 已返回但 finish() 未落账的节点一并埋成 skipped——Action 内 cancel() 先于返回时,轮询循环在 ctx.Done 立即醒来抢先清扫,取消前已成功的动作被误标。修复:清扫只埋未启动节点,在跑节点等自身终态(Action 收已取消 ctx 会尽快返回,等待有界)。+50 次取消竞态回归锚,5 连跑 250 次全绿。
- 发布树净化:docs/launch(发帖剧本)与 PROMOTION_PLAN(增长策略)移出公开树,私存仓外。

## [v4.9.1] — 2026-10-27 task run 脚本语言按目标 OS 自动选择

- **改进(2026-10-27 WSL 实测发现)**:裸 `task run -t script -s <文件>` 此前恒默认 `lang=ps1`,发给 linux agent 被 Agent 拒绝("lang=ps1 仅在 Windows 上可用")才知要手动传 lang——现在 CLI 查询目标 Agent OS 自动选 `sh`(linux/macOS),失败回退 ps1 旧行为(向后兼容)。
- 优先级:显式 `-lang` flag > 目标 OS 推断 > 文件扩展名(.bat/.cmd) > ps1 默认。
- 新增 `-lang` flag(此前只能靠 `-f` payload JSON 显式指定);+1 测试(五分支优先级表);WSL 真机验收:同一裸命令从报错变 19ms 成功。

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
