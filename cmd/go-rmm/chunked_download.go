// CLI 分片下载(O3 优化)。
//
// 大文件下载切成 N 个独立分片,每片一个 HTTP 请求(带 offset/limit),
// 并发下载到本地 .ftpartNN,全部完成后合并 + SHA256 校验。
//
// 解决问题:
//   - 单连接流式下载在慢链路撞 Relay 30min ctx 超时
//   - 并发下载重叠 RTT,提速(同上传)
//   - 断点续传:本地 .ftpartNN 存在则跳过
//
// 协议(复用 /api/transfer/download + 新增 offset/limit query 参数):
//   - Agent 收到 Offset>0 的 init 时 Seek 到偏移,limit>0 时只读 limit 字节
//   - 每片独立 transfer_id,写到本地 .ftpartNN

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

// remoteFileSize 查远端文件大小(下发 powershell)。
func remoteFileSize(cfg config, agentID, remotePath string) (int64, error) {
	cmd := fmt.Sprintf(`%s
$f = Join-Path $ws '%s'
if (Test-Path $f) { (Get-Item $f).Length } else { Write-Output 'NOTFOUND' }
`, fmt.Sprintf(workspaceProbePS, remotePath), remotePath)
	taskID := fmt.Sprintf("stat-%d", time.Now().UnixNano())
	body := map[string]any{
		"task_id":  taskID,
		"agent_id": agentID,
		"type":     "powershell",
		"payload":  map[string]string{"command": cmd},
	}
	if err := apiPost(cfg, "/api/tasks", body, nil); err != nil {
		return 0, err
	}
	result, err := waitTaskResult(cfg, taskID, 30)
	if err != nil {
		return 0, err
	}
	stdout, _ := result["stdout"].(string)
	for _, line := range splitLines(stdout) {
		if line == "NOTFOUND" {
			return 0, fmt.Errorf("远端文件不存在")
		}
		var sz int64
		if _, err := fmt.Sscanf(line, "%d", &sz); err == nil && sz > 0 {
			return sz, nil
		}
	}
	return 0, fmt.Errorf("无法解析文件大小: %s", stdout)
}

// remoteFileSHA256 查远端文件 SHA256(下发 powershell)。
func remoteFileSHA256(cfg config, agentID, remotePath string) (string, error) {
	cmd := fmt.Sprintf(`%s
$f = Join-Path $ws '%s'
if (Test-Path $f) { (Get-FileHash $f -Algorithm SHA256).Hash.ToLower() } else { Write-Output 'NOTFOUND' }
`, fmt.Sprintf(workspaceProbePS, remotePath), remotePath)
	taskID := fmt.Sprintf("sha-%d", time.Now().UnixNano())
	body := map[string]any{
		"task_id":  taskID,
		"agent_id": agentID,
		"type":     "powershell",
		"payload":  map[string]string{"command": cmd},
	}
	if err := apiPost(cfg, "/api/tasks", body, nil); err != nil {
		return "", err
	}
	result, err := waitTaskResult(cfg, taskID, 120)
	if err != nil {
		return "", err
	}
	stdout, _ := result["stdout"].(string)
	for _, line := range splitLines(stdout) {
		if line == "NOTFOUND" {
			return "", fmt.Errorf("远端文件不存在")
		}
		// SHA256 是 64 位十六进制
		if len(line) == 64 {
			return line, nil
		}
	}
	return "", fmt.Errorf("无法解析 SHA256: %s", stdout)
}

// fileDownloadChunked 分片下载大文件。
//
// 流程:
//  1. 查远端文件大小 + SHA256(powershell 任务)
//  2. 检查本地已存在的 .ftpartNN(断点续传)
//  3. 并发下载每片(offset/limit → /api/transfer/download)
//  4. 本地合并 + SHA256 校验
func fileDownloadChunked(cfg config, agentID, remotePath, localPath string, totalSize int64) error {
	chunkSz := chunkSizeArg
	totalChunks := int((totalSize + chunkSz - 1) / chunkSz)
	conc := concurrencyArg
	if conc < 1 {
		conc = 1
	}
	if conc > totalChunks {
		conc = totalChunks
	}

	fmt.Printf("📦 分片下载: %s:%s (%s, %d 片 × %s) → %s\n",
		agentID, remotePath, formatSize(totalSize), totalChunks, formatSize(chunkSz), localPath)
	fmt.Printf("   并发: %d 路,重试: %d 次/片\n", conc, chunkMaxRetryArg)

	// 1. 查远端 SHA256(用于最终校验,大小已知不用查)
	fmt.Printf("   查询远端 SHA256...\n")
	remoteSHA, err := remoteFileSHA256(cfg, agentID, remotePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️ 查询 SHA256 失败(继续,跳过校验): %v\n", err)
		remoteSHA = ""
	} else {
		fmt.Printf("   远端 SHA256: %s\n", remoteSHA)
	}

	// 2. 断点续传:检查本地 .ftpartNN
	partPrefix := localPath + ".ftpart"
	completed := make(map[int]bool)
	for i := 0; i < totalChunks; i++ {
		partFile := fmt.Sprintf("%s%02d", partPrefix, i)
		if info, err := os.Stat(partFile); err == nil {
			// 校验大小:已完成的片应有完整 chunkSz(最后一片可能更小)
			expected := chunkSz
			off := int64(i) * chunkSz
			if off+chunkSz > totalSize {
				expected = totalSize - off
			}
			if info.Size() == expected {
				completed[i] = true
			}
		}
	}
	if len(completed) > 0 {
		fmt.Printf("   断点续传:跳过 %d/%d 片已完成\n", len(completed), totalChunks)
	}

	// 3. 并发下载
	startTime := time.Now()
	var downloaded atomic.Int64
	for i := 0; i < totalChunks; i++ {
		if completed[i] {
			off := int64(i) * chunkSz
			sz := chunkSz
			if off+chunkSz > totalSize {
				sz = totalSize - off
			}
			downloaded.Add(sz)
		}
	}

	type job struct{ idx int }
	jobs := make(chan job, totalChunks)
	errs := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once

	// 生产者
	go func() {
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

	chunkLen := func(i int) int64 {
		off := int64(i) * chunkSz
		if off+chunkSz > totalSize {
			return totalSize - off
		}
		return chunkSz
	}

	// 消费者
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				off := int64(j.idx) * chunkSz
				thisSize := chunkLen(j.idx)
				partFile := fmt.Sprintf("%s%02d", partPrefix, j.idx)

				var lastErr error
				for retry := 0; retry < chunkMaxRetryArg; retry++ {
					if ctx.Err() != nil {
						return
					}
					if err := downloadSingleChunk(cfg, agentID, remotePath, partFile, off, thisSize, j.idx); err != nil {
						lastErr = err
						// R4 指数退避:1s/2s/4s,上限 30s
						backoff := time.Duration(1<<retry) * time.Second
						if backoff > 30*time.Second {
							backoff = 30 * time.Second
						}
						select {
						case <-time.After(backoff):
						case <-ctx.Done():
							return
						}
						continue
					}
					downloaded.Add(thisSize)
					lastErr = nil
					break
				}
				if lastErr != nil {
					once.Do(func() {
						cancel()
						errs <- fmt.Errorf("分片 %d 下载 %d 次均失败: %w", j.idx, chunkMaxRetryArg, lastErr)
					})
					return
				}
			}
		}()
	}

	// 进度(R6:瞬时速率 + ETA)
	var lastSample int64
	lastSampleTime := startTime
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
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
			dl := downloaded.Load()
			elapsed := time.Since(startTime).Seconds()
			avgRate := float64(dl) / 1024 / 1024 / elapsed
			now := time.Now()
			dt := now.Sub(lastSampleTime).Seconds()
			instRate := avgRate
			if dt > 0.1 {
				instRate = float64(dl-lastSample) / 1024 / 1024 / dt
			}
			lastSample = dl
			lastSampleTime = now
			pct := dl * 100 / totalSize
			eta := ""
			if instRate > 0.01 {
				remain := float64(totalSize-dl) / 1024 / 1024 / instRate
				if remain > 0 {
					eta = "| ETA " + formatDuration(int64(remain))
				}
			}
			fmt.Printf("\r   进度: %d%% (%s / %s) | %.2f MB/s %s | %d 路   ",
				pct, formatSize(dl), formatSize(totalSize), instRate, eta, conc)
		}
	}
	fmt.Println()

	select {
	case err := <-errs:
		return err
	default:
	}

	// 4. 本地合并 + 校验(S2:合并时流式算 SHA256,省一次全量读)
	fmt.Printf("   合并分片 + 校验...\n")
	localSHA, err := mergePartsLocal(partPrefix, localPath, totalChunks)
	if err != nil {
		return fmt.Errorf("合并失败: %w", err)
	}
	// 清理分片
	for i := 0; i < totalChunks; i++ {
		os.Remove(fmt.Sprintf("%s%02d", partPrefix, i))
	}

	elapsed := time.Since(startTime)
	fmt.Printf("✅ 分片下载完成: %s\n", formatSize(totalSize))
	fmt.Printf("   耗时: %s (%.2f MB/s)\n", elapsed.Truncate(time.Second), float64(totalSize)/elapsed.Seconds()/1024/1024)
	fmt.Printf("   本地 SHA256: %s\n", localSHA)
	if remoteSHA != "" {
		if localSHA == remoteSHA {
			fmt.Printf("   ✅ SHA256 一致\n")
		} else {
			fmt.Fprintf(os.Stderr, "   ⚠️  校验不一致!远端=%s\n", remoteSHA)
			return fmt.Errorf("SHA256 校验失败")
		}
	}
	return nil
}

// downloadSingleChunk 下载单个分片到 partFile。
// off = 文件内偏移,thisSize = 本次下载字节数。
func downloadSingleChunk(cfg config, agentID, remotePath, partFile string, off, thisSize int64, idx int) error {
	transferID := fmt.Sprintf("dl-%d-chunk%d", time.Now().UnixNano(), idx)
	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", remotePath)
	q.Set("transfer_id", transferID)
	q.Set("offset", fmt.Sprintf("%d", off))
	q.Set("limit", fmt.Sprintf("%d", thisSize))
	u := cfg.relay + "/api/transfer/download?" + q.Encode()

	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := streamingClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// R4:读错误 body 前 512 字节
		errSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(errSnippet))
		if msg != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 写到临时文件(原子替换语义:先写 .part 再 rename,但这里直接写 partFile,
	// 断点续传靠大小校验,不完整的片会被重新下载)
	f, err := os.Create(partFile)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	// 校验下载大小
	info, err := os.Stat(partFile)
	if err != nil {
		return err
	}
	if info.Size() != thisSize {
		return fmt.Errorf("分片 %d 大小不匹配: 期望 %d 实际 %d", idx, thisSize, info.Size())
	}
	return nil
}

// mergePartsLocal 本地合并分片,边合并边算 SHA256(S2:省一次全量读)。
// 返回合并后文件的 SHA256 hex。
func mergePartsLocal(partPrefix, dstFile string, totalChunks int) (string, error) {
	f, err := os.Create(dstFile)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// S2:MultiWriter 让 io.Copy 同时写文件 + 喂 hasher,合并完直接得到 hash
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	buf := make([]byte, 64*1024)
	for i := 0; i < totalChunks; i++ {
		partFile := fmt.Sprintf("%s%02d", partPrefix, i)
		pf, err := os.Open(partFile)
		if err != nil {
			return "", fmt.Errorf("打开分片 %d 失败: %w", i, err)
		}
		if _, err := io.CopyBuffer(mw, pf, buf); err != nil {
			pf.Close()
			return "", fmt.Errorf("合并分片 %d 失败: %w", i, err)
		}
		pf.Close()
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// computeLocalFileSHA256 计算本地文件 SHA256(供下载校验后用)。
func computeLocalFileSHA256(path string) (string, error) {
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

// 保留 unused 引用避免编译报错(json 在错误路径用到)
var _ = json.Marshal
