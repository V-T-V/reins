# Reins MCP Server 接入指南

`reins-mcp` 把 Reins 的远程运维能力暴露为 [Model Context Protocol](https://modelcontextprotocol.io) 工具(14 个,带 JSON Schema 输入描述),让 Claude Desktop、Cursor、ZCode 等支持 MCP 的 AI 客户端**原生驱动远程舰队**——不是解析 CLI 文本,是标准工具调用协议。

## 安装

从 [Releases](https://github.com/V-T-V/reins/releases) 下载对应平台的 `reins-mcp`(六平台齐备),放进 PATH;或源码构建:

```bash
go install github.com/V-T-V/reins/cmd/reins-mcp@latest
```

## 配置

环境变量(与 CLI 同名):`REINS_RELAY`(默认 `http://127.0.0.1:7780`)、`REINS_TOKEN`。

### Claude Desktop

配置文件路径:Windows `%APPDATA%\Claude\claude_desktop_config.json`,macOS `~/Library/Application Support/Claude/claude_desktop_config.json`

```json
{
  "mcpServers": {
    "reins": {
      "command": "reins-mcp",
      "env": {
        "REINS_RELAY": "http://your-server:7780",
        "REINS_TOKEN": "your-token"
      }
    }
  }
}
```

### Cursor / 通用 MCP 客户端

`.cursor/mcp.json`(或等价的客户端配置)同构:

```json
{
  "mcpServers": {
    "reins": {
      "command": "reins-mcp",
      "env": { "REINS_RELAY": "http://your-server:7780", "REINS_TOKEN": "your-token" }
    }
  }
}
```

## 可用工具(14)

| 工具 | 说明 |
|---|---|
| `reins_list_agents` | 列出在线 Agent(id/主机名/系统/能力)——其他工具的 agent_id 从这取 |
| `reins_status` | Relay 健康状态 |
| `reins_run_command` | 远程执行 PowerShell/shell 命令并返回 stdout |
| `reins_task_run` | 下发 9 类执行器任意任务(powershell/script/file/screenshot/browse/guiclick/update/power/monitor) |
| `reins_task_get` / `reins_task_list` | 任务查询 |
| `reins_batch_run` | 按标签批量下发 |
| `reins_sysinfo` | 远端系统体检(CPU/内存) |
| `reins_power` | 电源操作(shutdown/restart/sleep) |
| `reins_cron_list` / `reins_cron_add` / `reins_cron_delete` | 定时任务管理 |
| `reins_enroll_token` | 生成一次性注册令牌 |
| `reins_list_relay_tools` | 列出 Relay 侧全部 REST 工具(33+,含隧道/文件传输等) |

## 规则门禁:换客户端不换闸

MCP 是接入层,`.reins/rules.md` 的 ack 校验在**执行层**(Agent 侧):变更类任务(powershell/script/browse/guiclick/update/power)无 ack 或过期 ack 直接拒绝,规则文件一改旧 ack 立即失效。CLI、SDK、MCP 三种客户端走的是同一道闸——给 AI 代理的缰绳不因接入方式而松。

## 实测样例(真实会话)

```jsonc
// stdin
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reins_run_command","arguments":{"agent_id":"agent-WS-01","command":"hostname","timeout":10}}}

// stdout
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"reins-mcp"},...}}
{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\n  \"stdout\": \"WS-01\\n\"\n}"}],"isError":false}}
```

日志走 stderr(stdout 是协议通道);工具内部错误按 MCP 语义回 `isError:true`。
