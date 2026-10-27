# Reins 发版计划(作战手册)

> 版本:2026-10-27 · 首发 **v4.9.1** · 本文档是发布的唯一执行入口,所有动作配操作命令与成品文案路径。
> 前置文档:[推广方案](../PROMOTION_PLAN.md)(调研底座) · [发布文案包](./README.md)(发帖入口)

---

## 0. 当前就绪状态(发版前盘点,全部 ✅)

| 资产 | 状态 | 位置 |
|---|---|---|
| 仓库 | `V-T-V/reins` 私有,main=origin/main 同步,历史已脱敏(生产 IP 全史 0 残留) | github.com/V-T-V/reins |
| 首发版本 | **v4.9.1 draft,45 产物**(cli/agent/relay/mcp × darwin/linux/windows × amd64/arm64,全带 SBOM + checksums.txt) | Releases 页 |
| 流水线 | CI(568 测试+race+lint 0 issues)/Release/Pages 三工作流全绿 | .github/workflows/ |
| 双语 README | 英文主 + zh-CN,指标实测(256 文件/48.9K 行/568 测试),对比表/反滥用/误报节 | README.md / README.zh-CN.md |
| Demo | 动画 SVG 终端演示(真实会话回放)已嵌双语首屏 | docs/demo/demo-overview.svg |
| 官网 | 单文件 landing(缰绳 hero/对比表/800-207 对照),转公开自动上线 | docs/site/index.html |
| 发布文案 | 六件套:V2EX/HelloGitHub/周刊/Show HN/awesome-selfhosted/awesome-mcp-servers | docs/launch/*.md |
| 旗舰软文 | 《当 AI 开始替你重启服务器》全文+五渠道适配+事实核对 | docs/launch/articles/rules-gate.md |
| 安全 | SECURITY.md(真实邮箱/披露时限)/docs/antivirus.md(误报 SOP) | 仓库根/docs |
| MCP | v4.9.1 内置,真机管道验证过;接入指南齐 | docs/mcp.md |
| Linux | Ubuntu 24.04/WSL2 实机验证全链路(诚实边界已升级) | README 双语已更新 |
| 仓库元数据 | 描述/16 topics/Discussions 已配 | GitHub Settings |

**发版前最后自检(D0 上午,5 分钟)**:
```bash
git fetch origin && git status -sb          # 应显示与 origin/main 同步
gh run list --repo V-T-V/reins --limit 3    # 最近 CI 应全绿
gh api repos/V-T-V/reins/releases -q '.[] | select(.draft==true) | .tag_name'  # 应见 v4.9.1
```

---

## 1. D0 · 发布日(选周二/三,避开节假日)

### 1.1 上午 · 平台动作(约 30 分钟)

| # | 动作 | 操作 |
|---|---|---|
| 1 | **转公开** | GitHub → Settings → General → 拉到底 Danger Zone → Change repository visibility → **Public** |
| 2 | 确认官网 | 转公开后 pages.yml 自动跑;2 分钟后访问 `https://v-t-v.github.io/reins/`(若未自动启用:Settings → Pages → Source 选 GitHub Actions,手动触发一次 workflow) |
| 3 | **Publish 首发 Release** | Releases 页 → v4.9.1 draft → **Publish release**(45 产物即刻可下载) |
| 4 | Discussions 欢迎帖 | 新建 Announcements 分类帖:"Reins v4.9.1 发布——欢迎提问/报告/贡献"(3-5 句,附 quickstart) |
| 5 | 仓库首页预览 | 未登录浏览器打开仓库页:README 徽章全亮、demo SVG 动画播放、对比表渲染正常 |

### 1.2 下午 · 中文首发三连(第一条流量波)

| 顺序 | 渠道 | 成品 | 入口 |
|---|---|---|---|
| 1 | **V2EX 分享创造** | [v2ex.md](./v2ex.md) | v2ex.com → 分享创造节点(发帖前:未登录浏览器把 quickstart 走一遍) |
| 2 | **HelloGitHub 投稿** | [hellogithub.md](./hellogithub.md) | github.com/521xueweihan/HelloGitHub 开 Issue(每月 28 号刊,投晚一期也值) |
| 3 | **阮一峰周刊自荐** | [ruanyf-weekly.md](./ruanyf-weekly.md) | github.com/ruanyf/weekly 开 Issue,标题带【开源自荐】 |

> 当晚值守:V2EX 帖子前 2 小时每 15 分钟刷一次回复;所有技术问题当天答,答不了的引到 Discussions。

### 1.3 D0 值守纪律

- 每个 issue/Discussion **24h 内响应**(这是 star 转化率的最大变量)
- 杀软误报类 issue 一律按 [docs/antivirus.md](../antivirus.md) 口径回答并贴 SHA256 校验命令
- 词汇纪律全渠道执行:说"远程运维平台",不说"远控/RAT/silent"(清单见 [launch/README.md](./README.md))

---

## 2. W1-W2 · 第一幕余波(中文渠道纵深)

| 天 | 动作 | 素材 |
|---|---|---|
| D+1~3 | 知乎:回答 2-3 个既有高流量问题(TeamViewer 商用检测/Teleport 限制/开源运维面板)+ 专栏发架构深潜 | 选题库 §5.3 #1/#4 |
| D+3~7 | 掘金/CSDN:踩坑长文首发(编码考古或 Job Object 杀进程树——NOTES 里现成素材) | 选题库 #10/#11 |
| D+5~7 | B 站:60-90 秒竖版 demo(前 3 秒必须出画面)+ 5 分钟架构讲解 | [demo 拍摄指南](../demo/README.md) |
| D+7 | **awesome-selfhosted PR**(Remote Access 分类,当前 14 项中完整 RMM 仅 1——空位明显) | [awesome-selfhosted.md](./awesome-selfhosted.md) |
| D+7~14 | 腾讯云/阿里云开发者社区投稿对比评测(RustDesk 扩散路径复刻) | 选题库 #2/#3 |
| 持续 | GoCN 社区群联系每日新闻编辑;电鸭社区软性经验帖 | — |

**W1 复盘**(数据锚点):GitHub star / fork / clone、Release 下载量(去重 IP)、install.ps1 拉取、官网 UV、V2EX 帖 UV。目标见 §6。

---

## 3. W4-W8 · 第二幕(英文脉冲,第二波流量)

> 前提:W1-W2 攒到 100+ star;若未达到,顺延两周并先在中文渠道补内容。

| 步 | 动作 | 细节 |
|---|---|---|
| 1 | **Show HN 首发**(周二-四美东 8:00-11:00,即北京 20:00-23:00) | 标题与首评弹药齐备:[show-hn.md](./show-hn.md);发帖后第一小时守帖秒回;**绝不组织点赞** |
| 2 | 次日 | **r/selfhosted** 主帖(用 HN 评论区沉淀的 FAQ 改写;版规:首发可主帖,更新进周帖) |
| 3 | 当周 | **r/golang** "Show r/golang:" 经验帖(lessons-learned 框架,repo 放文末) |
| 4 | 当周 | dev.to 同步英文文(canonical URL 指回博客/仓库) |
| 5 | 当周 | **TLDR 免费投稿** submissions@tldr.tech + **Changelog** changelog.com/news/submit |
| 6 | 每周五 | r/msp Weekly Promo thread 露面(TacticalRMM 主场,长期经营) |
| 7 | W6 | YouTube 测评外联:Lawrence Systems 类 MSP 频道送测评授权(NetBird 验证路径) |

---

## 4. W8-W12 · 第三幕(MCP/AI 事件,差异化引爆)

| 步 | 动作 |
|---|---|
| 1 | 第二次 Show HN:`Show HN: Reins MCP – remote operations your AI agent can run, after it reads the rules` |
| 2 | **awesome-mcp-servers PR**(RMM 品类在列为零的独占窗口):[awesome-mcp-servers.md](./awesome-mcp-servers.md) |
| 3 | 旗舰软文发布:知乎/掘金/B 站三路同发 [articles/rules-gate.md](./articles/rules-gate.md) |
| 4 | 即刻/Twitter AI 圈:MCP 配置一条推(附 Claude Desktop JSON 截图) |
| 5 | **Go 夜读**议题提交(github.com/talkgo)——《用 49K 行 Go 写零信任远程运维平台》 |
| 6 | **GopherChina CFP** 投稿(经百格活动页/主办方公众号) |
| 7 | **2026-12-22 后:awesome-go PR**(项目须满 5 个月,CONTRIBUTING 硬规定——日历上标好) |

---

## 5. 发版节奏与工程纪律(常态)

- **月度 Release**:每月一个 minor(功能)或 patch(修复);tag 即真相源(CHANGELOG 头部对齐)
- **打 tag 标准流程**:
  ```bash
  make check-docs && go test ./... && git tag -a v4.9.2 -m "..." && git push origin v4.9.2
  # release.yml 自动:三平台×四组件 + SBOM + ghcr 镜像 + draft
  # 网络被干扰时的备用通道:gh api POST /git/refs 建轻量 tag ref 同样触发流水线(已实战验证)
  ```
- **每次发版后**:二进制提交微软误报门户 + VT 评注(antivirus.md SOP)
- **口径门禁**:`make check-docs` 进每次 PR(README 徽章=实测数,AGENTS 版本头=CHANGELOG 最新)
- 新增组件进 goreleaser 时:**builds 与 archives 两处都要加**(枚举制,漏 archives 会静默少产物——v4.9.0 首跑实证)

---

## 6. KPI 与止损线

| 指标 | D0 | W2 | W6 | W12 |
|---|---|---|---|---|
| GitHub star | 30-80 | 100-300 | 300-600 | 600-1000 |
| Release 下载(去重) | 50 | 300 | 800 | 2000 |
| Discussions/issue 响应 | 24h 内 | 同 | 同 | 同 |
| 官网 UV | 300 | 3k | 10k | 25k |
| awesome-selfhosted | — | PR 提交 | 收录 | — |

- **北极星是采用不是 star**:下载量/install.ps1 拉取/issue 活跃度为主线,star 只作过程指标
- 止损调整:Show HN 首战进不了前 3 页 → 不重发,带 MCP 钩子两周后再战;中文矩阵两周 <100 star → 检查 quickstart 转化(是否有反馈卡在安装环节)再投内容

## 7. 应急预案

| 场景 | 处置 |
|---|---|
| **杀软大规模误报** | antivirus.md 口径回应 + 当天提交微软申诉 + VT 评注;若主流引擎(Microsoft/Kaspersky/ESET)报毒先自查哈希是否被劫持 |
| **被安全媒体/威胁报告点名滥用** | RustDesk 危机模板:承认双用途现实 + 列出技术对抗(出站连接/证书吊销/审计链/无隐蔽功能)+ 渠道管控说明;不删帖不狡辩 |
| **HN/Reddit 恶意差评** | 技术质疑正面答(弹药见 show-hn.md 四问);情绪贴不复怼;诚实边界重复陈述 |
| **网络 SNI 干扰(github.com 不通)** | git 重试循环(HTTP/1.1);仍不通走 Git Data API 提交/换 tag(api.github.com 独立通道,全程实战验证过) |
| **Release 流水线 504/瞬时挂** | `gh run rerun <id> --failed` 重跑即可(已验证) |
| **首发后发现 bug** | patch 版本当周修(v4.9.2);不作废首发 tag;CHANGELOG 如实记录 |

---

## 附:发布日单页清单(打印贴墙)

```
□ 1. 转公开(Settings → Danger Zone → Public)
□ 2. 官网自动上线确认(v-t-v.github.io/reins)
□ 3. Publish v4.9.1(45 产物)
□ 4. Discussions 欢迎帖
□ 5. V2EX 分享创造(docs/launch/v2ex.md)
□ 6. HelloGitHub 投稿(docs/launch/hellogithub.md)
□ 7. 阮一峰周刊自荐(docs/launch/ruanyf-weekly.md)
□ 8. 当晚值守:回复一切,误报按 antivirus.md 口径
□ 9. 日志:记 star/下载/UV 基线,填 §6 表
```
