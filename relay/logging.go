// Package main — 结构化日志(S2 神级提升)。
//
// 用 Go 1.21+ 标准库 log/slog 替代 log.Printf。
// 生产环境输出 JSON(可接 ELK/Loki),开发模式输出彩色文本。
//
// 关键:带 trace context(agent_id/task_id/duration_ms 等字段化),
// 而非纯文本拼接,便于结构化检索。
package main

import (
	"log/slog"
	"os"
)

// slogLogger 全局结构化日志器。
// 由 main.go 初始化(根据 -dev flag 选 JSON 或 Text handler)。
var slogLogger *slog.Logger

// initLogger 初始化 slog 日志器。
// dev=true 用 Text(人类可读),否则用 JSON(机器可读)。
func initLogger(dev bool) {
	var handler slog.Handler
	if dev {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
	}
	slogLogger = slog.New(handler)
	slog.SetDefault(slogLogger)
}
