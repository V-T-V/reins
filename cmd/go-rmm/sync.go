// CLI sync 命令 — 目录级文件同步(F3 扩展)。
//
// 用途:把本地目录同步到远端 Agent(或反向),类似 rsync 的简化版。
// 基于 robocopy(Windows)/rsync(Linux),经 file upload 批量传输。
//
// 用法:
//
//	go-rmm sync push -a <agent> <local-dir> <remote-dir>   (本地→远端)
//	go-rmm sync pull -a <agent> <remote-dir> <local-dir>   (远端→本地)
//
// 场景:部署目录(代码/配置/资源文件),比单文件 upload 更高效。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdSync sync 子命令入口。
func cmdSync(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm sync <push|pull> -a <agent> <src-dir> <dst-dir>")
	}
	switch args[0] {
	case "push", "up":
		return syncPush(cfg, args[1:])
	case "pull", "down":
		return syncPull(cfg, args[1:])
	default:
		return fmt.Errorf("未知 sync 子命令: %s (可用: push pull)", args[0])
	}
}

// syncPush 本地目录 → 远端(逐文件 upload)。
func syncPush(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	srcDir := posArg(args, 1)
	dstDir := posArg(args, 2)
	if agentID == "" || srcDir == "" || dstDir == "" {
		return fmt.Errorf("用法: go-rmm sync push -a <agent> <local-dir> <remote-dir>")
	}

	// 遍历本地目录
	files, err := walkFiles(srcDir)
	if err != nil {
		return fmt.Errorf("遍历本地目录失败: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("目录 %s 无文件", srcDir)
	}

	fmt.Printf("🔄 同步 %s → %s:%s (%d 个文件)\n", srcDir, agentID, dstDir, len(files))

	// 在远端创建目标目录(双方言;原实现把 PS 与 mkdir -p 拼一条,Windows 上
	// mkdir -p 部分报错靠吞、Linux 上整条是 PS 语法不可用)
	linuxTarget := isLinuxAgent(cfg, agentID)
	mkdir := func(dir string) {
		if linuxTarget {
			runOpsScript(cfg, agentID, fmt.Sprintf("mkdir -p '%s' 2>/dev/null; true", dir), 10)
		} else {
			runOpsTask(cfg, agentID, fmt.Sprintf("New-Item -ItemType Directory -Force -Path '%s' 2>$null", dir), 10)
		}
	}
	mkdir(dstDir)

	// 逐文件上传
	success := 0
	failed := 0
	for i, relPath := range files {
		localPath := filepath.Join(srcDir, relPath)
		remotePath := filepath.ToSlash(filepath.Join(dstDir, relPath))

		fmt.Printf("\r[%d/%d] %s", i+1, len(files), relPath)
		// 确保远端子目录存在
		remoteDir := filepath.ToSlash(filepath.Dir(remotePath))
		if remoteDir != filepath.ToSlash(dstDir) {
			mkdir(remoteDir)
		}

		// 上传单个文件
		if err := runFileUpload(cfg, agentID, localPath, remotePath); err != nil {
			failed++
			fmt.Printf(" ✗ (%v)\n", err)
		} else {
			success++
		}
	}

	fmt.Printf("\n✅ 同步完成: %d 成功, %d 失败\n", success, failed)
	return nil
}

// syncPull 远端目录 → 本地(逐文件 download)。
func syncPull(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	srcDir := posArg(args, 1)
	dstDir := posArg(args, 2)
	if agentID == "" || srcDir == "" || dstDir == "" {
		return fmt.Errorf("用法: go-rmm sync pull -a <agent> <remote-dir> <local-dir>")
	}

	// 列出远端目录文件(双方言;原 fallback 经 powershell 类型下发,Linux 不可达)
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("find '%s' -type f 2>/dev/null", srcDir), 30)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Get-ChildItem -Path '%s' -Recurse -File | Select-Object -ExpandProperty FullName", srcDir), 30)
	}
	if err != nil {
		return err
	}

	remoteFiles := strings.Split(strings.TrimSpace(stdout), "\n")
	remoteFiles = filterEmpty(remoteFiles)
	if len(remoteFiles) == 0 {
		return fmt.Errorf("远端目录 %s 无文件", srcDir)
	}

	fmt.Printf("🔄 同步 %s:%s → %s (%d 个文件)\n", agentID, srcDir, dstDir, len(remoteFiles))

	// 创建本地目录
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("创建本地目录失败: %w", err)
	}

	success := 0
	failed := 0
	for i, remoteAbs := range remoteFiles {
		// 计算相对路径(去掉 srcDir 前缀)
		relPath := strings.TrimPrefix(strings.ReplaceAll(remoteAbs, "\\", "/"), filepath.ToSlash(srcDir))
		relPath = strings.TrimPrefix(relPath, "/")
		localPath := filepath.Join(dstDir, relPath)

		fmt.Printf("\r[%d/%d] %s", i+1, len(remoteFiles), relPath)

		// 确保本地子目录存在
		if dir := filepath.Dir(localPath); dir != "." {
			os.MkdirAll(dir, 0755)
		}

		// 下载单个文件
		if err := runFileDownload(cfg, agentID, strings.TrimSpace(remoteAbs), localPath); err != nil {
			failed++
			fmt.Printf(" ✗\n")
		} else {
			success++
		}
	}

	fmt.Printf("\n✅ 同步完成: %d 成功, %d 失败\n", success, failed)
	return nil
}

// walkFiles 递归遍历目录,返回相对路径列表。
func walkFiles(root string) ([]string, error) {
	var files []string
	// 清化 root(统一路径分隔符)
	root = filepath.Clean(root)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel) // 统一用 /
		files = append(files, rel)
		return nil
	})
	return files, err
}

// filterEmpty 过滤空字符串。
func filterEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// runFileDownload 辅助:复用 fileDownload 逻辑。
func runFileDownload(cfg config, agentID, remotePath, localPath string) error {
	return fileDownload(cfg, []string{"-a", agentID, remotePath, localPath})
}
