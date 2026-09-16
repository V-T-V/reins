package main

// Phase 1 内网直连传输:Agent 侧监听端点。
//
// 数据路径:CLI ──HTTPS(自签 TLS)──► Agent:xfer_listen
// 授权路径:CLI ──► Relay POST /api/transfer/direct ──控制WS推送hash──► Agent
//
// 三端点:
//   POST /xfer/probe  探活(票据 peek 校验,不消费)
//   POST /xfer/begin  握手(票据 verify+一次性消费 → HandleInit 复用中继
//                     全套语义:路径策略/续传偏移/ALREADY_COMPLETE 快路径,
//                     返回 data_token 绑定后续数据请求)
//   POST /xfer/data   上传数据流(从 begin 返回的 resume_offset 起发)
//   POST /xfer/fetch  下载数据流(octet-stream + trailer 携带 SHA256)
//
// 安全模型(与零信任姿态一致):
//   - 票据由 Relay 签发(其鉴权链已验证 CLI 身份),Agent 只存 SHA256 hash
//   - 一次性 + 60s 过期 + 路径(含 .ftpart 分片前缀)/方向绑定
//   - data_token 防会话劫持:同网段第三方拿到 transferID 也无 token 可用
//   - 自签 TLS:票据不落明文(防嗅探重放);身份真实性由票据本身保证
//     (Relay 已认证的请求才会产生合法票据)——TLS 层不校验证书属已知边界,
//     主动 MITM 在票据 60s 一次性窗口内仅能抢跑一次,事后审计可见
//   - 路径策略/per-chunk SHA256/断点续传全部复用 TransferEngine,与中继路径同源

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/QiuShichang/go-rmm/proto"
)

// --- 票据存储 ---

// xferTicketStore 直连票据存储:Relay 经控制 WS 推来的授权。
// 内存态、一次性、TTL 懒清理。
type xferTicketStore struct {
	mu      sync.Mutex
	tickets map[string]proto.XferTicketGrant
}

var xferTickets = &xferTicketStore{tickets: make(map[string]proto.XferTicketGrant)}

func (s *xferTicketStore) Add(g proto.XferTicketGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[g.TicketID] = g
}

// sweep 清理过期票据(周期调用,防堆积)。
func (s *xferTicketStore) sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, g := range s.tickets {
		if now.Unix() > g.ExpiryUnix {
			delete(s.tickets, id)
			n++
		}
	}
	return n
}

// verifyConsume 校验并消费票据(一次性)。consume=false 时仅查看(probe 用)。
// 校验:存在 + 未过期 + 方向匹配 + hash 恒时比对 + 路径绑定(精确或分片前缀)。
func (s *xferTicketStore) verifyConsume(ticketID, secret, path, direction string, consume bool) bool {
	s.mu.Lock()
	g, ok := s.tickets[ticketID]
	if ok && (time.Now().Unix() > g.ExpiryUnix) {
		// 过期即删(顺手清理)
		delete(s.tickets, ticketID)
		ok = false
	}
	if consume && ok {
		// 一次性:校验通过与否都消费(防对 hash 的在线穷举重试)
		delete(s.tickets, ticketID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	if g.Direction != direction {
		return false
	}
	if path != g.Path && !strings.HasPrefix(path, g.Path+".ftpart") {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(g.SecretHash)) == 1
}

// ticketAuth 从请求提取票据(X-Go-RMM-Ticket: "<id>:<secret>")并校验。
func ticketAuth(r *http.Request, path, direction string, consume bool) bool {
	h := r.Header.Get("X-Go-RMM-Ticket")
	id, secret, ok := strings.Cut(h, ":")
	if !ok || id == "" || secret == "" {
		return false
	}
	return xferTickets.verifyConsume(id, secret, path, direction, consume)
}

// --- 自签 TLS 证书 ---

// generateSelfSignedCert 生成 24h 有效期的自签证书(仅用于直连加密,
// 身份真实性由票据保证,客户端 InsecureSkipVerify + 票据校验)。
func generateSelfSignedCert(agentID string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "go-rmm-xfer-" + agentID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// --- 直连服务器 ---

// xferDirectServer Agent 侧直连监听服务器。
type xferDirectServer struct {
	srv    *http.Server
	ln     net.Listener
	addrs  []string // 上报给 Relay 的广播地址(ip:port)
	mu     sync.Mutex
	dataTk map[string]xferDataToken // transferID → 数据阶段凭证(会话绑定)
}

// xferDataToken begin 签发的数据阶段凭证(created 供 TTL 清理)。
type xferDataToken struct {
	token   string
	created time.Time
}

// advertiseAddrs 计算直连广播地址:xfer_advertise 显式覆盖,否则枚举非回环 IPv4。
func advertiseAddrs(listenAddr string, override []string) []string {
	if len(override) > 0 {
		return override
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return nil
	}
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
				continue // 只广播 IPv4(直连探测按 ip:port 直拼)
			}
			out = append(out, net.JoinHostPort(ipNet.IP.String(), port))
		}
	}
	return out
}

// startXferDirect 启动直连监听(进程级,不随 WS 连接起伏)。
// 监听失败返回错误(调用方降级为中继模式,不致命)。
func startXferDirect(ctx context.Context, cfg Config) (*xferDirectServer, error) {
	ln, err := net.Listen("tcp", cfg.XferListen)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", cfg.XferListen, err)
	}
	cert, err := generateSelfSignedCert(cfg.AgentID)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("生成自签证书失败: %w", err)
	}
	s := &xferDirectServer{
		ln:     ln,
		addrs:  advertiseAddrs(cfg.XferListen, cfg.XferAdvertise),
		dataTk: make(map[string]xferDataToken),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /xfer/probe", s.handleProbe)
	mux.HandleFunc("POST /xfer/begin", s.handleBegin)
	mux.HandleFunc("POST /xfer/data", s.handleData)
	mux.HandleFunc("POST /xfer/fetch", s.handleFetch)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()
	// 票据懒清理(60s 一轮,与票据 TTL 同量级)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				xferTickets.sweep(time.Now())
				s.sweepDataTokens(10 * time.Minute)
			}
		}
	}()
	go func() {
		if err := s.srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("[xfer-direct] 服务退出: %v", err)
		}
	}()
	log.Printf("[xfer-direct] 直连监听 %s(广播 %v)", cfg.XferListen, s.addrs)
	return s, nil
}

// Addrs 返回广播地址(注册时上报给 Relay)。
func (s *xferDirectServer) Addrs() []string { return s.addrs }

func writeXferJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func xferErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeXferJSON(w, code, map[string]any{"error": fmt.Sprintf(format, args...)})
}

// handleProbe 探活:票据 peek(不消费),CLI 据此从 addrs 列表选可达地址。
func (s *xferDirectServer) handleProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Direction string `json:"direction"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	if !ticketAuth(r, body.Path, body.Direction, false) {
		xferErr(w, http.StatusUnauthorized, "票据无效或已过期")
		return
	}
	writeXferJSON(w, http.StatusOK, map[string]any{"ok": true, "agent": agentBizID})
}

// handleBegin 直连握手:消费票据 → HandleInit(路径策略/续传/快路径全复用)
// → 返回 ready 镜像 + data_token。
func (s *xferDirectServer) handleBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Direction string `json:"direction"`
		TransferID string `json:"transfer_id"`
		Size       int64  `json:"size,omitempty"`
		SHA256     string `json:"sha256,omitempty"`
		Offset     int64  `json:"offset,omitempty"` // download 分片起始偏移
		Limit      int64  `json:"limit,omitempty"`  // download 分片长度
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		xferErr(w, http.StatusBadRequest, "请求体解析失败: %v", err)
		return
	}
	if body.TransferID == "" {
		// uuid 而非 UnixNano:4 路并发分片同纳秒 begin 会撞 ID,
		// sessions/dataTk 相互覆盖 → pathInUse 泄漏 → 后续全部"路径被占用"
		body.TransferID = "direct-" + uuid.NewString()
	}
	// 票据校验 + 一次性消费(方向/路径绑定在票据内)
	if !ticketAuth(r, body.Path, body.Direction, true) {
		xferErr(w, http.StatusUnauthorized, "票据无效或已过期")
		return
	}

	init := proto.FileTransferInit{
		TransferID: body.TransferID,
		Direction:  body.Direction,
		Path:       body.Path,
		Size:       body.Size,
		SHA256:     body.SHA256,
		Offset:     body.Offset,
	}
	if body.Direction == "download" && body.Limit > 0 {
		init.Size = body.Limit // 与中继分片下载同约定:分片模式下 Size 复用为 limit
	}
	ready := transfers.HandleInit(init)

	// R2 已满快路径:.part 已满且校验通过 → 直接返回成功(带 sha)
	if ready.Accepted && strings.HasPrefix(ready.Reason, "ALREADY_COMPLETE:") {
		writeXferJSON(w, http.StatusOK, map[string]any{
			"accepted": true, "already_complete": true,
			"sha256": strings.TrimPrefix(ready.Reason, "ALREADY_COMPLETE:"),
			"bytes":  ready.Size,
		})
		return
	}
	if !ready.Accepted {
		writeXferJSON(w, http.StatusForbidden, map[string]any{"accepted": false, "reason": ready.Reason})
		return
	}

	// data_token:绑定 transferID,数据阶段凭证(防同网段第三方凭 transferID 劫持)
	tokBytes := make([]byte, 32)
	_, _ = rand.Read(tokBytes)
	token := hex.EncodeToString(tokBytes)
	s.mu.Lock()
	s.dataTk[body.TransferID] = xferDataToken{token: token, created: time.Now()}
	s.mu.Unlock()

	writeXferJSON(w, http.StatusOK, map[string]any{
		"accepted":      true,
		"transfer_id":   body.TransferID,
		"resume_offset": ready.ResumeOffset,
		"size":          ready.Size,
		"data_token":    token,
	})
}

// popDataToken 校验并取出 data_token(数据阶段一次性,10min TTL)。
func (s *xferDirectServer) popDataToken(r *http.Request, transferID string) bool {
	token := r.Header.Get("X-Go-RMM-Data-Token")
	if token == "" {
		token = r.URL.Query().Get("data_token")
	}
	s.mu.Lock()
	e, exists := s.dataTk[transferID]
	delete(s.dataTk, transferID) // 一次性:数据阶段只允许一次
	s.mu.Unlock()
	if !exists || time.Since(e.created) > 10*time.Minute {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(e.token)) == 1
}

// sweepDataTokens 清理超龄 data_token(abandoned begin 的兜底)。
func (s *xferDirectServer) sweepDataTokens(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.dataTk {
		if time.Since(e.created) > ttl {
			delete(s.dataTk, id)
		}
	}
}

// handleData 上传数据流:body 从 resume_offset 起的剩余字节。
func (s *xferDirectServer) handleData(w http.ResponseWriter, r *http.Request) {
	transferID := r.URL.Query().Get("transfer_id")
	if !s.popDataToken(r, transferID) {
		log.Printf("[xfer-direct] data 鉴权失败 transferID=%s remote=%s", transferID, r.RemoteAddr)
		xferErr(w, http.StatusUnauthorized, "data_token 无效")
		return
	}

	completeCh := make(chan proto.FileTransferComplete, 1)
	noopProgress := func(proto.FileTransferProgress) {}
	collectComplete := func(c proto.FileTransferComplete) { completeCh <- c }

	// 读 body 喂传输引擎(写文件 + 增量 hash + 终态)
	buf := Get256K()
	defer Put256K(buf)
	for {
		n, readErr := r.Body.Read(buf)
		if n > 0 {
			transfers.ReceiveUploadData(transferID, buf[:n], false, noopProgress, collectComplete)
		}
		if readErr != nil {
			if readErr == io.EOF {
				transfers.ReceiveUploadData(transferID, nil, true, noopProgress, collectComplete)
			} else {
				transfers.Cancel(transferID)
				xferErr(w, http.StatusBadRequest, "客户端数据流中断: %v", readErr)
				return
			}
			break // body 读尽:退出读循环,进入终态等待(漏掉会 EOF 空转死循环,永不回响应)
		}
	}
	select {
	case c := <-completeCh:
		if c.Success {
			writeXferJSON(w, http.StatusOK, map[string]any{
				"transfer_id": c.TransferID, "bytes": c.Bytes, "sha256": c.SHA256Actual,
			})
		} else {
			writeXferJSON(w, http.StatusInternalServerError, map[string]any{"error": c.Error})
		}
	case <-time.After(5 * time.Minute):
		xferErr(w, http.StatusGatewayTimeout, "等待传输终态超时")
	}
}

// handleFetch 下载数据流:octet-stream + trailer 携带 SHA256/字节数。
func (s *xferDirectServer) handleFetch(w http.ResponseWriter, r *http.Request) {
	transferID := r.URL.Query().Get("transfer_id")
	if !s.popDataToken(r, transferID) {
		xferErr(w, http.StatusUnauthorized, "data_token 无效")
		return
	}

	// trailer 预声明(HTTP/1.1 chunked 尾部头;经代理可能被剥,CLI 侧缺省容错)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "X-Xfer-Sha256, X-Xfer-Bytes")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	completeCh := make(chan proto.FileTransferComplete, 1)
	noopProgress := func(proto.FileTransferProgress) {}
	collect := func(c proto.FileTransferComplete) { completeCh <- c }

	writeChunk := func(data []byte) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	go transfers.streamDownload(r.Context(), transferID, writeChunk, nil, noopProgress, collect)

	select {
	case c := <-completeCh:
		if c.Success {
			w.Header().Set(http.TrailerPrefix+"X-Xfer-Sha256", c.SHA256Actual)
			w.Header().Set(http.TrailerPrefix+"X-Xfer-Bytes", fmt.Sprint(c.Bytes))
		} else {
			log.Printf("[xfer-direct] 下载失败 %s: %s", transferID, c.Error)
		}
	case <-time.After(30 * time.Minute):
		log.Printf("[xfer-direct] 下载超时 %s", transferID)
	}
}
