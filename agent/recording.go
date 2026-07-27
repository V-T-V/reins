// Package main — Shell 会话录屏(L1 神级提升)。
//
// asciinema v2 格式录制 shell 会话,用于安全审计/取证回放。
// 每个 shell 会话的 stdout/stderr 都被记录,会话结束后归档。
//
// 格式(asciinema v2):
//
//	{"version":2,"width":80,"height":24,"timestamp":...,"env":{"SHELL":...}}
//	[0.123,"o","hello world\n"]
//	[0.456,"o","second output\n"]
//
// 第一行 header,后续每行 [相对秒, "o"=output, 内容]。
// 可用 asciinema play recording.cast 回放。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionRecorder 录制单个 shell 会话。
type SessionRecorder struct {
	mu       sync.Mutex
	file     *os.File
	startTime time.Time
	sessionID string
	agentID   string
}

// recordingDir 录屏文件目录。
func recordingDir() string {
	return filepath.Join(exeDir(), "recordings")
}

// NewSessionRecorder 创建录屏文件。
func NewSessionRecorder(sessionID, agentID string, cols, rows int) (*SessionRecorder, error) {
	dir := recordingDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建录屏目录失败: %w", err)
	}
	// 文件名: <timestamp>-<sessionID>.cast
	filename := fmt.Sprintf("%s.cast", sessionID)
	path := filepath.Join(dir, filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建录屏文件失败: %w", err)
	}
	r := &SessionRecorder{
		file:      f,
		startTime: time.Now(),
		sessionID: sessionID,
		agentID:   agentID,
	}
	// 写 asciinema v2 header
	ts := r.startTime.Unix()
	header := map[string]interface{}{
		"version":   2,
		"width":     cols,
		"height":    rows,
		"timestamp": ts,
		"env": map[string]string{
			"SHELL": "powershell",
			"TERM":  "xterm-256color",
		},
		"command": "go-rmm shell " + sessionID,
		"title":   "go-rmm session " + agentID,
	}
	hd, _ := json.Marshal(header)
	f.Write(hd)
	f.Write([]byte("\n"))
	return r, nil
}

// WriteOutput 记录一条 shell 输出。
func (r *SessionRecorder) WriteOutput(data []byte) {
	if r == nil || r.file == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := time.Since(r.startTime).Seconds()
	// asciicast v2: [elapsed, "o", data]
	entry := []interface{}{elapsed, "o", string(data)}
	line, _ := json.Marshal(entry)
	r.file.Write(line)
	r.file.Write([]byte("\n"))
}

// Close 关闭录屏文件。
func (r *SessionRecorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}
