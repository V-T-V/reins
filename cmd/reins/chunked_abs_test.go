package main

// 2026-09-16 绝对路径分片修复测试(sess_8d55d252 MERGE_FAIL 复盘):
//
// remotePath 为绝对路径时(如 C:\reins\slave-workspace\eval-dataset.tar):
//   - 列分片命令:直接 Get-ChildItem 目标目录,不得经过 Join-Path
//   - 合并命令:完整路径字面量 + -LiteralPath,不得包含 workspace 自省
//   - 解析前缀:用 base name(Get-ChildItem 返回裸文件名)
//   - MERGE_FAIL 结构性错误不触发整文件重试
// 相对路径行为不变(回归锚)。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestRemoteIsAbs(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{`C:\reins\slave-workspace\big.tar`, true},
		{"C:/reins/big.tar", true},
		{"/opt/data/big.tar", true},
		{`\srv\share\big.tar`, true},
		{"DigitalHuman.zip", false},
		{"sub/dir/file.zip", false},
		{"", false},
		{"CON", false},
	}
	for _, c := range cases {
		if got := remoteIsAbs(c.path); got != c.want {
			t.Errorf("remoteIsAbs(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestRemoteBaseAndDir(t *testing.T) {
	cases := []struct {
		path, base, dir string
	}{
		{`C:\reins\ws\big.tar`, "big.tar", `C:\reins\ws`},
		{"C:/reins/big.tar", "big.tar", "C:/reins"},
		{"/opt/big.tar", "big.tar", "/opt"},
		{"/big.tar", "big.tar", "/"},
		{"big.tar", "big.tar", "."},
	}
	for _, c := range cases {
		if got := remoteBaseName(c.path); got != c.base {
			t.Errorf("remoteBaseName(%q) = %q, want %q", c.path, got, c.base)
		}
		if got := remoteDirName(c.path); got != c.dir {
			t.Errorf("remoteDirName(%q) = %q, want %q", c.path, got, c.dir)
		}
	}
}

func TestBuildChunkListCommandAbs(t *testing.T) {
	cmd := buildChunkListCommand(`C:\reins\slave-workspace\eval-dataset.tar`)
	// 绝对路径:直接列目录,不经过 Join-Path / workspace 探测
	if !strings.Contains(cmd, `Get-ChildItem -LiteralPath 'C:\reins\slave-workspace'`) {
		t.Errorf("绝对路径应直接 Get-ChildItem -LiteralPath 目标目录:\n%s", cmd)
	}
	if !strings.Contains(cmd, `-Filter 'eval-dataset.tar.ftpart*'`) {
		t.Errorf("Filter 应为 base name + .ftpart*:\n%s", cmd)
	}
	if strings.Contains(cmd, "Join-Path") || strings.Contains(cmd, "$wsCandidates") {
		t.Errorf("绝对路径不应包含 Join-Path/workspace 探测:\n%s", cmd)
	}
}

func TestBuildChunkListCommandRel(t *testing.T) {
	cmd := buildChunkListCommand("DigitalHuman.zip")
	// 相对路径:回归锚——workspace 探测流程保持不变
	if !strings.Contains(cmd, "$wsCandidates") {
		t.Errorf("相对路径应保留 workspaceProbePS 探测:\n%s", cmd)
	}
	if !strings.Contains(cmd, `Join-Path $ws 'DigitalHuman.zip.ftpart*'`) {
		t.Errorf("相对路径应保留 Join-Path 探测(原行为):\n%s", cmd)
	}
}

func TestBuildMergeCommandAbs(t *testing.T) {
	cmd := buildMergeCommand(`C:\reins\slave-workspace\eval-dataset.tar`, 4)
	// 1. 不得包含 workspace 自省(绝对路径下分片就在 remotePath 旁)
	if strings.Contains(cmd, "$wsCandidates") || strings.Contains(cmd, "Join-Path") {
		t.Errorf("绝对路径合并不应包含 workspace 自省/Join-Path:\n%s", cmd)
	}
	// 2. base 用完整绝对路径字面量(PS 单引号包裹)
	if !strings.Contains(cmd, `$base = 'C:\reins\slave-workspace\eval-dataset.tar'`) {
		t.Errorf("base 应为绝对路径字面量:\n%s", cmd)
	}
	// 3. 插值安全:$($base).ftpart0000 子表达式形式(裸 $base.ftpart 会被
	//    PS 解析为属性访问 → 空串,这是绝对分支首版差点埋进去的坑)
	if !strings.Contains(cmd, `"$($base).ftpart0000"`) {
		t.Errorf("first 应为 `\"$($base).ftpart0000\"` 子表达式插值:\n%s", cmd)
	}
	if strings.Contains(cmd, `"$base.ftpart`) {
		t.Errorf("不得使用裸 $base.ftpart 插值(属性访问陷阱):\n%s", cmd)
	}
	// 4. -LiteralPath 用于 Test-Path(防路径含 [ ] 等通配符字符)
	if !strings.Contains(cmd, "Test-Path -LiteralPath $first") {
		t.Errorf("Test-Path 应用 -LiteralPath:\n%s", cmd)
	}
	// 5. MERGE_FAIL 诊断带目录名 + MERGE_OK 成功标记
	if !strings.Contains(cmd, `MERGE_FAIL partsdir=C:\reins\slave-workspace notfound`) {
		t.Errorf("MERGE_FAIL 诊断应带目标目录:\n%s", cmd)
	}
	if !strings.Contains(cmd, "MERGE_OK") {
		t.Errorf("合并成功标记 MERGE_OK 必须存在:\n%s", cmd)
	}
	// 6. 分片数正确展开
	if !strings.Contains(cmd, "$i -lt 4") {
		t.Errorf("totalChunks 应展开为 4:\n%s", cmd)
	}
}

func TestBuildMergeCommandRel(t *testing.T) {
	cmd := buildMergeCommand("DigitalHuman.zip", 16)
	// 回归锚:相对路径保持 workspace 探测 + Join-Path 流程
	if !strings.Contains(cmd, "$wsCandidates") {
		t.Errorf("相对路径应保留 workspaceProbePS:\n%s", cmd)
	}
	if !strings.Contains(cmd, "Join-Path $partsDir ($base + '.ftpart0000')") {
		t.Errorf("相对路径应保留原 Join-Path 合并流程:\n%s", cmd)
	}
}

func TestBuildMergeCommandAbsQuotesEscaped(t *testing.T) {
	// 路径含单引号时 PS 字面量转义(' 翻倍)
	cmd := buildMergeCommand(`C:\tmp\it's big.tar`, 1)
	if !strings.Contains(cmd, `$base = 'C:\tmp\it''s big.tar'`) {
		t.Errorf("单引号应翻倍转义:\n%s", cmd)
	}
}

func TestIsRetryableXferErrMergeFail(t *testing.T) {
	// MERGE_FAIL 结构性失败:不重试(sess 实证 3 轮重传 3×159MB 全废)
	if isRetryableXferErr(errors.New("合并完成但未获取到 SHA256, raw result: ... stdout:MERGE_FAIL partsdir=C:\\Windows\\system32 notfound")) {
		t.Error("MERGE_FAIL 应判为不可重试")
	}
	// 网络类错误:可重试
	if !isRetryableXferErr(errors.New("HTTP 502: WS写失败 connection reset")) {
		t.Error("网络错误应可重试")
	}
	// 用户取消:不重试
	if isRetryableXferErr(errors.New("context canceled")) {
		t.Error("context canceled 应不重试")
	}
}

// TestBuildMergeCommandAbsPSExecution 在真实 PowerShell 里执行绝对路径合并命令
// (2026-09-16 sess_8d55d252 复盘闭环:原命令在该场景必然 MERGE_FAIL
// partsdir=C:\Windows\system32 notfound)。非 Windows 跳过。
func TestBuildMergeCommandAbsPSExecution(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 有 PowerShell")
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("无 powershell")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "eval-dataset.tar")
	// 3 片 × 4KB 伪随机内容(命名遵循 .ftpart%04d 四位零填充约定)
	var full []byte
	for i := 0; i < 3; i++ {
		chunk := make([]byte, 4096)
		for j := range chunk {
			chunk[j] = byte(i*7 + j%251)
		}
		full = append(full, chunk...)
		partPath := fmt.Sprintf("%s.ftpart%04d", target, i)
		// 有意让第 2 片只落 .part 后缀:验证兜底分支(rename 偶尔未完成的临时文件)
		if i == 2 {
			partPath += ".part"
		}
		if err := os.WriteFile(partPath, chunk, 0644); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	script := buildMergeCommand(target, 3)
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("合并命令执行失败: %v\n输出: %s\n脚本:\n%s", err, out, script)
	}
	re := regexp.MustCompile(`MERGE_OK ([0-9a-f]{64})`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("未输出 MERGE_OK: %s\n脚本:\n%s", out, script)
	}
	if m[1] != wantSHA {
		t.Errorf("合并后 SHA256 不符: got %s want %s\n输出: %s", m[1], wantSHA, out)
	}
	// 合并产物存在且分片已清理
	if b, rerr := os.ReadFile(target); rerr != nil || len(b) != len(full) {
		t.Errorf("合并产物异常: err=%v len=%d want=%d", rerr, len(b), len(full))
	}
	if _, rerr := os.Stat(target + ".ftpart0"); !os.IsNotExist(rerr) {
		t.Errorf("分片未清理: %v", rerr)
	}
}
