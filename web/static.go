package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
)

//go:embed all:static
var staticFS embed.FS

// registerStatic 注册前端页面与静态资源（公开访问；接口层单独鉴权）。
func (s *Server) registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("[WEB] 静态资源目录缺失", "错误信息", err)
		return
	}

	// 首页：启动时读取一次 embed 内容并缓存
	indexData, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		slog.Error("[WEB] 读取 index.html 失败", "错误信息", err)
		indexData = []byte("<h1>index.html missing</h1>")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexData)
	})

	// 其余静态资源（css/js）
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
}
