# go-rmm 功能地图（verify-go-rmm）

全量回归顺序 = `probe all` 顺序。按需打开单文件省 token；
改路由/鉴权/投递/执行器/审计后**同一次提交里**更新对应 feature 文件。

未覆盖面（如实）：SSE 事件流/批量/DAG/隧道/文件传输/mTLS/console UI——e2e/ 已有 console 套件
（Playwright 驱动），本技能补「日常开发一条命令自证」的核心回路。

| # | feature 文件 | 覆盖 | 探针 |
|---|---|---|---|
| 1 | [healthz.md](healthz.md) | 深浅双形状健康契约（P1-S7） | `probe healthz` |
| 2 | [agents-auth.md](agents-auth.md) | Bearer 鉴权契约 + Agent 清单 | `probe agents-auth` |
| 3 | [task-execution.md](task-execution.md) | powershell 真实任务执行回路 | `probe task-run` |
| 4 | [audit-chain.md](audit-chain.md) | 审计落账 + hash chain 衔接 | `probe audit-chain` |
