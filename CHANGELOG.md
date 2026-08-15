# Changelog

本文件记录 go-rmm 的版本演进。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

> 版本单一真相源：`git tag`（Makefile 与 quickstart 通过 `git describe --tags` 读取）。文档内的静态版本号会随每次发版手动对齐。

## [Unreleased]

## [4.4.0] - 2026-08-13

### 产品化：交付与治理打磨（把「能 build」补成「能交付」）

#### 新增
- **安装器** `scripts/install.sh` / `scripts/install.ps1`：从 GitHub Release 下载二进制一键安装（支持 `--component agent|relay|cli`、`--version`、`--service` 注册系统服务）。修复 `make dist` 引用不存在的安装脚本导致的 target 报错。
- **单机一键部署** `docker-compose.yml` + `.env.example`：`docker compose up -d` 起一个带健康检查的 Relay。
- **发布流水线** `.github/workflows/release.yml`：tag 触发 → 构建三组件 → 打包 → 构建并推送 Docker 镜像到 ghcr.io → 触发 goreleaser。补齐「CI 只验证不发布」的交付断层。
- **配置示例** `config.example.json`：从 `DefaultConfig()` 抽取，给新用户配置样板。
- **治理文件**：`LICENSE`(MIT)、`CHANGELOG.md`、`CONTRIBUTING.md`。

#### 修复
- `scripts/quickstart.ps1`：去掉硬编码的 `D:\M_X_M\go-rmm`，改用脚本相对路径探测源码目录，外部用户可直接运行。
- `deploy/openapi.yaml`：合并重复的 `/api/tasks:` 键（原 post 与 get 分属两个同名键，YAML 解析时后者覆盖前者）。

#### 变更（版本身份统一）
- 统一发布身份到实际仓库 `V-T-V/go-rmm`：`.goreleaser.yaml` 的 `release.github.owner`、Helm 镜像 `ghcr.io/v-t-v/go-rmm-*`、quickstart 默认版本、README/AGENTS 版本号全部对齐到 `v4.5.0`。
- （说明）`go.mod` 模块名 `github.com/QiuShichang/go-rmm` 保持不变，避免破坏所有 import 路径；模块名与仓库 URL 不要求一致。

## [历史] - 4.x 及之前

- 反向 WebSocket 中继穿透 NAT + Windows Agent 服务化 + 9 类任务执行器。
- 安全体系：mTLS + 内置 CA + CRL + RBAC + hash chain 审计 + SIEM 导出。
- Web 控制台 + Go SDK + Helm chart + goreleaser 跨平台发布配置。
- 详见 `AGENTS.md` 的深度推进记录与 `docs/GO-RMM-USER-GUIDE.zh-CN.md`。
