package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// SessionManager 管理交互式 shell 会话。
//
// H3：终端不走 Executor 接口（无终态），由 recvLoop 直接驱动。
// 会话用内存注册表（不进 bbolt），Agent 重启后会话丢失（符合 RMM 交互预期）。
//
// H3 第一阶段用纯 os/exec 管道（有回显但无 Tab 补全/方向键）。
// H4 会替换为 ConPTY（完整终端体验）。
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*ShellSession
}

// ShellSession 一个交互式 shell 会话。
type ShellSession struct {
	ID        string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	lastInput time.Time
	recorder  *SessionRecorder // L1: 会话录屏
}

// NewSessionManager 构造会话管理器。
func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*ShellSession)}
}

// available 检查终端是否可用(H4 的 ConPTY 可用时升级)。
func (sm *SessionManager) available() bool {
	return true // 管道模式总是可用
}

// GetRecorder 获取会话的录屏器(L1:供 pipeToOutput 调用)。
func (sm *SessionManager) GetRecorder(sessionID string) *SessionRecorder {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[sessionID]; ok {
		return s.recorder
	}
	return nil
}

// OpenSession 启动持久 shell 进程，stdout/stderr 经 outputCb 回调推送。
func (sm *SessionManager) OpenSession(sessionID string, cols, rows int, shell string, outputCb func(proto.ShellOutput)) error {
	// 选择 shell
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "powershell.exe"
		} else {
			shell = "bash"
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, shell)
	setProcAttr(cmd) // P1-A1:进程组,防孙进程孤儿
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stdin.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		return err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}

	session := &ShellSession{
		ID:        sessionID,
		cmd:       cmd,
		stdin:     stdin,
		cancel:    cancel,
		lastInput: time.Now(),
	}
	// L1: 启动会话录屏(asciinema v2 格式)
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	rec, recErr := NewSessionRecorder(sessionID, agentBizID, cols, rows)
	if recErr == nil {
		session.recorder = rec
	} // 录屏失败不阻塞会话
	sm.mu.Lock()
	sm.sessions[sessionID] = session
	sm.mu.Unlock()

	// 读 stdout → outputCb
	go pipeToOutput(stdout, sessionID, "stdout", outputCb)
	// 读 stderr → outputCb
	go pipeToOutput(stderr, sessionID, "stderr", outputCb)
	// 等进程退出 → 发 exit output
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		outputCb(proto.ShellOutput{
			SessionID: sessionID,
			Exit:      true,
			ExitCode:  exitCode,
		})
		sm.CloseSession(sessionID, "process exited")
	}()

	log.Printf("[shell] 会话 %s 已开启 (shell=%s)", sessionID, shell)
	return nil
}

// WriteInput 向会话 stdin 写入字节。
func (sm *SessionManager) WriteInput(sessionID string, data []byte) error {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("会话不存在: %s", sessionID)
	}
	session.lastInput = time.Now()
	_, err := session.stdin.Write(data)
	return err
}

// CloseSession 关闭会话。
func (sm *SessionManager) CloseSession(sessionID, reason string) {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	if ok {
		delete(sm.sessions, sessionID)
	}
	sm.mu.Unlock()
	if !ok {
		return
	}
	session.cancel()
	session.stdin.Close()
	// L1: 关闭录屏文件
	if session.recorder != nil {
		session.recorder.Close()
	}
	log.Printf("[shell] 会话 %s 已关闭 (reason=%s)", sessionID, reason)
}

// CloseAll 关闭所有会话（连接断开时调用，防资源泄漏）。
func (sm *SessionManager) CloseAll() {
	sm.mu.Lock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	sm.mu.Unlock()
	for _, id := range ids {
		sm.CloseSession(id, "connection closed")
	}
}

// ReapIdle 清理空闲超时的会话。
func (sm *SessionManager) ReapIdle(timeout time.Duration) {
	sm.mu.Lock()
	var stale []string
	now := time.Now()
	for id, s := range sm.sessions {
		if now.Sub(s.lastInput) > timeout {
			stale = append(stale, id)
		}
	}
	sm.mu.Unlock()
	for _, id := range stale {
		sm.CloseSession(id, "idle timeout")
	}
}

// pipeToOutput 从 reader 读字节，经 outputCb 推送 ShellOutput。
func pipeToOutput(reader io.Reader, sessionID, stream string, cb func(proto.ShellOutput)) {
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := buf[:n]
			// L1: 录屏(只录 stdout,不录 stderr 噪音)
			if stream == "stdout" {
				if rec := sessions.GetRecorder(sessionID); rec != nil {
					rec.WriteOutput(data)
				}
			}
			cb(proto.ShellOutput{
				SessionID: sessionID,
				DataB64:   base64.StdEncoding.EncodeToString(data),
				Stream:    stream,
			})
		}
		if err != nil {
			return
		}
	}
}

// 全局 SessionManager 实例。
var sessions = NewSessionManager()
