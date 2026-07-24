package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
)

// N1 修复：Relay 内嵌 Console 的 Web 前端，不再需要独立 Console 进程。
// 部署从"3 exe 分别启动"简化为"1 个 Relay exe + 1 个 Agent exe"。

//go:embed web/index.html web/app.js
var webFS embed.FS

// registerWebUI 在根路径 / 提供 Web 控制台静态资源。
func (r *Relay) registerWebUI(mux *http.ServeMux) {
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Printf("[relay] Web 资源加载失败: %v", err)
		return
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", fileServer)
	log.Printf("[relay] Web 控制台已内嵌（/ 路径）")
}
