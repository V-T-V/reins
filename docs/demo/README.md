# Demo 素材拍摄清单(B4)

公开发布前需要三段素材,全部从**同一套本地环境**录制(relay 本机 + 1 台 Windows Agent),保证命令可复现。录制工具任选:ShareX(Windows,录屏+GIF 导出)、Peek(Linux)、`asciinema rec`(终端,配合 `agg` 转 GIF)。

## ① demo-overview.gif —— README 首屏(60-90 秒,决定转化率的第一要素)

脚本(按序录,一镜到底):

```bash
# 1. 起 relay(终端 1,能看到 token 打印)
./go-rmm-relay -addr :7780

# 2. 装 agent(终端 2,另一台 Windows/VM)——展示"一行命令"
iex (((irm 'http://localhost:7780/install.ps1') -replace '^\uFEFF'))

# 3. 回到操作机(终端 3)
go-rmm agent list                          # agent 已上线
go-rmm task run -a <agent> -c "hostname"   # 远程执行
go-rmm monitor top -a <agent>              # TUI 监控(视觉最丰富的收尾帧)
```

要求:字号 ≥16pt、深色背景、每步停留 2-3 秒;GIF 压到 <8MB(`ffmpeg -i in.mp4 -vf "fps=12,scale=960:-1" -loop 0 out.gif`)。完成后替换 README.md/README.zh-CN.md 顶部的 TODO 注释。

## ② demo-dag.gif —— 批量/DAG 编排(30-45 秒)

```bash
go-rmm batch start --tag env=prod -c "Get-Date" --retries 2
go-rmm batch watch <batch_id>              # 实时进度条
go-rmm dag apply -f docs/demo/flow.json    # 多阶段流水线
go-rmm dag status <dag_id> -w              # 逐节点状态翻转
```

flow.json 用 README 里 upgrade-flow 三节点例即可。用于官网 hero、r/selfhosted 帖、B 站视频素材。

## ③ demo-rules-gate.gif —— AI 规则门禁剧场(45-60 秒,差异化叙事专用)

```bash
# 场景:agent 开了 rules_gate: true,项目根有 .go-rmm/rules.md
go-rmm task run -a <agent> -c "..."        # ① 被拒:错误指引 rules read
go-rmm rules read -a <agent>               # ② 读规则全文+缓存 ack
go-rmm task run -a <agent> -c "..."        # ③ 同一条命令,通过
# ④ (编辑规则文件后)再次执行 → 旧 ack 失效被拒 → 重读后通过
```

这是"AI 原生运维"叙事的核心素材,配文金句:"先读规则,再碰服务器。"用于 Show HN 评论区、知乎/B 站视频、官网 AI 页。

## 分发规格

| 用途 | 规格 |
|---|---|
| README 首屏 | GIF ≤8MB / 960px 宽 / 12fps |
| 官网 hero | 同素材 mp4(webm 备用) |
| B 站/YouTube | 60-90s 竖版 1080×1920(前 3 秒出画面)+ 5 分钟横版架构讲解 |
| 知乎/掘金 | GIF 直接内嵌 |
