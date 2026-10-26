# 阮一峰《科技爱好者周刊》投稿（公开发布当天提交）

> 入口：https://github.com/ruanyf/weekly 开 Issue
> 标题格式必须带前缀：【开源自荐】。一句话 ≤30 字。

## Issue 标题

【开源自荐】Reins：单二进制自托管远程运维平台，被控端零入站端口

## Issue 正文

项目名称：Reins（缰绳）

项目地址：https://github.com/V-T-V/reins

一句话介绍：Go 实现的自托管零信任远程运维平台——单个二进制、反向 WebSocket 穿 NAT、mTLS+RBAC+防篡改审计链，并为 AI 代理提供"先读规则再动手"的规则门禁。

详细介绍：

- 单个 Go 二进制的 Relay + 一行命令安装的 Agent（`iex (irm .../install.ps1)`），50+ CLI 命令覆盖诊断/执行/传输/部署/监控/守护
- Agent 只有出站连接，被管机器对互联网完全不可达也能远程运维
- 企业安全原语：一机一证（mTLS+CA+CRL）、多租户 RBAC、hash chain 审计日志 + SIEM 导出
- 独有：被管项目在仓库根放 `.reins/rules.md` 运维规则，AI/自动化必须先读规则拿内容指纹 ack 才能执行，规则一改旧 ack 失效
- MIT 协议，559 测试，每次 Release 附 SBOM 与 SHA256 清单

![demo](https://github.com/V-T-V/reins/raw/main/docs/demo/demo-overview.svg)

中英文 README 均有，Docker/Helm/systemd 部署齐备。
