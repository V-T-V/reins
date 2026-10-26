# V2EX · 分享创造 · 首发帖（公开发布当天发布）

> 节点：分享创造。标题建议：`Reins — 自托管零信任远程运维平台，单个 Go 二进制，被控端零入站端口`
> 发帖前自检：未登录浏览器走完 README quickstart 全流程确认可用。

---

写了个远程运维工具 Reins（缰绳的意思），先放地址：https://github.com/V-T-V/reins

**一句话**：单个 Go 二进制的 Relay + 一行命令安装的 Agent + 50+ 命令的 CLI，让你从自己电脑上一个终端管一个舰队——被控端不开任何入站端口（Agent 反向 WebSocket 出站连回 Relay，NAT/家庭网络直接穿）。

**为什么自己写**：
1. TeamViewer/AnyDesk 的商用检测烦够了，RustDesk 解决了桌面远控但我要的是运维平面（批量分发、部署编排、监控、审计），不只是屏幕
2. Teleport 2024 年换了许可证（免费档限公司 100 人以下），而且它是访问平面不是运维平面
3. MeshCentral 的 Agent 因为被诈骗分子滥用，现在下载经常被杀软拦
4. TacticalRMM 是真开源 RMM 里最完整的，但 Django+Vue+Go+消息队列全家桶，我就想要个 `scp 二进制 && 运行` 的东西

**几个我觉得值得一提的设计**：
- Agent 只有出站连接。你所有机器可以对互联网完全不可达，照样远程运维（NIST 800-207 说的"消除隐式网络信任"的最直白实现）
- mTLS + 内置 CA + CRL 吊销，一机一证；多租户 RBAC（tenant:role:secret 三段式 token）
- 审计日志是 hash chain 防篡改的，能导出 CEF/Syslog 给 SIEM——审计你的审计员
- 给 AI 代理上了缰绳：被管项目在仓库根放 `.reins/rules.md` 写运维规则（"勿营业时段杀壳进程"这类），AI/自动化必须先 `rules read` 读规则拿到内容指纹 ack 才能执行变更，规则一改旧 ack 立即失效。拒绝信息不回显指纹，跳过阅读直接重放没用
- DAG 编排：备份→升级→健康检查→失败回滚这种流程画依赖图，失败节点下游整枝跳过（零投递），软依赖照跑
- 大文件传输：并发分片 + 断点续传 + per-chunk SHA256 + 内网直连优先中继回落

**规模**：253 个 Go 文件 / 48K 行 / 559 个测试（CI 带 race）/ 6 平台二进制（每个 Release 附 SBOM + SHA256 清单）。

**诚实边界**：Windows Agent 一等公民，Linux Agent 约 85%（script/monitor + 15 组方言化运维命令，实机验证进行中）；单 Relay 是默认形态（Redis 水平扩展接口已留）。

**杀软会报毒吗**：这个品类的结构性别问——MeshCentral 签名版照样被 Kaspersky 报过。判定标准和处置方法写在 [docs/antivirus.md](../../antivirus.md)，每个 Release 附哈希清单。

MIT，不收费没有 Pro 版没有企业版，欢迎拍砖/提 issue/贡献 Linux 实机验证。接下来想做 MCP 适配层（让 Claude/Cursor 这类代理原生接管运维），有想法的欢迎来聊。

---

*回复常见问题可以先准备：*
- *和 RustDesk 什么关系？* —— 互补不竞争：RustDesk 是"看到屏幕"，Reins 是"跑命令/传文件/管舰队"。都自托管。
- *安全吗？* —— mTLS/CA/CRL/RBAC/审计链/双侧命令策略都有，黑名单默认 32 条且是 best-effort（文档写明），严格环境开 allow-only 白名单。
- *能用 Docker 跑吗？* —— compose/Helm/systemd 全有，quickstart 四行。
