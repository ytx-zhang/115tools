// Package web 提供 HTTP 层：管理面板（登录鉴权、配置、离线下载、任务触发与状态推送）
// 与 /download 直链重定向器（Emby 免验证使用，见 redirector.go）。
// 所有业务逻辑经 app.App 代理；依赖 drive 仅用于接口/数据类型。
package web

import (
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ytx-zhang/115tools/internal/app"
	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/logs"
)

//go:embed all:static
var staticFS embed.FS

// Deps 由 main 注入的依赖。App 聚合所有模块，web 仅通过它交互。
// Cache 为透传本地缓存层（透传命中本地可跳过 115 上游回源），nil 时禁用本地命中。
type Deps struct {
	App    *app.App
	AppCtx context.Context
	Cache  *cache.Cache
}

// Server 管理面板 HTTP 服务。
type Server struct {
	Deps
	sessions sessionStore
}

// Register 注册全部管理路由到 mux。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := &Server{Deps: d}
	s.registerStatic(mux)

	// /download 直链重定向器（Emby 依赖，免鉴权）：直接持有 App.API 客户端实例（类型引用，不调用 app 方法），
	// 登录方式热切换后自动跟随。不能放保护路由组，需独立注册。
	redirector := NewRedirector(d.App.API, d.Cache)
	mux.Handle("GET /download", http.HandlerFunc(redirector.RedirectToRealURL))

	// 公开接口（登录/会话探测无需鉴权）
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/version", handleVersion) // 版本探针（公开）

	// 受保护接口
	protected := map[string]http.HandlerFunc{
		"POST /api/logout":          s.handleLogout,
		"GET /api/logs":             s.handleLogs,
		"GET /api/logs/counts":      s.handleLogsCounts,
		"GET /api/logs/history":     s.handleLogsHistory,
		"POST /api/logs/clear":      s.handleLogsClear,
		"POST /api/task/{name}":     s.handleTaskStart,
		"DELETE /api/task/{name}":   s.handleTaskStop,
		"GET /api/config":           s.handleGetConfig,
		"PUT /api/config":           s.handleSaveConfig,
		"GET /api/offline/tasks":    s.handleOfflineTasks,
		"GET /api/offline/quota":    s.handleOfflineQuota,
		"POST /api/offline/add":     s.handleOfflineAdd,
		"POST /api/offline/torrent": s.handleOfflineTorrent,
		"POST /api/offline/delete":  s.handleOfflineDelete,
		"POST /api/offline/clear":   s.handleOfflineClear,
		"GET /api/cache":            s.handleCacheList,
		"POST /api/cache/delete":    s.handleCacheDelete,
	}
	for pattern, h := range protected {
		mux.Handle(pattern, s.protect(h))
	}
	return s
}

// registerStatic 注册前端页面与静态资源（公开访问；接口层单独鉴权）。
func (s *Server) registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		logs.Error(logs.ModuleSystem, "静态资源目录缺失", "错误", err)
		return
	}
	indexData, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		logs.Error(logs.ModuleSystem, "读取 index.html 失败", "错误", err)
		indexData = []byte("<h1>index.html missing</h1>")
	}
	// 前端资源缓存策略：入口 HTML 与静态文件统一 no-cache（允许缓存但强制每次回源验证）。
	// ⚠️ embed.FS 的 ModTime() 恒为零值（src/embed/embed.go），标准库 FileServer/ServeContent
	// 因此不生成 Last-Modified、If-Modified-Since 验证永不触发。必须自己基于内容指纹生成 ETag：
	// 预先遍历全部静态文件算 SHA1；go build 重新嵌入后内容变化 → ETag 全变 → 浏览器必然拉新，
	// 天然解决「发布新版后用户仍看到旧页面/旧 JS」的缓存陈旧问题（无构建步骤，无法用内容指纹文件名+immutable）。
	// FileServer 的 checkPreconditions 会用预设的 ETag 头处理 If-None-Match → 304。
	etags := make(map[string]string, 16)
	if err := fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		h := sha1.Sum(data)
		etags[path] = `"` + hex.EncodeToString(h[:]) + `"`
		return nil
	}); err != nil {
		// 指纹计算失败：ETag 缺失时浏览器按 no-cache 每次回源验证，功能仍可用，仅告警
		logs.Warn(logs.ModuleSystem, "静态资源指纹计算失败", "错误", err)
	}

	indexHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		etag := etags["index.html"]
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if _, err := w.Write(indexData); err != nil {
			logs.Warn(logs.ModuleSystem, "写入首页响应失败", "错误", err)
		}
	})
	mux.Handle("GET /{$}", indexHandler)

	fileServer := http.StripPrefix("/static/", http.FileServerFS(sub))
	staticHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, "/static/")]; ok {
			w.Header().Set("ETag", etag) // FileServer 据此处理 If-None-Match → 304
		}
		fileServer.ServeHTTP(w, r)
	})
	mux.Handle("GET /static/", staticHandler)
}

// ──── HTTP 辅助 ────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 客户端已断开等场景编码/写出失败：连接不可恢复，仅告警
		logs.Warn(logs.ModuleSystem, "写入JSON响应失败", "状态码", code, "错误", err)
	}
}

// writeOK 收敛「成功响应 map[string]bool{"ok":true}」的样板。
func writeOK(w http.ResponseWriter, code int) {
	writeJSON(w, code, map[string]bool{"ok": true})
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, a...)})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(v)
}

// clientIP 返回真实客户端 IP（支持反代：XFF/X-Real-IP/X-Client-IP → RemoteAddr）。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if before, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(before)
		}
		return xff
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}
