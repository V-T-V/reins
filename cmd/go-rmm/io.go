// CLI 辅助:bufio/base64/HTTP client/退出码。
//
// 单独抽文件避免 main.go 膨胀,集中管理可复用基础设施。
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// --- bufio ---

type bufioReader = bufio.Reader

func newBufioReader(r io.Reader) *bufio.Reader {
	return bufio.NewReader(r)
}

// stdinReader 是全局共享的 stdin bufio.Reader。
//
// C4 修复:readLine 每次新建 bufio.Reader 会导致管道输入丢数据——
// 第一次 Read 可能从 OS 多读 4KB 进 bufio 内部 buffer,
// 第二次 newReader 看不到这些 buffer 字节,直接从 fd 当前位置继续读 → 跳过。
// 用全局单例确保 buffer 状态跨调用保留。
var (
	stdinOnce  sync.Once
	stdinBReader *bufio.Reader
)

func sharedStdinReader() *bufio.Reader {
	stdinOnce.Do(func() {
		stdinBReader = bufio.NewReader(os.Stdin)
	})
	return stdinBReader
}

// --- base64 ---

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// --- HTTP 客户端 ---
//
// C1 修复:之前每个 apiGet/apiPost/fileUpload 都新建 http.Client,
// 每次都走完整 TCP+TLS 握手。改为两个共享 client:
//   - controlClient:控制平面(GET/POST JSON),带 60s 超时
//   - streamingClient:数据平面(文件传输/SSE),无超时(靠 ctx)
// 两者共享一个 Transport,复用连接池。

// proxyFunc 代理决策函数(P2-B)。
// 对 loopback(localhost/127.0.0.1/[::1])目标直连,不走代理——
// 用户本机 http_proxy 会劫持 127.0.0.1 流量,导致本地 Relay/隧道请求
// 被送到代理服务器而假死(会话实测:隧道 8s 超时,curl --noproxy 才通)。
// 其余目标沿用环境代理(HTTP_PROXY/HTTPS_PROXY/NO_PROXY)。
func proxyFunc(req *http.Request) (*url.URL, error) {
	host := req.URL.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return nil, nil // 直连
	}
	return http.ProxyFromEnvironment(req)
}

var (
	httpTransport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               proxyFunc,
	}

	controlClient = &http.Client{
		Transport: httpTransport,
		Timeout:   60 * time.Second, // C2:从 10s/30s 统一提到 60s,适应慢 Relay
	}

	streamingClient = &http.Client{
		Transport: httpTransport,
		Timeout:   0, // 流式传输不设超时,靠 ctx
	}
)

// applyTLSConfig 根据配置调整 httpTransport 的 TLS 行为。
// P1-S9 修复:支持自签证书(--insecure 跳过校验 / --cacert 指定 CA)。
// 在 main 的 flag 解析后调用一次。非线程安全(仅启动期调用)。
func applyTLSConfig(cfg config) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	if cfg.cacert != "" {
		pemData, err := os.ReadFile(cfg.cacert)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pemData) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	// 重新构造 Transport(仅启动期,无并发)
	httpTransport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               proxyFunc, // P2-B:loopback 直连
		TLSClientConfig:     tlsCfg,
	}
	controlClient.Transport = httpTransport
	streamingClient.Transport = httpTransport
}

// outputJSON 把 v 以 JSON 格式打印到 stdout(用于 --output json 模式)。
// P2 修复:机器可读输出,避免脚本 grep/awk 解析对齐文本的脆弱性。
func outputJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// jsonMode 全局 flag:是否启用 JSON 输出(由 main 设置)。
var jsonMode bool

// filterCLIXML 过滤 PowerShell CLIXML 噪音输出。
// PowerShell 在非交互模式下 stderr 会输出 #< CLIXML\n<Objs>...</Objs> 编码块,
// 包含进度条/警告等噪音。此函数移除这些块,只保留有用文本。
func filterCLIXML(s string) string {
	if s == "" {
		return s
	}
	// 移除 #< CLIXML 开头的块
	if idx := strings.Index(s, "#< CLIXML"); idx >= 0 {
		// 找 CLIXML 块结束(</Objs> 或到末尾)
		end := strings.Index(s[idx:], "</Objs>")
		if end >= 0 {
			end += idx + len("</Objs>")
			s = s[:idx] + s[end:]
		} else {
			s = s[:idx] // 无结束标记,截断
		}
	}
	// 移除残留的 _x000D__x000A_ 编码(CR LF 的 CLIXML 编码)
	s = strings.ReplaceAll(s, "_x000D__x000A_", "")
	return strings.TrimSpace(s)
}
