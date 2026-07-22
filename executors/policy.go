package executors

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// FilePolicy 文件执行器的路径访问策略。
//
// 修复 S5(P0)：文件任务原本可读写任意路径（含系统目录、用户私钥），
// 现在强制要求路径在 AllowedRoots 白名单内，且不在 DeniedPaths 黑名单内。
//
// 默认策略：AllowedRoots 为空时拒绝所有路径（最安全）；调用方按需放开。
type FilePolicy struct {
	// AllowedRoots 允许的根目录（绝对路径）。路径必须落在其中之一。
	// 为空表示无白名单（需结合 AllowAll 才放行）。
	AllowedRoots []string
	// DeniedPaths 禁止访问的路径/路径前缀（即使落在 AllowedRoots 也拒绝）。
	// 默认含系统目录与敏感文件。
	DeniedPaths []string
	// AllowAll 完全放开（仅用于本地调试，生产禁用）。
	AllowAll bool
}

// DefaultFilePolicy 返回安全默认策略：拒绝系统目录、私钥、凭据。
//
// AllowedRoots 留空——调用方（agent）必须显式配置允许的工作目录。
func DefaultFilePolicy() FilePolicy {
	denied := []string{
		`C:\Windows\System32`,
		`C:\Windows\SysWOW64`,
		`C:\Program Files`,
		`C:\Program Files (x86)`,
	}
	if runtime.GOOS != "windows" {
		denied = []string{
			`/etc`,
			`/usr`,
			`/bin`,
			`/sbin`,
			`/boot`,
			`/root`,
		}
	}
	// 通用敏感文件（不分平台）
	denied = append(denied, `.ssh`, `.aws`, `.kube`, `id_rsa`, `id_ecdsa`)
	return FilePolicy{DeniedPaths: denied}
}

// Check 校验 path 是否允许访问。返回 nil 表示放行。
//
// 校验顺序：AllowAll → DeniedPaths → AllowedRoots。
func (p FilePolicy) Check(path string) error {
	if p.AllowAll {
		return nil
	}
	if path == "" {
		return fmt.Errorf("路径为空")
	}

	// 规范化：Clean + Abs（解析 .. 和符号链接的文本层）
	clean := filepath.Clean(path)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return fmt.Errorf("路径解析失败: %w", err)
	}

	// 黑名单：绝对路径或片段命中即拒绝
	absLower := strings.ToLower(abs)
	for _, d := range p.DeniedPaths {
		dLower := strings.ToLower(d)
		if strings.Contains(absLower, dLower) {
			return fmt.Errorf("路径 %s 命中禁止清单（含 %s）", path, d)
		}
	}

	// 白名单：必须落在某个允许根内
	if len(p.AllowedRoots) == 0 {
		return fmt.Errorf("未配置允许根目录，拒绝路径 %s", path)
	}
	for _, root := range p.AllowedRoots {
		rootAbs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil {
			continue
		}
		// rel 不以 ".." 开头且不等于 ".." 表示 abs 在 rootAbs 内
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil // 放行
		}
	}
	return fmt.Errorf("路径 %s 不在任何允许根目录内（%v）", path, p.AllowedRoots)
}
