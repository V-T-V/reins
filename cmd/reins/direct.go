package main

// Phase 1 内网直连传输:CLI 侧直连优先 + 自动回落中继。
//
// 决策序(每次 file upload/download):
//   1. --no-direct / REINS_NO_DIRECT 未设置时,先 POST /api/transfer/direct 向
//      Relay 换一次性票据(Agent 未开启直连 → direct=false,静默走中继)
//   2. 按 Agent 上报的直连地址逐个 probe(1.5s 超时),选第一个可达者
//   3. begin(票据消费,拿 resume_offset/data_token)→ data/fetch 传字节
//   4. 任何一步失败(跨网段不可达/票据过期/传输出错)→ 回落既有中继路径,
//      上传天然续传(.part 语义两条路径同源)
//
// 直连不可达的探测代价控制:首个 probe 失败后本进程内禁用直连
// (directDisabled),后续分片不再逐个吃探测超时。

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// noDirectMode --no-direct 显式关闭直连(或 REINS_NO_DIRECT=1)。
var noDirectMode bool

// directDisabled 进程级直连熔断:首个 probe 失败/Agent 未开启后置位,
// 后续分片不再重复探测(省每片 1.5s×N 超时)。
var directDisabled atomic.Bool

// errDirectUnavailable 直连不可用(Agent 未开启/地址不可达/票据通道失败)。
// 该错误静默回落中继(正常形态,不告警);其它错误打印一行再回落。
var errDirectUnavailable = errors.New("direct unavailable")

// applyDirectFlag 解析并剥离 --no-direct(照 applyChunkFlags 模式)。
func applyDirectFlag(args []string) []string {
	for _, a := range args {
		if a == "--no-direct" || a == "--no-direct=true" || a == "--no-direct=1" {
			noDirectMode = true
			break
		}
	}
	if os.Getenv("REINS_NO_DIRECT") != "" {
		noDirectMode = true
	}
	// 布尔 flag:纯 token 过滤(stripValueFlags 会把下一个参数当值吞掉)
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--no-direct" || a == "--no-direct=true" || a == "--no-direct=1" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// directSharedClient 直连专用共享 client(连接复用:分片传输每片 ticket/probe/
// begin/data 四次调用复用同一条 TLS 连接,不再每次握手)。
// 自签 TLS 跳过证书校验——身份真实性由一次性票据保证,TLS 只提供加密。
var directSharedClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 票据即身份,见文件头注释
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	},
}

// directHTTPClient 返回直连共享 client。
func directHTTPClient() *http.Client { return directSharedClient }

// directSession 一次直连会话的票据与已选定的可达地址。
type directSession struct {
	ticketID string
	secret   string
	addrs    []string
	baseURL  string // probe 后确定(https://ip:port)
}

// requestDirectTicket 向 Relay 换一次性票据。
// 返回 (nil, nil) = Agent 未开启直连(正常回落);(nil, err) = 通道错误。
func requestDirectTicket(cfg config, agentID, path, direction string) (*directSession, error) {
	body, _ := json.Marshal(map[string]string{
		"agent_id": agentID, "path": path, "direction": direction,
	})
	req, err := http.NewRequest("POST", cfg.relay+"/api/transfer/direct", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", "application/json")
	// 走 controlClient:与其它 relay API 同源 TLS 策略(--insecure/--cacert),
	// 不对持有效证书的 relay 无条件降级校验
	resp, err := controlClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("票据请求失败: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Direct     bool     `json:"direct"`
		TicketID   string   `json:"ticket_id"`
		Secret     string   `json:"secret"`
		Addrs      []string `json:"addrs"`
		ExpiryUnix int64    `json:"expiry_unix"`
		Reason     string   `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("票据响应解析失败(HTTP %d): %w", resp.StatusCode, err)
	}
	if !out.Direct {
		directDisabled.Store(true) // Agent 未开启直连,本进程不再尝试
		return nil, fmt.Errorf("%w: %s", errDirectUnavailable, out.Reason)
	}
	return &directSession{ticketID: out.TicketID, secret: out.Secret, addrs: out.Addrs}, nil
}

// probe 逐地址探活,选定首个可达者(1.5s 请求级超时/地址)。
// 超时经请求 ctx 中断:无游离 goroutine、无未关闭连接(原手动 goroutine+select
// 的超时路径会泄漏等待中的请求与响应体)。
func (ds *directSession) probe(path, direction string) error {
	for _, addr := range ds.addrs {
		base := "https://" + addr
		body, _ := json.Marshal(map[string]string{"path": path, "direction": direction})
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/xfer/probe", bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("X-Reins-Ticket", ds.ticketID+":"+ds.secret)
		resp, err := directHTTPClient().Do(req)
		cancel()
		if err != nil {
			continue
		}
		ok := resp.StatusCode == http.StatusOK
		resp.Body.Close()
		if ok {
			ds.baseURL = base
			return nil
		}
	}
	directDisabled.Store(true) // 全部地址不可达,本进程不再尝试(省每片探测超时)
	return fmt.Errorf("%w: 直连地址全部不可达(%v)", errDirectUnavailable, ds.addrs)
}

// directBegin begin 响应(镜像 FileTransferReady + data_token)。
type directBeginResult struct {
	Accepted        bool   `json:"accepted"`
	AlreadyComplete bool   `json:"already_complete"`
	SHA256          string `json:"sha256"`
	Bytes           int64  `json:"bytes"`
	ResumeOffset    int64  `json:"resume_offset"`
	Size            int64  `json:"size"`
	DataToken       string `json:"data_token"`
	TransferID      string `json:"transfer_id"`
	Reason          string `json:"reason"`
	Error           string `json:"error"`
}

// begin 握手:票据一次性消费 → 路径策略校验 → 续传偏移。
func (ds *directSession) begin(path, direction string, extra map[string]any) (*directBeginResult, error) {
	body := map[string]any{"path": path, "direction": direction}
	for k, v := range extra {
		body[k] = v
	}
	if body["transfer_id"] == nil {
		// uuid 而非 UnixNano:并发分片同纳秒撞 ID → agent 侧 sessions/dataTk
		// 相互覆盖 → data_token 401 + pathInUse 泄漏(E2E 逮住)
		body["transfer_id"] = "direct-" + uuid.NewString()
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", ds.baseURL+"/xfer/begin", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Reins-Ticket", ds.ticketID+":"+ds.secret)
	resp, err := directHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out directBeginResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: 票据被拒(过期/已用)", errDirectUnavailable)
		}
		if resp.StatusCode == http.StatusForbidden {
			return nil, errBlocked("直连被拒绝: " + out.Reason + out.Error)
		}
		return nil, fmt.Errorf("begin HTTP %d: %s %s", resp.StatusCode, out.Reason, out.Error)
	}
	return &out, nil
}

// uploadData 上传数据流(body 从 resume 偏移起的剩余字节)。
func (ds *directSession) uploadData(transferID, dataToken string, body io.Reader, length int64) (int64, string, error) {
	req, err := http.NewRequest("POST", ds.baseURL+"/xfer/data?transfer_id="+transferID, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-Reins-Data-Token", dataToken)
	req.ContentLength = length
	resp, err := directHTTPClient().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
		Error  string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("data HTTP %d: %s", resp.StatusCode, out.Error)
	}
	return out.Bytes, out.SHA256, nil
}

// fetch 下载流到 out,返回 trailer 携带的 SHA256(缺失为空,调用方容错)。
func (ds *directSession) fetch(transferID, dataToken string, out io.Writer) (string, error) {
	req, err := http.NewRequest("POST", ds.baseURL+"/xfer/fetch?transfer_id="+transferID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Reins-Data-Token", dataToken)
	resp, err := directHTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var eb struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return "", fmt.Errorf("fetch HTTP %d: %s", resp.StatusCode, eb.Error)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("写入数据失败: %w", err)
	}
	return resp.Trailer.Get("X-Xfer-Sha256"), nil
}

// --- 高层封装:单文件/分片 ---

// tryDirectUploadFile 单文件直连上传(成功返回 nil;errDirectUnavailable 静默回落)。
func tryDirectUploadFile(cfg config, agentID, localPath, remotePath string) error {
	ds, err := requestDirectTicket(cfg, agentID, remotePath, "upload")
	if err != nil {
		return err
	}
	if err := ds.probe(remotePath, "upload"); err != nil {
		return err
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	sha, err := computeFileSHA256(localPath)
	if err != nil {
		return err
	}
	begin, err := ds.begin(remotePath, "upload", map[string]any{
		"size": fi.Size(), "sha256": sha,
	})
	if err != nil {
		return err
	}
	if begin.AlreadyComplete {
		if !syncQuiet.Load() {
			fmt.Printf("✅ 直连上传完成(远端已完整): %s\n", remotePath)
			fmt.Printf("   SHA256: %s\n", begin.SHA256)
		}
		return nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if begin.ResumeOffset > 0 {
		if _, err := f.Seek(begin.ResumeOffset, io.SeekStart); err != nil {
			return err
		}
		if !syncQuiet.Load() {
			fmt.Printf("   直连续传: 跳过本地前 %s\n", formatSize(begin.ResumeOffset))
		}
	}
	var body io.Reader = f
	if fi.Size() > 0 {
		body = &progressReader{r: f, total: fi.Size(), label: "直连上传"}
	}
	if !syncQuiet.Load() {
		fmt.Printf("⚡ 直连上传 %s → %s:%s\n", localPath, agentID, remotePath)
	}
	bytes, gotSHA, err := ds.uploadData(begin.TransferID, begin.DataToken, body, fi.Size()-begin.ResumeOffset)
	if err != nil {
		return err
	}
	if !syncQuiet.Load() {
		fmt.Println()
		fmt.Printf("✅ 直连上传完成: %s\n", formatSize(bytes))
		fmt.Printf("   Agent SHA256: %s\n", gotSHA)
		if gotSHA == sha {
			fmt.Printf("   ✅ 本地 SHA256 一致\n")
		}
	}
	if gotSHA != "" && gotSHA != sha {
		return fmt.Errorf("SHA256 校验失败: 本地=%s 远端=%s", sha[:16], gotSHA[:16])
	}
	return nil
}

// tryDirectUploadChunk 分片直连上传(路径绑定用基路径,票据前缀覆盖 .ftpartNNNN)。
func tryDirectUploadChunk(cfg config, agentID, chunkName string, section *io.SectionReader, length int64, chunkSHA, basePath string) error {
	ds, err := requestDirectTicket(cfg, agentID, basePath, "upload")
	if err != nil {
		return err
	}
	if err := ds.probe(basePath, "upload"); err != nil {
		return err
	}
	begin, err := ds.begin(chunkName, "upload", map[string]any{"size": length, "sha256": chunkSHA})
	if err != nil {
		return err
	}
	if begin.AlreadyComplete {
		return nil
	}
	if begin.ResumeOffset > 0 {
		if _, err := section.Seek(begin.ResumeOffset, io.SeekStart); err != nil {
			return err
		}
	}
	_, _, err = ds.uploadData(begin.TransferID, begin.DataToken, section, length-begin.ResumeOffset)
	return err
}

// tryDirectDownloadFile 单文件直连下载(写 <local>.part 再 rename,原子语义)。
func tryDirectDownloadFile(cfg config, agentID, remotePath, localPath string) error {
	ds, err := requestDirectTicket(cfg, agentID, remotePath, "download")
	if err != nil {
		return err
	}
	if err := ds.probe(remotePath, "download"); err != nil {
		return err
	}
	begin, err := ds.begin(remotePath, "download", nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	partPath := localPath + ".dlpart"
	f, err := os.Create(partPath)
	if err != nil {
		return err
	}
	fmt.Printf("⚡ 直连下载 %s:%s → %s\n", agentID, remotePath, localPath)
	pw := &progressWriter{w: f, total: begin.Size, label: "直连下载"}
	trailerSHA, err := ds.fetch(begin.TransferID, begin.DataToken, pw)
	f.Close()
	if err != nil {
		os.Remove(partPath)
		return err
	}
	pw.finish()
	fmt.Println()
	if trailerSHA != "" {
		localSHA, herr := computeFileSHA256(partPath)
		if herr == nil && localSHA != trailerSHA {
			os.Remove(partPath)
			return fmt.Errorf("SHA256 校验失败: 本地=%s 远端=%s", localSHA[:16], trailerSHA[:16])
		}
	}
	if err := os.Rename(partPath, localPath); err != nil {
		os.Remove(partPath)
		return err
	}
	if !syncQuiet.Load() {
		fmt.Printf("✅ 直连下载完成: %s\n", localPath)
	}
	return nil
}

// tryDirectDownloadChunk 分片直连下载(offset/limit 分片读取)。
func tryDirectDownloadChunk(cfg config, agentID, remotePath, partFile string, off, thisSize int64) error {
	ds, err := requestDirectTicket(cfg, agentID, remotePath, "download")
	if err != nil {
		return err
	}
	if err := ds.probe(remotePath, "download"); err != nil {
		return err
	}
	begin, err := ds.begin(remotePath, "download", map[string]any{"offset": off, "limit": thisSize})
	if err != nil {
		return err
	}
	f, err := os.Create(partFile)
	if err != nil {
		return err
	}
	_, err = ds.fetch(begin.TransferID, begin.DataToken, f)
	f.Close()
	if err != nil {
		os.Remove(partFile)
		return err
	}
	info, serr := os.Stat(partFile)
	if serr != nil {
		return serr
	}
	if info.Size() != thisSize {
		os.Remove(partFile)
		return fmt.Errorf("分片大小不匹配: 期望 %d 实际 %d", thisSize, info.Size())
	}
	return nil
}
