package main

// Phase 2 数据面分离:Agent 侧数据连接池。
//
// 动机:Agent 到 Relay 只有单条反向 WS,文件传输二进制帧与心跳/任务/隧道
// 挤同一 TCP——生产实证并发下载吃满上行直接饿死心跳(P2-12 被迫加限速),
// 且单流吞吐受 cwnd×RTT 封顶(跨公网实测 0.54 MB/s)。
//
// 结构:配置 data_conns=N(默认 0=关闭,建议 2~4)后,主连接注册成功起 N 条
// 数据 WS(同端点,?data=<i>),握手 data_join→data_join_ack。此后:
//   - Relay→Agent 上传二进制帧:Relay 在池上轮转分发(见 relay 侧)
//   - Agent→Relay 下载二进制帧:本侧 writeFramePooled 轮转
//   - 心跳/任务/隧道/传输控制信令:仍走主连接
//
// 滚动升级安全:旧 Relay 不回 data_join_ack → 握手超时,该连接重连退避;
// 池始终为空时所有路径回落主连接,行为与旧版完全一致。数据连接断开自动
// 重连(退避),主连接断开(重连)则 connCtx 取消、数据连接整体重建。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// dataConnDialTimeout 单条数据连接握手(连接+join+ack)总时限。
const dataConnDialTimeout = 5 * time.Second

// dataConnLoop 维护第 index 条数据连接的完整生命周期(连→握手→入池→读→重连)。
// 绑定 connCtx:主连接断开时整体退出,由下一次 connectAndServe 重建。
func (a *Agent) dataConnLoop(ctx context.Context, mainWC *wsConn, index int) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := a.runDataConn(ctx, mainWC, index)
		if ctx.Err() != nil {
			return
		}
		log.Printf("[agent-data] 数据连接 %d 断开: %v(%v 后重连)", index, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runDataConn 建立一条数据连接并服务到断开。返回断开原因。
func (a *Agent) runDataConn(ctx context.Context, mainWC *wsConn, index int) error {
	// 1. 拨号(与主连接同凭证:token 或 mTLS 客户端证书)
	header := http.Header{}
	if a.certs == nil {
		if a.cfg.Token != "" {
			header.Set("Authorization", "Bearer "+a.cfg.Token)
		}
	}
	dialOpts := &websocket.DialOptions{HTTPHeader: header}
	if a.certs != nil {
		dialOpts.HTTPClient = mtlsHTTPClient(a.certs)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, dataConnDialTimeout)
	c, _, err := websocket.Dial(dialCtx, a.dataConnURL(index), dialOpts) //nolint:bodyclose // 成功时 resp.Body 为 nil
	dialCancel()
	if err != nil {
		return fmt.Errorf("拨号失败: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(32 << 20)
	wc := newWSConn(c)
	// writer 先于首次写启动(writeEnvelope 经 wsx 队列等 writer 消费,
	// 主连接同款时序——先 startWriter 后写)
	wc.startWriter(ctx)
	defer func() { wc.closeNow() }()

	// 2. 握手:发 data_join → 等 ack(旧 Relay 不识别该类型不回帧,读阻塞到超时)
	handshakeCtx, hsCancel := context.WithTimeout(ctx, dataConnDialTimeout)
	defer hsCancel()
	if err := wc.writeEnvelope(handshakeCtx, proto.TypeDataConnJoin, "relay",
		proto.DataConnJoin{AgentID: a.cfg.AgentID, Index: index}); err != nil {
		return fmt.Errorf("发 join 失败: %w", err)
	}
	_, data, err := wc.read(handshakeCtx)
	if err != nil {
		return fmt.Errorf("等 ack 超时(Relay 可能不支持数据面): %w", err)
	}
	var env proto.Envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Type != proto.TypeDataConnAck {
		return fmt.Errorf("首条消息须为 data_join_ack,实际 %q", env.Type)
	}

	// 3. 入池 + 服务
	a.addDataConn(wc)
	defer a.removeDataConn(wc)
	log.Printf("[agent-data] 数据连接 %d 就绪(池=%d)", index, a.dataConnCount())

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mt, data, err := wc.read(ctx)
		if err != nil {
			return err
		}
		if mt != websocket.MessageBinary {
			continue // 数据面不承载控制信令,JSON 帧忽略
		}
		// 上传数据帧:与主连接收到二进制帧同语义,progress/complete 回调
		// 仍经主连接发出(传输控制信令面不变)
		a.handleUploadData(ctx, mainWC, data)
	}
}

// writeFramePooled Agent→Relay 二进制帧写入:按帧内 transferID 稳定绑定数据
// 连接(与 Relay 侧 DataConnFor 同款 fnv32 规则——同传输恒走同一条 TCP,
// 保字节流顺序;跨传输并行)。池空/该连接刚断回落主连接。
func (a *Agent) writeFramePooled(ctx context.Context, mainWC *wsConn, frame []byte) error {
	if transferID, _, _, err := proto.ParseBinaryFrame(frame); err == nil {
		if wc := a.nextDataConnFor(transferID); wc != nil {
			if err := wc.writeBinaryData(ctx, frame); err == nil {
				return nil
			}
			// 该数据连接刚断(池未及清理):回落主连接,不因单连接抖动丢帧
		}
	}
	return mainWC.writeBinaryData(ctx, frame)
}

// --- 池管理 ---

func (a *Agent) addDataConn(wc *wsConn) {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	a.dataConns = append(a.dataConns, wc)
}

func (a *Agent) removeDataConn(wc *wsConn) {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	for i, c := range a.dataConns {
		if c == wc {
			a.dataConns = append(a.dataConns[:i], a.dataConns[i+1:]...)
			return
		}
	}
}

// nextDataConnFor 按 key 稳定取数据连接(与 relay DataConnFor 同款 fnv32)。
func (a *Agent) nextDataConnFor(key string) *wsConn {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	n := len(a.dataConns)
	if n == 0 {
		return nil
	}
	return a.dataConns[int(fnv32Key(key))%n]
}

// fnv32Key FNV-1a 32 位(与 relay 侧 fnv32 逐字节一致)。
func fnv32Key(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func (a *Agent) dataConnCount() int {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	return len(a.dataConns)
}

// dataConnURL 构造第 index 条数据连接的 WS 地址(主端点 + ?data=<i>)。
func (a *Agent) dataConnURL(index int) string {
	u := a.cfg.RelayURL
	sep := "?"
	if parsed, err := url.Parse(u); err == nil && parsed.RawQuery != "" {
		sep = "&"
	}
	return fmt.Sprintf("%s%sdata=%d", u, sep, index)
}
