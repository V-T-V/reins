// CLI config 命令 — 远端配置文件管理(F7 扩展)。
//
// 用途:读取/修改/比对远端配置文件(INI/JSON/YAML/XML)。
// 修改前自动备份(.bak),支持 diff 查看。
//
// 用法:
//
//	go-rmm config get -a <agent> <file>
//	go-rmm config set -a <agent> <file> <key=value>     (INI 格式)
//	go-rmm config backup -a <agent> <file>
//	go-rmm config diff -a <agent> <file1> <file2>
package main

import (
	"fmt"
	"strings"
)

// cmdConfigMgmt config 子命令入口(避免与全局 config 类型冲突)。
func cmdConfigMgmt(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm config <get|set|backup|diff> -a <agent> [file]")
	}
	switch args[0] {
	case "get", "read", "cat":
		return configGet(cfg, args[1:])
	case "set", "edit":
		return configSet(cfg, args[1:])
	case "backup", "bak":
		return configBackup(cfg, args[1:])
	case "diff", "compare":
		return configDiff(cfg, args[1:])
	default:
		return fmt.Errorf("未知 config 子命令: %s (可用: get set backup diff)", args[0])
	}
}

// configGet 读取配置文件。
func configGet(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	if agentID == "" || filePath == "" {
		return fmt.Errorf("用法: go-rmm config get -a <agent> <file>")
	}
	cmd := fmt.Sprintf("Get-Content '%s' -ErrorAction SilentlyContinue", filePath)
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		cmd = fmt.Sprintf("cat '%s' 2>/dev/null", filePath)
		stdout, err = runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// configSet 修改 INI 格式配置(key=value)。
func configSet(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	kv := posArg(args, 2) // key=value
	if agentID == "" || filePath == "" || kv == "" || !strings.Contains(kv, "=") {
		return fmt.Errorf("用法: go-rmm config set -a <agent> <file> <key=value>")
	}
	parts := strings.SplitN(kv, "=", 2)
	key := parts[0]
	val := parts[1]

	// 先备份
	backupCmd := fmt.Sprintf("Copy-Item '%s' '%s.bak' -Force -ErrorAction SilentlyContinue", filePath, filePath)
	runOpsTask(cfg, agentID, backupCmd, 5)

	// INI 替换:匹配 key= 行,替换值;不存在则追加
	cmd := fmt.Sprintf(`$f='%s'; $c=Get-Content $f; $found=$false; for($i=0;$i -lt $c.Count;$i++){ if($c[$i] -match '^%s='){ $c[$i]='%s=%s'; $found=$true } }; if(-not $found){ $c+='%s=%s' }; $c | Set-Content $f; Write-Output 'updated'`, filePath, key, key, val, key, val)
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		// Linux sed
		cmd = fmt.Sprintf("sed -i 's/^%s=.*/%s=%s/' '%s' && echo 'updated'", key, key, val, filePath)
		stdout, err = runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
	}
	fmt.Printf("✅ 配置已更新: %s = %s (备份: %s.bak)\n", key, val, filePath)
	_ = stdout
	return nil
}

// configBackup 备份配置文件。
func configBackup(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	if agentID == "" || filePath == "" {
		return fmt.Errorf("用法: go-rmm config backup -a <agent> <file>")
	}
	ts := "bak"
	bakPath := filePath + "." + ts
	cmd := fmt.Sprintf("Copy-Item '%s' '%s' -Force", filePath, bakPath)
	_, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		cmd = fmt.Sprintf("cp '%s' '%s'", filePath, bakPath)
		_, err = runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
	}
	fmt.Printf("✅ 已备份: %s → %s\n", filePath, bakPath)
	return nil
}

// configDiff 比较两个配置文件。
func configDiff(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	file1 := posArg(args, 1)
	file2 := posArg(args, 2)
	if agentID == "" || file1 == "" || file2 == "" {
		return fmt.Errorf("用法: go-rmm config diff -a <agent> <file1> <file2>")
	}
	cmd := fmt.Sprintf("Compare-Object (Get-Content '%s') (Get-Content '%s') | Format-Table -AutoSize | Out-String", file1, file2)
	stdout, err := runOpsTask(cfg, agentID, cmd, 15)
	if err != nil {
		cmd = fmt.Sprintf("diff '%s' '%s' 2>/dev/null", file1, file2)
		stdout, err = runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}
