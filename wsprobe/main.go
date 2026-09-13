// wsprobe — 生产 relay WS 长连接探针(模拟 agent 心跳,诊断 30s 掉线循环)。
// 用法: go run ./wsprobe <token> <minutes>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

func main() {
	token := os.Args[1]
	minutes := 5
	if len(os.Args) > 2 {
		minutes, _ = strconv.Atoi(os.Args[2])
	}
	url := "wss://203.0.113.99/rmm/agent"

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(minutes)*time.Minute+30*time.Second)
	defer cancel()

	hdr := http.Header{"Authorization": {"Bearer " + token}}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		log.Fatalf("拨号失败: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	agentID := "wsprobe-" + strconv.FormatInt(time.Now().UnixMilli()%100000, 10)

	// register
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID:      agentID,
		Hostname:     "wsprobe",
		OS:           "linux/amd64",
		Version:      "probe",
		Capabilities: []string{"powershell"},
	})
	regBytes, _ := json.Marshal(regEnv)
	if err := c.Write(ctx, websocket.MessageText, regBytes); err != nil {
		log.Fatalf("register 写失败: %v", err)
	}
	log.Printf("已注册 %s,开始 15s 心跳探测(总时长 %d 分钟)", agentID, minutes)

	// 读 goroutine:打印收到的帧(ack/其他),记录最近收包时间
	lastRecv := time.Now()
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				log.Printf("读结束(断连): %v", err)
				return
			}
			lastRecv = time.Now()
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil {
				log.Printf("recv %s @ %s", env.Type, lastRecv.Format("15:04:05"))
			}
		}
	}()

	// 心跳循环:15s 一发,记录发送与 ack 间隔
	seq := int64(0)
	for {
		select {
		case <-ctx.Done():
			log.Printf("探测结束: 总存活 %d 分钟, 最后收包 %s 前", minutes, time.Since(lastRecv).Round(time.Second))
			return
		default:
		}
		seq++
		hb := proto.Heartbeat{AgentID: agentID, Seq: seq}
		hbEnv, _ := proto.NewEnvelope(proto.TypeHeartbeat, agentID, "relay", hb)
		hbBytes, _ := json.Marshal(hbEnv)
		sent := time.Now()
		_ = sent
		if err := c.Write(ctx, websocket.MessageText, hbBytes); err != nil {
			log.Printf("心跳写失败(断连): %v", err)
			return
		}
		time.Sleep(2 * time.Second)
		ackGap := time.Since(lastRecv)
		if ackGap > 30*time.Second {
			log.Printf("⚠️ 心跳 %d 发出后已 %s 无任何收包(ack 丢失?)", seq, ackGap.Round(time.Second))
		} else {
			log.Printf("心跳 %d 发出, 距最近收包 %s", seq, ackGap.Round(time.Second))
		}
		time.Sleep(13 * time.Second)
	}
}

// 兼容 fmt 未使用告警
var _ = fmt.Sprintf
