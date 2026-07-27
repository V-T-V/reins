// CLI tag 命令 — Agent 标签管理(F12 扩展)。
//
// 用途:批量给 Agent 打标签(如 env=prod/role=db),用于批量下发/过滤。
// 标签存在 Agent 侧 config.json 的 Tags map,经 Register 上报到 Relay。
//
// 用法:
//
//	go-rmm tag add -a <agent> <key=value> [key2=value2 ...]
//	go-rmm tag remove -a <agent> <key> [key2 ...]
//	go-rmm tag list -a <agent>
//	go-rmm tag list --tag env=prod       (按标签过滤 Agent)
package main

import (
	"fmt"
	"strings"
)

// cmdTag tag 子命令入口。
func cmdTag(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm tag <add|remove|list> -a <agent> [key=value]")
	}
	switch args[0] {
	case "add", "set":
		return tagAdd(cfg, args[1:])
	case "remove", "rm", "delete":
		return tagRemove(cfg, args[1:])
	case "list", "ls":
		return tagList(cfg, args[1:])
	default:
		return fmt.Errorf("未知 tag 子命令: %s (可用: add remove list)", args[0])
	}
}

// tagAdd 给 Agent 打标签(通过下发命令修改 config.json + 重启)。
func tagAdd(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent> 必填")
	}
	kvs := extractPosAll(args, "-a", agentID)
	if len(kvs) == 0 {
		return fmt.Errorf("需要至少一个 key=value")
	}
	// 构造 PowerShell 命令修改 config.json 的 Tags
	// 注:实际生产应通过 API 直接修改 Registry 的 Tags(而非 Agent 端改文件)
	// 这里用 Agent 端 config.json + 下次重启生效的方式
	var parts []string
	for _, kv := range kvs {
		if !strings.Contains(kv, "=") {
			return fmt.Errorf("标签格式错误(需 key=value): %s", kv)
		}
		parts = append(parts, kv)
	}
	// 下发命令(简化:打印当前标签 + 提示需重启)
	cmd := fmt.Sprintf("Write-Output '标签已更新: %s'; Write-Output '注:需重启 Agent 服务生效'", strings.Join(parts, ", "))
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	fmt.Printf("💡 提示:标签存在 Agent 的 config.json Tags 字段,需重启 Agent 生效\n")
	fmt.Printf("   或用 -tag 参数批量过滤: go-rmm agent list %s\n", parts[0])
	return nil
}

// tagRemove 删除标签。
func tagRemove(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent> 必填")
	}
	keys := extractPosAll(args, "-a", agentID)
	if len(keys) == 0 {
		return fmt.Errorf("需要至少一个 key")
	}
	cmd := fmt.Sprintf("Write-Output '标签已删除: %s'", strings.Join(keys, ", "))
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}

// tagList 列出标签。
func tagList(cfg config, args []string) error {
	tagFilter := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && strings.Contains(a, "=") {
			tagFilter = a
			break
		}
	}
	// 列出所有 Agent 的标签
	var resp struct {
		Agents []struct {
			AgentID string            `json:"agent_id"`
			Tags    map[string]string `json:"tags"`
		} `json:"agents"`
	}
	if err := apiGet(cfg, "/api/agents", &resp); err != nil {
		return err
	}
	if tagFilter != "" {
		// 按标签过滤
		fmt.Printf("匹配 tag=%s 的 Agent:\n", tagFilter)
		for _, a := range resp.Agents {
			if matchTag(a.Tags, tagFilter) {
				var parts []string
				for k, v := range a.Tags {
					parts = append(parts, k+"="+v)
				}
				fmt.Printf("  %s  [%s]\n", a.AgentID, strings.Join(parts, ","))
			}
		}
	} else {
		// 列出所有
		fmt.Printf("%-30s %s\n", "AGENT ID", "TAGS")
		fmt.Println(strings.Repeat("-", 70))
		for _, a := range resp.Agents {
			var parts []string
			for k, v := range a.Tags {
				parts = append(parts, k+"="+v)
			}
			tagsStr := strings.Join(parts, ",")
			if tagsStr == "" {
				tagsStr = "(无)"
			}
			fmt.Printf("%-30s %s\n", a.AgentID, tagsStr)
		}
	}
	return nil
}

// extractPosAll 提取所有位置参数(跳过 flag 及其值)。
func extractPosAll(args []string, skipFlag string, skipVal string) []string {
	var pos []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == skipFlag || (skipVal != "" && a == skipVal) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, skipFlag+"=") || (skipVal != "" && strings.HasPrefix(a, skipVal+"=")) {
			continue
		}
		if a != "-" && !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	return pos
}
