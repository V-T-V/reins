# 贡献指南

感谢参与 go-rmm！本指南帮助你在本地跑通开发环境并提交高质量变更。

## 开发环境

- Go 1.25+
- 依赖：`go mod download`

```bash
git clone https://github.com/V-T-V/go-rmm.git
cd go-rmm
go mod download
```

## 常用命令（Makefile）

| 命令 | 作用 |
|------|------|
| `make all` | 编译 Relay + Agent + CLI 到 `bin/` |
| `make test` | 单元测试（含覆盖率） |
| `make test-race` | 并发安全测试（race detector，需 CGO） |
| `make lint` | golangci-lint 静态检查 |
| `make vuln` | govulncheck 漏洞扫描 |
| `make docker` | 构建本地 Docker 镜像 |
| `make release` | goreleaser 跨平台发布 |
| `make dist` | 构建发行包（zip/tar.gz + 安装脚本） |
| `make ci` | 完整 CI 流程（vet + test + race + lint） |

## 提交前必过门禁

```bash
make ci    # vet + test + test-race + lint
```

CI（`.github/workflows/ci.yml`）会自动跑同样的验证；`.github/workflows/release.yml` 在打 tag 时发布。

## 架构约定

- **三组件**：`relay/`（中继服务端）/ `agent/`（被控端，Windows 服务或 Linux daemon）/ `cmd/go-rmm/`（CLI 客户端）。共享 `proto/`（协议层）、`executors/`（任务执行器）、`client/`（Go SDK）。
- **协议层** `proto/`：Envelope + 消息类型，二进制帧 over WebSocket。新增消息类型在此登记。
- **执行器** `executors/`：每类任务一个执行器，遵循统一接口注册。
- **工具错误永不抛出**：执行失败作为结果回填，由上层决策重试。

## 版本与发布

- 版本单一真相源：`git tag`（语义化版本 `vMAJOR.MINOR.PATCH`）。
- 发布流程：`git tag v4.4.0 && git push --tags` → `release.yml` 自动构建三组件 + Docker 镜像（ghcr.io/v-t-v/）+ goreleaser GitHub Release。
- 发版后更新 `CHANGELOG.md`，并核对 README/quickstart/Helm values 的静态版本号已对齐。

## 部署相关

- 单机试用：`docker compose up -d`（见 `docker-compose.yml`）。
- 生产部署：见 `DEPLOYMENT.md`（mTLS、Nginx 反代、systemd、Helm）。
