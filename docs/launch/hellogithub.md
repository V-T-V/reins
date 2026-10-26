# HelloGitHub 投稿（公开发布当天提交）

> 入口：https://github.com/521xueweihan/HelloGitHub 开 Issue 投稿（每月 28 号刊）

## Issue 标题

推荐项目：Reins —— 自托管零信任远程运维平台

## Issue 正文

**项目名称**：Reins

**项目地址**：https://github.com/V-T-V/reins

**项目简介**：自托管零信任远程运维平台。单个 Go 二进制 Relay + 一行命令安装的 Agent + 50+ 命令 CLI，通过反向 WebSocket 穿透 NAT——被控端零入站端口。mTLS + 内置 CA + 多租户 RBAC + 防篡改审计 hash chain，并为 AI 代理提供规则门禁（自动化操作前必须先读项目运维规则并携带内容指纹 ack）。

**语言/技术栈**：Go（纯 Go 无 CGO，单二进制）

**项目截图**：

![demo](https://github.com/V-T-V/reins/raw/main/docs/demo/demo-overview.svg)

**推荐理由（特点）**：
1. 真正的单二进制自托管：`scp` 过去就能跑，bbolt 持久化零外部依赖（Redis 可选水平扩展）
2. 零入站端口的安全模型：Agent 只有出站连接，被管机器可以对互联网完全不可达
3. 企业级安全原语齐全：一机一证（mTLS+CA+CRL 吊销）、tenant:role:secret 三段式 RBAC、hash chain 审计 + SIEM 导出、双侧命令黑/白名单
4. AI 代理缰绳 `.reins/rules.md`：规则门禁机制，规则变更旧 ack 立即失效
5. 运维编排：批量分发（重试+SSE 进度）、DAG 依赖编排（失败级联跳过）、部署 playbook、进程守护
6. 工程质量：48K 行 / 559 测试（CI 带 race）/ 三平台编译门禁 / 每次 Release 附 SBOM + SHA256 清单

**适用人群**：需要管理多台 Windows/Linux 机器的运维工程师、MSP、自托管爱好者、想给 AI 代理配运维工具链的开发者

**LICENSE**：MIT

**中文文档**：有，README.zh-CN.md（架构图/配置/安全/多租户/批量分发/DAG 编排/规则门禁全覆盖）
