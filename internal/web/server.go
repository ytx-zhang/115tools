// Package web 提供 HTTP 层：管理面板（登录鉴权、配置、离线下载、任务触发与状态推送）
// 与 /download 直链重定向器（Emby 免验证使用，见 redirector.go）。
// 所有业务逻辑经 app.App 代理；依赖 drive 仅用于接口/数据类型。
package web

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/app"
	"github.com/ytx-zhang/115tools/internal/logs"
)

//go:embed all:static
var staticFS embed.FS

// Deps 由 main 注入的依赖。App 聚合所有模块，web 仅通过它交互。
type Deps struct {
	App    *app.App
	AppCtx context.Context
}

// Server 管理面板 HTTP 服务。
type Server struct {
	Deps
	sessions sessionStore
}

// Register 注册全部管理路由到 mux。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := &Server{Deps: d, sessions: sessionStore{tokens: make(map[string]time.Time)}}
	s.registerStatic(mux)

	// /download 直链重定向器（Emby 依赖，免鉴权）：直接持有 App.API 客户端实例（类型引用，不调用 app 方法），
	// 登录方式热切换后自动跟随。不能放保护路由组，需独立注册。
	redirector := NewRedirector(d.App.API)
	mux.Handle("GET /download", http.HandlerFunc(redirector.RedirectToRealURL))

	// 公开接口（登录/会话探测无需鉴权）
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)

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
	_ = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
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
	})

	indexHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		etag := staticETag(etags["index.html"], r)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(indexData)
	})
	mux.Handle("GET /{$}", gzipMiddleware(indexHandler))

	fileServer := http.StripPrefix("/static/", http.FileServerFS(sub))
	staticHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, "/static/")]; ok {
			w.Header().Set("ETag", staticETag(etag, r)) // FileServer 据此处理 If-None-Match → 304
		}
		fileServer.ServeHTTP(w, r)
	})
	mux.Handle("GET /static/", gzipMiddleware(staticHandler))
}

// ──── 静态资源 gzip 压缩 ────

// staticETag 按请求协商结果给 ETag 加编码后缀：gzip 请求在引号内追加 -gzip。
// ⚠️ 后缀必须加在引号内部（"hex-gzip"）：若加在引号外（"hex"-gzip）是非法 ETag 格式，
// FileServer 的 scanETag 只会解析到第一个引号，304 校验永不命中。
// 强 ETag 语义要求同一 ETag 对应字节完全一致的表示，不同编码必须用不同 ETag，
// 否则浏览器可能把 gzip 版缓存回放给不支持 gzip 的请求（配合 Vary 双层保险）。
func staticETag(base string, r *http.Request) string {
	if !acceptsGzip(r) {
		return base
	}
	return `"` + strings.Trim(base, `"`) + `-gzip"`
}

// acceptsGzip 判断是否用 gzip 响应：请求声明支持 gzip 且非 Range 请求。
// Range 请求跳过压缩：压缩改变字节流，Content-Range 的偏移基于原始字节会错乱。
// 管理面板静态资源（HTML/CSS/JS）浏览器不会发 Range，此判断是防御性兜底。
func acceptsGzip(r *http.Request) bool {
	return r.Header.Get("Range") == "" && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// gzipResponseWriter 透明 gzip 包装：仅在实际写 body 时压缩，304/204 无 body 原样透传。
// ⚠️ 必须删 Content-Length（长度随压缩改变，由 net/http 回退 chunked）。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz     *gzip.Writer
	wrote  bool
	status int
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wrote {
		return
	}
	g.wrote = true
	g.status = code
	if code != http.StatusNotModified && code != http.StatusNoContent {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wrote {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Close 收尾 gzip 流（写 footer）。无压缩时直接透传。
func (g *gzipResponseWriter) Close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}

// Unwrap 供 http.ResponseController 访问底层 ResponseWriter。
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// Flush 透传刷新，同时冲刷 gzip 缓冲（FileServer 对小文件不会主动 flush，保留以兼容）。
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMiddleware 按 Accept-Encoding 协商压缩，补 Vary 头后包 gzipResponseWriter。
// Vary: Accept-Encoding 必须带，否则反代缓存/浏览器会按编码混淆缓存条目。
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipResponseWriter{ResponseWriter: w}
		// 响应已写出，此处 Close 仅收尾 gzip 尾部，出错也无从补救
		defer func() { _ = gw.Close() }()
		next.ServeHTTP(gw, r)
	})
}

// ──── HTTP 辅助 ────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
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
