// CLI sync 命令 — 目录级文件同步(F3 扩展)。
//
// 用途:把本地目录同步到远端 Agent(或反向),类似 rsync 的简化版。
// 基于 robocopy(Windows)/rsync(Linux),经 file upload 批量传输。
//
// 用法:
//
//	reins sync push -a <agent> <local-dir> <remote-dir>   (本地→远端)
//	reins sync pull -a <agent> <remote-dir> <local-dir>   (远端→本地)
//
// 场景:部署目录(代码/配置/资源文件),比单文件 upload 更高效。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// syncQuiet sync 并行模式下的 fileUpload 输出静默开关(多 worker 下逐文件
// 进度必然交错,静默后只保留汇总与失败清单;fileUpload 各打印点查询)。
var syncQuiet atomic.Bool

// cmdSync sync 子命令入口。
func cmdSync(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins sync <push|pull> -a <agent> <src-dir> <dst-dir>")
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
		return fmt.Errorf("用法: reins sync push -a <agent> <local-dir> <remote-dir>")
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

	// 并行上传(worker 池,-j 默认 1=串行保持原行为;>1 时逐文件进度必交错,
	// 静默 fileUpload 输出只留汇总+失败清单)。直连/数据面下多文件天然多流。
	// -j 只取值不剥离(位置参数在本函数已消费完毕,args 不再下游使用——
	// 旧版"剥离到 filtered 再赋回 args"是只写不读的死代码,golangci ineffassign+SA4010 逮住删除)
	jobs := 1
	if j := extractArg(args, "-j"); j != "" {
		if n, err := strconv.Atoi(j); err == nil && n > 0 {
			jobs = n
		}
	}
	quiet := jobs > 1

	// 先建全部子目录(串行,幂等,免 worker 间竞争 mkdir 任务)
	dirSet := map[string]bool{filepath.ToSlash(dstDir): true}
	for _, relPath := range files {
		remotePath := filepath.ToSlash(filepath.Join(dstDir, relPath))
		dirSet[filepath.ToSlash(filepath.Dir(remotePath))] = true
	}
	for d := range dirSet {
		if d != filepath.ToSlash(dstDir) {
			mkdir(d)
		}
	}

	var (
		mu      sync.Mutex
		success int
		failed  int
		done    int
		errs    []string
	)
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			for relPath := range work {
				localPath := filepath.Join(srcDir, relPath)
				remotePath := filepath.ToSlash(filepath.Join(dstDir, relPath))
				if quiet {
					syncQuiet.Store(true)
				}
				err := runFileUpload(cfg, agentID, localPath, remotePath)
				syncQuiet.Store(false)
				mu.Lock()
				done++
				if err != nil {
					failed++
					errs = append(errs, fmt.Sprintf("%s: %v", relPath, err))
				} else {
					success++
				}
				mu.Unlock()
			}
		}()
	}
	for i, relPath := range files {
		if !quiet {
			fmt.Printf("\r[%d/%d] %s", i+1, len(files), relPath)
		}
		work <- relPath
	}
	close(work)
	wg.Wait()

	fmt.Printf("\n✅ 同步完成: %d 成功, %d 失败 (%d 路并行)\n", success, failed, jobs)
	for _, e := range errs {
		fmt.Printf("   ✗ %s\n", e)
	}
	return nil
}

// syncPull 远端目录 → 本地(逐文件 download)。
func syncPull(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	srcDir := posArg(args, 1)
	dstDir := posArg(args, 2)
	if agentID == "" || srcDir == "" || dstDir == "" {
		return fmt.Errorf("用法: reins sync pull -a <agent> <remote-dir> <local-dir>")
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

	// 并行下载(worker 池,-j 与 push 同语义;>1 时静默 fileDownload 进度)
	jobs := 1
	filtered := make([]string, 0, len(args))
	for i, a := range args {
		if a == "-j" || strings.HasPrefix(a, "-j=") {
			continue
		}
		if i > 0 && args[i-1] == "-j" && isAllDigit(a) {
			continue
		}
		filtered = append(filtered, a)
	}
	if j := extractArg(args, "-j"); j != "" {
		if n, err := strconv.Atoi(j); err == nil && n > 0 {
			jobs = n
		}
	}
	_ = filtered // pull 的位置参数已在上层提取,filtered 仅作剥离记录

	quiet := jobs > 1
	var (
		mu      sync.Mutex
		success int
		failed  int
		errs    []string
	)
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			for remoteAbs := range work {
				remoteAbs = strings.TrimSpace(remoteAbs)
				relPath := strings.TrimPrefix(strings.ReplaceAll(remoteAbs, "\\", "/"), filepath.ToSlash(srcDir))
				relPath = strings.TrimPrefix(relPath, "/")
				localPath := filepath.Join(dstDir, relPath)
				if dir := filepath.Dir(localPath); dir != "." {
					os.MkdirAll(dir, 0755)
				}
				if quiet {
					syncQuiet.Store(true)
				}
				err := runFileDownload(cfg, agentID, remoteAbs, localPath)
				syncQuiet.Store(false)
				mu.Lock()
				if err != nil {
					failed++
					errs = append(errs, fmt.Sprintf("%s: %v", relPath, err))
				} else {
					success++
				}
				mu.Unlock()
			}
		}()
	}
	for i, remoteAbs := range remoteFiles {
		if !quiet {
			fmt.Printf("\r[%d/%d] %s", i+1, len(remoteFiles), strings.TrimSpace(remoteAbs))
		}
		work <- remoteAbs
	}
	close(work)
	wg.Wait()

	fmt.Printf("\n✅ 同步完成: %d 成功, %d 失败 (%d 路并行)\n", success, failed, jobs)
	for _, e := range errs {
		fmt.Printf("   ✗ %s\n", e)
	}
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
