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
//
// O1 优化:并发上传(worker pool + 信号量),把串行的 RTT 等待重叠起来。
// 跨网络实测从 0.54 MB/s(串行)提升到带宽极限。

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 分片参数(部分可由 CLI flag 覆盖,见 main.go)
const (
	chunkThreshold = 50 * 1024 * 1024 // > 50MB 走分片模式
)

// 运行时可调参数(由 flag 注入,默认值兜底)
var (
	chunkSizeArg    int64 = 10 * 1024 * 1024 // --chunk-size,每片大小
	concurrencyArg  int   = 4                // --concurrency,并发上传数
	chunkMaxRetryArg int  = 3                // --chunk-retry,每片重试次数
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
	chunkSz := chunkSizeArg
	totalChunks := int((totalSize + chunkSz - 1) / chunkSz)
	conc := concurrencyArg
	if conc < 1 {
		conc = 1
	}
	if conc > totalChunks {
		conc = totalChunks
	}

	fmt.Printf("📦 分片上传: %s (%s, %d 片 × %s) → %s:%s\n",
		localPath, formatSize(totalSize), totalChunks, formatSize(chunkSz), agentID, remotePath)
	fmt.Printf("   并发: %d 路,重试: %d 次/片\n", conc, chunkMaxRetryArg)

	// 1. 本地算整文件 SHA256
	fmt.Printf("   计算 SHA256...\n")
	localSHA, err := computeFileSHA256(localPath)
	if err != nil {
		return fmt.Errorf("算 SHA256 失败: %w", err)
	}
	fmt.Printf("   SHA256: %s\n", localSHA)

	// 2. 文件存在性检查(实际打开在每个 worker 内,各自独立 fd)
	if _, err := os.Stat(localPath); err != nil {
		return err
	}

	// 3. 查已传分片(断点续传)
	completed := checkCompletedChunks(cfg, agentID, remotePath, totalChunks, totalSize)
	skipCount := len(completed)
	if skipCount > 0 {
		fmt.Printf("   断点续传:跳过 %d/%d 片已完成\n", skipCount, totalChunks)
	}

	// 4. 并发上传(worker pool)
	startTime := time.Now()
	var uploaded atomic.Int64
	// 先把跳过的片计入进度
	for i := 0; i < totalChunks; i++ {
		if completed[i] {
			off := int64(i) * chunkSz
			sz := chunkSz
			if off+chunkSz > totalSize {
				sz = totalSize - off
			}
			uploaded.Add(sz)
		}
	}

	// 任务队列:所有未完成的分片索引
	type job struct{ idx int }
	jobs := make(chan job, totalChunks)
	errs := make(chan error, conc) // 第一个错误
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once

	// 计算每片大小
	chunkLen := func(i int) int64 {
		off := int64(i) * chunkSz
		if off+chunkSz > totalSize {
			return totalSize - off
		}
		return chunkSz
	}

	// 生产者:投递未完成分片
	var prodWG sync.WaitGroup
	prodWG.Add(1)
	go func() {
		defer prodWG.Add(-1)
		defer close(jobs)
		for i := 0; i < totalChunks; i++ {
			if completed[i] {
				continue
			}
			select {
			case jobs <- job{idx: i}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// 消费者:并发上传
	// 每个 worker 独立打开文件 fd,避免共享 *os.File 的 Seek+Read 在多 goroutine 间交错
	// (实测共享 fd 并发 Seek 导致读到错位数据 → SHA256 不一致)。
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			wf, err := os.Open(localPath)
			if err != nil {
				once.Do(func() {
					cancel()
					errs <- fmt.Errorf("worker 打开文件失败: %w", err)
				})
				return
			}
			defer wf.Close()

			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				off := int64(j.idx) * chunkSz
				thisSize := chunkLen(j.idx)
				chunkName := fmt.Sprintf("%s.ftpart%02d", remotePath, j.idx)

				var lastErr error
				for retry := 0; retry < chunkMaxRetryArg; retry++ {
					if ctx.Err() != nil {
						return
					}
					// R3 流式:用 SectionReader 边读边传(不再 make 10MB slice)。
					// 内存从 10MB/worker 降到 ~64KB/worker,且 ContentLength 正确。
					section := io.NewSectionReader(wf, off, thisSize)
					if err := uploadSingleChunk(cfg, agentID, chunkName, section, thisSize, localPath, j.idx); err != nil {
						lastErr = err
						// R4 指数退避:1s/2s/4s,上限 30s;"占用冲突"用更长退避让前一个传输完成
						backoff := time.Duration(1<<retry) * time.Second
						if backoff > 30*time.Second {
							backoff = 30 * time.Second
						}
						if strings.Contains(err.Error(), "占用") {
							backoff = 5 * time.Second // 占用冲突等更久
						}
						select {
						case <-time.After(backoff):
						case <-ctx.Done():
							return
						}
						continue
					}
					uploaded.Add(thisSize)
					lastErr = nil
					break
				}
				if lastErr != nil {
					once.Do(func() {
						cancel()
						errs <- fmt.Errorf("分片 %d 传 %d 次均失败: %w", j.idx, chunkMaxRetryArg, lastErr)
					})
					return
				}
			}
		}()
	}

	// 进度刷新(主 goroutine,避免多 goroutine 交错输出)
	// R6:瞬时速率(滑动窗口最近 2s)+ ETA
	var lastSample int64
	lastSampleTime := startTime
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
outer:
	for {
		select {
		case <-done:
			break outer
		case <-ctx.Done():
			break outer
		case <-ticker.C:
			up := uploaded.Load()
			elapsed := time.Since(startTime).Seconds()
			avgRate := float64(up) / 1024 / 1024 / elapsed
			// R6:瞬时速率 = 最近采样窗口的 Δbytes / Δt
			now := time.Now()
			dt := now.Sub(lastSampleTime).Seconds()
			instRate := avgRate
			if dt > 0.1 {
				instRate = float64(up-lastSample) / 1024 / 1024 / dt
			}
			lastSample = up
			lastSampleTime = now
			pct := up * 100 / totalSize
			// ETA = 剩余字节 / 瞬时速率
			eta := ""
			if instRate > 0.01 {
				remain := float64(totalSize-up) / 1024 / 1024 / instRate
				if remain > 0 {
					eta = "| ETA " + formatDuration(int64(remain))
				}
			}
			fmt.Printf("\r   进度: %d%% (%s / %s) | %.2f MB/s %s | %d 路   ",
				pct, formatSize(up), formatSize(totalSize), instRate, eta, conc)
		}
	}
	fmt.Println()

	// 检查错误
	select {
	case err := <-errs:
		return err
	default:
	}

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

	// 等合并结果(合并 1GB 需要时间,给 600s)
	result, err := waitTaskResult(cfg, mergeTaskID, 600)
	if err != nil {
		return fmt.Errorf("获取合并结果失败: %w", err)
	}

	// 6. 校验 SHA256
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
// R3 流式:body 是分片内容 reader(SectionReader),length 是精确字节数。
// R4:失败时读取错误 body 前 512 字节拼进错误信息(区分磁盘满/路径拒绝/占用)。
func uploadSingleChunk(cfg config, agentID, chunkName string, body io.Reader, length int64, srcFile string, idx int) error {
	transferID := fmt.Sprintf("xfer-%d-chunk%d", time.Now().UnixNano(), idx)

	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", chunkName)
	q.Set("transfer_id", transferID)
	q.Set("size", fmt.Sprintf("%d", length))
	u := cfg.relay + "/api/transfer/upload?" + q.Encode()

	req, err := http.NewRequest("POST", u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.ContentLength = length // R3:精确 CL(不再 -1),nginx/Agent 更稳

	resp, err := streamingClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// R4:读错误 body 前 512 字节,拼进错误信息
		errSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(errSnippet))
		if msg != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// checkCompletedChunks 查目标机已完成的分片(断点续传)。
// 返回 map[int]bool,idx → 是否已完成。
// O6 修复:严格校验分片名(纯数字序号)+ 大小匹配,拒绝 .ftpartNN.part 临时文件。
func checkCompletedChunks(cfg config, agentID string, remotePath string, totalChunks int, totalSize int64) map[int]bool {
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
	prefix := baseName + ".ftpart"
	// isDigit 判断纯数字(2位序号),拒绝 .ftpart08.part 这种未完成临时文件
	isDigit := func(s string) bool {
		if s == "" {
			return false
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	for _, line := range splitLines(stdout) {
		// line 格式: DigitalHuman.zip.ftpart00|10485760
		parts := splitBy(line, "|")
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		if len(name) <= len(prefix) {
			continue
		}
		idxStr := name[len(prefix):]
		// 序号必须是纯数字(无 .part 后缀),否则是传输中的临时文件
		if !isDigit(idxStr) {
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
			continue
		}
		if idx < 0 || idx >= totalChunks {
			continue
		}
		// 校验大小:已完成的片应有完整 chunkSize(最后一片可能更小)
		var sz int64
		fmt.Sscanf(parts[1], "%d", &sz)
		expected := chunkSizeArg
		off := int64(idx) * chunkSizeArg
		if off+chunkSizeArg > totalSize {
			expected = totalSize - off
		}
		if sz == expected {
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
    # 兜底:正式名不存在时,尝试 .part 后缀(Agent rename 偶尔未完成的临时文件)
    if (-not (Test-Path $part)) { $part = $part + '.part' }
    if (Test-Path $part) { $parts += $part }
}
if ($parts.Count -lt %d) { Write-Output "MERGE_FAIL parts=$($parts.Count)/%d partsdir=$partsDir"; exit 1 }
$fs = [System.IO.File]::Create($dst)
# O2 优化:流式 CopyTo(64KB 内部 buffer),替代 ReadAllBytes 整片读进内存。
# 合并 101 片 × 10MB 内存峰值从 ~1GB → ~64KB。
foreach ($p in $parts) {
    $inStream = [System.IO.File]::OpenRead($p)
    $inStream.CopyTo($fs)
    $inStream.Close()
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
