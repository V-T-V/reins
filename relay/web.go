package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// N1 修复：Relay 内嵌 Console 的 Web 前端，不再需要独立 Console 进程。
// 部署从"3 exe 分别启动"简化为"1 个 Relay exe + 1 个 Agent exe"。

//go:embed web/index.html web/app.js web/install.ps1
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

// registerDownloadDir 在 /dl/ 提供 Agent 二进制下载（一行安装用）。
func (r *Relay) registerDownloadDir(mux *http.ServeMux) {
	dlPath := dlDir()
	// 确保目录存在
	if err := os.MkdirAll(dlPath, 0755); err != nil {
		log.Printf("[relay] 下载目录创建失败 %s: %v", dlPath, err)
		return
	}
	dlFS := http.FileServer(http.Dir(dlPath))
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, req *http.Request) {
		// 安全检查：仅允许 .exe 下载，拒绝目录遍历
		if filepath.Ext(req.URL.Path) != ".exe" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Disposition", "attachment")
		http.StripPrefix("/dl/", dlFS).ServeHTTP(w, req)
	})
	log.Printf("[relay] Agent 下载目录已启用: %s", dlPath)
}
