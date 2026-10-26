// Package main — Agent 自更新执行器。
//
// 自更新流程(Windows rename + copy + SCM 自重启):
//  1. 校验 BinaryPath 文件存在
//  2. 重算 SHA256,与 UpdateTask.SHA256 比对(防损坏/篡改)
//  3. 版本防降级:新版本不得低于 MinVersion(除非 Force)
//  4. 写 .update-pending 标记文件(供健康自检识别)
//  5. rename 当前 exe → .bak(Windows 允许重命名运行中的 exe)
//  6. rename .new → exe
//  7. 设 pendingRestart flag,主循环检测后优雅退出
//  8. SCM OnFailure=restart 自动拉起新版本
//  9. 新版本启动健康自检,成功注册 Relay 后删 .update-pending
//
// 10. 若 60s 未注册,健康自检用 .bak 回滚
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/V-T-V/reins/proto"
)

// pendingRestart 自更新完成后置 true,主循环检测后退出让 SCM 重启。
var pendingRestart bool

// UpdateExecutor 自更新执行器(agent 包内部,不走标准 Executor 接口)。
// 强依赖 agent 包的 os.Executable/version/exeDir,故在 agent 包内实现。
// runTask 对 task.Type==update 做特殊分发(见 connection.go)。
type UpdateExecutor struct{}

// updateExec 全局实例。
var updateExec = &UpdateExecutor{}

// updateMu 更新互斥:推送式 update 任务(WC 通道下发)与 auto_update 轮询
// (HTTP 通道)可能并发到达,execUpdate 的 rename 舞步不可重入。
var updateMu sync.Mutex

// versionsMatchMin 防降级检查:version 是否 >= minVersion。
// 简化 semver 比较:解析 "v1.2.3" → [1,2,3] 逐段比。
func versionGE(version, minVersion string) bool {
	if minVersion == "" {
		return true // 无约束
	}
	v := parseSemver(version)
	m := parseSemver(minVersion)
	if len(v) < 3 || len(m) < 3 {
		return version >= minVersion // 字符串兜底
	}
	for i := 0; i < 3; i++ {
		if v[i] > m[i] {
			return true
		}
		if v[i] < m[i] {
			return false
		}
	}
	return true // 相等
}

// parseSemver 解析 "v1.2.3" → [1,2,3]。失败返回空。
// 遇预发布分隔符 '-' 即停止:后续数字不属主版本(v4.5.5-c9c48bc 的主版本是
// 4.5.5)——原实现把后缀数字累进末段,v4.5.5-c9c48bc 被解析成 4.5.5948、
// v4.5.4-b63201d 成 4.5.460201,导致正常升级被防降级误拒(2026-08-18 实证)。
func parseSemver(s string) []int {
	out := make([]int, 0, 3)
	current := 0
	started := false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			current = current*10 + int(c-'0')
			started = true
		} else if c == '.' {
			if started {
				out = append(out, current)
				current = 0
				started = false
			}
		} else if c == '-' {
			break // 预发布段开始,主版本解析结束
		}
	}
	if started {
		out = append(out, current)
	}
	return out
}

// computeSHA256 计算文件 SHA256。
func computeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// updatePendingMarkerPath 标记文件路径(<exeDir>/.update-pending)。
func updatePendingMarkerPath() string {
	return filepath.Join(exeDir(), ".update-pending")
}

// execUpdate 核心更新逻辑(由 runTask 对 update 类型任务调用)。
// 支持两种二进制来源:
//   - BinaryB64: 内联 base64(解码写临时文件)
//   - BinaryPath: 已存在的文件路径
func (e *UpdateExecutor) execUpdate(payload []byte) proto.TaskResult {
	updateMu.Lock()
	defer updateMu.Unlock()

	var task proto.UpdateTask
	if err := jsonUnmarshalUpdate(payload, &task); err != nil {
		return proto.TaskResult{Success: false, Error: "解析 update 任务失败: " + err.Error()}
	}

	// 确定二进制来源:优先 DownloadURL(HTTP 流式自取,公网推荐),
	// 其次 BinaryB64(内联),再次 BinaryPath(文件)
	var binaryPath string
	cleanupTemp := false
	if task.DownloadURL != "" {
		// download 模式:Agent 自己从 Relay 版本仓库拉取(复用自动更新的下载器,
		// 鉴权凭证用进程级 Agent 实例的 token/mTLS)
		a := currentAgent
		if a == nil {
			a = &Agent{}
		}
		if err := a.downloadTo(context.Background(), task.DownloadURL, task.SHA256); err != nil {
			return proto.TaskResult{Success: false, Error: "下载新二进制失败: " + err.Error()}
		}
		binaryPath = autoUpdateBinPath()
	} else if task.BinaryB64 != "" {
		// 解码 base64 写到 exeDir/reins-agent.exe.new
		binData, err := base64.StdEncoding.DecodeString(task.BinaryB64)
		if err != nil {
			return proto.TaskResult{Success: false, Error: "base64 解码失败: " + err.Error()}
		}
		binaryPath = filepath.Join(exeDir(), "reins-agent.exe.new")
		if err := os.WriteFile(binaryPath, binData, 0755); err != nil {
			return proto.TaskResult{Success: false, Error: "写临时文件失败: " + err.Error()}
		}
		cleanupTemp = true
	} else if task.BinaryPath != "" {
		binaryPath = task.BinaryPath
		if !filepath.IsAbs(binaryPath) {
			binaryPath = filepath.Join(exeDir(), binaryPath)
		}
	} else {
		return proto.TaskResult{Success: false, Error: "binary_path 或 binary_b64 不能都为空"}
	}
	// 失败时清理临时文件
	defer func() {
		if cleanupTemp {
			os.Remove(binaryPath)
		}
	}()

	// 1. 文件存在检查
	if _, err := os.Stat(binaryPath); err != nil {
		return proto.TaskResult{Success: false, Error: "新二进制不存在: " + err.Error()}
	}

	// 2. SHA256 校验
	actualSHA, err := computeSHA256(binaryPath)
	if err != nil {
		return proto.TaskResult{Success: false, Error: "SHA256 计算失败: " + err.Error()}
	}
	if task.SHA256 != "" && actualSHA != task.SHA256 {
		return proto.TaskResult{Success: false, Error: fmt.Sprintf("SHA256 校验失败: 期望 %s 实际 %s", task.SHA256[:16], actualSHA[:16])}
	}

	// 3. 版本防降级(Force 跳过)
	if !task.Force {
		if task.MinVersion != "" && !versionGE(task.Version, task.MinVersion) {
			return proto.TaskResult{Success: false, Error: fmt.Sprintf("版本 %s 低于最低要求 %s(降级被拒)", task.Version, task.MinVersion)}
		}
	}

	// 4. 当前 exe 路径
	currentExe, err := os.Executable()
	if err != nil {
		return proto.TaskResult{Success: false, Error: "获取当前 exe 路径失败: " + err.Error()}
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe)
	bakPath := currentExe + ".bak"

	// 5. 写 .update-pending 标记(健康自检识别用)
	//    内容是新版本号,自检成功后删除
	if err := os.WriteFile(updatePendingMarkerPath(), []byte(task.Version), 0644); err != nil {
		return proto.TaskResult{Success: false, Error: "写 update-pending 标记失败: " + err.Error()}
	}

	// U2:copy 当前 exe → .bak(exe 保留原位,任何时刻 exe 路径有效,断电不砖)
	os.Remove(bakPath)
	if err := copyFile(currentExe, bakPath); err != nil {
		os.Remove(updatePendingMarkerPath())
		return proto.TaskResult{Success: false, Error: "备份当前 exe→.bak 失败: " + err.Error()}
	}
	// Windows:运行中的 exe 可被移走(rename),但不可被顶名替换——目标路径
	// 被进程映射时 MoveFile 返回 Access denied(2026-08-18 生产实证)。
	// 两步走:先把运行中 exe 移到 .old-running,再把新二进制 rename 进空位;
	// 残留的 .old-running 由下次启动清理(旧进程死后才可删)。
	oldRunning := currentExe + ".old-running"
	os.Remove(oldRunning)
	if err := os.Rename(currentExe, oldRunning); err != nil {
		os.Remove(updatePendingMarkerPath())
		return proto.TaskResult{Success: false, Error: "移走运行中 exe 失败(新旧版本均未受影响): " + err.Error()}
	}
	if _, placeErr := placeBinary(os.Rename, binaryPath, currentExe); placeErr != nil {
		os.Rename(oldRunning, currentExe) // 回滚:旧 exe 归位
		os.Remove(updatePendingMarkerPath())
		return proto.TaskResult{Success: false, Error: "新二进制落位失败(已回滚旧版本原位): " + placeErr.Error()}
	}

	// 8. 标记重启(主循环检测后退出,SCM 自动拉新版本)
	pendingRestart = true

	return proto.TaskResult{
		Success: true,
		Stdout:  fmt.Sprintf("更新成功: %s → %s (sha256=%s),等待服务重启", version, task.Version, actualSHA[:16]),
		Data:    []byte(fmt.Sprintf(`{"old_version":%q,"new_version":%q,"sha256":%q}`, version, task.Version, actualSHA)),
	}
}

// placeBinary 把新二进制落到 dst(当前 exe 的正位)。
// 优先 rename(原子);被安全软件拒绝时回退字节级 copy——Windows Defender 等
// 会确定性拦截"未签名新文件 rename 顶替服务 exe"(2026-09-17 4060 生产 4 次复现,
// 与账户/ACL 无关;同操作 copy 落名畅通,人工 copy 舞步即此理)。
// renameFn 参数化便于测试注入;返回是否走了 copy 回退。
func placeBinary(renameFn func(from, to string) error, src, dst string) (bool, error) {
	if err := renameFn(src, dst); err == nil {
		return false, nil
	}
	if err := copyBinaryStream(src, dst); err != nil {
		return true, fmt.Errorf("rename 与 copy 均失败: %w", err)
	}
	log.Printf("[update] rename 被拒(疑似安全软件拦截服务 exe 顶替),已回退 copy 落位")
	return true, nil
}

// copyBinaryStream 流式复制文件(不在内存全量载入,12MB+ 二进制友好)。
func copyBinaryStream(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyFile 复制文件(U2:更新备份用,保持源文件在原位)。
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}

// jsonUnmarshalUpdate 辅助:JSON 反序列化。
func jsonUnmarshalUpdate(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}

// cleanupOldRunning 清理自更新两步 rename 遗留的 .old-running 文件。
// 运行中的 exe 不能删除,须等旧进程退出(即下次启动)后才能删。
func cleanupOldRunning() {
	if exe, err := os.Executable(); err == nil {
		if err := os.Remove(exe + ".old-running"); err == nil {
			log.Printf("[agent] 已清理自更新残留 %s.old-running", filepath.Base(exe))
		}
	}
}
