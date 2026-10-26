// CLI config 命令 — 远端配置文件管理(F7 扩展)。
//
// 用途:读取/修改/比对远端配置文件(INI/JSON/YAML/XML)。
// 修改前自动备份(.bak),支持 diff 查看。
//
// 用法:
//
//	reins config get -a <agent> <file>
//	reins config set -a <agent> <file> <key=value>     (INI 格式)
//	reins config backup -a <agent> <file>
//	reins config diff -a <agent> <file1> <file2>
package main

import (
	"fmt"
	"strings"
)

// cmdConfigMgmt config 子命令入口(避免与全局 config 类型冲突)。
func cmdConfigMgmt(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins config <get|set|backup|diff> -a <agent> [file]")
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
		return fmt.Errorf("用法: reins config get -a <agent> <file>")
	}
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("cat '%s' 2>/dev/null", filePath), 10)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Get-Content '%s' -ErrorAction SilentlyContinue", filePath), 10)
	}
	if err != nil {
		return err
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
		return fmt.Errorf("用法: reins config set -a <agent> <file> <key=value>")
	}
	parts := strings.SplitN(kv, "=", 2)
	key := parts[0]
	val := parts[1]

	var stdout string
	if isLinuxAgent(cfg, agentID) {
		// 备份:cp
		runOpsScript(cfg, agentID, fmt.Sprintf("cp '%s' '%s.bak' 2>/dev/null; true", filePath, filePath), 5)
		// INI 替换:awk -v 传值(避免 sed 元字符注入),匹配 key= 行替换,无则追加
		stdout, _ = runOpsScript(cfg, agentID, fmt.Sprintf(
			`awk -v k='%s' -v v='%s' 'BEGIN{f=0} index($0,k"=")==1{print k"="v; f=1; next} {print} END{if(!f) print k"="v}' '%s' > '%s.tmp' && mv '%s.tmp' '%s' && echo updated`,
			key, val, filePath, filePath, filePath, filePath), 10)
	} else {
		// 备份:Copy-Item
		runOpsTask(cfg, agentID, fmt.Sprintf("Copy-Item '%s' '%s.bak' -Force -ErrorAction SilentlyContinue", filePath, filePath), 5)
		// INI 替换:匹配 key= 行,替换值;不存在则追加
		stdout, _ = runOpsTask(cfg, agentID, fmt.Sprintf(`$f='%s'; $c=Get-Content $f; $found=$false; for($i=0;$i -lt $c.Count;$i++){ if($c[$i] -match '^%s='){ $c[$i]='%s=%s'; $found=$true } }; if(-not $found){ $c+='%s=%s' }; $c | Set-Content $f; Write-Output 'updated'`, filePath, key, key, val, key, val), 10)
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
		return fmt.Errorf("用法: reins config backup -a <agent> <file>")
	}
	ts := "bak"
	bakPath := filePath + "." + ts
	var err error
	if isLinuxAgent(cfg, agentID) {
		_, err = runOpsScript(cfg, agentID, fmt.Sprintf("cp '%s' '%s'", filePath, bakPath), 10)
	} else {
		_, err = runOpsTask(cfg, agentID, fmt.Sprintf("Copy-Item '%s' '%s' -Force", filePath, bakPath), 10)
	}
	if err != nil {
		return err
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
		return fmt.Errorf("用法: reins config diff -a <agent> <file1> <file2>")
	}
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("diff -u '%s' '%s' 2>/dev/null; true", file1, file2), 15)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Compare-Object (Get-Content '%s') (Get-Content '%s') | Format-Table -AutoSize | Out-String", file1, file2), 15)
	}
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}
