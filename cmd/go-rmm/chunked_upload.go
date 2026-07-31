// CLI 分片上传(FT:解决大文件经 nginx 504 超时)。
//
// 策略:大文件切成 N 个独立分片,每片一个 HTTP 请求(小请求不会超时)。
// 已传的分片跳过(断点续传),全部传完后合并 + SHA256 校验。
//
// 协议(复用现有 /api/transfer/upload,不改 Relay/Agent):
//   - 分片文件名: <remotePath>.ftpart<NN>(NN=00,01,...)
//   - 每片独立上传到 Agent 的 workspace
//   - 断点续传:上传前查目标机 .ftpart<NN> 是否存在且大小匹配,存在则跳过
//   - 合并:全部传完后下发 powershell 合并分片 + 算 SHA256

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// 分片参数
const (
	chunkThreshold = 50 * 1024 * 1024  // > 50MB 走分片模式
	chunkSize      = 10 * 1024 * 1024  // 每片 10MB
	chunkMaxRetry  = 3                 // 每片失败重试次数
)

// fileUploadChunked 分片上传大文件。
//
// 流程:
//  1. 本地算整文件 SHA256(一次读)
//  2. 切分计划:totalChunks = ceil(totalSize / chunkSize)
//  3. 查已传分片(下发 powershell 列 .ftpart* 文件)→ 跳过已完成的
//  4. 逐片上传(每片独立 HTTP POST,失败重试)
//  5. 全部完成后下发 powershell 合并 + 算 SHA256
//  6. 比对 SHA256,清理分片
func fileUploadChunked(cfg config, agentID, localPath, remotePath string, totalSize int64) error {
	totalChunks := int((totalSize + chunkSize - 1) / chunkSize)

	fmt.Printf("📦 分片上传: %s (%s, %d 片 × 10MB) → %s:%s\n",
		localPath, formatSize(totalSize), totalChunks, agentID, remotePath)

	// 1. 本地算整文件 SHA256
	fmt.Printf("   计算 SHA256...\n")
	localSHA, err := computeFileSHA256(localPath)
	if err != nil {
		return fmt.Errorf("算 SHA256 失败: %w", err)
	}
	fmt.Printf("   SHA256: %s\n", localSHA)

	// 2. 打开文件
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// 3. 查已传分片(断点续传)
	completed := checkCompletedChunks(cfg, agentID, remotePath, totalChunks)
	skipCount := len(completed)
	if skipCount > 0 {
		fmt.Printf("   断点续传:跳过 %d/%d 片已完成\n", skipCount, totalChunks)
	}

	// 4. 逐片上传
	startTime := time.Now()
	var uploaded int64
	for i := 0; i < totalChunks; i++ {
		chunkName := fmt.Sprintf("%s.ftpart%02d", remotePath, i)
		if completed[i] {
			// 已传,算字节数用于进度
			offset := int64(i) * chunkSize
			thisSize := chunkSize
			if offset+int64(thisSize) > totalSize {
				thisSize = int(totalSize - offset)
			}
			uploaded += int64(thisSize)
			continue
		}

		// 读分片
		offset := int64(i) * chunkSize
		thisSize := chunkSize
		if offset+int64(thisSize) > totalSize {
			thisSize = int(totalSize - offset)
		}

		// 重试
		var lastErr error
		for retry := 0; retry < chunkMaxRetry; retry++ {
			// Seek 到偏移(每次重试要重新定位)
			if _, err := f.Seek(offset, 0); err != nil {
				return fmt.Errorf("seek 失败: %w", err)
			}
			chunkData := make([]byte, thisSize)
			if _, err := io.ReadFull(f, chunkData); err != nil {
				return fmt.Errorf("读分片 %d 失败: %w", i, err)
			}

			err := uploadSingleChunk(cfg, agentID, chunkName, chunkData, localPath, i)
			if err == nil {
				break
			}
			lastErr = err
			fmt.Printf("\r   片 %d/%d 失败(重试 %d/%d): %v   ", i+1, totalChunks, retry+1, chunkMaxRetry, err)
			time.Sleep(2 * time.Second)
		}
		if lastErr != nil {
			return fmt.Errorf("分片 %d 传 %d 次均失败: %w", i, chunkMaxRetry, lastErr)
		}

		uploaded += int64(thisSize)
		elapsed := time.Since(startTime).Seconds()
		rate := float64(uploaded) / 1024 / 1024 / elapsed
		pct := uploaded * 100 / totalSize
		fmt.Printf("\r   进度: %d%% (%s / %s) | %.2f MB/s | 片 %d/%d   ",
			pct, formatSize(uploaded), formatSize(totalSize), rate, i+1, totalChunks)
	}
	fmt.Println()

	// 5. 合并分片(下发 powershell)
	fmt.Printf("   合并分片 + 校验...\n")
	mergeCmd := buildMergeCommand(remotePath, totalChunks)
	mergeTaskID := fmt.Sprintf("merge-%d", time.Now().UnixNano())
	taskBody := map[string]any{
		"task_id":  mergeTaskID,
		"agent_id": agentID,
		"type":     "powershell",
		"payload":  map[string]string{"command": mergeCmd},
	}
	var mergeResp map[string]any
	if err := apiPost(cfg, "/api/tasks", taskBody, &mergeResp); err != nil {
		return fmt.Errorf("下发合并命令失败: %w", err)
	}

	// 等合并结果(合并 1GB 需要时间,给 300s)
	result, err := waitTaskResult(cfg, mergeTaskID, 300)
	if err != nil {
		return fmt.Errorf("获取合并结果失败: %w", err)
	}

	// 6. 校验 SHA256
	// 合并命令的 stdout 会输出 "MERGE_OK <sha256>"(合并+清理一步完成)
	stdout, _ := result["stdout"].(string)
	remoteSHA := extractSHAFromOutput(stdout)
	if remoteSHA == "" {
		b, _ := json.Marshal(result)
		return fmt.Errorf("合并完成但未获取到 SHA256, raw result: %s", string(b))
	}

	// 比对
	elapsed := time.Since(startTime)
	fmt.Printf("✅ 分片上传完成: %s\n", formatSize(totalSize))
	fmt.Printf("   耗时: %s (%.2f MB/s)\n", elapsed.Truncate(time.Second), float64(totalSize)/elapsed.Seconds()/1024/1024)
	fmt.Printf("   Agent SHA256: %s\n", remoteSHA)
	if remoteSHA == localSHA {
		fmt.Printf("   ✅ SHA256 一致\n")
	} else {
		fmt.Fprintf(os.Stderr, "   ⚠️  校验不一致!本地=%s\n", localSHA)
		return fmt.Errorf("SHA256 校验失败")
	}
	return nil
}

// uploadSingleChunk 上传单个分片(复用现有 upload 端点)。
// chunkData 是分片内容,chunkName 是目标文件名。
func uploadSingleChunk(cfg config, agentID, chunkName string, chunkData []byte, srcFile string, idx int) error {
	transferID := fmt.Sprintf("chunk-%s-%d", srcFile, idx)
	transferID = fmt.Sprintf("xfer-%d-chunk%d", time.Now().UnixNano(), idx)

	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", chunkName)
	q.Set("transfer_id", transferID)
	q.Set("size", fmt.Sprintf("%d", len(chunkData)))
	u := cfg.relay + "/api/transfer/upload?" + q.Encode()

	req, err := newPostRequest(u, chunkData)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.ContentLength = -1 // chunked,避免大 CL 问题

	resp, err := streamingClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// checkCompletedChunks 查目标机已完成的分片(断点续传)。
// 返回 map[int]bool,idx → 是否已完成。
func checkCompletedChunks(cfg config, agentID string, remotePath string, totalChunks int) map[int]bool {
	completed := make(map[int]bool)
	// 下发 powershell 先探测 workspace,再列 .ftpart* 文件(相对路径会落到 CWD 找不到)
	listCmd := fmt.Sprintf(`%s
Get-ChildItem (Join-Path $ws '%s.ftpart*') -ErrorAction SilentlyContinue | ForEach-Object { $_.Name + '|' + $_.Length }
`, fmt.Sprintf(workspaceProbePS, remotePath), remotePath)
	listTaskID := fmt.Sprintf("list-%d", time.Now().UnixNano())
	body := map[string]any{
		"task_id":  listTaskID,
		"agent_id": agentID,
		"type":     "powershell",
		"payload":  map[string]string{"command": listCmd},
	}
	var resp map[string]any
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return completed // 查询失败,保守返回空(全传)
	}
	result, err := waitTaskResult(cfg, listTaskID, 30)
	if err != nil {
		return completed
	}
	stdout, _ := result["stdout"].(string)
	// 解析 "name|size\n..." 格式
	baseName := remotePath
	for _, line := range splitLines(stdout) {
		// line 格式: DigitalHuman.zip.ftpart00|10485760
		parts := splitBy(line, "|")
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		// 提取分片序号: <baseName>.ftpart<NN>
		prefix := baseName + ".ftpart"
		if len(name) <= len(prefix) {
			continue
		}
		idxStr := name[len(prefix):]
		var idx int
		fmt.Sscanf(idxStr, "%d", &idx)
		if idx >= 0 && idx < totalChunks {
			completed[idx] = true
		}
	}
	return completed
}

// workspaceProbePS 是 PowerShell 片段模板(%s = 分片 base 名):
// 探测含 <base>.ftpart00 的 workspace,结果存到 $ws。
// 目标机可能同时跑 master+slave 等多个 agent 进程,不能只取第一个进程的
// workspace(实测 master 先匹配导致去找 master-workspace);
// 这里收集所有 agent 进程的 workspace + 常见安装目录,取第一个实际含分片的。
const workspaceProbePS = `
$wsCandidates = @()
foreach ($n in @('go-rmm-master','go-rmm-slave','go-rmm','go-rmm-agent')) {
    Get-Process $n -ErrorAction SilentlyContinue | Where-Object { $_.Path } | ForEach-Object {
        $exeDir = Split-Path $_.Path
        $f = Join-Path $exeDir 'config.json'
        try { $w = (Get-Content $f -Raw -ErrorAction SilentlyContinue | ConvertFrom-Json).workspace } catch { $w = $null }
        if ($w) { $wsCandidates += $w } else { $wsCandidates += (Join-Path $exeDir 'workspace') }
    }
}
foreach ($d in @("$env:ProgramFiles\go-rmm-master","$env:ProgramFiles\go-rmm-slave","$env:ProgramFiles\go-rmm",'C:\go-rmm','C:\go-rmm\master','C:\go-rmm\slave','C:\go-rmm\master-workspace','C:\go-rmm\slave-workspace')) { $wsCandidates += $d }
$wsCandidates = $wsCandidates | Select-Object -Unique
$ws = $null
foreach ($w in $wsCandidates) {
    if (Test-Path (Join-Path $w '%s.ftpart00')) { $ws = $w; break }
}
if (-not $ws) { $ws = (Get-Location).Path }
`

// buildMergeCommand 构造 powershell 合并命令。
// 分片由 agent 的 resolveToWorkspace 落到 workspace,CLI 不知道 workspace
// 绝对路径,所以合并命令在目标机上自省(workspaceProbePS):
//  1. 探测含分片的 workspace(唯一真相)
//  2. 在 workspace 里合并分片 + 清理分片 + 输出 "MERGE_OK <sha256>"
func buildMergeCommand(remotePath string, totalChunks int) string {
	base := remotePath
	return fmt.Sprintf(`%s
# 1. 探测 workspace(分片所在目录)
$partsDir = $ws
$base = '%s'
$first = Join-Path $partsDir ($base + '.ftpart00')
if (-not (Test-Path $first)) {
    Write-Output "MERGE_FAIL partsdir=$partsDir notfound"
    exit 1
}
# 2. 合并
$dst = Join-Path $partsDir $base
$parts = @()
for ($i = 0; $i -lt %d; $i++) {
    $part = Join-Path $partsDir ($base + '.ftpart' + $i.ToString('00'))
    if (Test-Path $part) { $parts += $part }
}
if ($parts.Count -lt %d) { Write-Output "MERGE_FAIL parts=$($parts.Count)/%d partsdir=$partsDir"; exit 1 }
$fs = [System.IO.File]::Create($dst)
foreach ($p in $parts) {
    $bytes = [System.IO.File]::ReadAllBytes($p)
    $fs.Write($bytes, 0, $bytes.Length)
}
$fs.Close()
# 3. 清理分片 + 输出 SHA256
Remove-Item (Join-Path $partsDir ($base + '.ftpart*')) -Force -ErrorAction SilentlyContinue
$hash = (Get-FileHash $dst -Algorithm SHA256).Hash
Write-Output "MERGE_OK $($hash.ToLower())"
`, fmt.Sprintf(workspaceProbePS, base), base, totalChunks, totalChunks, totalChunks)
}

// computeFileSHA256 算文件的 SHA256(流式,64KB buffer)。
func computeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 64*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractSHAFromOutput 从 "MERGE_OK <sha256>" 格式提取 hash。
func extractSHAFromOutput(stdout string) string {
	for _, line := range splitLines(stdout) {
		if len(line) > 10 && line[:9] == "MERGE_OK " {
			return line[9:]
		}
	}
	return ""
}

// waitTaskResult 轮询任务结果(用于合并等耗时操作)。
// Relay 返回 {"task": {...}, "result": {stdout, status, ...}},这里展平返回 result。
func waitTaskResult(cfg config, taskID string, timeoutSec int) (map[string]any, error) {
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		var resp map[string]any
		if err := apiGet(cfg, "/api/tasks/"+taskID, &resp); err == nil {
			// task 状态在外层,result 在 "result" 字段
			task, _ := resp["task"].(map[string]any)
			status, _ := task["status"].(string)
			if status == "completed" || status == "failed" {
				result, _ := resp["result"].(map[string]any)
				if result == nil {
					result = map[string]any{}
				}
				result["status"] = status
				return result, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("超时")
}

// splitLines 简单分行。
func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, ch := range s {
		if ch == '\n' || ch == '\r' {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
		} else {
			cur += string(ch)
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// splitBy 简单分割。
func splitBy(s, sep string) []string {
	var parts []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			parts = append(parts, cur)
			cur = ""
			i += len(sep) - 1
		} else {
			cur += string(s[i])
		}
	}
	parts = append(parts, cur)
	return parts
}

// newPostRequest 构造 POST 请求(避免主文件 import bytes 冲突)。
func newPostRequest(u string, body []byte) (*http.Request, error) {
	return http.NewRequest("POST", u, bytesReader(body))
}

func bytesReader(b []byte) io.Reader { return &byteReader{data: b} }

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
