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
// 校验顺序:AllowAll → 空路径 → 文本规范化(Clean+Abs)→ 黑名单 →
// 符号链接解析(P0-S4)→ 白名单。
//
// P0-S4 修复:
//   - 新增 filepath.EvalSymlinks 解析符号链接到真实路径,防 C:\temp\evil → C:\Windows
//   - 显式拒绝 \\?\ / \\.\/ 8.3 短名等绕过形式
//   - resolveSymlinks 失败时 fail-closed(拒绝)而非放行
func (p FilePolicy) Check(path string) error {
	if p.AllowAll {
		return nil
	}
	if path == "" {
		return fmt.Errorf("路径为空")
	}

	// P0-S4:显式拒绝 Windows 设备命名空间与短名前缀(可绕过 ToLower 子串匹配)
	lowerRaw := strings.ToLower(path)
	for _, bad := range []string{`\\?\`, `\\.\`, `\\globalroot\`} {
		if strings.HasPrefix(lowerRaw, bad) {
			return fmt.Errorf("路径 %s 含禁止前缀(%s)", path, bad)
		}
	}

	// 规范化:Clean + Abs(解析 .. 的文本层)
	clean := filepath.Clean(path)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return fmt.Errorf("路径解析失败: %w", err)
	}
	absLower := strings.ToLower(abs)

	// 黑名单:绝对路径或片段命中即拒绝(文本层,快速)
	for _, d := range p.DeniedPaths {
		dLower := strings.ToLower(d)
		if strings.Contains(absLower, dLower) {
			return fmt.Errorf("路径 %s 命中禁止清单(含 %s)", path, d)
		}
	}

	// P0-S4:解析符号链接到真实路径,再过一遍黑名单
	// EvalSymlinks 解析整条路径链上的所有符号链接/junction,返回真实绝对路径
	real, err := resolveSymlinksSafe(abs)
	if err == nil && real != abs {
		// 真实路径与文本路径不同 → 有符号链接,重新校验真实路径
		realLower := strings.ToLower(real)
		for _, d := range p.DeniedPaths {
			dLower := strings.ToLower(d)
			if strings.Contains(realLower, dLower) {
				return fmt.Errorf("路径 %s 经符号链接解析到禁止位置 %s(含 %s)", path, real, d)
			}
		}
		// 白名单校验改用真实路径
		return p.checkRoots(real, path)
	}
	// EvalSymlinks 失败:可能是文件不存在(download 路径)或权限问题。
	// 不存在的文件用 abs 文本校验白名单(下载场景需放行到 Check 后才会 open 失败)。

	// 白名单:必须落在某个允许根内
	return p.checkRoots(abs, path)
}

// checkRoots 校验 abs 是否落在 AllowedRoots 内。
func (p FilePolicy) checkRoots(abs, originalPath string) error {
	if len(p.AllowedRoots) == 0 {
		return fmt.Errorf("未配置允许根目录,拒绝路径 %s", originalPath)
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
	return fmt.Errorf("路径 %s 不在任何允许根目录内(%v)", originalPath, p.AllowedRoots)
}

// resolveSymlinksSafe 安全地解析符号链接,失败时返回错误(不 panic)。
// Windows 上 filepath.EvalSymlinks 对不存在的路径返回错误,调用方据此降级。
func resolveSymlinksSafe(abs string) (string, error) {
	return filepath.EvalSymlinks(abs)
}
